package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The tool's network-free halves: the rewrite (every pin applied, no
// floating reference left), the CSV validation (digest-pinned or
// refused), the platform verification's arithmetic and JSON decoding,
// the index hash (bytes that do not hash to the pin are refused
// before anything is pulled), the child selection (concrete image
// manifests only, pulled by their digests), the platform comparison
// (identical content or refused), the strict export reading (end
// marker included), the pull-then-export sequencing (through a faked
// crane runner the read-path helpers take as a parameter: a failed
// pull never reaches the export, and the export unpacks exactly the
// archive the pull wrote), the flat-name write gate, the digest-only
// refusal, the crane subprocess's isolated credential environment
// (inherited credentials and Docker HTTP headers cannot reach it, and
// the wiring that builds its command), and run's removal of the
// crane workspace when the build fails. The crane reads against the
// real registries are exercised by running the tool itself.

// fixtureManifests is a miniature bundle: a CSV whose image references
// carry the floating tags the pins rewrite, plus one CRD, so the
// validation has something to validate.
func fixtureManifests() map[string][]byte {
	return map[string][]byte{
		"compliance-operator.clusterserviceversion.yaml": []byte(`apiVersion: operators.coreos.com/v1alpha1
kind: ClusterServiceVersion
metadata:
  name: compliance-operator.v1.0.0-test
  annotations:
    must-gather-image: quay.io/redhat-user-workloads/ocp-isc-tenant/compliance-operator-must-gather-dev:master
spec:
  version: 1.0.0-test
  relatedImages:
  - name: operator
    image: quay.io/redhat-user-workloads/ocp-isc-tenant/compliance-operator-dev:master
  install:
    strategy: deployment
    spec:
      deployments:
      - name: compliance-operator
        spec:
          template:
            spec:
              containers:
              - name: operator
                image: quay.io/redhat-user-workloads/ocp-isc-tenant/compliance-operator-dev:master
                env:
                - name: RELATED_IMAGE_OPENSCAP
                  value: quay.io/redhat-user-workloads/ocp-isc-tenant/compliance-operator-openscap-dev:master
  customresourcedefinitions:
    owned:
    - name: profilebundles.compliance.openshift.io
      group: compliance.openshift.io
      kind: ProfileBundle
      version: v1alpha1
`),
		"compliance.openshift.io_profilebundles.yaml": []byte(`apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: profilebundles.compliance.openshift.io
`),
	}
}

// fixturePins mirrors the real table's shape against the fixtures.
func fixturePins() []imagePin {
	return []imagePin{
		{
			floating: "quay.io/redhat-user-workloads/ocp-isc-tenant/compliance-operator-dev:master",
			digest:   "sha256:" + strings.Repeat("a", 64),
		},
		{
			floating: "quay.io/redhat-user-workloads/ocp-isc-tenant/compliance-operator-openscap-dev:master",
			digest:   "sha256:" + strings.Repeat("b", 64),
		},
		{
			floating: "quay.io/redhat-user-workloads/ocp-isc-tenant/compliance-operator-must-gather-dev:master",
			digest:   "sha256:" + strings.Repeat("c", 64),
		},
	}
}

// TestRewriteManifestsPinsEveryReference checks the rewrite's contract:
// every pinned floating reference becomes its digest, and a floating
// reference no pin covers refuses the whole set.
func TestRewriteManifestsPinsEveryReference(t *testing.T) {
	rewritten, err := rewriteManifests(fixtureManifests(), fixturePins())
	if err != nil {
		t.Fatalf("rewriteManifests: %v", err)
	}
	csv := rewritten["compliance-operator.clusterserviceversion.yaml"]
	for _, pin := range fixturePins() {
		if bytes.Contains(csv, []byte(pin.floating)) {
			t.Errorf("the floating reference %s survived the rewrite", pin.floating)
		}
		if !bytes.Contains(csv, []byte(pin.digest)) {
			t.Errorf("the digest %s does not appear in the rewritten CSV", pin.digest)
		}
	}
	// Untouched content survives byte-for-byte.
	if !bytes.Equal(rewritten["compliance.openshift.io_profilebundles.yaml"], fixtureManifests()["compliance.openshift.io_profilebundles.yaml"]) {
		t.Error("a manifest with no image references was modified")
	}
}

// TestRewriteManifestsRefusesUnknownFloatingReference pins the refusal:
// a floating reference outside the pin table must fail the bump, not
// ship.
func TestRewriteManifestsRefusesUnknownFloatingReference(t *testing.T) {
	manifests := fixtureManifests()
	manifests["compliance-operator.clusterserviceversion.yaml"] = append(
		manifests["compliance-operator.clusterserviceversion.yaml"],
		[]byte("    image: quay.io/redhat-user-workloads/ocp-isc-tenant/compliance-operator-unknown-dev:master\n")...,
	)
	if _, err := rewriteManifests(manifests, fixturePins()); err == nil {
		t.Fatal("rewriteManifests accepted an unknown floating reference, want the refusal")
	}
}

// TestValidateCSVAcceptsPinnedManifests runs the validation against a
// fully rewritten fixture: it must summarize the CSV and its digest
// count. The must-gather annotation is not counted — validation reads
// the spec's install and relatedImages, and the file-scan rewrite
// check is what covers annotations.
func TestValidateCSVAcceptsPinnedManifests(t *testing.T) {
	rewritten, err := rewriteManifests(fixtureManifests(), fixturePins())
	if err != nil {
		t.Fatalf("rewriteManifests: %v", err)
	}
	summary, err := validateCSV(rewritten)
	if err != nil {
		t.Fatalf("validateCSV: %v", err)
	}
	if summary.Name != "compliance-operator.v1.0.0-test" || summary.Version != "1.0.0-test" {
		t.Errorf("summary = %s %s, want the fixture's CSV name and version", summary.Name, summary.Version)
	}
	if len(summary.Images) != 3 {
		t.Errorf("summary images = %v, want the 3 spec references the fixture carries (relatedImage, deployment image, RELATED_IMAGE env)", summary.Images)
	}
}

// TestValidateCSVRefusesFloatingReference pins the other direction: a
// rewrite that leaves anything floating must not pass validation.
func TestValidateCSVRefusesFloatingReference(t *testing.T) {
	if _, err := validateCSV(fixtureManifests()); err == nil {
		t.Fatal("validateCSV accepted floating image references, want the refusal")
	}
}

// TestCompareManifestSets pins the platform comparison: identical
// content passes, and a file that differs — or exists on one platform
// only — is refused with the file named.
func TestCompareManifestSets(t *testing.T) {
	amd64 := [2]string{"linux", "amd64"}
	arm64 := [2]string{"linux", "arm64"}
	same := fixtureManifests()
	if err := compareManifestSets(same, fixtureManifests(), amd64, arm64); err != nil {
		t.Errorf("compareManifestSets refused identical manifests: %v", err)
	}
	differing := fixtureManifests()
	differing["compliance.openshift.io_profilebundles.yaml"] = append(differing["compliance.openshift.io_profilebundles.yaml"], ' ')
	err := compareManifestSets(same, differing, amd64, arm64)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("compliance.openshift.io_profilebundles.yaml")) {
		t.Errorf("compareManifestSets on differing content = %v, want the file named", err)
	}
	extra := fixtureManifests()
	extra["extra.yaml"] = []byte("apiVersion: v1\nkind: ConfigMap\n")
	err = compareManifestSets(same, extra, amd64, arm64)
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("extra.yaml")) {
		t.Errorf("compareManifestSets on a one-platform file = %v, want the file named", err)
	}
}

// fixtureIndex is a manifest list carrying both required platforms plus
// the platform-less attestation entries real indexes carry.
func fixtureIndex() []indexEntry {
	return []indexEntry{
		{Platform: &indexPlatform{OS: "linux", Architecture: "amd64"}},
		{Platform: &indexPlatform{OS: "linux", Architecture: "arm64"}},
		{},
		{},
	}
}

// TestMissingPlatforms pins the platform verification's arithmetic:
// a list carrying both required platforms reports nothing missing, and
// each absent platform is reported by name.
func TestMissingPlatforms(t *testing.T) {
	if missing := missingPlatforms(fixtureIndex()); len(missing) != 0 {
		t.Errorf("missingPlatforms = %v, want none", missing)
	}
	amdOnly := fixtureIndex()[:1]
	missing := missingPlatforms(amdOnly)
	if len(missing) != 1 || missing[0] != "linux/arm64" {
		t.Errorf("missingPlatforms = %v, want [linux/arm64]", missing)
	}
}

// TestParseManifestList pins the manifest decoding's three failure
// paths — malformed JSON, a plain manifest with no list, a list
// missing a platform — beside the pass, which returns the parsed
// children.
func TestParseManifestList(t *testing.T) {
	ref := "quay.io/org/repo@sha256:" + strings.Repeat("a", 64)
	both := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[
		{"digest":"sha256:` + strings.Repeat("b", 64) + `","platform":{"os":"linux","architecture":"amd64"}},
		{"digest":"sha256:` + strings.Repeat("c", 64) + `","platform":{"os":"linux","architecture":"arm64"}},
		{"digest":"sha256:` + strings.Repeat("d", 64) + `"}]}`)
	index, err := parseManifestList(both, ref)
	if err != nil {
		t.Errorf("parseManifestList on a list carrying both platforms: %v", err)
	} else if len(index.Manifests) != 3 {
		t.Errorf("parseManifestList returned %d children, want the 3 the list carries", len(index.Manifests))
	}
	amdOnly := []byte(`{"manifests":[{"platform":{"os":"linux","architecture":"amd64"}}]}`)
	if _, err := parseManifestList(amdOnly, ref); err == nil || !strings.Contains(err.Error(), "linux/arm64") {
		t.Errorf("parseManifestList on an amd64-only list = %v, want the linux/arm64 refusal", err)
	}
	notAList := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`)
	if _, err := parseManifestList(notAList, ref); err == nil || !strings.Contains(err.Error(), "not a manifest list") {
		t.Errorf("parseManifestList on a plain manifest = %v, want the not-a-list refusal", err)
	}
	if _, err := parseManifestList([]byte("{not json"), ref); err == nil || !strings.Contains(err.Error(), "decoding the manifest") {
		t.Errorf("parseManifestList on malformed JSON = %v, want the decode refusal", err)
	}
}

// TestVerifyManifestListHashesTheFetchedManifest pins the pins' own
// path: the bytes `crane manifest` printed are hashed against the
// requested digest before anything trusts them, so a registry that
// serves bytes the pin does not vouch for fails the pin check itself.
func TestVerifyManifestListHashesTheFetchedManifest(t *testing.T) {
	body := []byte(`{"manifests":[
		{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:` + strings.Repeat("b", 64) + `","platform":{"os":"linux","architecture":"amd64"}},
		{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:` + strings.Repeat("c", 64) + `","platform":{"os":"linux","architecture":"arm64"}}]}`)
	crane := func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		if args[0] != "manifest" {
			return nil, fmt.Errorf("unexpected crane call: %q", args)
		}
		return body, nil
	}

	// The pin names a digest these bytes do not hash to.
	if err := verifyManifestList(context.Background(), crane, "quay.io/org/repo@sha256:"+strings.Repeat("a", 64)); err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Errorf("verifyManifestList on bytes that do not hash to the pin = %v, want the hash refusal", err)
	}
	// A registry serving exactly the pinned bytes passes the hash and
	// the platform check.
	if err := verifyManifestList(context.Background(), crane, digestRefFor(body)); err != nil {
		t.Errorf("verifyManifestList on honest bytes: %v", err)
	}
}

// TestVerifyManifestListRefusesNonDigestReferences pins the digest-only
// contract: the tool must refuse a tag — or a malformed digest —
// before any registry is contacted, because regeneration requests
// digests only.
func TestVerifyManifestListRefusesNonDigestReferences(t *testing.T) {
	// The refusal must come before any runner call.
	crane := func(context.Context, io.Reader, ...string) ([]byte, error) {
		t.Error("verifyManifestList contacted the crane runner for a non-digest reference")
		return nil, errors.New("must not be called")
	}
	for _, ref := range []string{
		"quay.io/org/repo:latest",                            // a floating tag
		"quay.io/org/repo",                                   // no digest at all
		"quay.io/org/repo@sha256:short",                      // a malformed digest
		"quay.io/org/repo@sha256:" + strings.Repeat("g", 64), // not hexadecimal
		"ghcr.io/org/repo@" + strings.Repeat("a", 64),        // no algorithm
	} {
		if err := verifyManifestList(context.Background(), crane, ref); err == nil || !strings.Contains(err.Error(), "never resolves a tag") {
			t.Errorf("verifyManifestList(%q) = %v, want the digest-only refusal", ref, err)
		}
	}
}

// TestReadBundleFiles pins the export reading: every regular file
// under manifests/ is returned keyed by its stripped path, and files
// outside manifests/ and directories are not carried.
func TestReadBundleFiles(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeTarFile(t, tw, "manifests/a.yaml", "a: 1")
	writeTarFile(t, tw, "metadata/annotations.yaml", "x: y")
	writeTarDir(t, tw, "manifests/dir")
	writeTarFile(t, tw, "tests/scorecard/config.yaml", "z: 3")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	files, err := readBundleFiles(buf.Bytes())
	if err != nil {
		t.Fatalf("readBundleFiles: %v", err)
	}
	if len(files) != 1 || string(files["a.yaml"]) != "a: 1" {
		t.Errorf("readBundleFiles = %v, want exactly manifests/a.yaml with its content", files)
	}
}

// TestReadBundleFilesRefusesMalformedArchives pins the strict reading:
// a corrupt archive and a truncated entry fail instead of yielding a
// partial file set — the tool that accepted those wrote catalogs from
// half-read bundles.
func TestReadBundleFilesRefusesMalformedArchives(t *testing.T) {
	if _, err := readBundleFiles([]byte("this is not a tarball at all")); err == nil {
		t.Error("readBundleFiles accepted garbage, want the refusal")
	}

	// A valid archive cut mid-entry: the header survives, the entry's
	// data does not.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeTarFile(t, tw, "manifests/a.yaml", strings.Repeat("a", 2048))
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readBundleFiles(buf.Bytes()[:700]); err == nil {
		t.Error("readBundleFiles accepted a truncated entry, want the refusal")
	}
}

// TestReadBundleFilesRefusesDuplicateEntries pins the duplicate
// refusal: two entries claiming one name are an ambiguity, not a
// last-writer-wins.
func TestReadBundleFilesRefusesDuplicateEntries(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	writeTarFile(t, tw, "manifests/a.yaml", "first")
	writeTarFile(t, tw, "manifests/a.yaml", "second")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readBundleFiles(buf.Bytes()); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("readBundleFiles on a duplicate entry = %v, want the twice refusal", err)
	}
}

// TestReadBundleFilesRefusesAMissingEndMarker pins the end-marker
// check: Go's tar reader accepts an archive cut at a header boundary
// or after a single zero block, and both must fail here instead of
// passing as a complete export. Zero padding after the marker is
// legitimate tar blocking and stays accepted.
func TestReadBundleFilesRefusesAMissingEndMarker(t *testing.T) {
	full := exportTarball(t, map[string]string{"manifests/a.yaml": "a: 1"})
	if len(full) != 2048 || !allZeros(full[len(full)-1024:]) {
		t.Fatalf("the fixture is %d bytes and does not end in two zero blocks; fix the fixture", len(full))
	}

	cases := map[string][]byte{
		// Cut at a header boundary: the entry's data is complete, the
		// end marker is gone. Go's reader accepts this as EOF.
		"cut at a header boundary": full[:len(full)-1024],
		// One zero block where the marker needs two. Go's reader
		// accepts this as EOF too.
		"one zero block": full[:len(full)-512],
		// A complete marker followed by non-zero bytes.
		"garbage after the marker": append(append([]byte{}, full...), "garbage"...),
	}
	for name, archive := range cases {
		if _, err := readBundleFiles(archive); err == nil || !strings.Contains(err.Error(), "end marker") {
			t.Errorf("readBundleFiles on %s = %v, want the end-marker refusal", name, err)
		}
	}

	padded := append(append([]byte{}, full...), make([]byte, 512)...)
	if files, err := readBundleFiles(padded); err != nil || len(files) != 1 {
		t.Errorf("readBundleFiles on a zero-padded archive = %v, %v; want it accepted", files, err)
	}
}

// TestPullAndExportBundlePullsBeforeExporting pins the read path's
// contract: one platform's files come from a `crane pull` of the
// concrete child digest that writes a local archive first, and only
// then from a `crane export` that unpacks exactly that archive from
// stdin — no registry read happens between the two, and the pulled
// archive is removed afterwards.
func TestPullAndExportBundlePullsBeforeExporting(t *testing.T) {
	digestRef := "quay.io/org/repo@sha256:" + strings.Repeat("a", 64)
	manifestTar := exportTarball(t, map[string]string{"manifests/a.yaml": "a: 1"})

	var calls []string
	var exportStdin []byte
	var pullPath string
	crane := func(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "pull":
			if len(args) != 3 || args[1] != digestRef {
				t.Errorf("pull args = %q, want the direct digest reference %q and the archive path", args, digestRef)
			}
			pullPath = args[2]
			return nil, os.WriteFile(pullPath, []byte("pulled archive"), 0o644)
		case "export":
			var err error
			if exportStdin, err = io.ReadAll(stdin); err != nil {
				t.Fatalf("reading the fake export's stdin: %v", err)
			}
			return manifestTar, nil
		}
		return nil, fmt.Errorf("unexpected crane call: %q", args)
	}

	files, err := pullAndExportBundle(context.Background(), crane, digestRef)
	if err != nil {
		t.Fatalf("pullAndExportBundle: %v", err)
	}
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "pull "+digestRef+" ") || calls[1] != "export - -" {
		t.Errorf("crane calls = %q, want the direct pull then `export - -`", calls)
	}
	if string(exportStdin) != "pulled archive" {
		t.Errorf("the export read %q from stdin, want exactly the archive the pull wrote", exportStdin)
	}
	if len(files) != 1 || string(files["a.yaml"]) != "a: 1" {
		t.Errorf("files = %v, want the fake export's manifest set", files)
	}
	if _, err := os.Stat(pullPath); !os.IsNotExist(err) {
		t.Errorf("the pulled archive %s survived the run: %v", pullPath, err)
	}
}

// TestPullAndExportBundleRefusesAFailedPull pins the failure
// sequencing: a pull that fails — a blob checksum mismatch inside
// crane surfaces exactly as a failed pull — must fail the run before
// the export is ever asked for, so unverified bytes cannot reach the
// comparison, the rewrite, or the writer above it.
func TestPullAndExportBundleRefusesAFailedPull(t *testing.T) {
	var calls []string
	crane := func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "pull" {
			return nil, errors.New("crane pull quay.io/org/repo@sha256…: exit status 1: error verifying sha256 checksum after reading 2048 bytes")
		}
		return nil, fmt.Errorf("unexpected crane call: %q", args)
	}

	digestRef := "quay.io/org/repo@sha256:" + strings.Repeat("a", 64)
	if _, err := pullAndExportBundle(context.Background(), crane, digestRef); err == nil {
		t.Fatal("pullAndExportBundle accepted a failed pull, want the refusal")
	}
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "pull ") {
		t.Errorf("crane calls = %q, want the failed pull and nothing after it", calls)
	}
}

// digestRefFor renders the digest reference a registry serving exactly
// body would answer for, so a fixture's pin and its bytes agree.
func digestRefFor(body []byte) string {
	sum := sha256.Sum256(body)
	return "quay.io/org/repo@sha256:" + hex.EncodeToString(sum[:])
}

// fixtureList is a manifest list carrying both required platforms as
// concrete image manifests — the entries the child selection pulls —
// plus a platform-less attestation entry like the ones real indexes
// carry.
func fixtureList(amdDigest, armDigest string) []byte {
	return []byte(`{"manifests":[
		{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + amdDigest + `","platform":{"os":"linux","architecture":"amd64"}},
		{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"` + armDigest + `","platform":{"os":"linux","architecture":"arm64"}},
		{"digest":"sha256:` + strings.Repeat("d", 64) + `"}]}`)
}

// TestExportBundleManifestsPullsTheVerifiedIndexChildren pins the
// read path's ordering and addressing: the bundle's index is fetched
// once and, only after its bytes hash to the pin, both platforms are
// pulled by the concrete child digests that verified index names —
// the direct child references, never a --platform resolution on the
// bundle reference, which would let the registry pick the child.
func TestExportBundleManifestsPullsTheVerifiedIndexChildren(t *testing.T) {
	amdChild := "sha256:" + strings.Repeat("b", 64)
	armChild := "sha256:" + strings.Repeat("c", 64)
	list := fixtureList(amdChild, armChild)
	digestRef := digestRefFor(list)
	manifestTar := exportTarball(t, map[string]string{"manifests/a.yaml": "a: 1"})

	var calls []string
	crane := func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "manifest":
			return list, nil
		case "pull":
			return nil, os.WriteFile(args[len(args)-1], []byte("pulled archive"), 0o644)
		case "export":
			return manifestTar, nil
		}
		return nil, fmt.Errorf("unexpected crane call: %q", args)
	}

	files, err := exportBundleManifests(context.Background(), crane, digestRef)
	if err != nil {
		t.Fatalf("exportBundleManifests: %v", err)
	}
	if len(files) != 1 || string(files["a.yaml"]) != "a: 1" {
		t.Errorf("files = %v, want the fake export's manifest set", files)
	}
	want := []string{
		"manifest " + digestRef,
		"pull quay.io/org/repo@" + amdChild + " ",
		"export - -",
		"pull quay.io/org/repo@" + armChild + " ",
		"export - -",
	}
	if len(calls) != len(want) {
		t.Fatalf("crane calls = %q, want %q", calls, want)
	}
	for i, call := range calls {
		if !strings.HasPrefix(call, want[i]) {
			t.Errorf("crane call %d = %q, want it to start with %q (the verified index's child, pulled directly)", i, call, want[i])
		}
	}
}

// TestExportBundleManifestsRefusesAnUnverifiedIndex pins the P1
// regression: a lying registry can hand `crane manifest` a parseable
// two-platform index whose bytes do not hash to the pinned digest —
// the pinned crane substitutes the registry's Docker-Content-Digest
// header for its own computation on signed schema-1 responses, and
// prints what it fetched without a media-type check. Those bytes must
// be refused before any pull runs: nothing the index names is ever
// fetched, so no child the registry picked can reach the comparison,
// the rewrite, or the writer.
func TestExportBundleManifestsRefusesAnUnverifiedIndex(t *testing.T) {
	list := fixtureList("sha256:"+strings.Repeat("b", 64), "sha256:"+strings.Repeat("c", 64))
	// The pin names a digest these bytes do not hash to.
	digestRef := "quay.io/org/repo@sha256:" + strings.Repeat("e", 64)

	var calls []string
	crane := func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "manifest" {
			return list, nil
		}
		return nil, fmt.Errorf("unexpected crane call: %q", args)
	}

	_, err := exportBundleManifests(context.Background(), crane, digestRef)
	if err == nil || !strings.Contains(err.Error(), "hashes to") {
		t.Errorf("exportBundleManifests on an index that does not hash to the pin = %v, want the hash refusal", err)
	}
	if len(calls) != 1 || calls[0] != "manifest "+digestRef {
		t.Errorf("crane calls = %q, want the manifest read and nothing after it", calls)
	}
}

// TestExportBundleManifestsRefusesNonImageChildren pins the child
// selection: a platform the verified index serves only as a nested
// index or as a schema-1 manifest — the shapes whose digest handling
// the tool cannot rely on — is a refusal, with nothing pulled for any
// platform: both children are selected from the verified index before
// the first blob is fetched.
func TestExportBundleManifestsRefusesNonImageChildren(t *testing.T) {
	for name, mediaType := range map[string]string{
		"a nested index":      "application/vnd.oci.image.index.v1+json",
		"a schema-1 manifest": "application/vnd.docker.distribution.manifest.v1+prettyjws",
	} {
		list := []byte(`{"manifests":[
			{"mediaType":"application/vnd.oci.image.manifest.v1+json","digest":"sha256:` + strings.Repeat("b", 64) + `","platform":{"os":"linux","architecture":"amd64"}},
			{"mediaType":"` + mediaType + `","digest":"sha256:` + strings.Repeat("c", 64) + `","platform":{"os":"linux","architecture":"arm64"}}]}`)
		var calls []string
		crane := func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
			calls = append(calls, strings.Join(args, " "))
			if args[0] == "manifest" {
				return list, nil
			}
			return nil, fmt.Errorf("unexpected crane call: %q", args)
		}
		if _, err := exportBundleManifests(context.Background(), crane, digestRefFor(list)); err == nil || !strings.Contains(err.Error(), "no concrete image manifest") {
			t.Errorf("exportBundleManifests with %s for linux/arm64 = %v, want the child-selection refusal", name, err)
		}
		if len(calls) != 1 {
			t.Errorf("crane calls after refusing %s = %q, want the manifest read only", name, calls)
		}
	}
}

// TestExportBundleManifestsRefusesASingleManifestBundle keeps the
// other half of the index check: a digest that points at a plain
// single-platform manifest is refused as not a list, with no pull —
// crane would return a plain manifest unchanged for either
// --platform value, and the platform comparison would agree
// trivially.
func TestExportBundleManifestsRefusesASingleManifestBundle(t *testing.T) {
	plainManifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json","layers":[]}`)
	digestRef := digestRefFor(plainManifest)

	var calls []string
	crane := func(_ context.Context, _ io.Reader, args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		if args[0] == "manifest" {
			return plainManifest, nil
		}
		return nil, fmt.Errorf("unexpected crane call: %q", args)
	}

	if _, err := exportBundleManifests(context.Background(), crane, digestRef); err == nil || !strings.Contains(err.Error(), "not a manifest list") {
		t.Errorf("exportBundleManifests on a single-manifest bundle = %v, want the not-a-list refusal", err)
	}
	if len(calls) != 1 || calls[0] != "manifest "+digestRef {
		t.Errorf("crane calls = %q, want the manifest read and nothing else", calls)
	}
}

// exportTarball builds a crane-export-shaped tarball: the given files
// plus the two-block end marker tar.Writer.Close writes.
func exportTarball(t *testing.T, files map[string]string) []byte {
	t.Helper()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range names {
		writeTarFile(t, tw, name, files[name])
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestWriteManifestsWritesAndCleans pins the writer's happy path: flat
// names land under the directory with their content, a re-run with
// unchanged content is a no-op, and a file the new set no longer
// carries is removed.
func TestWriteManifestsWritesAndCleans(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "manifests")
	summary, err := writeManifests(dir, map[string][]byte{"a.yaml": []byte("a: 1\n")})
	if err != nil {
		t.Fatalf("writeManifests: %v", err)
	}
	if summary.files != 1 || summary.bytes != 5 {
		t.Errorf("summary = %d files %d bytes, want 1 and 5", summary.files, summary.bytes)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "a.yaml")); err != nil || string(got) != "a: 1\n" {
		t.Errorf("a.yaml = %q %v, want the written content", got, err)
	}

	// Unchanged content: a no-op that still counts.
	if summary, err = writeManifests(dir, map[string][]byte{"a.yaml": []byte("a: 1\n")}); err != nil || summary.files != 1 {
		t.Errorf("rewriting unchanged content = %d files, %v; want the no-op", summary.files, err)
	}

	// A stale file is removed; the README-shaped non-YAML file is not.
	if err := os.WriteFile(filepath.Join(dir, "b.yaml"), []byte("b: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := writeManifests(dir, map[string][]byte{"a.yaml": []byte("a: 1\n")}); err != nil {
		t.Fatalf("writeManifests (clean): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "b.yaml")); !os.IsNotExist(err) {
		t.Errorf("the stale b.yaml survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Errorf("the non-YAML README.md did not survive: %v", err)
	}
}

// TestWriteManifestsRefusesUnsafeNames pins the write gate: every
// output must be a flat .yaml file name under the intended directory,
// and the refusal must come before anything is written — not even the
// directory may appear.
func TestWriteManifestsRefusesUnsafeNames(t *testing.T) {
	for _, name := range []string{
		"sub/dir.yaml",   // a subdirectory the embed would drop
		"../escape.yaml", // a path escape
		"sub/../escape.yaml",
		"plain.txt",      // not YAML
		"/absolute.yaml", // an absolute path
		".hidden.yaml",   // the embed pattern does not match dotfiles
	} {
		dir := filepath.Join(t.TempDir(), "out")
		if _, err := writeManifests(dir, map[string][]byte{name: []byte("a: 1\n")}); err == nil {
			t.Errorf("writeManifests accepted %q, want the flat-name refusal", name)
		} else if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
			t.Errorf("writeManifests(%q) created %s before refusing: %v", name, dir, statErr)
		}
	}
}

// TestDigestRefAndDedupe pin the small helpers the rewrite and the
// effective-reference verification depend on.
func TestDigestRefAndDedupe(t *testing.T) {
	pin := imagePin{
		floating: "quay.io/org/repo/name:master",
		digest:   "sha256:" + strings.Repeat("d", 64),
	}
	if got := pin.digestRef(); got != "quay.io/org/repo/name@"+pin.digest {
		t.Errorf("digestRef = %q, want the repository with the digest", got)
	}
	deduped := dedupe([]string{"a", "b", "a", "c", "b"})
	if strings.Join(deduped, ",") != "a,b,c" {
		t.Errorf("dedupe = %v, want first-occurrence order without duplicates", deduped)
	}
}

func writeTarFile(t *testing.T, tw *tar.Writer, name, content string) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
}

func writeTarDir(t *testing.T, tw *tar.Writer, name string) {
	t.Helper()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
}

// leakingDockerConfig is a Docker config carrying a usable credential
// for quay.io and a custom HTTP header, the material the crane
// subprocess must never see: the pinned crane's default keychain would
// resolve the credential for any registry that asks, and its root
// command would send the header with every request. The auth value is
// a well-formed username:password pair (hostile:maintainer-secret) —
// the shape the pinned Docker config loader accepts as a credential,
// which a value without the separator is not.
var leakingDockerConfig = []byte(`{"auths":{"quay.io":{"auth":"aG9zdGlsZTptYWludGFpbmVyLXNlY3JldA=="}},"HttpHeaders":{"X-Maintainer":"leak"}}`)

// hostileParentEnv builds a maintainer environment in which every
// credential-bearing variable the pinned crane's keychain reads —
// $HOME/.docker/config.json, $DOCKER_CONFIG/config.json,
// $REGISTRY_AUTH_FILE, and $XDG_RUNTIME_DIR/containers/auth.json —
// points at a real file carrying leakingDockerConfig, beside the
// variables the allowlist must pass through and one it must drop.
func hostileParentEnv(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".docker"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".docker", "config.json"), leakingDockerConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	dockerConfig := t.TempDir()
	if err := os.WriteFile(filepath.Join(dockerConfig, "config.json"), leakingDockerConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	xdgRuntime := t.TempDir()
	if err := os.MkdirAll(filepath.Join(xdgRuntime, "containers"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdgRuntime, "containers", "auth.json"), leakingDockerConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	registryAuthFile := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(registryAuthFile, leakingDockerConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{
		"HOME=" + home,
		"DOCKER_CONFIG=" + dockerConfig,
		"XDG_RUNTIME_DIR=" + xdgRuntime,
		"REGISTRY_AUTH_FILE=" + registryAuthFile,
		"DOCKER_AUTH_CONFIG=maintainer-secret",
		"PATH=/usr/bin:/bin",
		"TMPDIR=/tmp",
		"HTTPS_PROXY=http://proxy.corp.example:3128",
		"no_proxy=.corp.example",
		"UNRELATED=maintainer-secret",
	}
}

// TestCraneChildEnvBlocksInheritedCredentials pins the security
// boundary around the crane subprocess: the environment it runs in
// carries none of the credential settings the maintainer's
// environment may hold. The pinned crane's default keychain reads
// $HOME/.docker/config.json, $DOCKER_CONFIG/config.json,
// $REGISTRY_AUTH_FILE, and $XDG_RUNTIME_DIR/containers/auth.json, and
// its root command adds the Docker config's HTTPHeaders to every
// request — so a compromised registry could answer a read with an
// auth challenge naming a token realm and be answered with the
// maintainer's credential for it before the tool verified any digest.
// Every one of those variables is replaced by a private, explicitly
// empty counterpart, and nothing passes through but the allowlist.
func TestCraneChildEnvBlocksInheritedCredentials(t *testing.T) {
	ws, err := newCraneWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("newCraneWorkspace: %v", err)
	}
	env := craneChildEnv(hostileParentEnv(t), ws)
	joined := strings.Join(env, "\n")

	values := map[string]string{}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	for name, want := range map[string]string{
		"HOME":            ws.home,
		"DOCKER_CONFIG":   ws.dockerConfig,
		"XDG_RUNTIME_DIR": ws.xdgRuntime,
		"PATH":            "/usr/bin:/bin",
		"TMPDIR":          "/tmp",
		"HTTPS_PROXY":     "http://proxy.corp.example:3128",
		"no_proxy":        ".corp.example",
	} {
		if values[name] != want {
			t.Errorf("the crane child's %s = %q, want %q", name, values[name], want)
		}
	}
	for _, name := range []string{"REGISTRY_AUTH_FILE", "DOCKER_AUTH_CONFIG", "UNRELATED"} {
		if _, ok := values[name]; ok {
			t.Errorf("the crane child's env carries the inherited %s", name)
		}
	}
	// The fixture's credential password, shared by the canary values
	// above: base64 cannot carry it (the separator is outside the
	// alphabet), so any appearance is an inherited value leaking.
	if strings.Contains(joined, "maintainer-secret") {
		t.Error("the crane child's env carries material from the maintainer's credential settings")
	}

	// The replacements are real and empty where the keychain reads
	// them: the Docker config exists and is explicitly empty, and the
	// home and runtime directories carry no auth files.
	config, err := os.ReadFile(filepath.Join(ws.dockerConfig, "config.json"))
	if err != nil {
		t.Fatalf("reading the workspace's config.json: %v", err)
	}
	if string(config) != "{}\n" {
		t.Errorf("the workspace's config.json = %q, want the explicitly empty Docker config", config)
	}
	if _, err := os.Stat(filepath.Join(ws.home, ".docker")); !os.IsNotExist(err) {
		t.Errorf("the workspace's HOME carries a .docker entry: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws.xdgRuntime, "containers")); !os.IsNotExist(err) {
		t.Errorf("the workspace's XDG_RUNTIME_DIR carries a containers entry: %v", err)
	}
}

// TestNewCraneCmdRunsCraneInTheIsolatedEnvironment pins the wiring:
// the command the runner executes is the workspace's binary, by
// absolute path, carrying the isolated credential environment — not
// the process's own environment. An exec.CommandContext without an
// Env assignment would inherit the hostile variables this test sets.
func TestNewCraneCmdRunsCraneInTheIsolatedEnvironment(t *testing.T) {
	for _, entry := range hostileParentEnv(t) {
		name, value, _ := strings.Cut(entry, "=")
		t.Setenv(name, value)
	}

	ws, err := newCraneWorkspace(t.TempDir())
	if err != nil {
		t.Fatalf("newCraneWorkspace: %v", err)
	}
	ref := "quay.io/org/repo@sha256:" + strings.Repeat("a", 64)
	cmd := newCraneCmd(context.Background(), ws, "manifest", ref)

	if cmd.Path != ws.bin {
		t.Errorf("the crane command's path = %q, want the workspace binary %q", cmd.Path, ws.bin)
	}
	if len(cmd.Args) != 3 || cmd.Args[0] != ws.bin || cmd.Args[1] != "manifest" || cmd.Args[2] != ref {
		t.Errorf("the crane command's args = %q, want the binary, manifest, and the reference", cmd.Args)
	}
	values := map[string]string{}
	for _, entry := range cmd.Env {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	if values["HOME"] != ws.home || values["DOCKER_CONFIG"] != ws.dockerConfig || values["XDG_RUNTIME_DIR"] != ws.xdgRuntime {
		t.Errorf("the crane command's credential variables = HOME %q DOCKER_CONFIG %q XDG_RUNTIME_DIR %q, want the workspace's private replacements", values["HOME"], values["DOCKER_CONFIG"], values["XDG_RUNTIME_DIR"])
	}
	if _, ok := values["REGISTRY_AUTH_FILE"]; ok {
		t.Error("the crane command's env carries the inherited REGISTRY_AUTH_FILE")
	}
	if strings.Contains(strings.Join(cmd.Env, "\n"), "maintainer-secret") {
		t.Error("the crane command's env carries material from the maintainer's credential settings")
	}
}

// TestRunRemovesTheCraneWorkspaceOnAFailedBuild pins the workspace's
// lifecycle: run owns it — created at the start of the run, removed by
// a deferred cleanup registered the moment the directory exists — so a
// failed crane build, the first thing that can fail after creation,
// leaves nothing behind, exactly like a completed regeneration. The
// test owns TMPDIR so it can name where the workspace would have to
// be left, and the failing `go` stops the run at the build, before
// any registry is contacted.
func TestRunRemovesTheCraneWorkspaceOnAFailedBuild(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	// A `go` that always fails, found first on PATH. It prints a unique
	// stderr marker first: the run's build error carries the child's
	// stderr, and asserting the marker proves the fake go itself ran —
	// a failure to start the binary would share the error's prefix
	// without the marker.
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "go"), []byte("#!/bin/sh\necho fake-go-build-failure-marker >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := run(); err == nil || !strings.Contains(err.Error(), "building the pinned crane") {
		t.Fatalf("run with a failing go = %v, want the build failure", err)
	} else if !strings.Contains(err.Error(), "fake-go-build-failure-marker") {
		t.Fatalf("run with a failing go = %v, want the fake go's stderr marker in the error (it proves the fake ran)", err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatalf("reading the test's TMPDIR: %v", err)
	}
	for _, entry := range entries {
		t.Errorf("the failed build left %s behind in the test's TMPDIR", entry.Name())
	}
}

// TestRunFailsCleanlyWhenTheWorkspaceCannotBeCreated pins the first
// failure path of run: when the private directory cannot be created at
// all (here: a TMPDIR the user cannot write), the run fails immediately
// with the creation error, before any child runs and with nothing to
// clean up. Skipped under root, which ignores directory permissions.
func TestRunFailsCleanlyWhenTheWorkspaceCannotBeCreated(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root ignores directory permissions")
	}
	tmp := t.TempDir()
	blocked := filepath.Join(tmp, "blocked")
	if err := os.Mkdir(blocked, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", blocked)

	err := run()
	if err == nil || !strings.Contains(err.Error(), "creating the pinned crane's private directory") {
		t.Fatalf("run with an unwritable TMPDIR = %v, want the workspace-creation failure", err)
	}
}

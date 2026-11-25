// Command pin-compliance-catalog regenerates the Compliance Operator
// fitness proof's committed catalog manifests
// (internal/verify/compliance/catalog/manifests) from pinned image
// digests.
//
// The proof does not install from an upstream catalog. Upstream's
// catalog — the digest the PoC carried — names a bundle by the floating
// tag :latest, and the bundle's ClusterServiceVersion pins nothing: its
// Deployment image and RELATED_IMAGE_OPERATOR, RELATED_IMAGE_OPENSCAP,
// and RELATED_IMAGE_PROFILE all carry :master tags, so a registry tag
// change could change which code OLM installs without any change to
// this repo. Instead, the proof carries its own catalog: the bundle
// manifests below are the pinned bundle image's /manifests content with
// every image reference rewritten to an immutable digest, and the proof
// assembles them into a file-based catalog whose bundle entry embeds
// the manifests inline (no bundle image is pulled at proof time at
// all). What OLM installs is exactly what is committed here.
//
// The registry reads go through a pinned crane
// (github.com/google/go-containerregistry/cmd/crane, built once per
// run into a private temporary directory at the version below, so the
// version is fixed and the module's checksums are verified on
// download — nothing joins this repo's module graph). Every index the
// tool trusts is bound to its pin by the tool's own hash: the exact
// bytes `crane manifest` printed are hashed against the requested
// digest before they are parsed, because the pinned crane's fetcher
// substitutes the registry's Docker-Content-Digest header for its own
// computation when a response is a signed schema-1 manifest, and
// `crane manifest` prints those bytes without a media-type check.
// The bundle's two platform images are then pulled by the concrete
// child digests that verified index names. A direct pull applies
// crane's schema-1 rejection to the fetched child; the --platform
// resolution path does not, because it fetches the child through the
// index entry's media type and never sees the response's own. Blobs
// are read in two steps so their digest check cannot be skipped:
// `crane pull` first writes each platform's image to a local archive
// — its writer copies every compressed layer through EOF, which is
// exactly when crane checks the blob digest — and `crane export - -`
// then unpacks that archive from stdin without contacting a registry
// again. (Exporting straight from the registry would stop reading
// each layer at the tar end marker inside it, before the checksum is
// checked.) The tool requests digests only — no tag is ever
// resolved — and its own code is limited to comparing the two
// platform exports, rewriting the known floating image references,
// validating, and writing.
//
// The crane subprocess runs in a private, explicitly empty credential
// environment (craneChildEnv): a fresh HOME and XDG_RUNTIME_DIR, an
// empty Docker config in a private DOCKER_CONFIG, and nothing
// inherited but the system PATH, the temporary directory, proxy
// settings, and CA bundle locations. The maintainer's registry
// credentials and custom Docker HTTP headers cannot reach a registry's
// auth challenge, so a compromised registry that names a token realm
// cannot be answered with one. Crane sends no inherited registry
// credentials or Docker HTTP headers — it can still obtain and send
// anonymous bearer tokens, and its requests travel through any
// configured proxy, proxy credentials included. What the tool vouches
// for is its own hashes on the content that matters — the manifests
// it hashes itself and the layers whose pulls crane digest-verifies —
// not every byte crane exchanges.
//
// Pins, all immutable and all verified for both linux/amd64 and
// linux/arm64 before anything is written:
//
//   - bundleImage: the bundle image the manifests are extracted from.
//     The extraction requires the /manifests content to be byte-identical
//     across the two platforms, so the committed catalog is
//     platform-independent.
//   - imagePins: the floating reference each workload image is known by
//     in the bundle's CSV, and the digest that reference must be
//     rewritten to.
//   - the proof's catalog server image, read from the proof's own
//     compliance.DefaultCatalogServerImage so the tool's verification
//     and the proof's deployment cannot drift apart.
//
// Beyond the pin table, the tool verifies the effective image
// references the rewritten CSV carries — what the proof will actually
// pull — the same way: every reference must be a manifest list carrying
// both platforms, so the proof works unchanged on amd64 and arm64
// hosts.
//
// To bump the Compliance Operator: resolve the new bundle digest and
// the digests its CSV's floating references currently point at (for
// both platforms), edit the constants below, run
// `make pin-compliance-catalog`, commit the regenerated manifests, and
// re-run the live proof (the tagged integration test) on both default
// clusters. The tool refuses to write anything unless every check
// passes: a pin that stops resolving, loses a platform, or yields a
// bundle whose platforms disagree leaves the working tree alone.
//
// Run via `make pin-compliance-catalog` or
// `go run ./tools/pin-compliance-catalog`; the result is committed, so
// builds, tests, and the proof itself need no network. Regeneration
// does: the registries, and on first use the pinned crane's module
// download.
package main

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/stackrox/kind-ocp/internal/verify/compliance"
)

// craneModule and craneVersion pin the one external tool the
// regeneration reads the registries through. `go install` with GOBIN
// pointed at a private directory resolves exactly that version and
// verifies its checksums against the module proxy's database, so the
// invocation is as reproducible as a committed binary without adding
// anything to this repo's module graph. Bumping it is a maintenance
// decision: read the release notes, bump, and re-run
// `make pin-compliance-catalog` expecting byte-identical output. The
// pinned graph carries a known advisory (GO-2026-6865,
// klauspost/compress, fixed in v1.20.2) with no reachable path in
// this use — no layer bytes reach the affected decoder unverified —
// and it must be reassessed at the next bump of this pin or the
// bundle.
const (
	craneModule  = "github.com/google/go-containerregistry/cmd/crane"
	craneVersion = "v0.21.5"
)

// runTimeout bounds one whole regeneration: the registry reads, and on
// first use the module download and build of the pinned crane.
const runTimeout = 30 * time.Minute

// bundleImage is the digest-pinned bundle image the committed catalog
// manifests are extracted from. The tag the upstream catalog floats on
// (:latest) resolved to this digest when this pin was recorded.
const bundleImage = "ghcr.io/complianceascode/compliance-operator-bundle@sha256:7dcfa003c4a47886a24e46234fd4c4598100de769e61e6d949cbff3af230e78d"

// imagePin rewrites one floating image reference, wherever it appears
// in the bundle manifests, to an immutable digest. The floating shape
// is the reference the bundle's CSV carries; the digest is the manifest
// list that reference resolved to when this pin was recorded.
type imagePin struct {
	floating string
	digest   string
}

// imagePins is the rewrite table. Every entry's digest is verified to
// be a manifest list carrying linux/amd64 and linux/arm64 before the
// rewritten manifests are written.
var imagePins = []imagePin{
	{
		floating: "quay.io/redhat-user-workloads/ocp-isc-tenant/compliance-operator-dev:master",
		digest:   "sha256:db7d55544de2621d0c5c423763b8cba1d41538e00307b9cf66358c0258d9939c",
	},
	{
		floating: "quay.io/redhat-user-workloads/ocp-isc-tenant/compliance-operator-openscap-dev:master",
		digest:   "sha256:f031084ff31a17f33c8e58a0566665d1c1325ed8928c26d679a0985165b32036",
	},
	{
		floating: "quay.io/redhat-user-workloads/ocp-isc-tenant/compliance-operator-content-dev:master",
		digest:   "sha256:d798f32ebb3e726e2f0231364e583d5d10485ac7d7396d5bdb34206c89a1f6e2",
	},
	{
		floating: "quay.io/redhat-user-workloads/ocp-isc-tenant/compliance-operator-must-gather-dev:master",
		digest:   "sha256:32ee7fd9c5aeaba83d4bd81f51806bb6a9779cc30f1c0bfd384898d495f9db90",
	},
}

// outputDir is the committed directory the manifests are written to,
// relative to the repository root (the tool is run from there).
const outputDir = "internal/verify/compliance/catalog/manifests"

// platforms is the platform set every pin, the server image, and the
// bundle extraction is verified against: kind-ocp runs on amd64 and
// arm64 developer machines and CI runners, and a manifest list missing
// either platform would make the proof work on one and fail on the
// other.
var platforms = [][2]string{{"linux", "amd64"}, {"linux", "arm64"}}

// digestPattern matches an exact sha256 digest.
var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// digestRefPattern matches a whole digest-pinned image reference —
// registry and repository plus an exact sha256 digest — the only shape
// the tool ever asks a registry for.
var digestRefPattern = regexp.MustCompile(`^[^@\s]+@sha256:[0-9a-f]{64}$`)

// manifestNamePattern matches the file names the committed catalog can
// carry: one flat .yaml file name. The assembly embeds
// catalog/manifests/*.yaml exactly, so a subdirectory, a path escape,
// or a non-YAML file would be silently dropped by the proof — the
// writer refuses those names before touching anything.
var manifestNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*\.yaml$`)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "pin-compliance-catalog: %v\n", err)
		os.Exit(1)
	}
}

// run is one whole regeneration: it owns the pinned crane's private
// workspace for the run — created first, removed by a deferred cleanup
// registered the moment the directory exists, so every path out of the
// run (a failed build, a refused pin, a completed regeneration)
// removes it. A hard kill of the process leaves it behind like any
// other temporary file the operating system reclaims.
func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	root, err := os.MkdirTemp("", "pin-compliance-catalog-crane-")
	if err != nil {
		return fmt.Errorf("creating the pinned crane's private directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	ws, err := newCraneWorkspace(root)
	if err != nil {
		return err
	}
	// Built once per run; every invocation below goes through the one
	// bound runner.
	if err := buildCrane(ctx, ws); err != nil {
		return err
	}
	crane := craneRunnerFor(ws)

	// Every workload pin first: a pin that does not resolve, or that
	// stops carrying both platforms, fails the run before anything is
	// fetched for the bundle.
	for _, pin := range imagePins {
		if err := verifyManifestList(ctx, crane, pin.digestRef()); err != nil {
			return err
		}
		fmt.Printf("verified %s -> %s (linux/amd64 and linux/arm64 present)\n", pin.floating, pin.digest)
	}

	// The proof's catalog server image, verified through the proof's
	// own pin: the tool and the proof read the same constant, so they
	// cannot drift apart.
	if err := verifyManifestList(ctx, crane, compliance.DefaultCatalogServerImage); err != nil {
		return err
	}
	fmt.Printf("verified the proof's catalog server %s (linux/amd64 and linux/arm64 present)\n", compliance.DefaultCatalogServerImage)

	// The bundle: proven to be a manifest list on both platforms, then
	// pulled and unpacked per platform and compared.
	manifests, err := exportBundleManifests(ctx, crane, bundleImage)
	if err != nil {
		return err
	}
	fmt.Printf("verified %s carries %d identical manifest files on linux/amd64 and linux/arm64\n", bundleImage, len(manifests))

	rewritten, err := rewriteManifests(manifests, imagePins)
	if err != nil {
		return err
	}
	csv, err := validateCSV(rewritten)
	if err != nil {
		return err
	}
	fmt.Printf("validated the CSV %s (version %s): every image reference carries a digest\n", csv.Name, csv.Version)

	// The effective workload references — what the rewritten CSV
	// actually carries, not what the pin table says — verified the
	// same way as the pins: a reference the CSV already pins by digest
	// outside the rewrite table must fail here, not on a
	// single-platform host at proof time.
	for _, ref := range dedupe(csv.Images) {
		if err := verifyManifestList(ctx, crane, ref); err != nil {
			return err
		}
	}
	fmt.Printf("verified all %d image references the CSV carries are manifest lists on linux/amd64 and linux/arm64\n", len(dedupe(csv.Images)))

	written, err := writeManifests(outputDir, rewritten)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %d manifest files (%d bytes) to %s\n", written.files, written.bytes, outputDir)
	fmt.Println("commit the result together with the pin bump; re-run the live proof on both default clusters")
	return nil
}

// craneRunner runs the pinned crane with the given arguments and
// returns its stdout. stdin, when not nil, is piped to the command —
// `crane export - -` reads the docker archive to unpack from there.
// stderr is kept for the error path only. The read-path helpers take
// it as a parameter so the tests can supply a fake and pin their
// sequencing without a network; run supplies the one bound to its
// workspace.
type craneRunner func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error)

// craneRunnerFor binds one workspace into a crane runner: every
// invocation is the workspace's built binary, by absolute path,
// executed directly — not through `go run` — so an expired context
// kills crane itself rather than a wrapper whose child keeps the
// output pipes open, and every invocation carries the isolated
// credential environment newCraneCmd builds.
func craneRunnerFor(ws *craneWorkspace) craneRunner {
	return func(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
		cmd := newCraneCmd(ctx, ws, args...)
		cmd.Stdin = stdin
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("crane %s: %w: %s", strings.Join(args, " "), err, truncate(stderr.Bytes(), 500))
		}
		return stdout.Bytes(), nil
	}
}

// newCraneCmd builds one crane invocation from the workspace: the
// built binary, by absolute path, carrying the isolated credential
// environment. It is its own function so the wiring is pinnable
// without building crane: the command the runner executes must never
// inherit the process's environment (see craneChildEnv).
func newCraneCmd(ctx context.Context, ws *craneWorkspace, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, ws.bin, args...)
	cmd.Env = craneChildEnv(os.Environ(), ws)
	return cmd
}

// craneWorkspace is the pinned crane's private per-run world: the
// built binary and the isolated credential environment every crane
// invocation runs in. run creates it, and its deferred cleanup removes
// the tree it was laid out in when the run ends, on success and failure
// alike.
type craneWorkspace struct {
	// bin is the built crane binary, <root>/bin/crane.
	bin string

	// dockerConfig is a private DOCKER_CONFIG directory whose
	// config.json is an explicitly empty Docker config: no
	// credentials, no credential store, no HTTP headers.
	dockerConfig string

	// home and xdgRuntime are private, empty replacements for HOME
	// and XDG_RUNTIME_DIR.
	home       string
	xdgRuntime string
}

// newCraneWorkspace lays out the private directory tree the pinned
// crane is built into and run from: the binary's directory, the
// explicitly empty Docker config, and the empty HOME and
// XDG_RUNTIME_DIR replacements.
func newCraneWorkspace(root string) (*craneWorkspace, error) {
	ws := &craneWorkspace{
		bin:          filepath.Join(root, "bin", "crane"),
		dockerConfig: filepath.Join(root, "docker-config"),
		home:         filepath.Join(root, "home"),
		xdgRuntime:   filepath.Join(root, "xdg-runtime"),
	}
	for _, dir := range []string{filepath.Join(root, "bin"), ws.dockerConfig, ws.home, ws.xdgRuntime} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("creating the crane workspace's %s: %w", dir, err)
		}
	}
	// The Docker config must exist and be valid, not merely be pointed
	// at: the pinned keychain treats an absent config as "look
	// somewhere else" (the home config, then the Podman files), so the
	// empty config has to be found for the isolation to hold.
	if err := os.WriteFile(filepath.Join(ws.dockerConfig, "config.json"), []byte("{}\n"), 0o600); err != nil {
		return nil, fmt.Errorf("writing the empty Docker config: %w", err)
	}
	return ws, nil
}

// buildCrane builds the pinned crane into the workspace's private bin
// directory. The build keeps the maintainer's environment — it talks
// to the module proxy, never to the image registries.
func buildCrane(ctx context.Context, ws *craneWorkspace) error {
	cmd := exec.CommandContext(ctx, "go", "install", craneModule+"@"+craneVersion)
	cmd.Env = append(os.Environ(), "GOBIN="+filepath.Dir(ws.bin))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building the pinned crane %s: %w: %s", craneVersion, err, truncate(stderr.Bytes(), 500))
	}
	return nil
}

// craneEnvPassthrough is the allowlist of environment variables the
// crane subprocess inherits from the maintainer's environment: what
// it needs to reach the registries — the system PATH, the temporary
// directory, the proxy configuration, and custom CA bundle locations
// — and nothing else.
var craneEnvPassthrough = []string{
	"PATH",
	"TMPDIR",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"http_proxy", "https_proxy", "no_proxy", "all_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
}

// craneChildEnv builds the environment the crane subprocess runs in
// from the maintainer's environment parent: the passthrough allowlist
// above, with every variable the pinned crane's credential lookup
// reads replaced by a private, explicitly empty counterpart. The
// pinned crane's default keychain (pkg/authn/keychain.go) reads, in
// order, $HOME/.docker/config.json, $DOCKER_CONFIG/config.json,
// $REGISTRY_AUTH_FILE, and $XDG_RUNTIME_DIR/containers/auth.json, and
// its root command adds the Docker config's HTTPHeaders to every
// request. The replacements leave every one of those sources empty or
// absent:
//
//   - HOME and XDG_RUNTIME_DIR point at empty private directories, so
//     the home config and the Podman auth file resolve to paths that
//     do not exist. HOME is replaced rather than dropped because the
//     keychain's home lookup falls back to querying the operating
//     system for the user's home directory when the variable is
//     empty; XDG_RUNTIME_DIR because an unset variable would turn the
//     Podman auth path into one relative to the working directory.
//   - DOCKER_CONFIG points at a directory whose config.json is an
//     explicitly empty Docker config, so the keychain resolves
//     anonymous for every registry and the root command adds no
//     headers.
//   - REGISTRY_AUTH_FILE is simply not passed through.
//
// A registry that answers a read with an auth challenge naming a token
// realm therefore cannot be answered with the maintainer's
// credentials: crane sends no inherited registry credentials or Docker
// HTTP headers. It can still obtain and send anonymous bearer tokens,
// and its requests can use a passed-through proxy's credentials; what
// the tool trusts is its own hashes on the verified manifests and
// layers, not the anonymous fetch itself.
func craneChildEnv(parent []string, ws *craneWorkspace) []string {
	var env []string
	for _, entry := range parent {
		name, _, _ := strings.Cut(entry, "=")
		if slices.Contains(craneEnvPassthrough, name) {
			env = append(env, entry)
		}
	}
	return append(env,
		"HOME="+ws.home,
		"DOCKER_CONFIG="+ws.dockerConfig,
		"XDG_RUNTIME_DIR="+ws.xdgRuntime,
	)
}

// verifyManifestList asks the pinned crane for one reference's
// manifest, hashes the exact bytes it printed against the requested
// digest, and refuses anything that is not a manifest list carrying
// every platform the proof runs on. The reference must be
// digest-pinned: the tool never resolves a tag. This check reads the
// index alone — the entries it advertises, not the children: only the
// bundle's child manifests and layers are ever pulled during
// regeneration.
func verifyManifestList(ctx context.Context, crane craneRunner, ref string) error {
	if !digestRefPattern.MatchString(ref) {
		return fmt.Errorf("%q is not an exact digest-pinned image reference — the tool never resolves a tag", ref)
	}
	_, err := fetchVerifiedIndex(ctx, crane, ref)
	return err
}

// fetchVerifiedIndex reads one reference's index and returns it only
// when the exact bytes `crane manifest` printed hash to the digest the
// reference carries — the tool's own hash, not crane's. The pinned
// crane's fetcher substitutes the registry's Docker-Content-Digest
// header for its own computation when a response is a signed schema-1
// manifest (pkg/v1/remote/fetcher.go), and `crane manifest` prints
// those bytes without a media-type check, so a lying registry can
// hand crane bytes that do not hash to the requested digest at all.
// Nothing downstream — the platform coverage check, the child
// selection, the pulls — runs before this hash has bound the bytes to
// the pin.
func fetchVerifiedIndex(ctx context.Context, crane craneRunner, ref string) (imageIndex, error) {
	manifest, err := crane(ctx, nil, "manifest", ref)
	if err != nil {
		return imageIndex{}, err
	}
	if err := verifyFetchedDigest(manifest, ref); err != nil {
		return imageIndex{}, err
	}
	return parseManifestList(manifest, ref)
}

// verifyFetchedDigest hashes the exact bytes a `crane manifest` call
// returned against the digest the reference asked for.
func verifyFetchedDigest(manifest []byte, ref string) error {
	_, want, _ := strings.Cut(ref, "@")
	sum := sha256.Sum256(manifest)
	got := "sha256:" + hex.EncodeToString(sum[:])
	if got != want {
		return fmt.Errorf("the manifest fetched for %s hashes to %s, not the pinned %s — the registry served bytes the pin does not vouch for", ref, got, want)
	}
	return nil
}

// parseManifestList decodes one manifest and refuses it unless it is
// a manifest list carrying every platform the proof runs on.
func parseManifestList(manifest []byte, ref string) (imageIndex, error) {
	var index imageIndex
	if err := json.Unmarshal(manifest, &index); err != nil {
		return imageIndex{}, fmt.Errorf("decoding the manifest of %s: %w", ref, err)
	}
	if len(index.Manifests) == 0 {
		return imageIndex{}, fmt.Errorf("%s is not a manifest list", ref)
	}
	if missing := missingPlatforms(index.Manifests); len(missing) > 0 {
		return imageIndex{}, fmt.Errorf("%s carries no entry for %s", ref, strings.Join(missing, " or "))
	}
	return index, nil
}

// imageIndex is the manifest-list shape the verification and the
// child selection read: the child entries and their digests, media
// types, and platforms.
type imageIndex struct {
	Manifests []indexEntry `json:"manifests"`
}

// indexEntry is one child of a manifest list. An attestation entry
// carries the unknown/unknown platform or none; entries without an
// exact platform match are simply not matches.
type indexEntry struct {
	MediaType string         `json:"mediaType"`
	Digest    string         `json:"digest"`
	Platform  *indexPlatform `json:"platform"`
}

// imageManifestMediaTypes are the concrete image-manifest media types
// the tool is willing to pull: an OCI image manifest or a Docker v2
// image manifest. A nested index is not a concrete image, and the
// schema-1 shapes are exactly the compatibility path whose digest
// handling this tool does not rely on — neither is ever selected.
var imageManifestMediaTypes = map[string]bool{
	"application/vnd.oci.image.manifest.v1+json":           true,
	"application/vnd.docker.distribution.manifest.v2+json": true,
}

// concreteImageChild returns the child the verified index names for
// one platform: the first entry, in index order, whose platform
// matches the required one exactly and whose media type is a concrete
// image manifest with a well-formed digest, rendered as a
// digest-pinned reference in the index's own repository. An entry
// that matches the platform but carries another media type — a nested
// index, a schema-1 manifest — is not a candidate; a platform with no
// concrete image child is a refusal naming the platform.
func concreteImageChild(index imageIndex, ref, os, arch string) (string, error) {
	repo, _, _ := strings.Cut(ref, "@")
	for _, m := range index.Manifests {
		if m.Platform == nil || m.Platform.OS != os || m.Platform.Architecture != arch {
			continue
		}
		if !imageManifestMediaTypes[m.MediaType] || !digestPattern.MatchString(m.Digest) {
			continue
		}
		return repo + "@" + m.Digest, nil
	}
	return "", fmt.Errorf("the verified index of %s names no concrete image manifest (media type application/vnd.oci.image.manifest.v1+json or application/vnd.docker.distribution.manifest.v2+json) for %s/%s", ref, os, arch)
}

type indexPlatform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

// missingPlatforms returns the platforms the proof requires that a
// manifest list's entries do not cover. For the pins, the server
// image, and the CSV's references this is the whole check: what is
// verified is the pinned index's advertised entries, and only the
// bundle's child manifests and layers are pulled.
func missingPlatforms(manifests []indexEntry) []string {
	var missing []string
	for _, platform := range platforms {
		os, arch := platform[0], platform[1]
		carried := false
		for _, m := range manifests {
			if m.Platform != nil && m.Platform.OS == os && m.Platform.Architecture == arch {
				carried = true
				break
			}
		}
		if !carried {
			missing = append(missing, os+"/"+arch)
		}
	}
	return missing
}

// exportBundleManifests returns the bundle image's /manifests files,
// read separately for both required platforms and returned only when
// the two carry byte-identical content: the committed catalog is
// platform-independent by construction, not by assumption.
//
// The bundle's index is fetched once and bound to the pin by the
// tool's own hash (fetchVerifiedIndex), and each platform's image is
// then pulled by the concrete child digest that verified index names
// for it — never through crane's --platform resolution. The direct
// pull applies crane's schema-1 rejection to the fetched child and
// must hash the child's bytes to the digest the verified index
// carries; the --platform path fetches the child through the index
// entry's media type instead of the response's own, so a signed
// schema-1 child response naming the right header digest would pass
// that fetch unhashed. A digest that pointed at a plain
// single-platform manifest is still refused before any pull: the
// child selection reads the index, and a manifest with no list of
// children is not one — crane would return a plain manifest unchanged
// for either --platform value, and the comparison below would agree
// trivially.
func exportBundleManifests(ctx context.Context, crane craneRunner, image string) (map[string][]byte, error) {
	if !digestRefPattern.MatchString(image) {
		return nil, fmt.Errorf("%q is not an exact digest-pinned image reference — the tool never resolves a tag", image)
	}
	index, err := fetchVerifiedIndex(ctx, crane, image)
	if err != nil {
		return nil, err
	}
	// Every child is selected from the verified index before any blob
	// is fetched: what will be pulled is decided by the pinned bytes
	// alone, never by a response that arrives mid-read.
	children := make([]string, len(platforms))
	for i, platform := range platforms {
		child, err := concreteImageChild(index, image, platform[0], platform[1])
		if err != nil {
			return nil, err
		}
		children[i] = child
	}
	perPlatform := map[string]map[string][]byte{}
	for i, platform := range platforms {
		name := platform[0] + "/" + platform[1]
		files, err := pullAndExportBundle(ctx, crane, children[i])
		if err != nil {
			return nil, fmt.Errorf("the bundle's %s image: %w", name, err)
		}
		perPlatform[name] = files
	}
	first := platforms[0]
	reference := perPlatform[first[0]+"/"+first[1]]
	for _, platform := range platforms[1:] {
		other := perPlatform[platform[0]+"/"+platform[1]]
		if err := compareManifestSets(reference, other, first, platform); err != nil {
			return nil, err
		}
	}
	if len(reference) == 0 {
		return nil, fmt.Errorf("%s carries no files under /manifests", image)
	}
	return reference, nil
}

// pullAndExportBundle reads one platform image's /manifests files in
// two steps, so every blob byte is digest-verified before anything is
// parsed:
//
//   - `crane pull` writes the image to a local archive first. The
//     archive writer copies each compressed layer through EOF, and
//     crane checks the blob digest exactly then, so a pull that
//     succeeds has verified every byte it wrote against the
//     manifest's digests. The reference is the concrete child digest
//     the verified index named — pulled directly, not through
//     --platform resolution — so the fetch is bound to that digest
//     from the response's own media type: any response that is not a
//     signed schema-1 manifest must hash to the requested digest, and
//     a signed schema-1 one is rejected by crane's Image() outright.
//   - `crane export - -` then unpacks that local archive from stdin
//     and writes the filesystem tarball to stdout. It contacts no
//     registry, so what it unpacks is exactly what the pull verified.
//
// Reading the export straight from the registry instead
// (`crane export --platform`) would stop at the tar end marker inside
// each layer — before the checksum check — so the exported bytes
// would be trusted without verification. A failed pull, a checksum
// mismatch included, fails here before the export runs.
func pullAndExportBundle(ctx context.Context, crane craneRunner, image string) (map[string][]byte, error) {
	dir, err := os.MkdirTemp("", "pin-compliance-catalog-")
	if err != nil {
		return nil, fmt.Errorf("creating the pull directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	archive := filepath.Join(dir, "bundle.tar")
	if _, err := crane(ctx, nil, "pull", image, archive); err != nil {
		return nil, err
	}
	pulled, err := os.Open(archive)
	if err != nil {
		return nil, fmt.Errorf("opening the pulled archive: %w", err)
	}
	defer func() { _ = pulled.Close() }()
	exported, err := crane(ctx, pulled, "export", "-", "-")
	if err != nil {
		return nil, err
	}
	files, err := readBundleFiles(exported)
	if err != nil {
		return nil, fmt.Errorf("reading the pulled image's export: %w", err)
	}
	return files, nil
}

// readBundleFiles reads one crane export tarball and returns every
// regular file under manifests/, keyed by its path with the prefix
// stripped. The read is strict: a corrupt archive, an unreadable
// entry, two entries claiming the same name, or a missing tar end
// marker fail instead of yielding a partial file set. The end-marker
// check covers the truncations Go's own tar reader accepts — an
// archive cut at a header boundary, or after a single zero block. It
// does not try to tell a real marker from zero-filled entry data: the
// bytes are indistinguishable, and the digest-verified pull is what
// bounds that ambiguity.
func readBundleFiles(tarball []byte) (map[string][]byte, error) {
	files := map[string][]byte{}
	source := bytes.NewReader(tarball)
	reader := tar.NewReader(source)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			// The reader stops inside the end marker: after two zero
			// blocks in a complete archive, after one, or at a header
			// boundary when the archive is cut short. Require the full
			// two-block marker: the last 1024 bytes the reader
			// consumed — and anything after them — must be zero.
			consumed := len(tarball) - source.Len()
			if consumed < 1024 || !allZeros(tarball[consumed-1024:]) {
				return nil, errors.New("the export's two-block tar end marker is missing — the archive is truncated or corrupt")
			}
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading the export: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return nil, fmt.Errorf("reading %s from the export: %w", header.Name, err)
		}
		if rest, found := strings.CutPrefix(header.Name, "manifests/"); found && rest != "" {
			if _, duplicate := files[rest]; duplicate {
				return nil, fmt.Errorf("the export carries %s twice", header.Name)
			}
			files[rest] = data
		}
	}
}

// allZeros reports whether b is entirely zero bytes.
func allZeros(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// compareManifestSets fails when two platforms' file sets or file
// contents differ, naming the first difference.
func compareManifestSets(a, b map[string][]byte, aPlatform, bPlatform [2]string) error {
	aName := aPlatform[0] + "/" + aPlatform[1]
	bName := bPlatform[0] + "/" + bPlatform[1]
	for name := range a {
		if _, ok := b[name]; !ok {
			return fmt.Errorf("the bundle's /manifests differs across platforms: %s has %q, %s does not", aName, name, bName)
		}
	}
	for name := range b {
		if _, ok := a[name]; !ok {
			return fmt.Errorf("the bundle's /manifests differs across platforms: %s has %q, %s does not", bName, name, aName)
		}
	}
	for name, content := range a {
		if !bytes.Equal(content, b[name]) {
			return fmt.Errorf("the bundle's /manifests differs across platforms: %q is not byte-identical between %s and %s", name, aName, bName)
		}
	}
	return nil
}

// csvSummary is the shape of the bundle's ClusterServiceVersion the
// tool validates: its identity and its image references.
type csvSummary struct {
	Name    string
	Version string
	Images  []string // every image reference the CSV carries, digest-checked
}

// validateCSV finds the bundle's single ClusterServiceVersion and
// checks that every image reference it carries is an exact digest
// reference — the rewrite must have left nothing floating.
func validateCSV(manifests map[string][]byte) (*csvSummary, error) {
	var csvName string
	for name, content := range manifests {
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal(content, &doc); err != nil {
			return nil, fmt.Errorf("parsing %s as YAML: %w", name, err)
		}
		if doc.Kind == "ClusterServiceVersion" {
			if csvName != "" {
				return nil, fmt.Errorf("the bundle carries two ClusterServiceVersions: %s and %s", csvName, name)
			}
			csvName = name
		}
	}
	if csvName == "" {
		return nil, errors.New("the bundle carries no ClusterServiceVersion")
	}
	var csv struct {
		Metadata struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec struct {
			Version       string `yaml:"version"`
			RelatedImages []struct {
				Name  string `yaml:"name"`
				Image string `yaml:"image"`
			} `yaml:"relatedImages"`
			Install struct {
				Strategy string `yaml:"strategy"`
				Spec     struct {
					Deployments []struct {
						Name string `yaml:"name"`
						Spec struct {
							Template struct {
								Spec struct {
									Containers []struct {
										Name  string `yaml:"name"`
										Image string `yaml:"image"`
										Env   []struct {
											Name  string `yaml:"name"`
											Value string `yaml:"value"`
										} `yaml:"env"`
									} `yaml:"containers"`
								} `yaml:"spec"`
							} `yaml:"template"`
						} `yaml:"spec"`
					} `yaml:"deployments"`
				} `yaml:"spec"`
			} `yaml:"install"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(manifests[csvName], &csv); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", csvName, err)
	}
	if csv.Metadata.Name == "" || csv.Spec.Version == "" {
		return nil, fmt.Errorf("%s carries no metadata.name or no spec.version", csvName)
	}
	summary := &csvSummary{Name: csv.Metadata.Name, Version: csv.Spec.Version}
	for _, ri := range csv.Spec.RelatedImages {
		summary.Images = append(summary.Images, ri.Image)
	}
	for _, d := range csv.Spec.Install.Spec.Deployments {
		for _, c := range d.Spec.Template.Spec.Containers {
			summary.Images = append(summary.Images, c.Image)
			for _, env := range c.Env {
				if strings.HasPrefix(env.Name, "RELATED_IMAGE_") {
					summary.Images = append(summary.Images, env.Value)
				}
			}
		}
	}
	if len(summary.Images) == 0 {
		return nil, fmt.Errorf("%s carries no image references at all — the shape this tool validates has changed", csvName)
	}
	for _, image := range summary.Images {
		if _, digest, found := strings.Cut(image, "@"); !found || !digestPattern.MatchString(digest) {
			return nil, fmt.Errorf("%s still carries the non-digest image reference %q (was a pin missed from the rewrite table?)", csvName, image)
		}
	}
	return summary, nil
}

// rewriteManifests applies every pin to every manifest file, and
// refuses the result when any file still mentions a floating :master or
// :latest reference — an unknown floating reference must fail the bump,
// not ship.
func rewriteManifests(manifests map[string][]byte, pins []imagePin) (map[string][]byte, error) {
	rewritten := make(map[string][]byte, len(manifests))
	for name, content := range manifests {
		out := content
		for _, pin := range pins {
			out = bytes.ReplaceAll(out, []byte(pin.floating), []byte(pin.digestRef()))
		}
		rewritten[name] = out
	}
	names := make([]string, 0, len(rewritten))
	for name := range rewritten {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, line := range strings.Split(string(rewritten[name]), "\n") {
			trimmed := strings.TrimSpace(line)
			for _, field := range []string{"image:", "value:", "must-gather-image:"} {
				if value, found := strings.CutPrefix(trimmed, field); found {
					value = strings.TrimSpace(value)
					if strings.HasSuffix(value, ":master") || strings.HasSuffix(value, ":latest") {
						return nil, fmt.Errorf("%s still carries the floating reference %q after the rewrite (was a pin missed from the rewrite table?)", name, value)
					}
				}
			}
		}
	}
	return rewritten, nil
}

// digestRef renders one pin as the immutable reference it rewrites to.
func (p imagePin) digestRef() string {
	rest, _, _ := strings.Cut(p.floating, "@")
	rest, _, _ = strings.Cut(rest, ":")
	return rest + "@" + p.digest
}

// dedupe returns refs without duplicates, first occurrence first.
func dedupe(refs []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		out = append(out, ref)
	}
	return out
}

// writeManifests writes the manifest set to dir, and removes stale
// .yaml files the new set no longer carries, so the directory mirrors
// the bundle exactly. Every output name must be a flat .yaml file
// name — what the committed catalog's embed and assembly read — and
// every name is validated before anything is written, created, or
// removed, so a bad input leaves the working tree alone. Non-YAML
// files (the README) are left alone. A file whose content would not
// change is not rewritten.
type writeSummary struct {
	files int
	bytes int
}

func writeManifests(dir string, manifests map[string][]byte) (writeSummary, error) {
	names := make([]string, 0, len(manifests))
	for name := range manifests {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !manifestNamePattern.MatchString(name) {
			return writeSummary{}, fmt.Errorf("%q is not a flat .yaml file name: the committed catalog embeds catalog/manifests/*.yaml exactly, and a subdirectory, a path escape, or a non-YAML file would be silently dropped by the assembly", name)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return writeSummary{}, fmt.Errorf("creating %s: %w", dir, err)
	}
	var summary writeSummary
	for _, name := range names {
		path := filepath.Join(dir, name)
		// A file whose content would not change is not written, so
		// regenerating in-sync pins is a no-op for the working tree.
		if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, manifests[name]) {
			summary.files++
			summary.bytes += len(manifests[name])
			continue
		}
		if err := os.WriteFile(path, manifests[name], 0o644); err != nil {
			return summary, fmt.Errorf("writing %s: %w", path, err)
		}
		summary.files++
		summary.bytes += len(manifests[name])
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return summary, fmt.Errorf("reading %s: %w", dir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		if _, ok := manifests[entry.Name()]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
			return summary, fmt.Errorf("removing the stale %s: %w", entry.Name(), err)
		}
		fmt.Printf("removed the stale %s (the pinned bundle no longer carries it)\n", entry.Name())
	}
	return summary, nil
}

func truncate(body []byte, limit int) string {
	if len(body) <= limit {
		return string(body)
	}
	return string(body[:limit]) + "…"
}

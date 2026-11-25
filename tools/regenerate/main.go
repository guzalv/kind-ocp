// Command regenerate rewrites the platform-objects layer's upstream-derived
// asset content (assets/platform-objects) from pinned upstream commits:
//
//   - sccs.yaml is entirely upstream — the two real
//     SecurityContextConstraints manifests OpenShift itself installs,
//     verbatim, behind a hand-authored header;
//   - objects.yaml is otherwise first-party: only its two upstream fields
//     are spliced in place, between "# BEGIN/END UPSTREAM" marker comments
//     that bound them, leaving every other byte of the file untouched:
//     the openshift-kube-apiserver/config ConfigMap's apiServerArguments
//     and the openshift-kube-controller-manager/config ConfigMap's
//     extendedArguments.
//
// Pins are full 40-hex commit SHAs (never a branch: both upstream repos'
// default branch is "main", and "master" only resolves through GitHub's
// post-rename redirect). To bump them, edit the constants below and re-run
// — or resolve each repo's latest commit and do both. Run via
// `make regenerate` or `go run ./tools/regenerate`; the result is committed,
// so builds and tests need no network.
//
// Every fetch goes through internal/kube's manifest fetcher: https-only,
// redirects included, so a redirect can never downgrade the transport that
// carries bytes a maintainer is about to commit. Both assets are built and
// validated in full before anything is written — the SCC stream must be
// exactly the two SecurityContextConstraints it claims to be, carrying
// every top-level field the pinned CRD marks required, the nullable ones
// present even when null; the upstream argument
// fields must be the map-of-argument-lists shape the ConfigMaps embed; and
// the spliced objects.yaml must decode to the same object set it had
// before the splice, with every object's decoded content unchanged but
// the two managed argument maps. That decoded before/after comparison is
// what catches a YAML alias: a sibling key aliasing the anchored
// config.yaml decodes from the spliced block, so its data would change
// with the splice even though its own bytes never moved. Inside the
// managed blobs only the managed argument map is interpreted; every
// other JSON key is hand-maintained, compared as raw bytes, never
// decoded into Go values — a number no float64 can represent is valid
// JSON and none of this tool's business. A fetch or
// validation failure leaves the working tree exactly as it was; only a
// failure inside the write loop itself, after validation, could leave the
// files it already wrote. A file whose content would not change
// is not written, so regenerating in-sync pins is a no-op for the working
// tree.
//
// The objects.yaml splice is marker-bounded, byte-exact, and bound to
// its destination. A BEGIN
// UPSTREAM marker must be closed by the very next marker line, its own END
// UPSTREAM, so a pair can never borrow another field's END, nest, or
// overlap; a field with two pairs, a pair no splice manages, or a missing
// pair is refused. The destination identity must be unique in the file
// whether or not every copy carries a config.yaml — the layer applies
// documents in order under one field manager — and the managed key must
// occur exactly once in the blob, because JSON decoding keeps the last
// of two same-named keys. The supported layout is exact, and imposed on
// the two splice destinations alone: the destination's
// config.yaml is a literal block scalar carrying one physical JSON line,
// with the pair's BEGIN on the line above the block's header and its END
// on the line below the payload, both at the key's own indentation — so
// neither marker can be content of a sibling block scalar. Markers
// pasted around another ConfigMap's content, a destination config.yaml
// in any other shape, or a duplicate key on the destination lookup path
// are refused, never spliced; a config.yaml in another shape on a
// ConfigMap no splice manages is left alone. Only the BEGIN marker line and the payload line it
// bounds are rewritten, as byte ranges, in the order the markers appear
// in the file; every other byte of the file, including
// the presence or absence of a final newline, survives verbatim. A blob
// whose decoded field already equals the pinned upstream value keeps
// its original bytes; a changed one is rendered by the standard encoder.
//
// Ported from the PoC's regenerate.sh (stackrox PR #23841), python and
// PyYAML replaced by Go (BRIEF §7.1: the repo stays single-language).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stackrox/kind-ocp/internal/kube"
	"github.com/stackrox/kind-ocp/internal/layers/platformobjects"
)

// The pinned upstream commits the assets are regenerated from.
const (
	// kasCommit pins cluster-kube-apiserver-operator: the source of both
	// SCC manifests and of the apiServerArguments default.
	kasCommit = "5105250647f33a232b1bf2982372f7cc66580deb" // cluster-kube-apiserver-operator@main HEAD as of 2026-11-06
	// kcmCommit pins cluster-kube-controller-manager-operator: the source
	// of the extendedArguments default.
	kcmCommit = "cd45079d8bb7a391ba0eb3282d9ef75232fe425c" // cluster-kube-controller-manager-operator@main HEAD as of 2026-11-06
)

// The upstream repositories and files the pins address.
const (
	kasRepo = "cluster-kube-apiserver-operator"
	kcmRepo = "cluster-kube-controller-manager-operator"

	defaultConfigPath = "bindata/assets/config/defaultconfig.yaml"
	sccPathTemplate   = "bindata/bootkube/scc-manifests/0000_20_kube-apiserver-operator_00_scc-%s.yaml"

	objectsFile = "objects.yaml"
	sccsFile    = "sccs.yaml"
)

// fetchTimeout bounds one upstream fetch, redirects and body read included.
const fetchTimeout = 2 * time.Minute

func main() {
	root, err := moduleRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "regenerate: %v\n", err)
		os.Exit(1)
	}
	if err := run(context.Background(), http.DefaultTransport, root); err != nil {
		fmt.Fprintf(os.Stderr, "regenerate: %v\n", err)
		os.Exit(1)
	}
}

// write is one file regeneration has validated and may write.
type write struct {
	path    string
	content []byte
}

// run regenerates both assets under root's assets/platform-objects
// directory, recording the fetched upstream bytes under root as the
// offline fixtures the no-op test replays. The transport is a parameter
// so tests can replay canned responses; main passes the default one and
// the module root. Nothing is written until both assets have been built
// and validated in full, so a late failure cannot leave a half-regenerated
// tree behind.
func run(ctx context.Context, transport http.RoundTripper, root string) error {
	assetsDir := filepath.Join(root, "assets", "platform-objects")
	fixturesDir := filepath.Join(root, "tools", "regenerate", "testdata")

	fetched, err := fetchUpstream(ctx, transport)
	if err != nil {
		return err
	}
	sccsContent, err := assembleSCCs(fetched)
	if err != nil {
		return err
	}
	objectsContent, err := updateObjects(fetched, assetsDir)
	if err != nil {
		return err
	}

	// Validation is done. The fetched bytes are the ground truth the
	// assets were built from; recording them keeps the offline no-op
	// test meaningful across pin bumps — one command refreshes assets
	// and fixtures together.
	writes := make([]write, 0, len(fixtureNames)+2)
	for _, name := range fixtureNames {
		writes = append(writes, write{path: filepath.Join(fixturesDir, name), content: fetched[name]})
	}
	writes = append(writes,
		write{path: filepath.Join(assetsDir, sccsFile), content: sccsContent},
		write{path: filepath.Join(assetsDir, objectsFile), content: objectsContent})
	if err := os.MkdirAll(fixturesDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", fixturesDir, err)
	}
	for _, w := range writes {
		changed, err := writeFileIfChanged(w.path, w.content)
		if err != nil {
			return err
		}
		if changed {
			fmt.Printf("regenerate: wrote %s\n", w.path)
		} else {
			fmt.Printf("regenerate: %s already matches the pinned upstream content\n", w.path)
		}
	}
	return nil
}

// fixtureNames are the fetched upstream files, as recorded under
// tools/regenerate/testdata.
var fixtureNames = []string{"kas-defaultconfig.yaml", "kcm-defaultconfig.yaml", "scc-restricted-v2.yaml", "scc-privileged.yaml"}

// fetchUpstream fetches every upstream file the pins address, keyed by
// its fixture name.
func fetchUpstream(ctx context.Context, transport http.RoundTripper) (map[string][]byte, error) {
	fetch := func(repo, commit, path string) ([]byte, error) {
		return kube.FetchManifest(ctx, transport, fetchTimeout, rawURL(repo, commit, path))
	}
	kasConfig, err := fetch(kasRepo, kasCommit, defaultConfigPath)
	if err != nil {
		return nil, err
	}
	kcmConfig, err := fetch(kcmRepo, kcmCommit, defaultConfigPath)
	if err != nil {
		return nil, err
	}
	restricted, err := fetch(kasRepo, kasCommit, fmt.Sprintf(sccPathTemplate, "restricted-v2"))
	if err != nil {
		return nil, err
	}
	privileged, err := fetch(kasRepo, kasCommit, fmt.Sprintf(sccPathTemplate, "privileged"))
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		"kas-defaultconfig.yaml": kasConfig,
		"kcm-defaultconfig.yaml": kcmConfig,
		"scc-restricted-v2.yaml": restricted,
		"scc-privileged.yaml":    privileged,
	}, nil
}

// assembleSCCs builds the full sccs.yaml content from the fetched bytes —
// the hand-authored header, then the two real SCC manifests verbatim and
// in the fixed order, separated by a document separator — and validates
// what it built before returning it. Nothing is written here.
func assembleSCCs(fetched map[string][]byte) ([]byte, error) {
	// Shape validation: each fetched file must be exactly the one SCC it
	// claims to be, or the pin has stopped pointing at the expected
	// layout.
	for name, fixture := range map[string]string{
		"restricted-v2": "scc-restricted-v2.yaml",
		"privileged":    "scc-privileged.yaml",
	} {
		if err := checkOneSCC(fetched[fixture], name); err != nil {
			return nil, err
		}
	}
	content := []byte(sccsHeader() + string(fetched["scc-restricted-v2.yaml"]) + "---\n" + string(fetched["scc-privileged.yaml"]))
	if err := validateSCCStream(content); err != nil {
		return nil, err
	}
	return content, nil
}

// validateSCCStream checks the assembled sccs.yaml content — the bytes
// that would be committed, not just the fetched inputs: exactly the two
// expected SecurityContextConstraints, in order, each carrying every
// top-level field the pinned CRD marks required (the same set the
// layer's Verify reads live, so an upstream manifest that drops one —
// nullable or not — fails here, at regeneration time, instead of in
// every later create run).
func validateSCCStream(content []byte) error {
	objs, err := kube.DecodeManifests(bytes.NewReader(content))
	if err != nil {
		return fmt.Errorf("the assembled SCC stream does not decode: %w", err)
	}
	wantNames := []string{"restricted-v2", "privileged"}
	if len(objs) != len(wantNames) {
		return fmt.Errorf("the assembled SCC stream carries %d objects, want %d", len(objs), len(wantNames))
	}
	for i, obj := range objs {
		if obj.GetAPIVersion() != "security.openshift.io/v1" || obj.GetKind() != "SecurityContextConstraints" {
			return fmt.Errorf("the assembled SCC stream's object %d is a %s %s, want a security.openshift.io/v1 SecurityContextConstraints",
				i+1, obj.GetAPIVersion(), obj.GetKind())
		}
		if obj.GetName() != wantNames[i] {
			return fmt.Errorf("the assembled SCC stream's object %d is %q, want %q", i+1, obj.GetName(), wantNames[i])
		}
		for _, field := range platformobjects.RequiredSCCFields {
			if _, present := obj.Object[field]; !present {
				return fmt.Errorf("the assembled %s SCC carries no %q field, which the CRD requires (has the upstream manifest changed?)", obj.GetName(), field)
			}
		}
	}
	return nil
}

// checkOneSCC validates one fetched SCC manifest: a single document, a
// SecurityContextConstraints carrying the expected metadata.name.
func checkOneSCC(b []byte, wantName string) error {
	objs, err := kube.DecodeManifests(bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("the pinned %s SCC manifest: %w", wantName, err)
	}
	if len(objs) != 1 {
		return fmt.Errorf("the pinned %s SCC manifest: %d objects, want exactly one", wantName, len(objs))
	}
	obj := objs[0]
	if obj.GetKind() != "SecurityContextConstraints" {
		return fmt.Errorf("the pinned %s SCC manifest: kind %q, want SecurityContextConstraints", wantName, obj.GetKind())
	}
	if obj.GetName() != wantName {
		return fmt.Errorf("the pinned SCC manifest: name %q, want %q (has the upstream layout changed?)", obj.GetName(), wantName)
	}
	return nil
}

// sccsHeader renders the hand-authored header, including the pins it was
// regenerated from. It is a constant of this tool, not extracted from the
// previous file, so it survives even if sccs.yaml is deleted or corrupted.
func sccsHeader() string {
	var b strings.Builder
	b.WriteString("# Real SecurityContextConstraints, verbatim from cluster-kube-apiserver-operator's\n")
	b.WriteString("# bootkube bindata (the manifests OpenShift itself installs), at the pinned\n")
	b.WriteString("# commit below. The platform-objects layer installs these with create\n")
	b.WriteString("# semantics, never an apply: the real SecurityContextConstraints CRD requires\n")
	b.WriteString("# twelve top-level fields, five of them nullable — priority,\n")
	b.WriteString("# allowedCapabilities, defaultAddCapabilities, requiredDropCapabilities, and\n")
	b.WriteString("# volumes — which must stay PRESENT even when null, and an apply-path merge\n")
	b.WriteString("# strips explicit `key: null` fields, which then fails CRD validation\n")
	b.WriteString("# (\"missing required field\"). A plain create sends the object as-is, nulls\n")
	b.WriteString("# included; a re-run deletes and recreates. See the layer's package comment.\n")
	b.WriteString("#\n")
	b.WriteString("# Regenerated by tools/regenerate -- do not hand-edit. Source:\n")
	fmt.Fprintf(&b, "# cluster-kube-apiserver-operator@%s\n", kasCommit)
	fmt.Fprintf(&b, "#   %s\n", fmt.Sprintf(sccPathTemplate, "restricted-v2"))
	fmt.Fprintf(&b, "#   %s\n", fmt.Sprintf(sccPathTemplate, "privileged"))
	b.WriteString("\n")
	return b.String()
}

// cmRef names one ConfigMap in a manifest stream.
type cmRef struct {
	namespace, name string
}

// String renders the ref the way error messages name objects:
// "namespace/name", or "name" for a nameless namespace.
func (r cmRef) String() string {
	if r.namespace == "" {
		return r.name
	}
	return r.namespace + "/" + r.name
}

// spliceSpec is one upstream field to splice into objects.yaml: where
// the value comes from (repo, commit, sourcePath, field), where it goes
// (dest — the ConfigMap whose data.config.yaml carries the field), and
// the value itself. The destination is defined here and nowhere else;
// the splice, the destination binding, and the post-splice validation
// all read this one struct.
type spliceSpec struct {
	repo       string
	commit     string
	sourcePath string              // the upstream file the value comes from
	field      string              // the JSON key replaced in the blob
	dest       cmRef               // the ConfigMap whose data.config.yaml carries the field
	args       map[string][]string // the field's new value
}

// updateObjects builds the new objects.yaml content: the two upstream
// fields spliced between their marker comments, then validated — the
// spliced file must decode to the same object set it had before the
// splice, with both ConfigMap blobs still valid JSON carrying exactly the
// spliced argument maps. The current file is read from assetsDir; nothing
// is written here.
func updateObjects(fetched map[string][]byte, assetsDir string) ([]byte, error) {
	kasArgs, err := upstreamArguments(fetched["kas-defaultconfig.yaml"], kasRepo, kasCommit, defaultConfigPath, "apiServerArguments")
	if err != nil {
		return nil, err
	}
	kcmArgs, err := upstreamArguments(fetched["kcm-defaultconfig.yaml"], kcmRepo, kcmCommit, defaultConfigPath, "extendedArguments")
	if err != nil {
		return nil, err
	}
	specs := []spliceSpec{
		{
			repo:       kasRepo,
			commit:     kasCommit,
			sourcePath: defaultConfigPath,
			field:      "apiServerArguments",
			dest:       cmRef{namespace: "openshift-kube-apiserver", name: "config"},
			args:       kasArgs,
		},
		{
			repo:       kcmRepo,
			commit:     kcmCommit,
			sourcePath: defaultConfigPath,
			field:      "extendedArguments",
			dest:       cmRef{namespace: "openshift-kube-controller-manager", name: "config"},
			args:       kcmArgs,
		},
	}

	path := filepath.Join(assetsDir, objectsFile)
	current, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	updated, err := spliceObjects(current, specs)
	if err != nil {
		return nil, fmt.Errorf("splicing %s: %w", objectsFile, err)
	}
	if err := validateSplicedObjects(current, updated, specs); err != nil {
		return nil, fmt.Errorf("validating the spliced %s: %w", objectsFile, err)
	}
	return updated, nil
}

// upstreamArguments reads one argument field (apiServerArguments,
// extendedArguments) from an upstream default config and returns it as
// the map of argument name to its values that the ConfigMap blobs embed.
// The values are coerced to the strings the real types use — "true" for
// booleans, decimals for numbers — and anything else (a null, a nested
// object) is refused: it is not the shape the ConfigMap carries, and
// guessing a rendering for it would silently change what the layer
// applies.
func upstreamArguments(config []byte, repo, commit, sourcePath, field string) (map[string][]string, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(config, &doc); err != nil {
		return nil, fmt.Errorf("parsing %s@%s %s: %w", repo, commit, sourcePath, err)
	}
	raw, ok := doc[field]
	if !ok {
		return nil, fmt.Errorf("%s@%s %s carries no %q field (has the upstream schema changed?)", repo, commit, sourcePath, field)
	}
	arguments, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s@%s %s: %q is a %T, want a map of argument names to value lists", repo, commit, sourcePath, field, raw)
	}
	out := make(map[string][]string, len(arguments))
	for name, values := range arguments {
		list, ok := values.([]any)
		if !ok {
			return nil, fmt.Errorf("%s@%s %s: argument %q is a %T, want a list of values", repo, commit, sourcePath, name, values)
		}
		strs := make([]string, 0, len(list))
		for _, v := range list {
			s, err := coerceArgumentValue(v)
			if err != nil {
				return nil, fmt.Errorf("%s@%s %s: argument %q: %w", repo, commit, sourcePath, name, err)
			}
			strs = append(strs, s)
		}
		out[name] = strs
	}
	return out, nil
}

// coerceArgumentValue renders one upstream argument value as the string
// the ConfigMap's map[string][]string shape carries.
func coerceArgumentValue(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	default:
		return "", fmt.Errorf("value %v is a %T, not a scalar with a string rendering", v, v)
	}
}

// The UPSTREAM marker lines the splice recognizes.
const (
	beginMarkerPrefix = "# BEGIN UPSTREAM"
	endMarker         = "# END UPSTREAM"
)

// spliceObjects replaces the upstream fields in the objects.yaml
// content, between their marker comments. The markers cannot live inside
// the blob — it is a literal block scalar parsed downstream as JSON —
// and a plain-text splice inside the one-line JSON would need
// hand-balanced braces, so the payload line is parsed, one top-level key's
// value span is located by decoder offsets, and exactly that span is
// replaced.
//
// The replacement is bound to its destination and emitted in the
// markers' own file order. Each pair must sit immediately outside the
// config.yaml block of the ConfigMap its splice names — a pair pasted
// around another ConfigMap's content is refused, never spliced — and the
// strict pairing rules (see markerPairs) mean the pairs, and the two
// lines each rewrites, are disjoint and ascending, so the output is
// built in one pass with no edit list to sort or check.
func spliceObjects(content []byte, specs []spliceSpec) ([]byte, error) {
	spans := scanLines(content)
	pairs, err := markerPairs(content, spans, specs)
	if err != nil {
		return nil, err
	}
	dests, err := configYAMLDestinations(content, spans, specs)
	if err != nil {
		return nil, err
	}
	var out []byte
	prev := 0
	for _, pair := range pairs {
		spec := pair.spec
		blob, err := bindToDestination(content, spans, pair, dests, spec)
		if err != nil {
			return nil, err
		}
		// The BEGIN marker line carries the pin, so a bump rewrites it;
		// its indentation is part of the hand-maintained file and stays.
		begin := lineText(content, spans[pair.begin])
		indent := begin[:len(begin)-len(strings.TrimLeft(begin, " \t"))]
		out = append(out, content[prev:spans[pair.begin].start]...)
		out = append(out, indent+fmt.Sprintf("# BEGIN UPSTREAM: %s@%s %s (%s)",
			spec.repo, spec.commit, spec.sourcePath, spec.field)...)
		prev = spans[pair.begin].end

		text := lineText(content, spans[blob])
		newText, err := replaceTopLevelField(text, spec.field, spec.args)
		if err != nil {
			return nil, fmt.Errorf("the JSON blob at line %d: %w", blob+1, err)
		}
		out = append(out, content[prev:spans[blob].start]...)
		out = append(out, newText...)
		prev = spans[blob].end
	}
	return append(out, content[prev:]...), nil
}

// bindToDestination proves that a marker pair bounds its splice's
// destination and returns the payload line index it owns. The markers
// must sit immediately outside the named ConfigMap's config.yaml block:
// BEGIN on the line above the block's header, END on the line below its
// single payload line, both at the header key's own indentation — a
// marker indented deeper than the key is content of some other block
// scalar, not a comment. Markers pasted around another ConfigMap's
// content, swallowed into a sibling block, or left straddling a moved
// config.yaml fail here, before anything is written.
func bindToDestination(content []byte, spans []lineSpan, pair markerPair, dests map[cmRef]destLocation, spec spliceSpec) (int, error) {
	loc, ok := dests[spec.dest]
	if !ok {
		return 0, fmt.Errorf("the UPSTREAM markers for %s bound lines %d-%d, but objects.yaml carries no %s ConfigMap with a one-line literal-block config.yaml",
			markerKey(spec.repo, spec.field), pair.begin+1, pair.end+1, spec.dest)
	}
	if pair.begin+1 != loc.header-1 || pair.end+1 != loc.payload+1 {
		return 0, fmt.Errorf("the UPSTREAM markers for %s bound lines %d-%d, which do not sit immediately outside the config.yaml block of the %s ConfigMap (header line %d, payload line %d): the markers no longer bound their destination",
			markerKey(spec.repo, spec.field), pair.begin+1, pair.end+1, spec.dest, loc.header, loc.payload)
	}
	header := lineText(content, spans[loc.header-1])
	indent := header[:len(header)-len(strings.TrimLeft(header, " \t"))]
	for _, marker := range []struct {
		line int
		what string
	}{
		{pair.begin, "BEGIN"},
		{pair.end, "END"},
	} {
		text := lineText(content, spans[marker.line])
		if got := text[:len(text)-len(strings.TrimLeft(text, " \t"))]; got != indent {
			return 0, fmt.Errorf("the %s UPSTREAM marker for %s, at line %d, is indented %q, not the config.yaml key's %q of the %s ConfigMap: a marker inside a sibling block's content is not a comment outside the block",
				marker.what, markerKey(spec.repo, spec.field), marker.line+1, got, indent, spec.dest)
		}
	}
	return loc.payload - 1, nil
}

// destLocation is where one ConfigMap's data.config.yaml block sits in
// the raw file: the 1-based line of its header (the "config.yaml: |"
// key line) and of its single payload line.
type destLocation struct {
	header, payload int
}

// configYAMLDestinations maps each splice destination to the raw-file
// location of its data.config.yaml. ConfigMaps the splices do not
// manage are identified but not inspected: the exact layout below is
// imposed on the destinations the splice rewrites, not on every
// ConfigMap in the file — a hand-maintained config.yaml in another
// shape is none of the splice's business. Identity is still every
// ConfigMap's business: a destination name carried by two ConfigMaps
// is an ambiguous destination whether or not the second copy holds a
// config.yaml, because the layer applies both definitions in order
// under one field manager. The supported layout is exactly one:
//
//	data:
//	  # BEGIN UPSTREAM: repo@commit path (field)
//	  config.yaml: |-
//	    {"…one JSON line…"}
//	  # END UPSTREAM
//
// The location is bound to the raw bytes, not inferred from the decoded
// value: the header line must literally be the config.yaml key with a
// block indicator, and the payload line must carry the whole decoded
// value. A destination's config.yaml in any other shape, or a duplicate
// key on the destination lookup path, is refused, because the splice
// could not prove which bytes it owns.
func configYAMLDestinations(content []byte, spans []lineSpan, specs []spliceSpec) (map[cmRef]destLocation, error) {
	destRefs := make(map[cmRef]bool, len(specs))
	for _, spec := range specs {
		destRefs[spec.dest] = true
	}
	dec := yaml.NewDecoder(bytes.NewReader(content))
	locs := make(map[cmRef]destLocation)
	firstLine := make(map[cmRef]int, len(specs))
	var doc yaml.Node
	for i := 1; ; i++ {
		err := dec.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parsing objects.yaml document %d: %w", i, err)
		}
		if len(doc.Content) == 0 {
			continue // an empty document carries nothing to splice
		}
		root := doc.Content[0]
		if root.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("objects.yaml document %d is not a mapping", i)
		}
		kind, err := mapValueUnique(root, "kind")
		if err != nil {
			return nil, fmt.Errorf("objects.yaml document %d: %w", i, err)
		}
		if kind == nil || kind.Value != "ConfigMap" {
			continue
		}
		meta, err := mapValueUnique(root, "metadata")
		if err != nil {
			return nil, fmt.Errorf("objects.yaml document %d: %w", i, err)
		}
		nameNode, err := mapValueUnique(meta, "name")
		if err != nil {
			return nil, fmt.Errorf("the ConfigMap in objects.yaml document %d: %w", i, err)
		}
		if nameNode == nil || nameNode.Kind != yaml.ScalarNode {
			continue // an unnamed ConfigMap cannot be a destination
		}
		nsNode, err := mapValueUnique(meta, "namespace")
		if err != nil {
			return nil, fmt.Errorf("the ConfigMap in objects.yaml document %d: %w", i, err)
		}
		ref := cmRef{namespace: nodeString(nsNode), name: nameNode.Value}
		if !destRefs[ref] {
			continue // not a destination: identity read, layout not its business
		}
		if first, seen := firstLine[ref]; seen {
			return nil, fmt.Errorf("objects.yaml carries two ConfigMaps named %s (lines %d and %d); the splice destination is ambiguous",
				ref, first, nameNode.Line)
		}
		firstLine[ref] = nameNode.Line
		data, err := mapValueUnique(root, "data")
		if err != nil {
			return nil, fmt.Errorf("the %s ConfigMap: %w", ref, err)
		}
		if data == nil {
			continue
		}
		scalar, err := mapValueUnique(data, "config.yaml")
		if err != nil {
			return nil, fmt.Errorf("the %s ConfigMap: %w", ref, err)
		}
		if scalar == nil {
			continue // this ConfigMap is not carrying the destination's config.yaml
		}
		loc, err := literalBlockLocation(content, spans, scalar, ref)
		if err != nil {
			return nil, err
		}
		locs[ref] = loc
	}
	return locs, nil
}

// literalBlockLocation binds one config.yaml scalar to its raw lines:
// the header (the key line carrying the block indicator) and the single
// payload line. Anything that does not match the one supported layout
// is an error naming the ConfigMap, so a destination the splice cannot
// see byte-exactly is refused rather than guessed.
func literalBlockLocation(content []byte, spans []lineSpan, scalar *yaml.Node, ref cmRef) (destLocation, error) {
	if scalar.Kind != yaml.ScalarNode || scalar.Style != yaml.LiteralStyle {
		return destLocation{}, fmt.Errorf("the config.yaml data of the %s ConfigMap is not a literal block scalar; the splice supports exactly one JSON line in a literal block", ref)
	}
	body := strings.TrimSuffix(scalar.Value, "\n")
	if strings.Contains(body, "\n") {
		return destLocation{}, fmt.Errorf("the config.yaml data of the %s ConfigMap spans several physical lines; the splice supports exactly one", ref)
	}
	header, payload := scalar.Line, scalar.Line+1
	if header < 1 || payload > len(spans) {
		return destLocation{}, fmt.Errorf("the config.yaml block of the %s ConfigMap does not sit inside the file (header line %d)", ref, header)
	}
	if trimmed := strings.TrimSpace(lineText(content, spans[header-1])); !strings.HasPrefix(trimmed, "config.yaml:") || !strings.Contains(trimmed, "|") {
		return destLocation{}, fmt.Errorf("line %d, the config.yaml header of the %s ConfigMap, is %q; want the config.yaml key with a literal block indicator", header, ref, trimmed)
	}
	if got := strings.TrimLeft(lineText(content, spans[payload-1]), " \t"); got != body {
		return destLocation{}, fmt.Errorf("line %d does not carry the whole config.yaml data of the %s ConfigMap; the decoded value and the raw bytes disagree", payload, ref)
	}
	return destLocation{header: header, payload: payload}, nil
}

// mapValueUnique is the lookup the destination walk uses: like
// mapValue, but a mapping that carries the key twice is an error, not a
// value to pick from — the walk could not tell which of the two the
// splice manages. A nil mapping or a missing key is still no result.
func mapValueUnique(mapping *yaml.Node, key string) (*yaml.Node, error) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, nil
	}
	var found *yaml.Node
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != key {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("mapping key %q appears twice (lines %d and %d)", key, found.Line, mapping.Content[i].Line)
		}
		found = mapping.Content[i+1]
	}
	return found, nil
}

// nodeString returns a scalar node's value, or "" for anything else.
func nodeString(n *yaml.Node) string {
	if n != nil && n.Kind == yaml.ScalarNode {
		return n.Value
	}
	return ""
}

// lineSpan is one physical line of a file: the byte range [start, end),
// excluding its line terminator.
type lineSpan struct {
	start, end int
}

// scanLines splits content into line spans. A trailing newline does not
// produce a final empty line; content without one simply has a last span
// that ends at EOF.
func scanLines(content []byte) []lineSpan {
	var spans []lineSpan
	start := 0
	for i, b := range content {
		if b == '\n' {
			spans = append(spans, lineSpan{start: start, end: i})
			start = i + 1
		}
	}
	if start < len(content) {
		spans = append(spans, lineSpan{start: start, end: len(content)})
	}
	return spans
}

// lineText returns one line's text without its terminator.
func lineText(content []byte, span lineSpan) string {
	return string(content[span.start:span.end])
}

// markerPair is one BEGIN/END UPSTREAM marker pair: the splice it
// serves and the line indexes of its two marker lines.
type markerPair struct {
	spec       spliceSpec
	begin, end int
}

// markerKey is the identity a marker pair is keyed by: the repo and the
// field it bounds. The commit and path are deliberately not part of it —
// a pin bump rewrites them.
func markerKey(repo, field string) string {
	return repo + " (" + field + ")"
}

// markerPairs parses the file's UPSTREAM markers under strict pairing
// rules and returns the pairs in the order their BEGIN markers appear
// in the file, each carrying the splice it serves:
//
//   - a BEGIN marker must be closed by the very next marker line, and
//     that line must be its END marker — no other marker may sit inside
//     a pair, so a pair can never borrow another field's END, nest, or
//     overlap;
//   - a field with two pairs is refused, as is a pair no splice manages
//     and a splice with no pair: all three are drift between the file
//     and the tool, not something to splice around;
//   - a marker-shaped line that does not parse is an error, never a
//     comment to skip.
//
// The returned order is the file order the splice emits its
// replacements in; strict pairing makes the pairs disjoint and
// ascending, so no caller needs to sort or re-check them.
func markerPairs(content []byte, spans []lineSpan, specs []spliceSpec) ([]markerPair, error) {
	byKey := make(map[string]spliceSpec, len(specs))
	for _, spec := range specs {
		key := markerKey(spec.repo, spec.field)
		if _, duplicate := byKey[key]; duplicate {
			return nil, fmt.Errorf("two splices manage %s", key)
		}
		byKey[key] = spec
	}
	var pairs []markerPair
	seen := make(map[string]bool, len(specs))
	open := -1 // the index into pairs of the pair being closed, or -1
	for i, span := range spans {
		text := strings.TrimLeft(lineText(content, span), " \t")
		switch {
		case strings.HasPrefix(text, beginMarkerPrefix):
			repo, field, err := parseBeginMarker(text, i+1)
			if err != nil {
				return nil, err
			}
			key := markerKey(repo, field)
			if open >= 0 {
				return nil, fmt.Errorf("line %d: a BEGIN UPSTREAM marker inside the pair for %s, opened at line %d and never closed",
					i+1, markerKey(pairs[open].spec.repo, pairs[open].spec.field), pairs[open].begin+1)
			}
			if seen[key] {
				return nil, fmt.Errorf("line %d: a second BEGIN UPSTREAM marker for %s", i+1, key)
			}
			spec, managed := byKey[key]
			if !managed {
				return nil, fmt.Errorf("line %d: a BEGIN UPSTREAM marker for %s, which no splice manages", i+1, key)
			}
			seen[key] = true
			pairs = append(pairs, markerPair{spec: spec, begin: i, end: -1})
			open = len(pairs) - 1
		case text == endMarker:
			if open < 0 {
				return nil, fmt.Errorf("line %d: an END UPSTREAM marker with no BEGIN open", i+1)
			}
			pairs[open].end = i
			open = -1
		case strings.HasPrefix(text, endMarker):
			return nil, fmt.Errorf("line %d: %q is not a well-formed END UPSTREAM marker", i+1, text)
		}
	}
	if open >= 0 {
		return nil, fmt.Errorf("the BEGIN UPSTREAM marker for %s, opened at line %d, is never closed",
			markerKey(pairs[open].spec.repo, pairs[open].spec.field), pairs[open].begin+1)
	}
	for key := range byKey {
		if !seen[key] {
			return nil, fmt.Errorf("no BEGIN/END UPSTREAM marker pair for %s", key)
		}
	}
	return pairs, nil
}

// parseBeginMarker parses one BEGIN UPSTREAM marker line —
// "# BEGIN UPSTREAM: repo@commit path (field)" — into its repo and
// field. The commit and path are validated for presence but not matched:
// a pin bump rewrites them, so a pair is identified by its repo and
// field alone.
func parseBeginMarker(text string, line int) (repo, field string, err error) {
	fail := func() (string, string, error) {
		return "", "", fmt.Errorf("line %d: %q is not a well-formed BEGIN UPSTREAM marker: want %q",
			line, text, "# BEGIN UPSTREAM: repo@commit path (field)")
	}
	payload := strings.TrimSpace(strings.TrimPrefix(text, beginMarkerPrefix))
	if !strings.HasPrefix(payload, ":") {
		return fail()
	}
	payload = strings.TrimSpace(strings.TrimPrefix(payload, ":"))
	at := strings.IndexByte(payload, '@')
	open := strings.LastIndex(payload, " (")
	if at <= 0 || open < at || !strings.HasSuffix(payload, ")") {
		return fail()
	}
	repo = payload[:at]
	field = payload[open+2 : len(payload)-1]
	commit, path, ok := strings.Cut(payload[at+1:open], " ")
	if repo == "" || field == "" || !ok || commit == "" || path == "" {
		return fail()
	}
	return repo, field, nil
}

// replaceTopLevelField replaces the value of one top-level key in a
// single-line JSON object with args, leaving every other byte of the
// line — key order and separator style included — exactly as it was.
// The span is located with decoder input offsets, not string search:
// the field's name or value could appear inside other keys' values.
// The managed key must occur exactly once: decoding a JSON object into
// a map keeps the last of two same-named keys, so a duplicated managed
// key would let the splice rewrite the occurrence every reader ignores
// while believing itself in sync over the one they read. When the key's
// decoded value already equals args, the original bytes are retained,
// so an in-sync splice rewrites nothing; a changed value is rendered by
// the standard encoder.
func replaceTopLevelField(line, field string, args map[string][]string) (string, error) {
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	open, err := dec.Token()
	if err != nil {
		return "", fmt.Errorf("not valid JSON: %w", err)
	}
	if d, ok := open.(json.Delim); !ok || d != '{' {
		return "", fmt.Errorf("not a JSON object")
	}
	var (
		found                        bool
		keyEnd, valueStart, valueEnd int
		raw                          json.RawMessage
	)
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return "", fmt.Errorf("not valid JSON: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return "", fmt.Errorf("object key is not a string")
		}
		kEnd := dec.InputOffset()
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return "", fmt.Errorf("reading key %q's value: %w", key, err)
		}
		vEnd := dec.InputOffset()
		if key != field {
			continue
		}
		if found {
			return "", fmt.Errorf("key %q appears twice in the JSON blob; decoding keeps the last of two same-named keys, so the splice refuses to pick one", field)
		}
		found, keyEnd, valueEnd, raw = true, int(kEnd), int(vEnd), value
	}
	if !found {
		return "", fmt.Errorf("key %q not found in the JSON blob", field)
	}
	// keyEnd points just past the key string; the value starts after
	// the colon and whitespace between the two offsets.
	valueStart = keyEnd
	for valueStart < valueEnd &&
		(line[valueStart] == ':' || line[valueStart] == ' ' || line[valueStart] == '\t') {
		valueStart++
	}
	if valueEnd > len(line) || line[valueStart:valueEnd] != string(raw) {
		return "", fmt.Errorf("locating key %q's value: the decoded span does not match the line", field)
	}
	var current map[string][]string
	if err := json.Unmarshal(raw, &current); err == nil && reflect.DeepEqual(current, args) {
		return line, nil
	}
	rendered, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("rendering key %q's new value: %w", field, err)
	}
	return line[:valueStart] + string(rendered) + line[valueEnd:], nil
}

// validateSplicedObjects is the shape validation no candidate passes
// without: the spliced file decodes as a manifest with exactly the same
// object references as before the splice, every object's decoded
// content is unchanged but the managed argument maps, and every splice
// destination is still exactly one object carrying a config.yaml whose
// managed field is the spliced map. The decoded comparison is what
// catches a YAML alias: an anchored config.yaml with a sibling key
// aliasing it decodes the sibling from the spliced block, so the
// sibling's data changes with the splice even though its own bytes
// never moved — the byte-range edit cannot see that, the before/after
// decode can. A splice that disturbed anything it does not manage
// fails here instead of in a create run.
func validateSplicedObjects(before, after []byte, specs []spliceSpec) error {
	beforeObjs, err := kube.DecodeManifests(bytes.NewReader(before))
	if err != nil {
		return fmt.Errorf("the pre-splice file: %w", err)
	}
	afterObjs, err := kube.DecodeManifests(bytes.NewReader(after))
	if err != nil {
		return fmt.Errorf("the spliced file does not decode: %w", err)
	}
	if len(beforeObjs) != len(afterObjs) {
		return fmt.Errorf("the splice changed the object count: %d -> %d", len(beforeObjs), len(afterObjs))
	}
	for i := range beforeObjs {
		b, a := beforeObjs[i], afterObjs[i]
		if b.GetAPIVersion() != a.GetAPIVersion() || b.GetKind() != a.GetKind() ||
			b.GetNamespace() != a.GetNamespace() || b.GetName() != a.GetName() {
			return fmt.Errorf("the splice changed object %d: %s %s -> %s %s",
				i+1, b.GetKind(), objectRef(b), a.GetKind(), objectRef(a))
		}
	}
	specByDest := make(map[cmRef]spliceSpec, len(specs))
	for _, spec := range specs {
		if _, dup := specByDest[spec.dest]; dup {
			return fmt.Errorf("two splices manage the %s ConfigMap", spec.dest)
		}
		specByDest[spec.dest] = spec
	}
	// Each destination must be exactly one object: the layer applies
	// documents in order under one field manager, so a second ConfigMap
	// under the same identity would be a silent last-writer-wins.
	destIndex := make(map[cmRef]int, len(specs))
	for i, obj := range afterObjs {
		if obj.GetKind() != "ConfigMap" {
			continue
		}
		ref := cmRef{namespace: obj.GetNamespace(), name: obj.GetName()}
		if _, managed := specByDest[ref]; !managed {
			continue
		}
		if _, dup := destIndex[ref]; dup {
			return fmt.Errorf("the spliced file carries two ConfigMaps named %s; the splice destination is ambiguous", ref)
		}
		destIndex[ref] = i
	}
	for _, spec := range specs {
		i, ok := destIndex[spec.dest]
		if !ok {
			return fmt.Errorf("the %s ConfigMap is missing from the spliced file", spec.dest)
		}
		if err := compareDestination(beforeObjs[i], afterObjs[i], spec); err != nil {
			return err
		}
	}
	managed := make(map[int]bool, len(specs))
	for _, i := range destIndex {
		managed[i] = true
	}
	for i, obj := range afterObjs {
		if managed[i] {
			continue
		}
		if !reflect.DeepEqual(beforeObjs[i].Object, obj.Object) {
			return fmt.Errorf("the splice changed the decoded %s %s, which no splice manages",
				obj.GetKind(), objectRef(obj))
		}
	}
	return nil
}

// compareDestination enforces the one decoded change a splice may make
// to its destination: the named argument map inside data.config.yaml.
// Inside that JSON, the managed key must be exactly the spliced map and
// every other key's raw bytes must survive verbatim — the blob's other
// fields are hand-maintained and may carry any valid JSON, numbers no
// Go float64 can represent included, so they are compared as bytes and
// never decoded into Go values. Everywhere else in the object —
// sibling data keys included — the whole decoded object must be
// unchanged, which is what catches a sibling key aliased to the managed
// config.yaml: its bytes never move, but its decoded value follows the
// spliced block.
func compareDestination(before, after *unstructured.Unstructured, spec spliceSpec) error {
	bBlob, bFound, err := unstructured.NestedString(before.Object, "data", "config.yaml")
	if err != nil || !bFound || bBlob == "" {
		return fmt.Errorf("the pre-splice %s ConfigMap carries no config.yaml data", spec.dest)
	}
	aBlob, aFound, err := unstructured.NestedString(after.Object, "data", "config.yaml")
	if err != nil || !aFound || aBlob == "" {
		return fmt.Errorf("the %s ConfigMap carries no config.yaml data after the splice", spec.dest)
	}
	bJSON, err := blobKeys(bBlob)
	if err != nil {
		return fmt.Errorf("the pre-splice %s ConfigMap's config.yaml: %w", spec.dest, err)
	}
	aJSON, err := blobKeys(aBlob)
	if err != nil {
		return fmt.Errorf("the %s ConfigMap's config.yaml after the splice: %w", spec.dest, err)
	}
	// The managed field itself must be exactly the spliced map.
	managed, present := aJSON[spec.field]
	if !present {
		return fmt.Errorf("the %s ConfigMap's config.yaml carries no %q field after the splice", spec.dest, spec.field)
	}
	var got map[string][]string
	if err := json.Unmarshal(managed, &got); err != nil {
		return fmt.Errorf("the %s ConfigMap's %q field is not a map of argument names to value lists: %w", spec.dest, spec.field, err)
	}
	if !reflect.DeepEqual(got, spec.args) {
		return fmt.Errorf("the %s ConfigMap's %q field does not match the spliced argument map after the splice", spec.dest, spec.field)
	}
	// Every other key in the blob must survive the splice byte-for-byte.
	for _, k := range slices.Sorted(maps.Keys(bJSON)) {
		if k == spec.field {
			continue
		}
		afterValue, present := aJSON[k]
		if !present {
			return fmt.Errorf("the splice removed %q from the %s ConfigMap's config.yaml", k, spec.dest)
		}
		if !bytes.Equal(bJSON[k], afterValue) {
			return fmt.Errorf("the splice changed the %s ConfigMap beyond its managed %q field: the config.yaml key %q differs",
				spec.dest, spec.field, k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(aJSON)) {
		if k == spec.field {
			continue
		}
		if _, present := bJSON[k]; !present {
			return fmt.Errorf("the splice added %q to the %s ConfigMap's config.yaml", k, spec.dest)
		}
	}
	// Everything else in the object — sibling data keys included — must
	// decode identically; config.yaml itself is blanked out on both
	// sides because the blob rules above are its own comparison.
	bData, bOK := before.Object["data"].(map[string]any)
	aData, aOK := after.Object["data"].(map[string]any)
	if !bOK || !aOK {
		return fmt.Errorf("the %s ConfigMap's data is not a mapping after the splice", spec.dest)
	}
	delete(bData, "config.yaml")
	delete(aData, "config.yaml")
	if !reflect.DeepEqual(before.Object, after.Object) {
		return siblingDataDifference(bData, aData, spec)
	}
	return nil
}

// blobKeys decodes one config.yaml blob into its top-level keys, each
// value kept as its raw JSON bytes. Only the top level is mapped: the
// values are compared as bytes, never decoded into Go values, so a
// hand-maintained number (or any other value) no Go float64 can
// represent survives decoding untouched.
func blobKeys(blob string) (map[string]json.RawMessage, error) {
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(blob), &keys); err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}
	return keys, nil
}

// siblingDataDifference reports the object-level difference the
// standard-equality check in compareDestination found, naming the data
// key that differs. It reads only data's own keys — a deep difference
// inside one sibling's value is that sibling's difference — so pointing
// at data.maintainer.yaml needs no recursive path walker.
func siblingDataDifference(bData, aData map[string]any, spec spliceSpec) error {
	for _, k := range slices.Sorted(maps.Keys(bData)) {
		afterValue, present := aData[k]
		if !present {
			return fmt.Errorf("the splice removed data.%s from the %s ConfigMap beyond its managed %q field", k, spec.dest, spec.field)
		}
		if !reflect.DeepEqual(bData[k], afterValue) {
			return fmt.Errorf("the splice changed the decoded %s ConfigMap beyond its managed %q field: data.%s differs",
				spec.dest, spec.field, k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(aData)) {
		if _, present := bData[k]; !present {
			return fmt.Errorf("the splice added data.%s to the %s ConfigMap beyond its managed %q field", k, spec.dest, spec.field)
		}
	}
	return fmt.Errorf("the splice changed the decoded %s ConfigMap beyond its managed %q field outside its data", spec.dest, spec.field)
}

// objectRef renders an object reference for error messages:
// "namespace/name" for namespaced objects, "name" otherwise.
func objectRef(obj *unstructured.Unstructured) string {
	if ns := obj.GetNamespace(); ns != "" {
		return ns + "/" + obj.GetName()
	}
	return obj.GetName()
}

// moduleRoot returns this module's root directory, wherever inside the
// module the tool runs from, via `go env GOMOD`.
func moduleRoot() (string, error) {
	gomod, err := goOutput("env", "GOMOD")
	if err != nil {
		return "", err
	}
	if gomod == "" || gomod == os.DevNull {
		return "", fmt.Errorf("not running inside a Go module (GOMOD=%q)", gomod)
	}
	return filepath.Dir(gomod), nil
}

// goOutput runs the go command and returns its trimmed stdout.
func goOutput(args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, excerpt(stderr.Bytes()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// excerpt renders a short, single-line excerpt of b for error messages.
func excerpt(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// writeFileIfChanged writes content to path only when it differs from
// what is there, reporting whether it wrote. Regenerating in-sync
// content is a no-op for the working tree.
func writeFileIfChanged(path string, content []byte) (changed bool, err error) {
	if current, err := os.ReadFile(path); err == nil && bytes.Equal(current, content) {
		return false, nil
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		return false, fmt.Errorf("writing %s: %w", path, err)
	}
	return true, nil
}

// rawURL builds the raw-content URL for one file at one pinned commit.
func rawURL(repo, commit, path string) string {
	return "https://raw.githubusercontent.com/openshift/" + repo + "/" + commit + "/" + path
}

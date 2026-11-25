package compliance

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

// The catalog tests pin the committed catalog's contract from three
// sides: the manifests themselves (nothing floats), the assembly
// (deterministic, valid, digest-pinned), and the size (the catalog
// rides a ConfigMap, whose object limit is 1MiB).

// TestCatalogManifestsCarryNoFloatingTags scans every committed
// manifest for the floating tags the rewrite exists to remove — the
// check that reaches fields the CSV parse does not (the
// must-gather-image annotation among them). A regenerated manifest
// set that ships a floating reference fails here before it fails a
// proof.
func TestCatalogManifestsCarryNoFloatingTags(t *testing.T) {
	names, err := catalogManifestFiles()
	if err != nil {
		t.Fatalf("catalogManifestFiles: %v", err)
	}
	if len(names) < 10 {
		t.Fatalf("committed manifests = %d files, want the full bundle (dozens)", len(names))
	}
	for _, name := range names {
		raw, err := catalogManifests.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			trimmed := strings.TrimSpace(line)
			for _, suffix := range []string{":master", ":latest"} {
				if strings.HasSuffix(trimmed, suffix) {
					t.Errorf("%s line %d carries the floating reference %q — regenerate the catalog with a complete pin table (make pin-compliance-catalog)", name, i+1, trimmed)
				}
			}
		}
	}
}

// TestCatalogAssembly pins the assembled catalog's shape: three
// documents, the package and channel consistent with the CSV, the
// bundle entry free of any image reference (its manifests are inline),
// one inline object per committed manifest, and every related image
// digest-pinned.
func TestCatalogAssembly(t *testing.T) {
	catalog, err := renderCatalog()
	if err != nil {
		t.Fatalf("renderCatalog: %v", err)
	}
	docs := decodeJSONDocuments(t, catalog)
	if len(docs) != 3 {
		t.Fatalf("the catalog renders %d documents, want 3 (package, channel, bundle)", len(docs))
	}
	pkg, channel, bundle := docs[0], docs[1], docs[2]

	if pkg["schema"] != "olm.package" || pkg["name"] != catalogPackageName || pkg["defaultChannel"] != catalogChannel {
		t.Errorf("package document = %v, want %s on channel %s", pkg, catalogPackageName, catalogChannel)
	}
	if channel["schema"] != "olm.channel" || channel["package"] != catalogPackageName || channel["name"] != catalogChannel {
		t.Errorf("channel document = %v, want the %s channel of %s", channel, catalogChannel, catalogPackageName)
	}
	entries, _ := channel["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("channel entries = %v, want exactly one (the bundle)", entries)
	}
	entry, _ := entries[0].(map[string]any)

	if bundle["schema"] != "olm.bundle" || bundle["package"] != catalogPackageName {
		t.Errorf("bundle document = %v, want the %s bundle", bundle, catalogPackageName)
	}
	if _, carries := bundle["image"]; carries {
		t.Errorf("the bundle entry carries an image reference — the committed catalog's manifests are inline and nothing should be pulled for the resolution")
	}
	if entry["name"] != bundle["name"] {
		t.Errorf("the channel entry names %v but the bundle is %v — a Subscription resolving the channel head would find nothing", entry["name"], bundle["name"])
	}

	names, err := catalogManifestFiles()
	if err != nil {
		t.Fatalf("catalogManifestFiles: %v", err)
	}
	var gvk, objects int
	properties, _ := bundle["properties"].([]any)
	for _, p := range properties {
		property, _ := p.(map[string]any)
		switch property["type"] {
		case "olm.gvk":
			gvk++
		case "olm.bundle.object":
			objects++
			value, _ := property["value"].(map[string]any)
			data, _ := value["data"].(string)
			decoded, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				t.Errorf("an olm.bundle.object does not decode from base64: %v", err)
				continue
			}
			if !json.Valid(decoded) {
				t.Error("an olm.bundle.object does not decode as JSON — opm's loader would refuse the catalog")
			}
		}
	}
	if objects != len(names) {
		t.Errorf("inline objects = %d, committed manifests = %d, want one object per manifest", objects, len(names))
	}
	if gvk == 0 {
		t.Error("the bundle carries no olm.gvk properties — OLM's resolver needs the provided APIs")
	}

	related, _ := bundle["relatedImages"].([]any)
	if len(related) == 0 {
		t.Fatal("the bundle carries no relatedImages")
	}
	for _, r := range related {
		image, _ := r.(map[string]any)["image"].(string)
		if !catalogDigestPattern.MatchString(image) {
			t.Errorf("relatedImage %v is not digest-pinned", r)
		}
	}
}

// TestCatalogIsDeterministic pins the assembly's determinism: the same
// committed manifests render to the same bytes, so the ConfigMap the
// proof applies is stable across runs and platforms.
func TestCatalogIsDeterministic(t *testing.T) {
	first, err := renderCatalog()
	if err != nil {
		t.Fatalf("renderCatalog: %v", err)
	}
	second, err := renderCatalog()
	if err != nil {
		t.Fatalf("renderCatalog (again): %v", err)
	}
	if first != second {
		t.Error("renderCatalog is not deterministic: the same manifests rendered to different bytes")
	}
}

// TestCatalogFitsItsConfigMap pins the size budget: the catalog rides
// one ConfigMap, and a ConfigMap object may not exceed 1MiB. The
// margin keeps room for the object's own metadata.
func TestCatalogFitsItsConfigMap(t *testing.T) {
	catalog, err := renderCatalog()
	if err != nil {
		t.Fatalf("renderCatalog: %v", err)
	}
	const limit = 1024 * 1024
	if len(catalog) >= limit {
		t.Errorf("the assembled catalog is %d bytes, at or beyond a ConfigMap's %d-byte limit", len(catalog), limit)
	}
	if len(catalog) >= limit-64*1024 {
		t.Errorf("the assembled catalog is %d bytes, within 64KiB of a ConfigMap's %d-byte limit — leave headroom or split the content", len(catalog), limit)
	}
}

// decodeJSONDocuments splits one concatenated-JSON stream (the format
// operator tooling reads) into its documents, failing the test on any
// parse error.
func decodeJSONDocuments(t *testing.T, stream string) []map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(stream))
	var docs []map[string]any
	for dec.More() {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("decoding the assembled catalog: %v", err)
		}
		docs = append(docs, doc)
	}
	return docs
}

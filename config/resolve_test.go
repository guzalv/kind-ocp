package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kind-ocp.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

func TestResolveDefaults(t *testing.T) {
	cfg, err := Resolve(Flags{}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Prefix != DefaultPrefix {
		t.Errorf("prefix = %q, want %q", cfg.Prefix, DefaultPrefix)
	}
	if cfg.Count != DefaultCount {
		t.Errorf("count = %d, want %d", cfg.Count, DefaultCount)
	}
	if cfg.Profile != DefaultProfile {
		t.Errorf("profile = %q, want %q", cfg.Profile, DefaultProfile)
	}
	if cfg.KindNodeImage != DefaultKindNodeImage {
		t.Errorf("kindNodeImage = %q, want the pinned default", cfg.KindNodeImage)
	}
	if !filepath.IsAbs(cfg.StateRoot) {
		t.Errorf("stateRoot %q is not absolute", cfg.StateRoot)
	}
	if cfg.Timeouts.NodeReady != 5*time.Minute {
		t.Errorf("nodeReady = %s, want 5m", cfg.Timeouts.NodeReady)
	}
}

// The precedence contract (AC-2.2): flag > env > file > default.
func TestResolvePrecedence(t *testing.T) {
	file := writeConfig(t, "prefix: from-file\ncount: 3\nprofile: from-file\n")

	t.Run("file overrides defaults", func(t *testing.T) {
		cfg, err := Resolve(Flags{ConfigPath: strPtr(file)}, Env{})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Prefix != "from-file" || cfg.Count != 3 || cfg.Profile != "from-file" {
			t.Errorf("got %+v, want file values", cfg)
		}
	})

	t.Run("env overrides file", func(t *testing.T) {
		cfg, err := Resolve(Flags{ConfigPath: strPtr(file)}, Env{Prefix: "from-env", Count: "4", Profile: "minimal"})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Prefix != "from-env" || cfg.Count != 4 || cfg.Profile != "minimal" {
			t.Errorf("got %+v, want env values", cfg)
		}
	})

	t.Run("flag overrides env and file", func(t *testing.T) {
		cfg, err := Resolve(
			Flags{ConfigPath: strPtr(file), Prefix: strPtr("from-flag"), Count: intPtr(5), Profile: strPtr("full")},
			Env{Prefix: "from-env", Count: "4", Profile: "minimal"},
		)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Prefix != "from-flag" || cfg.Count != 5 || cfg.Profile != "full" {
			t.Errorf("got %+v, want flag values", cfg)
		}
	})

	t.Run("env without file", func(t *testing.T) {
		cfg, err := Resolve(Flags{}, Env{Count: "4"})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Count != 4 {
			t.Errorf("count = %d, want 4", cfg.Count)
		}
	})

	t.Run("malformed env count is ignored when the flag wins", func(t *testing.T) {
		cfg, err := Resolve(Flags{Count: intPtr(3)}, Env{Count: "abc"})
		if err != nil {
			t.Fatalf("a valid --count must not fail on a malformed KIND_OCP_COUNT: %v", err)
		}
		if cfg.Count != 3 {
			t.Errorf("count = %d, want 3 (flag wins)", cfg.Count)
		}
	})

	t.Run("state root flag and xdg default", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "/tmp/xdg-state")
		cfg, err := Resolve(Flags{}, Env{})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(cfg.StateRoot, "/tmp/xdg-state") {
			t.Errorf("stateRoot %q, want under the XDG state home", cfg.StateRoot)
		}
		cfg, err = Resolve(Flags{StateRoot: strPtr("/tmp/flag-root")}, Env{})
		if err != nil {
			t.Fatal(err)
		}
		if cfg.StateRoot != "/tmp/flag-root" {
			t.Errorf("stateRoot = %q, want /tmp/flag-root", cfg.StateRoot)
		}
	})
}

func TestResolveRejectsBadInput(t *testing.T) {
	// Scope input (the prefix) is validated for every command.
	cases := []struct {
		name  string
		flags Flags
		env   Env
	}{
		{"prefix uppercase", Flags{Prefix: strPtr("KindOcp")}, Env{}},
		{"prefix trailing dash", Flags{Prefix: strPtr("kind-")}, Env{}},
		{"prefix underscore", Flags{Prefix: strPtr("kind_ocp")}, Env{}},
		{"prefix too long", Flags{Prefix: strPtr("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")}, Env{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Resolve(tc.flags, tc.env); err == nil {
				t.Fatal("expected an error")
			}
		})
	}

	// Create-only input is validated by ValidateCreate, not Resolve: the
	// scope commands must never be blocked by it.
	t.Run("create-only values resolve but fail ValidateCreate", func(t *testing.T) {
		flags := func(count int) Flags { return Flags{Count: &count} }
		for _, count := range []int{0, -1, 65} {
			cfg, err := Resolve(flags(count), Env{})
			if err != nil {
				t.Fatalf("Resolve(--count %d) = %v, want success (create-only validation is deferred)", count, err)
			}
			if err := cfg.ValidateCreate(); err == nil {
				t.Errorf("ValidateCreate(--count %d) = nil, want an error", count)
			}
		}
		if _, err := Resolve(Flags{}, Env{Count: "abc"}); err == nil {
			t.Fatal("Resolve with a malformed KIND_OCP_COUNT must still fail: the winning value is unparseable")
		}
		cfg, err := Resolve(Flags{}, Env{})
		if err != nil {
			t.Fatal(err)
		}
		cfg.Profile = ""
		if err := cfg.ValidateCreate(); err == nil {
			t.Error("ValidateCreate must reject an empty profile")
		}
		cfg, err = Resolve(Flags{}, Env{})
		if err != nil {
			t.Fatal(err)
		}
		cfg.KindNodeImage = ""
		if err := cfg.ValidateCreate(); err == nil {
			t.Error("ValidateCreate must reject an empty node image")
		}
	})
}

func TestResolveUnknownFileFieldFails(t *testing.T) {
	file := writeConfig(t, "profle: full\n")
	if _, err := Resolve(Flags{ConfigPath: strPtr(file)}, Env{}); err == nil {
		t.Fatal("expected unknown-field error (strict decoding)")
	}
}

func TestLoadFileRejectsTrailingDocuments(t *testing.T) {
	file := writeConfig(t, "count: 2\n---\ncount: 3\n")
	if _, err := Resolve(Flags{ConfigPath: strPtr(file)}, Env{}); err == nil {
		t.Fatal("expected an error for a config file with two YAML documents")
	}
	// A single document, comments, and a trailing newline stay valid.
	file = writeConfig(t, "# comment\ncount: 2\n\n")
	if _, err := Resolve(Flags{ConfigPath: strPtr(file)}, Env{}); err != nil {
		t.Fatalf("single-document file must load: %v", err)
	}
}

func TestResolveFileCountZeroFails(t *testing.T) {
	// `count: 0` is an explicit value, not an omitted one: ValidateCreate
	// rejects it rather than silently falling back to the default of 2, but
	// Resolve accepts it so scope commands are not blocked by the file.
	file := writeConfig(t, "count: 0\n")
	cfg, err := Resolve(Flags{ConfigPath: strPtr(file)}, Env{})
	if err != nil {
		t.Fatalf("Resolve must accept count: 0 (create-only validation): %v", err)
	}
	if cfg.Count != 0 {
		t.Errorf("count = %d, want the explicit file value 0", cfg.Count)
	}
	if err := cfg.ValidateCreate(); err == nil {
		t.Fatal("expected ValidateCreate to reject count: 0")
	}
	// A valid file count still wins over the default.
	file = writeConfig(t, "count: 3\n")
	cfg, err = Resolve(Flags{ConfigPath: strPtr(file)}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Count != 3 {
		t.Errorf("count = %d, want 3", cfg.Count)
	}
}

func TestResolveMissingFileFails(t *testing.T) {
	if _, err := Resolve(Flags{ConfigPath: strPtr("/nonexistent/kind-ocp.yaml")}, Env{}); err == nil {
		t.Fatal("expected an error for a missing config file")
	}
}

func TestClusterNames(t *testing.T) {
	cfg := &Config{Prefix: "p", Count: 3}
	names := cfg.ClusterNames()
	want := []string{"p-1", "p-2", "p-3"}
	if len(names) != 3 {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
}

func TestProfileForPerClusterOverride(t *testing.T) {
	file := writeConfig(t, `
count: 2
clusters:
  kind-ocp-1:
    profile: minimal
`)
	cfg, err := Resolve(Flags{ConfigPath: strPtr(file)}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ProfileFor("kind-ocp-1"); got != "minimal" {
		t.Errorf("cluster 1 profile = %q, want minimal", got)
	}
	if got := cfg.ProfileFor("kind-ocp-2"); got != "full" {
		t.Errorf("cluster 2 profile = %q, want full (default)", got)
	}
}

func TestResolveClusterOverrideScope(t *testing.T) {
	// An override for a cluster outside the current count is a create-only
	// problem: Resolve accepts it so scope commands (delete, list, verify,
	// kubeconfig) are never blocked by it; ValidateCreate rejects it.
	file := writeConfig(t, `
count: 2
clusters:
  kind-ocp-9:
    profile: minimal
`)
	cfg, err := Resolve(Flags{ConfigPath: strPtr(file)}, Env{})
	if err != nil {
		t.Fatalf("Resolve must accept the file (scope commands use it): %v", err)
	}
	if err := cfg.ValidateCreate(); err == nil {
		t.Fatal("expected ValidateCreate to reject the override of a cluster that is not generated")
	}

	file = writeConfig(t, `
count: 2
clusters:
  kind-ocp-2:
    profile: minimal
`)
	cfg, err = Resolve(Flags{ConfigPath: strPtr(file)}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateCreate(); err != nil {
		t.Errorf("ValidateCreate must accept an override of a generated cluster: %v", err)
	}
}

func TestIsGeneratedClusterName(t *testing.T) {
	cases := []struct {
		prefix, name string
		want         bool
	}{
		{"p", "p-1", true},
		{"p", "p-2", true},
		{"p", "p-10", true},           // multi-digit
		{"p", "p-3", true},            // exists even when count is smaller
		{"p", "p-0", false},           // zero is not a generated index
		{"p", "p-01", false},          // leading zero is not a generated index
		{"p", "p-1a", false},          // trailing garbage
		{"p", "p-a", false},           // not an integer
		{"p", "p-", false},            // empty index
		{"p", "p-1-2", false},         // compound index
		{"team", "team-dev-1", false}, // overlapping prefix, not ours
		{"team-dev", "team-dev-1", true},
		{"p", "px-1", false},
		{"p", "p", false},
	}
	for _, tc := range cases {
		if got := IsGeneratedClusterName(tc.prefix, tc.name); got != tc.want {
			t.Errorf("IsGeneratedClusterName(%q, %q) = %v, want %v", tc.prefix, tc.name, got, tc.want)
		}
	}
}

func TestValidatePrefix(t *testing.T) {
	for _, bad := range []string{"", "KindOcp", "kind-", "-kind", "kind_ocp", strings.Repeat("a", 41), "../escape"} {
		if err := ValidatePrefix(bad); err == nil {
			t.Errorf("ValidatePrefix(%q) = nil, want error", bad)
		}
	}
	for _, good := range []string{"k", "kind", "kind-ocp", "team-dev", strings.Repeat("a", 40)} {
		if err := ValidatePrefix(good); err != nil {
			t.Errorf("ValidatePrefix(%q) = %v, want nil", good, err)
		}
	}
}

// A custom profile shadowing a built-in name is a create-only problem:
// Resolve accepts it, ValidateCreate rejects it.
func TestReservedProfileNameFailsCreateOnly(t *testing.T) {
	file := writeConfig(t, `
profiles:
  full:
    layers: []
`)
	cfg, err := Resolve(Flags{ConfigPath: strPtr(file)}, Env{})
	if err != nil {
		t.Fatalf("Resolve must accept the file (scope commands use it): %v", err)
	}
	if err := cfg.ValidateCreate(); err == nil {
		t.Fatal("expected ValidateCreate to reject a custom profile colliding with a built-in name")
	}
}

func TestCloneIsIndependent(t *testing.T) {
	cfg, err := Resolve(Flags{}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Prefix = "p"
	cfg.Count = 3
	cfg.Clusters["p-1"] = ClusterOverride{Profile: "minimal"}
	cfg.Profiles["custom"] = Profile{Layers: []string{"z"}}
	cfg.Profiles["shared"] = Profile{Layers: []string{"x", "y"}}
	cfg.Layers["x"] = mustYAMLNode(t, "version: v1")
	cfg.Layers["nested"] = mustYAMLNode(t, "region: eu")

	clone := cfg.Clone()
	// Mutating the original after cloning must not affect the clone: not
	// by replacing map entries, and not by writing through the profile
	// slices and YAML node trees those entries point at.
	cfg.Prefix = "other"
	cfg.Count = 9
	cfg.Clusters["p-1"] = ClusterOverride{Profile: "full"}
	cfg.Clusters["p-99"] = ClusterOverride{Profile: "minimal"}
	cfg.Profiles["custom"] = Profile{Layers: []string{"z2"}}
	cfg.Profiles["shared"].Layers[0] = "mutated"
	cfg.Layers["x"] = mustYAMLNode(t, "version: v2")
	cfg.Layers["nested"].Content[0].Content[1].Value = "us"

	if clone.Prefix != "p" || clone.Count != 3 {
		t.Errorf("clone = (%q, %d), want the snapshot values (p, 3)", clone.Prefix, clone.Count)
	}
	if got := clone.Clusters["p-1"].Profile; got != "minimal" {
		t.Errorf("clone.Clusters[p-1].Profile = %q, want minimal", got)
	}
	if _, exists := clone.Clusters["p-99"]; exists {
		t.Error("clone shares the Clusters map with the original")
	}
	if got := clone.Profiles["custom"].Layers; len(got) != 1 || got[0] != "z" {
		t.Errorf("clone.Profiles[custom].Layers = %v, want [z]", got)
	}
	if got := clone.Profiles["shared"].Layers[0]; got != "x" {
		t.Errorf("clone shares a profile's Layers slice with the original: %q", got)
	}
	if got := mustYAMLDecode(t, clone.Layers["x"]); got != "v1" {
		t.Errorf("clone.Layers[x] = %q, want the snapshot value v1", got)
	}
	if got := mustYAMLDecodeRegion(t, clone.Layers["nested"]); got != "eu" {
		t.Errorf("clone shares the layer node tree with the original: %q", got)
	}
}

// mustYAMLDecodeRegion decodes a one-key region mapping, the way layer
// settings are consumed.
func mustYAMLDecodeRegion(t *testing.T, node yaml.Node) string {
	t.Helper()
	var s struct {
		Region string `yaml:"region"`
	}
	if err := node.Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s.Region
}

// YAML node graphs parsed from a config file are not trees: a
// self-referencing alias (`&x` on a mapping, `*x` inside it) parses into a
// cycle, one anchor aliased several times parses into shared nodes, and
// aliases in two layer entries share one target node. Clone must terminate
// on such graphs — the naive recursive copy exhausts the stack — must not
// share any node with the original, and must preserve cross-entry sharing:
// one cloned target, not a fresh copy of the target graph per entry.
func TestCloneHandlesAliasGraphs(t *testing.T) {
	file := writeConfig(t, `
layers:
  cyclic: &cyclic
    me: *cyclic
  shared-anchor: &shared-anchor
    first: &value
      version: v1
    second: *value
    third: *value
  alias-one: *shared-anchor
  alias-two: *shared-anchor
`)
	cfg, err := Resolve(Flags{ConfigPath: strPtr(file)}, Env{})
	if err != nil {
		t.Fatalf("Resolve must accept alias graphs in layer settings: %v", err)
	}

	original := collectLayerNodes(cfg.Layers)
	clone := cfg.Clone() // without memoization, this call never returns
	cloned := collectLayerNodes(clone.Layers)
	for n := range cloned {
		if original[n] {
			t.Fatal("the clone shares a YAML node with the original")
		}
	}

	// Aliases in two layer entries share one target node, so their clones
	// must share one cloned target rather than repeat the target graph
	// once per entry, and that target must be a new node, not the
	// original's.
	first, second := clone.Layers["alias-one"], clone.Layers["alias-two"]
	if first.Alias == nil || first.Alias != second.Alias {
		t.Fatalf("aliases in two layer entries must share one cloned target, got %p and %p", first.Alias, second.Alias)
	}
	if original[first.Alias] {
		t.Fatal("the shared cloned target is a node of the original")
	}

	// Ordinary aliases still resolve through the clone: every reference
	// to a shared anchor decodes to the anchor's content.
	var shared struct {
		First, Second, Third struct {
			Version string `yaml:"version"`
		}
	}
	sharedNode := clone.Layers["shared-anchor"]
	if err := sharedNode.Decode(&shared); err != nil {
		t.Fatal(err)
	}
	if shared.First.Version != "v1" || shared.Second.Version != "v1" || shared.Third.Version != "v1" {
		t.Errorf("references decoded through the clone = %+v, want v1 at every reference", shared)
	}

	// The shared cloned target carries the anchor's content, so an alias
	// entry still decodes to it through the clone.
	var viaAlias struct {
		First struct {
			Version string `yaml:"version"`
		}
	}
	aliasOne := clone.Layers["alias-one"]
	if err := aliasOne.Decode(&viaAlias); err != nil {
		t.Fatal(err)
	}
	if viaAlias.First.Version != "v1" {
		t.Errorf("alias entry decoded through the clone = %+v, want the anchor's content", viaAlias)
	}
}

// collectLayerNodes returns every node pointer reachable from the layer
// settings through Content and Alias. The walk is cycle-safe because alias
// graphs can be self-referencing. It starts at each map value's children:
// map values are per-map copies that two maps never share, so node sharing
// between an original and its clone can only appear below the roots.
func collectLayerNodes(ls map[string]yaml.Node) map[*yaml.Node]bool {
	seen := make(map[*yaml.Node]bool)
	var walk func(*yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil || seen[n] {
			return
		}
		seen[n] = true
		walk(n.Alias)
		for _, child := range n.Content {
			walk(child)
		}
	}
	for _, node := range ls {
		walk(node.Alias)
		for _, child := range node.Content {
			walk(child)
		}
	}
	return seen
}

// An explicitly empty --config disables config-file lookup entirely,
// KIND_OCP_CONFIG included. kind-ocp pins it in the commands it prints for
// humans to paste, so those commands cannot be broken by a losing config
// source in the paste environment.
func TestResolveExplicitEmptyConfigDisablesLookup(t *testing.T) {
	empty := ""
	cfg, err := Resolve(Flags{ConfigPath: &empty}, Env{ConfigPath: "/nonexistent/kind-ocp.yaml"})
	if err != nil {
		t.Fatalf("Resolve with an explicit empty --config: %v", err)
	}
	if cfg.Prefix != DefaultPrefix {
		t.Errorf("prefix = %q, want the default %q (no file may be read)", cfg.Prefix, DefaultPrefix)
	}

	// Without the flag, the environment path is still read and still fails.
	if _, err := Resolve(Flags{}, Env{ConfigPath: "/nonexistent/kind-ocp.yaml"}); err == nil {
		t.Fatal("expected the env config path to be read when no --config flag was passed")
	}
}

func mustYAMLNode(t *testing.T, content string) yaml.Node {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(content), &node); err != nil {
		t.Fatal(err)
	}
	return node
}

func mustYAMLDecode(t *testing.T, node yaml.Node) string {
	t.Helper()
	var s struct {
		Version string `yaml:"version"`
	}
	if err := node.Decode(&s); err != nil {
		t.Fatal(err)
	}
	return s.Version
}

func TestResolveCustomProfilesAndLayerSettings(t *testing.T) {
	file := writeConfig(t, `
profiles:
  olm-only:
    layers: [olm, node-labels]
layers:
  olm:
    version: v0.99.0
`)
	cfg, err := Resolve(Flags{ConfigPath: strPtr(file)}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cfg.Profiles["olm-only"]
	if !ok {
		t.Fatal("custom profile not parsed")
	}
	if len(p.Layers) != 2 || p.Layers[0] != "olm" || p.Layers[1] != "node-labels" {
		t.Errorf("custom profile layers = %v", p.Layers)
	}

	// The raw layer settings section must round-trip through layers.Env
	// (covered in the layers package tests); here we assert it is carried.
	if _, ok := cfg.Layers["olm"]; !ok {
		t.Fatal("layers.olm section not parsed")
	}
	node := cfg.Layers["olm"]
	var s struct {
		Version string `yaml:"version"`
	}
	if err := node.Decode(&s); err != nil {
		t.Fatal(err)
	}
	if s.Version != "v0.99.0" {
		t.Errorf("layer settings version = %q, want v0.99.0", s.Version)
	}
}

func TestDefaultStateRoot(t *testing.T) {
	t.Run("xdg", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "/tmp/xdg-state")
		root, err := DefaultStateRoot()
		if err != nil {
			t.Fatal(err)
		}
		if root != "/tmp/xdg-state/kind-ocp" {
			t.Errorf("root = %q", root)
		}
	})
	t.Run("home fallback", func(t *testing.T) {
		t.Setenv("XDG_STATE_HOME", "")
		t.Setenv("HOME", "/tmp/home")
		root, err := DefaultStateRoot()
		if err != nil {
			t.Fatal(err)
		}
		if root != "/tmp/home/.local/state/kind-ocp" {
			t.Errorf("root = %q", root)
		}
	})
}

func TestResolveTimeouts(t *testing.T) {
	file := writeConfig(t, "timeouts:\n  nodeReady: 2m\n")
	cfg, err := Resolve(Flags{ConfigPath: strPtr(file)}, Env{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Timeouts.NodeReady != 2*time.Minute {
		t.Errorf("nodeReady = %s, want 2m", cfg.Timeouts.NodeReady)
	}

	file = writeConfig(t, "timeouts:\n  nodeReady: bananas\n")
	if _, err := Resolve(Flags{ConfigPath: strPtr(file)}, Env{}); err == nil {
		t.Fatal("expected an error for an invalid duration")
	}
}

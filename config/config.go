// Package config defines kind-ocp's configuration: the on-disk file format,
// the built-in defaults (including every pinned external input), and the
// single resolution path that merges defaults, file, environment, and flags.
//
// Precedence, highest first (documented and unit-tested):
//
//  1. command-line flags
//  2. environment variables (KIND_OCP_*)
//  3. the config file passed via --config / KIND_OCP_CONFIG
//  4. built-in defaults
//
// Only the winning value of each key is parsed: a malformed KIND_OCP_COUNT is
// ignored when --count was passed, not an error. The state root has a single
// per-invocation override (--state-root); its default base is
// $XDG_STATE_HOME. --count and --profile are create inputs; the other
// commands accept and ignore them (the CLI does not even read them for
// scope commands, and Resolve checks only the prefix, so a create-only
// mistake never blocks cleanup).
package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// File is the on-disk YAML configuration. Zero values mean "not set"; the
// resolver overlays non-zero values onto the defaults. Count is a pointer so
// that an explicit `count: 0` is distinguishable from an omitted count (the
// former is a validation error, not a silent default).
type File struct {
	Prefix        string                     `yaml:"prefix"`
	Count         *int                       `yaml:"count"`
	Profile       string                     `yaml:"profile"`
	KindNodeImage string                     `yaml:"kindNodeImage"`
	StateRoot     string                     `yaml:"stateRoot"`
	Timeouts      Timeouts                   `yaml:"timeouts"`
	Clusters      map[string]ClusterOverride `yaml:"clusters"`
	Profiles      map[string]Profile         `yaml:"profiles"`
	Layers        map[string]yaml.Node       `yaml:"layers"`
}

// Timeouts carries wait timeouts. Durations are YAML strings ("5m"), not
// numbers, to match user expectations from kubectl and kind.
type Timeouts struct {
	NodeReady Duration `yaml:"nodeReady"`
}

// ClusterOverride overrides settings for one cluster, keyed by the full
// generated cluster name (for example "kind-ocp-1").
type ClusterOverride struct {
	Profile string `yaml:"profile"`
}

// Profile is a named, user-defined set of layers.
type Profile struct {
	Layers []string `yaml:"layers"`
}

// Config is the fully-resolved configuration for one kind-ocp invocation.
type Config struct {
	Prefix        string
	Count         int
	Profile       string
	KindNodeImage string
	StateRoot     string // absolute
	Timeouts      TimeoutsConfig
	Clusters      map[string]ClusterOverride
	Profiles      map[string]Profile
	Layers        map[string]yaml.Node
}

// TimeoutsConfig is the resolved form of Timeouts.
type TimeoutsConfig struct {
	NodeReady time.Duration
}

// Clone returns an independent copy of the resolved configuration: value
// fields are copied, the Clusters, Profiles, and Layers maps are duplicated,
// and nested values (each profile's Layers slice, each layer's YAML node
// graph) are deep-copied. A later mutation of the original cannot be observed
// through the clone. Consumers that hold a Config across mutations
// (kindocp.New snapshots its Config at construction) use this so changes to
// the caller's copy cannot re-scope an already-built client.
func (c *Config) Clone() *Config {
	clone := *c
	clone.Clusters = maps.Clone(c.Clusters)
	clone.Profiles = make(map[string]Profile, len(c.Profiles))
	for name, p := range c.Profiles {
		clone.Profiles[name] = Profile{Layers: slices.Clone(p.Layers)}
	}
	clone.Layers = make(map[string]yaml.Node, len(c.Layers))
	// One memo for all entries: yaml aliases can share a target across
	// layer entries (an anchored layer aliased by two others), and those
	// aliases must share one clone too, not repeat the target graph per
	// entry.
	memo := make(map[*yaml.Node]*yaml.Node)
	for name, node := range c.Layers {
		clone.Layers[name] = *cloneNode(memo, &node)
	}
	return &clone
}

// cloneNode returns a deep copy of a YAML node graph — the node itself
// plus, recursively, its Content children and Alias target — using the
// caller's memo, which spans every graph the caller copies. Callers share
// resolved layer settings as raw node graphs; without the deep copy, a
// mutation through one Config would be visible through its clone.
//
// Node graphs parsed from YAML are not trees: a self-referencing alias
// (`a: &x` with `b: *x` inside it) parses into a cycle, and one anchor
// aliased several times — possibly from several layer entries — parses
// into shared nodes. The copy therefore memoizes every source node it
// clones: recursion terminates on cycles, and a node reachable by several
// paths is copied once and stays shared in the copy. Every node in the
// copy is freshly allocated, so the copy never shares a node with the
// original.
func cloneNode(memo map[*yaml.Node]*yaml.Node, n *yaml.Node) *yaml.Node {
	if n == nil {
		return nil
	}
	if out, ok := memo[n]; ok {
		return out
	}
	out := *n
	memo[n] = &out
	out.Alias = cloneNode(memo, n.Alias)
	if len(n.Content) > 0 {
		out.Content = make([]*yaml.Node, len(n.Content))
		for i, child := range n.Content {
			out.Content[i] = cloneNode(memo, child)
		}
	}
	return &out
}

// ClusterNames returns the generated cluster names, <prefix>-1 through
// <prefix>-N. It is the one name generator: create plans with it, and
// verify's count assertion builds its expected name set with it too
// (on a Config carrying just the persisted expectation and the prefix),
// so the assertion can never drift from what create named the clusters.
// Callers pass a count IsCreatableCount accepted, which bounds the
// returned set by the cap.
func (c *Config) ClusterNames() []string {
	names := make([]string, 0, c.Count)
	for i := 1; i <= c.Count; i++ {
		names = append(names, fmt.Sprintf("%s-%d", c.Prefix, i))
	}
	return names
}

// IsCreatableCount reports whether count is one create can set out to
// build: the exact range ValidateCreate enforces (1 through the safety
// cap). Verify's count assertion classifies a persisted expectation
// with it, so a value no create could have written is treated as
// corrupt state — and never handed to name generation, whose sized
// allocation would follow a corrupt count. It is exported because the
// classification must be the same bound create validates with, and the
// two live in different packages of this module's public surface
// (config validates, kindocp classifies).
func IsCreatableCount(count int) bool {
	return count >= 1 && count <= maxCount
}

// IsGeneratedClusterName reports whether name is one this prefix generates:
// exactly <prefix>-<positive integer>, with no leading zeros. It is the single
// matcher behind delete, list, and name lookup, so scope operations never
// touch clusters a different prefix generated (deleting prefix "team" must
// not delete "team-dev-1") and never touch names kind-ocp did not generate.
func IsGeneratedClusterName(prefix, name string) bool {
	rest, ok := strings.CutPrefix(name, prefix+"-")
	if !ok || rest == "" {
		return false
	}
	if rest[0] < '1' || rest[0] > '9' {
		return false // "0", "01", and non-digits are not generated names
	}
	for i := 1; i < len(rest); i++ {
		if rest[i] < '0' || rest[i] > '9' {
			return false
		}
	}
	return true
}

// ProfileFor returns the effective profile for a cluster: the per-cluster
// override if set, otherwise the global default.
func (c *Config) ProfileFor(cluster string) string {
	if o, ok := c.Clusters[cluster]; ok && o.Profile != "" {
		return o.Profile
	}
	return c.Profile
}

// ValidateCreate checks the invariants only create needs: a usable count, a
// non-empty profile and node image, positive timeouts, custom profiles that
// do not shadow the built-in names, and per-cluster overrides that reference
// generated cluster names. The scope-only commands (delete, list, verify,
// kubeconfig) never call it, so a config file written for a different count
// or a create-only mistake never blocks cleanup: Resolve validates only the
// scope (the prefix) plus the structural file format, which every command
// shares.
func (c *Config) ValidateCreate() error {
	if !IsCreatableCount(c.Count) {
		return fmt.Errorf("count must be between 1 and %d, got %d", maxCount, c.Count)
	}
	if c.Profile == "" {
		return errors.New("profile must not be empty")
	}
	if c.KindNodeImage == "" {
		return errors.New("kindNodeImage must not be empty")
	}
	for name := range c.Profiles {
		if name == "full" || name == "minimal" {
			return fmt.Errorf("profiles entry %q collides with a built-in profile name", name)
		}
	}
	if c.Timeouts.NodeReady <= 0 {
		return fmt.Errorf("timeouts.nodeReady must be positive, got %s", c.Timeouts.NodeReady)
	}
	names := c.ClusterNames()
	valid := make(map[string]bool, len(names))
	for _, n := range names {
		valid[n] = true
	}
	for name := range c.Clusters {
		if !valid[name] {
			return fmt.Errorf("clusters override for unknown cluster %q (generated clusters: %s)",
				name, strings.Join(names, ", "))
		}
	}
	return nil
}

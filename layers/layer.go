// Package layers is kind-ocp's layer framework. A Layer is one discrete
// OpenShift-ness concern applied to a cluster (for example `olm` or
// `scc-admission`); profiles select sets of layers; the Registry holds the
// implementations.
//
// Adding a layer means implementing this interface and registering it — the
// provisioning loop never changes.
package layers

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"

	"github.com/stackrox/kind-ocp/config"
	"gopkg.in/yaml.v3"
)

// Layer is one discrete OpenShift-ness concern behind one interface.
// Implementations must be safe to construct once and use for many
// clusters.
type Layer interface {
	// Name is the stable identifier used in profiles and config. It
	// must be a safe state-tree name: lowercase ASCII letters and
	// digits with '-' inside, starting and ending alphanumeric
	// (internal/safename) — the shape of "openshift-api-crds". The name
	// becomes a cache-directory name for layers that fetch content, and
	// on a case-insensitive filesystem two names differing only by case
	// would be one directory, so kind-ocp refuses them at registration.
	Name() string
	// Apply converges the cluster towards the layer's desired state. It must
	// be idempotent: re-running against an already-applied layer succeeds.
	//
	// Apply may be called concurrently on one layer instance for
	// different clusters (kind-ocp applies clusters 2..N in one wave),
	// so a layer that keeps state across calls must guard it; env
	// carries everything that differs per cluster.
	Apply(ctx context.Context, env Env) error
	// Verify checks the layer's postconditions. It must not leave state
	// behind: a check that has to create transient state to prove its
	// postcondition (the scc-admission layer's probe pod) must remove it
	// within the check, and a cleanup failure is reported as an error,
	// never as a PASS. A non-nil error is reported as a FAIL with the
	// error as detail. Verify runs sequentially, one cluster at a time.
	Verify(ctx context.Context, env Env) error
}

// CacheDirFunc is the cache-directory service the framework binds to one
// layer. Calling it returns the walk-verified directory that layer may
// cache content it fetches from pinned upstream sources in, creating the
// directory when absent. The single argument is the layer's content pin
// ("" when the layer pins its content some other way or not at all):
// caches for different pins never share files.
//
// The pin must be a safe state-tree name — lowercase ASCII letters and
// digits with '-' inside, starting and ending alphanumeric
// (internal/safename), the shape of a git commit SHA: it becomes a
// directory name under the state root, and on a case-insensitive
// filesystem two pins differing only by case would be one directory.
// The service rejects such a pin instead of merging the caches; it
// never renames it.
type CacheDirFunc func(pin string) (string, error)

// Env is the execution environment handed to every layer. It carries the
// target cluster and the layer's own resolved configuration.
type Env struct {
	// ClusterName is the kind cluster name, for example "kind-ocp-2".
	ClusterName string
	// ContextName is the kubectl context, kind's rule: "kind-" + ClusterName.
	ContextName string
	// ClusterInstanceID is the identity of the cluster instance the
	// embedding validated for this cluster: the kube-system namespace
	// UID. kindocp sets it on the verify path, from the state record
	// whose binding to the live cluster the instance-id check has just
	// confirmed, so a layer whose Verify mutates the cluster (the
	// scc-admission probe pod) can refuse to run against a kubeconfig
	// that was replaced after that check — a same-name cluster
	// replacement. It is empty when the embedding provides no binding
	// to enforce (a direct library caller); Apply environments leave it
	// unset: the create path binds the record itself, before any layer
	// runs.
	ClusterInstanceID string
	// KubeconfigPath is the cluster-admin kubeconfig exported into the state
	// directory.
	KubeconfigPath string
	// Log is the cluster-scoped logger.
	Log *slog.Logger

	// CacheDir is the cache-directory service bound to the layer being
	// applied — the framework constructs it per layer, so a layer can
	// only ever reach its own cache directory, under the pin it passes.
	// kindocp supplies the state-root implementation, so a cache is
	// shared across the prefix's clusters, removed with the rest of the
	// prefix's state by delete, and inside kind-ocp's verified state
	// boundary.
	//
	// It is nil when the embedding provides no state root — a layer that
	// needs a cache then refuses to run rather than write somewhere
	// unverified — and it is always nil in the environments handed to
	// Verify: calling it creates a directory, and Verify leaves no state
	// behind (its one transient mutation, the scc-admission probe pod,
	// is removed within the check).
	CacheDir CacheDirFunc

	// LoadImage loads one image that exists on the local Docker daemon
	// into the target cluster's container runtime, so pods on the
	// cluster can run it without a registry. The framework binds it per
	// cluster — a layer that builds a local image loads it into the
	// cluster it is being applied to, and nothing else.
	//
	// It is nil when the embedding provides no image loading — the
	// framework binds it only when the provisioner implements the
	// optional image-loading capability, and a layer that needs it then
	// refuses to run rather than build an image nothing can run. It is
	// always nil in the environments handed to Verify: loading an image
	// mutates the cluster.
	LoadImage func(ctx context.Context, imageRef string) error

	// BuildImage builds one container image from a populated
	// build-context directory and tags it with the reference the caller
	// supplies, on the runtime the provisioner drives. A layer that
	// runs a locally built image (scc-admission) calls this with its
	// rendered context and then hands the same reference to LoadImage;
	// the build happens on the daemon, the load per cluster.
	//
	// It is nil when the embedding provides no image builder — the
	// framework binds it only when the provisioner implements the
	// optional image-building capability, and a layer that needs it
	// then refuses to run rather than invoke a container-runtime CLI
	// itself (the boundary must not assume Docker; a podman
	// implementation follows). It is always nil in the environments
	// handed to Verify: building an image is create-path work.
	BuildImage func(ctx context.Context, ref, contextDir string) error

	// Settings holds the raw per-layer configuration from the config file,
	// keyed by layer name. Layers consume their own section via
	// DecodeSettings.
	Settings map[string]yaml.Node
}

// DecodeSettings decodes the config-file section for the named layer into
// dst. dst should be pre-populated with the layer's defaults; only keys
// present in the file are overridden, and unknown keys are errors.
// A missing or null section leaves dst untouched.
func (e Env) DecodeSettings(layer string, dst any) error {
	node, ok := e.Settings[layer]
	if !ok || node.IsZero() || node.Tag == "!!null" {
		return nil
	}
	b, err := yaml.Marshal(&node)
	if err != nil {
		return fmt.Errorf("layer %s: re-encoding settings: %w", layer, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("layer %s: invalid settings: %w", layer, err)
	}
	return nil
}

// Built-in profile names.
const (
	// ProfileFull selects every registered layer, in registration order.
	ProfileFull = "full"
	// ProfileMinimal selects no layers: a plain kind cluster.
	ProfileMinimal = "minimal"
)

// LayerNamesForProfile resolves a profile name to the ordered list of layer
// names to apply. "full" and "minimal" are built in; any other name must be
// defined in the config file's profiles section. Custom profiles apply in
// registry registration order (dependency-safe), not in listed order.
func LayerNamesForProfile(reg *Registry, profile string, custom map[string]config.Profile) ([]string, error) {
	switch profile {
	case ProfileFull:
		return reg.Names(), nil
	case ProfileMinimal:
		return nil, nil
	}
	p, ok := custom[profile]
	if !ok {
		return nil, fmt.Errorf("unknown profile %q (built-in: %q, %q; others come from the config file's profiles section)",
			profile, ProfileFull, ProfileMinimal)
	}
	for _, name := range p.Layers {
		if _, ok := reg.Get(name); !ok {
			return nil, fmt.Errorf("profile %q references unknown layer %q", profile, name)
		}
	}
	var out []string
	for _, name := range reg.Names() {
		if contains(p.Layers, name) {
			out = append(out, name)
		}
	}
	if len(out) != len(p.Layers) {
		return nil, fmt.Errorf("profile %q lists a layer more than once", profile)
	}
	return out, nil
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

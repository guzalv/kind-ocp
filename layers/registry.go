package layers

import (
	"fmt"

	"github.com/guzalv/kind-ocp/internal/safename"
)

// Registry holds layer implementations in registration order, which is the
// apply order (later increments register layers in dependency-safe order;
// see docs/PLAN.md §3.2).
type Registry struct {
	order  []string
	byName map[string]Layer
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byName: map[string]Layer{}}
}

// Register adds a layer. Registration happens at process start (see
// internal/builtin); it is not safe for concurrent use.
//
// The name must be a safe state-tree name — lowercase ASCII letters and
// digits with '-' inside (internal/safename): the name becomes the
// layer's cache directory under the state root, and on a
// case-insensitive filesystem (macOS) two names differing only by case
// are one directory, so two registered layers would silently share
// cache files. Checking here fails a bad name at registration, before
// any layer runs; state.Store.CacheDirFor enforces the same rule at the
// directory mapping itself.
func (r *Registry) Register(l Layer) error {
	name := l.Name()
	if err := safename.Name(name); err != nil {
		return fmt.Errorf("layer name: %w", err)
	}
	if _, exists := r.byName[name]; exists {
		return fmt.Errorf("layer %q registered twice", name)
	}
	r.byName[name] = l
	r.order = append(r.order, name)
	return nil
}

// Get returns the layer registered under name.
func (r *Registry) Get(name string) (Layer, bool) {
	l, ok := r.byName[name]
	return l, ok
}

// Names returns all registered layer names in registration order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

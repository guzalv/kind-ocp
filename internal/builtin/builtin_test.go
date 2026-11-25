package builtin

import (
	"slices"
	"testing"

	"github.com/stackrox/kind-ocp/layers"
)

// The registry's names are the apply order for profile "full"; this test
// pins the order so every added layer extends it deliberately
// (docs/PLAN.md §3.2).
func TestRegistrationOrder(t *testing.T) {
	reg := NewRegistry()
	want := []string{"config-openshift", "node-labels", "olm", "openshift-api-crds", "platform-objects", "scc-admission"}
	if got := reg.Names(); !slices.Equal(got, want) {
		t.Errorf("builtin registry = %v, want %v", got, want)
	}
	for _, name := range want {
		if _, ok := reg.Get(name); !ok {
			t.Errorf("layer %q is not registered", name)
		}
	}
	if _, ok := reg.Get("no-such-layer"); ok {
		t.Error("Get(no-such-layer) reported found")
	}
	if _, err := layers.LayerNamesForProfile(reg, layers.ProfileFull, nil); err != nil {
		t.Errorf("resolving profile full: %v", err)
	}
}

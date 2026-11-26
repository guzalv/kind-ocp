// Package builtin registers every builtin layer, in dependency-safe
// apply order. This is the ONLY file that changes when a layer is added
// or removed; the provisioning loop in kindocp is never touched (AC-2.4).
//
// Registration order = apply order for the six builtins (docs/PLAN.md
// §3.2): config-openshift → node-labels → olm → openshift-api-crds →
// platform-objects → scc-admission. Rationale: OLM-created deployments
// need the master node label (node-labels before olm); the real
// openshift/api CRDs supersede the config-openshift placeholders
// (config-openshift before openshift-api-crds); SCCs only validate
// against the real SCC CRD (openshift-api-crds before
// platform-objects); the scc-admission webhook scopes itself to its
// configured namespaces, which it creates, and depends on no other
// layer — it is last by convention, not necessity.
package builtin

import (
	"github.com/guzalv/kind-ocp/internal/layers/configopenshift"
	"github.com/guzalv/kind-ocp/internal/layers/nodelabels"
	"github.com/guzalv/kind-ocp/internal/layers/olm"
	"github.com/guzalv/kind-ocp/internal/layers/openshiftapicrds"
	"github.com/guzalv/kind-ocp/internal/layers/platformobjects"
	"github.com/guzalv/kind-ocp/internal/layers/sccadmission"
	"github.com/guzalv/kind-ocp/layers"
)

// NewRegistry returns a registry with every builtin layer registered, in
// registration order.
func NewRegistry() *layers.Registry {
	reg := layers.NewRegistry()
	mustRegister(reg, configopenshift.Layer{})
	mustRegister(reg, nodelabels.Layer{})
	mustRegister(reg, olm.Layer{})
	mustRegister(reg, openshiftapicrds.Layer{})
	mustRegister(reg, platformobjects.Layer{})
	mustRegister(reg, sccadmission.Layer{})
	return reg
}

// mustRegister registers a builtin layer. It panics only on a duplicate
// name among the static registrations above — a programming error in
// this file, not a runtime condition.
func mustRegister(reg *layers.Registry, l layers.Layer) {
	if err := reg.Register(l); err != nil {
		panic(err)
	}
}

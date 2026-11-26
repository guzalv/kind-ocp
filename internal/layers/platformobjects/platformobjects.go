// Package platformobjects implements the platform-objects layer: the
// namespaces, config.openshift.io and operator.openshift.io singletons,
// the misc objects some ocp4-cis rules probe, the four control-plane
// ConfigMaps whose contents drive most ocp4-cis PASS/FAIL results, and
// the two real SecurityContextConstraints — everything the Compliance
// Operator's api-resource-collector needs to fetch so ocp4 platform
// checks evaluate instead of reporting NOT-APPLICABLE.
//
// Ported from the PoC's apply_platform_shim (stackrox PR #23841,
// assets/openshift-platform-shim). It registers after openshift-api-crds
// for two reasons: the singletons are objects of the real CRDs' kinds,
// which must be established before any of them is applied, and the SCCs
// only validate against the real security.openshift.io CRD — the
// config-openshift placeholders under the same names do not exist for
// the security group at all. A profile that selects this layer without
// openshift-api-crds fails loudly at the first operator.openshift.io or
// security.openshift.io object: the kinds are not served.
//
// The two assets are not equal citizens. objects.yaml is first-party
// content — hand-authored singletons describing a kind cluster's shape —
// with exactly two upstream-derived fields spliced between
// BEGIN/END UPSTREAM markers (the openshift-kube-apiserver/config
// apiServerArguments and the openshift-kube-controller-manager/config
// extendedArguments, real upstream operator defaults, so scanning them
// produces a realistic PASS/FAIL mix the way a freshly installed
// OpenShift cluster would). sccs.yaml is entirely upstream: the real
// restricted-v2 and privileged SCCs, verbatim. tools/regenerate rewrites
// exactly that upstream content from pinned upstream commits; everything
// else in either file is hand-maintained.
//
// Apply semantics, ported and corrected:
//
//   - The objects apply as one server-side apply batch in document
//     order, which puts the six namespaces first (a ported PoC rule:
//     kind needs them created explicitly before namespaced objects).
//   - Four singletons carry status in the asset. The PoC applied them
//     inline with `kubectl apply --server-side` and lost three of the
//     four: infrastructures, clusteroperators, and images serve a
//     status subresource, which drops status from a main-resource
//     apply. This layer applies each status through the subresource
//     after the main apply (the ported status-subresource gotcha). The
//     fourth, networks.config.openshift.io, is the real CRD's exception
//     — it defines status as a plain schema field and serves no
//     subresource — so its status travels in the main apply, and the
//     subresource write for it is refused with 404 by the apiserver;
//     Apply recognizes that refusal only for that one known kind
//     (statusTravelsInMainResource) and leaves the inline status the
//     batch already wrote. For any other kind a 404 is an error, not
//     convergence: the object or its subresource is missing, and
//     Verify catches a status that neither path populated.
//   - The SCCs install with create semantics, never an apply: the real
//     SecurityContextConstraints CRD requires twelve top-level fields
//     (RequiredSCCFields below). Five are nullable — priority,
//     allowedCapabilities, defaultAddCapabilities,
//     requiredDropCapabilities, and volumes — so they must stay
//     present even when null, and every update path that merges strips
//     explicit `key: null` fields, which then fails CRD validation. A
//     plain create sends the object as-is, nulls included. On a re-run
//     the create reports AlreadyExists, and the layer deletes and
//     re-creates: the only idempotent path that preserves the nulls.
//     Nothing holds a long-lived reference that this churns away — SCCs
//     are evaluated per pod admission, and a create re-run shares the
//     run with nothing that admits pods.
//   - The applies run under the layer's own field manager
//     (kube.LayerFieldManager), not kind-ocp's shared one. This layer
//     and config-openshift converge the same APIServer and
//     Infrastructure singletons — the bootstrap writes minimal specs,
//     this layer fills them in — and under one shared manager the
//     bootstrap's re-run would release this layer's fields (its
//     `spec: {}` APIServer re-apply emptied this layer's spec into a
//     value the real CRD rejects as null). Under per-layer managers each layer
//     owns the fields it applies, and this layer — applying later —
//     wins any shared field it also writes.
//
// The layer has no settings: everything it applies is embedded, and it
// consumes no external input there would be a reason to override
// (AC-4.3 covers pins; the pins here are build-time asset content, not
// runtime fetches). Verify's reads are bounded by a fixed readTimeout.
package platformobjects

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/guzalv/kind-ocp/assets"
	"github.com/guzalv/kind-ocp/internal/kube"
	"github.com/guzalv/kind-ocp/layers"
)

// Name is the layer's stable identifier, as referenced by profiles and
// the config file.
const Name = "platform-objects"

// sccGVR is the SecurityContextConstraints resource path, fixed by the
// security.openshift.io CRD the openshift-api-crds layer installs. The
// creates and deletes go straight to it — no discovery round — because
// the layer owns the CRD that defines the mapping.
var sccGVR = schema.GroupVersionResource{
	Group:    "security.openshift.io",
	Version:  "v1",
	Resource: "securitycontextconstraints",
}

// objectResources maps every (group, kind) the assets carry to the
// resource name its objects read through — Verify's fixed path table,
// in place of a discovery round per check (the layer depends on CRDs
// that fix these mappings). TestLoadAssets pins that it covers every
// object in both assets, so an asset change that introduces a new kind
// fails the build until the table — and Verify — grow with it.
var objectResources = map[schema.GroupKind]string{
	{Group: "", Kind: "Namespace"}:                                       "namespaces",
	{Group: "", Kind: "ConfigMap"}:                                       "configmaps",
	{Group: "apps", Kind: "DaemonSet"}:                                   "daemonsets",
	{Group: "rbac.authorization.k8s.io", Kind: "ClusterRole"}:            "clusterroles",
	{Group: "config.openshift.io", Kind: "APIServer"}:                    "apiservers",
	{Group: "config.openshift.io", Kind: "ClusterOperator"}:              "clusteroperators",
	{Group: "config.openshift.io", Kind: "Image"}:                        "images",
	{Group: "config.openshift.io", Kind: "Infrastructure"}:               "infrastructures",
	{Group: "config.openshift.io", Kind: "Network"}:                      "networks",
	{Group: "config.openshift.io", Kind: "OAuth"}:                        "oauths",
	{Group: "operator.openshift.io", Kind: "IngressController"}:          "ingresscontrollers",
	{Group: "operator.openshift.io", Kind: "KubeAPIServer"}:              "kubeapiservers",
	{Group: "operator.openshift.io", Kind: "Network"}:                    "networks",
	{Group: "operator.openshift.io", Kind: "OpenShiftAPIServer"}:         "openshiftapiservers",
	{Group: "security.openshift.io", Kind: "SecurityContextConstraints"}: "securitycontextconstraints",
}

// RequiredSCCFields are every top-level field the pinned
// security.openshift.io SecurityContextConstraints CRD marks required —
// in the CRD's own order. Five of them are nullable, so they must stay
// present even when null: that is the create-vs-apply gotcha. Verify
// reads the set off the live objects, so an SCC that lost a field (an
// apply-path merge stripping an explicit null, say) FAILs with the
// reason named; tools/regenerate checks the same set against the
// assembled asset, so an upstream manifest that drops one fails at
// regeneration time, not in every later create run.
// TestRequiredSCCFieldsAgainstPinnedCRD pins the list against the
// pinned openshift/api module's CRD, so a module bump that changes the
// required set fails the build, not a create run.
var RequiredSCCFields = []string{
	"allowHostDirVolumePlugin",
	"allowHostIPC",
	"allowHostNetwork",
	"allowHostPID",
	"allowHostPorts",
	"allowPrivilegedContainer",
	"allowedCapabilities",
	"defaultAddCapabilities",
	"priority",
	"readOnlyRootFilesystem",
	"requiredDropCapabilities",
	"volumes",
}

// readTimeout bounds Verify's reads.
const readTimeout = 2 * time.Minute

// Layer applies the OpenShift platform objects. It is stateless;
// construct once.
type Layer struct{}

// Name returns "platform-objects".
func (Layer) Name() string { return Name }

// Apply converges the cluster to the layer's desired state: server-side
// apply the objects asset in document order (namespaces first), write
// the four status-bearing singletons' status — through the status
// subresource where the kind serves one, carried by the main apply
// where it does not — then install the two SCCs with create semantics,
// deleting and re-creating an existing one so the nulls the CRD
// requires survive. Re-running converges: the applies are no-ops
// against identical content, and the SCC delete+recreate is the
// null-preserving idempotent path.
func (l Layer) Apply(ctx context.Context, env layers.Env) error {
	objects, sccs, err := loadAssets()
	if err != nil {
		return err
	}
	client, err := kube.NewClient(env.KubeconfigPath)
	if err != nil {
		return fmt.Errorf("building Kubernetes clients: %w", err)
	}

	env.Log.Info("applying the platform objects",
		"objects", len(objects), "sccs", len(sccs))
	manager := kube.LayerFieldManager(Name)
	if err := client.Apply(ctx, manager, objects); err != nil {
		return fmt.Errorf("applying the platform objects: %w", err)
	}
	for _, obj := range objects {
		if _, carriesStatus := obj.Object["status"]; !carriesStatus {
			continue
		}
		if err := client.ApplyStatus(ctx, manager, statusObject(obj)); err != nil {
			if apierrors.IsNotFound(err) && statusTravelsInMainResource(obj) {
				// The one kind known to serve no status subresource —
				// the real networks.config.openshift.io CRD — so the
				// status cannot go through one; the main apply above
				// already carried it as a plain field. Any status
				// Verify finds missing is a real failure it reports.
				env.Log.Info("status travels in the main resource for this kind; no status subresource to write",
					"kind", obj.GetKind(), "name", obj.GetName())
				continue
			}
			return fmt.Errorf("writing the %s/%s status: %w", obj.GetKind(), obj.GetName(), err)
		}
	}

	recreated, err := applySCCs(ctx, client, sccs)
	if err != nil {
		return err
	}
	env.Log.Info("installed the SecurityContextConstraints",
		"sccs", len(sccs), "recreated", recreated)
	return nil
}

// applySCCs installs the SCCs with create semantics. A create that
// reports AlreadyExists means the object is there — from this layer's
// own earlier run or a foreign owner — and the idempotent path that
// preserves the CRD-required nulls is delete and re-create, never an
// update: every update path that merges strips the explicit nulls and
// then fails CRD validation (the ported PoC gotcha). It reports how
// many SCCs it re-created.
func applySCCs(ctx context.Context, client *kube.Client, sccs []*unstructured.Unstructured) (recreated int, err error) {
	for _, scc := range sccs {
		name := scc.GetName()
		if _, err := client.Dynamic.Resource(sccGVR).Create(ctx, scc, metav1.CreateOptions{}); err == nil {
			continue
		} else if !apierrors.IsAlreadyExists(err) {
			return recreated, fmt.Errorf("creating the SecurityContextConstraints %s: %w", name, err)
		}
		if err := client.Dynamic.Resource(sccGVR).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return recreated, fmt.Errorf("deleting the SecurityContextConstraints %s for re-create: %w", name, err)
		}
		// SCCs carry no finalizers, so the delete completes before it
		// returns and the create cannot race a lingering object; a
		// contrary result is an error to surface, not to mask.
		if _, err := client.Dynamic.Resource(sccGVR).Create(ctx, scc, metav1.CreateOptions{}); err != nil {
			return recreated, fmt.Errorf("re-creating the SecurityContextConstraints %s after its delete: %w (does the live object carry a finalizer?)", name, err)
		}
		recreated++
	}
	return recreated, nil
}

// Verify checks the layer's postconditions and mutates nothing: every
// object in the assets is readable, the four ConfigMaps carry the data
// keys they apply, the status-bearing singletons carry a populated
// status, and both SCCs carry the fields the CRD requires even when
// null. The reads are bounded by readTimeout.
func (l Layer) Verify(ctx context.Context, env layers.Env) error {
	return l.verify(ctx, env, readTimeout)
}

// verify is Verify with an explicit read budget. Verify passes the
// production bound (readTimeout); the bounded-read regression test
// drives this helper with a short budget, which is why the budget is a
// parameter and not a setting.
func (l Layer) verify(ctx context.Context, env layers.Env, readBudget time.Duration) error {
	objects, sccs, err := loadAssets()
	if err != nil {
		return err
	}
	client, err := kube.NewClient(env.KubeconfigPath)
	if err != nil {
		return fmt.Errorf("building Kubernetes clients: %w", err)
	}
	// The reads run under a deadline context: the caller's context can
	// be unbounded (the CLI passes context.Background), so an apiserver
	// that wedges a response must not outlive the budget.
	readCtx, cancel := context.WithTimeout(ctx, readBudget)
	defer cancel()
	all := make([]*unstructured.Unstructured, 0, len(objects)+len(sccs))
	all = append(all, objects...)
	all = append(all, sccs...)
	var problems []string
	for _, obj := range all {
		if problem := checkObject(readCtx, client, obj); problem != "" {
			problems = append(problems, problem)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%d of %d platform objects failed their check: %s (was the %s layer applied?)",
			len(problems), len(objects)+len(sccs), strings.Join(problems, "; "), Name)
	}
	return nil
}

// checkObject reads one asset object live and returns "" when it passes
// its checks, or the failure named. Every object must be readable; a
// ConfigMap must carry every data key the asset gives it; an object the
// asset gives a status must carry a non-empty live status; an SCC must
// carry the null-tolerant required fields.
func checkObject(ctx context.Context, client *kube.Client, obj *unstructured.Unstructured) string {
	ref := objectRef(obj)
	gvr, err := gvrFor(obj)
	if err != nil {
		return fmt.Sprintf("%s: %v", ref, err)
	}
	resource := client.Dynamic.Resource(gvr)
	var ri dynamic.ResourceInterface = resource
	if ns := obj.GetNamespace(); ns != "" {
		ri = resource.Namespace(ns)
	}
	live, err := ri.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if err != nil {
		return fmt.Sprintf("%s: %v", ref, err)
	}
	if _, carriesStatus := obj.Object["status"]; carriesStatus {
		if status, found, _ := unstructured.NestedMap(live.Object, "status"); !found || len(status) == 0 {
			return fmt.Sprintf("%s carries no populated status: the status write did not happen or was lost — re-run create", ref)
		}
	}
	if obj.GetKind() == "ConfigMap" {
		data, found, _ := unstructured.NestedMap(live.Object, "data")
		if !found {
			return fmt.Sprintf("%s carries no data", ref)
		}
		wantData, _, _ := unstructured.NestedMap(obj.Object, "data")
		for key := range wantData {
			got, found := data[key]
			gotString, isString := got.(string)
			if !found || !isString || gotString == "" {
				return fmt.Sprintf("%s is missing its %q data key", ref, key)
			}
		}
	}
	if obj.GetKind() == "SecurityContextConstraints" {
		for _, field := range RequiredSCCFields {
			if _, present := live.Object[field]; !present {
				return fmt.Sprintf("%s is missing its %q field, which the CRD requires — an update path may have stripped an explicit null; re-run create to delete and re-create it", ref, field)
			}
		}
	}
	return ""
}

// gvrFor resolves one asset object's resource path through the fixed
// kind table — the same mapping a REST mapper would give, without a
// discovery round per object.
func gvrFor(obj *unstructured.Unstructured) (schema.GroupVersionResource, error) {
	gvk := obj.GroupVersionKind()
	resource, ok := objectResources[gvk.GroupKind()]
	if !ok {
		return schema.GroupVersionResource{}, fmt.Errorf("no resource path known for %s %s", gvk.Kind, objectRef(obj))
	}
	return gvk.GroupVersion().WithResource(resource), nil
}

// statusTravelsInMainResource reports whether one asset object's kind
// is known to serve no status subresource: the real
// networks.config.openshift.io CRD, which defines status as a plain
// schema field. The 404 its status-subresource write returns is
// expected, and the main apply has already carried the status. For
// every other kind a 404 on that write stays an error — the object or
// the subresource is missing, and mistaking that for convergence is
// exactly what Verify exists to catch.
func statusTravelsInMainResource(obj *unstructured.Unstructured) bool {
	gvk := obj.GroupVersionKind()
	return gvk.Group == "config.openshift.io" && gvk.Kind == "Network"
}

// statusObject builds the status-only apply for one object: the
// subresource ignores every other field, so callers send status-only
// objects (the same shape kube.ApplyStatus documents).
func statusObject(obj *unstructured.Unstructured) *unstructured.Unstructured {
	metadata := map[string]any{"name": obj.GetName()}
	if ns := obj.GetNamespace(); ns != "" {
		metadata["namespace"] = ns
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": obj.GetAPIVersion(),
		"kind":       obj.GetKind(),
		"metadata":   metadata,
		"status":     obj.Object["status"],
	}}
}

// loadAssets decodes the layer's embedded manifests. Their structural
// contract — the object set, the namespaces-first order, the
// status-bearing singletons, the ConfigMap data keys, the SCCs with
// their null-tolerant required fields — is fixed by the embedded bytes
// and the regeneration tool's shape validation, not runtime input, so
// TestLoadAssets pins it against the decoded output: an accidental
// asset change fails the build, not a create run.
func loadAssets() (objects, sccs []*unstructured.Unstructured, err error) {
	objects, err = kube.DecodeManifests(strings.NewReader(assets.PlatformObjectsObjects))
	if err != nil {
		return nil, nil, fmt.Errorf("decoding the platform-objects asset: %w", err)
	}
	sccs, err = kube.DecodeManifests(strings.NewReader(assets.PlatformObjectsSCCs))
	if err != nil {
		return nil, nil, fmt.Errorf("decoding the SecurityContextConstraints asset: %w", err)
	}
	return objects, sccs, nil
}

// objectRef renders an object reference for error messages:
// "kind namespace/name" for namespaced objects, "kind name" otherwise.
func objectRef(obj *unstructured.Unstructured) string {
	if ns := obj.GetNamespace(); ns != "" {
		return obj.GetKind() + " " + ns + "/" + obj.GetName()
	}
	return obj.GetKind() + " " + obj.GetName()
}

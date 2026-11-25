package kube

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
)

// FieldManager is the prefix of the field managers kind-ocp's layers
// server-side apply under (LayerFieldManager appends the layer's name).
// There is no default manager: every apply supplies its own, because
// the manager is the caller's ownership identity, not a client
// property. Force is set on every apply so a re-run after a partial
// failure re-acquires any field another manager took over, instead of
// failing on a conflict; on a fresh cluster there is nothing to
// conflict with.
const FieldManager = "kind-ocp"

// LayerFieldManager returns the field manager one layer's applies run
// under: FieldManager + "/" + the layer's name. Layers apply under
// their own manager because two layers may legitimately converge the
// same object — config-openshift bootstraps minimal config.openshift.io
// singletons that platform-objects later fills in — and a manager's
// re-apply releases only the fields that manager itself previously
// owned. Under one shared manager, the earlier layer's re-run would
// strip the later layer's fields from the object (config-openshift's
// spec-less APIServer re-apply emptied platform-objects' spec, which
// the real CRD then rejected as spec: null); under per-layer managers
// each layer owns exactly the fields it applies, and the last layer to
// write a shared field still wins (force).
func LayerFieldManager(layer string) string {
	return FieldManager + "/" + layer
}

// Client bundles the Kubernetes clients needed to converge one cluster:
// the typed clientset for reads and waits, the dynamic client for
// server-side apply, and discovery for REST mapping and group
// availability. NewClient builds it from a state kubeconfig; the fields
// are interfaces so tests can substitute fakes.
type Client struct {
	Clientset kubernetes.Interface
	Dynamic   dynamic.Interface
	Discovery discovery.DiscoveryInterface
}

// NewClient builds a Client for a kubeconfig file path. The kubeconfig
// passes the same privacy verification as every other kubeconfig load
// (RESTConfig): kind-ocp does not authenticate with a shared or
// restored credential.
func NewClient(kubeconfigPath string) (*Client, error) {
	cfg, err := RESTConfig(kubeconfigPath)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("building discovery client: %w", err)
	}
	return &Client{Clientset: cs, Dynamic: dyn, Discovery: disc}, nil
}

// NewNoRedirectClientFromConfig builds a Client for an already-loaded
// REST config — the one-load counterpart of NewClient — whose every
// client, the typed clientset, the dynamic client, and discovery,
// shares one http.Client whose transport refuses redirects. It exists
// for the caller that must send some request exactly once (a create
// under a name it will have to resolve the outcome of) while its
// reads, deletes, and applies keep talking to the same cluster: one
// load builds one client set, so there is no second load to point part
// of the work at a replacement cluster, and no separate client whose
// construction could drift from the rest — the exactly-once request
// borrows the clientset's own CoreV1 REST client (the same generated
// construction the typed create goes through), with retries disabled
// on that one request by the caller. No current requirement needs any
// of these requests to follow a redirect, and refusing one is loud,
// not lossy: the refusal names the original request and the status,
// never the target (a redirect Location can carry credentials in its
// userinfo or query). See refuseRedirects for why the refusal belongs
// in the transport, and noRedirectHTTPClient for the transport build
// itself.
func NewNoRedirectClientFromConfig(cfg *rest.Config) (*Client, error) {
	httpClient, err := noRedirectHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("building clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("building dynamic client: %w", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("building discovery client: %w", err)
	}
	return &Client{Clientset: cs, Dynamic: dyn, Discovery: disc}, nil
}

// Apply server-side applies objects in the given order, under the field
// manager the caller supplies (its ownership identity — see
// LayerFieldManager). Server-side apply — not client-side — is required
// for the OLM CRDs: the ClusterServiceVersion CRD is too large for the
// client-side last-applied-configuration annotation (a ported PoC
// gotcha), and apply-configurations for schema-less custom resources do
// not exist, so the dynamic client's apply is the one apply path that
// covers everything kind-ocp installs.
//
// Each object must carry apiVersion, kind, and metadata.name, and a
// namespaced object must carry its namespace; the caller supplies the
// dependencies (a manifest's namespaces precede the objects in them).
// An empty object list is an error, not a no-op: kind-ocp applies what
// it fetched, and an empty list means the manifest or release layout
// changed under it.
//
// Apply sets no deadline of its own: the discovery read that builds the
// REST mapper and the apply requests run under the caller's context.
// Callers that need a bound pass one; the waits that follow an apply
// (rollouts, establishment) carry their own timeouts.
func (c *Client) Apply(ctx context.Context, fieldManager string, objs []*unstructured.Unstructured) error {
	if len(objs) == 0 {
		return errors.New("applying: no objects")
	}
	if fieldManager == "" {
		return errors.New("applying: no field manager")
	}
	mapper, err := c.restMapper(ctx)
	if err != nil {
		return err
	}
	for _, obj := range objs {
		if err := applyObject(ctx, c.Dynamic, mapper, fieldManager, obj); err != nil {
			return err
		}
	}
	return nil
}

// ApplyStatus server-side applies one object's status through the
// status subresource, under the field manager the caller supplies and
// with the same force semantics as Apply (a ported PoC gotcha: a CRD
// with the status subresource enabled ignores status writes on the main
// resource, so status must go through the subresource).
//
// The object must satisfy the same identification rules as an Apply
// object and carry the status to converge to; the subresource ignores
// every other field, so callers send status-only objects. The kind's
// CRD must already exist, be established, and enable the status
// subresource — otherwise the request fails.
//
// ApplyStatus sets no deadline of its own, like Apply.
func (c *Client) ApplyStatus(ctx context.Context, fieldManager string, obj *unstructured.Unstructured) error {
	if obj == nil {
		return errors.New("applying status: no object")
	}
	if fieldManager == "" {
		return errors.New("applying status: no field manager")
	}
	mapper, err := c.restMapper(ctx)
	if err != nil {
		return err
	}
	return applyStatusObject(ctx, c.Dynamic, mapper, fieldManager, obj)
}

// CreateAbsentCRD creates one CustomResourceDefinition only when no CRD
// of its name exists yet, reporting whether it created one. An existing
// CRD is left exactly as it is: unlike Apply there is no converge and
// no force, so this never overwrites or fights over a CRD something
// else may have installed under the same name. That is the bootstrap
// semantics a shim layer needs — a later layer (or a foreign owner)
// may install a better CRD under the name, and re-running the shim
// must not replace it with the bootstrap content.
//
// The CRD resource path is fixed by the API (crdGVR), so the create
// needs no discovery round: a batch of placeholder CRDs costs one POST
// each, not a REST-mapper rebuild per object. The object must be a
// CustomResourceDefinition carrying metadata.name. Like Apply,
// CreateAbsentCRD sets no deadline of its own.
func (c *Client) CreateAbsentCRD(ctx context.Context, crd *unstructured.Unstructured) (bool, error) {
	if crd == nil {
		return false, errors.New("creating CRD: no object")
	}
	if crd.GetKind() != "CustomResourceDefinition" {
		return false, fmt.Errorf("creating CRD: object is a %s, not a CustomResourceDefinition", crd.GetKind())
	}
	if crd.GetName() == "" {
		return false, errors.New("creating CRD: no metadata.name")
	}
	if _, err := c.Dynamic.Resource(crdGVR).Create(ctx, crd, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, fmt.Errorf("creating CRD %s: %w", crd.GetName(), err)
	}
	return true, nil
}

// restMapper builds a discovery-backed REST mapper for one call. A fresh
// mapper per call is the simple correctness rule: applying is how new
// kinds (CRDs) get installed, so a cached mapper would not see the kinds
// an earlier call in the same run installed. Discovery is a handful of
// requests against a kind-scale apiserver.
func (c *Client) restMapper(ctx context.Context) (meta.RESTMapper, error) {
	groupResources, err := restmapper.GetAPIGroupResourcesWithContext(
		ctx, discovery.ToDiscoveryInterfaceWithContext(c.Discovery))
	if err != nil {
		return nil, fmt.Errorf("discovering the server's API resources: %w", err)
	}
	return restmapper.NewDiscoveryRESTMapper(groupResources), nil
}

func applyObject(ctx context.Context, dyn dynamic.Interface, mapper meta.RESTMapper, fieldManager string, obj *unstructured.Unstructured) error {
	ri, err := resourceInterfaceFor(dyn, mapper, obj)
	if err != nil {
		return err
	}
	if _, err := ri.Apply(ctx, obj.GetName(), obj, metav1.ApplyOptions{
		FieldManager: fieldManager,
		Force:        true,
	}); err != nil {
		return fmt.Errorf("applying %s %s: %w", obj.GetKind(), objectRef(obj), err)
	}
	return nil
}

func applyStatusObject(ctx context.Context, dyn dynamic.Interface, mapper meta.RESTMapper, fieldManager string, obj *unstructured.Unstructured) error {
	ri, err := resourceInterfaceFor(dyn, mapper, obj)
	if err != nil {
		return err
	}
	if _, err := ri.ApplyStatus(ctx, obj.GetName(), obj, metav1.ApplyOptions{
		FieldManager: fieldManager,
		Force:        true,
	}); err != nil {
		return fmt.Errorf("applying %s %s status: %w", obj.GetKind(), objectRef(obj), err)
	}
	return nil
}

// resourceInterfaceFor resolves obj's REST mapping and returns the
// dynamic resource interface to act on it with: namespace-qualified
// when the kind is namespaced. Both apply paths (main resource and
// status subresource) resolve objects the same way.
func resourceInterfaceFor(dyn dynamic.Interface, mapper meta.RESTMapper, obj *unstructured.Unstructured) (dynamic.ResourceInterface, error) {
	kind, name := obj.GetKind(), obj.GetName()
	if obj.GetAPIVersion() == "" || kind == "" {
		return nil, fmt.Errorf("applying object %q: no apiVersion/kind", objectRef(obj))
	}
	if name == "" {
		return nil, fmt.Errorf("applying %s: no metadata.name", kind)
	}
	gvk := obj.GroupVersionKind()
	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("mapping %s %q to an API resource: %w (is the CRD defining the kind installed?)",
			kind, name, err)
	}
	resource := dyn.Resource(mapping.Resource)
	if mapping.Scope == meta.RESTScopeNamespace {
		if ns := obj.GetNamespace(); ns != "" {
			return resource.Namespace(ns), nil
		}
		return nil, fmt.Errorf("applying %s %q: namespaced object has no namespace", kind, name)
	}
	return resource, nil
}

// objectRef renders an object reference for error messages:
// "namespace/name" for namespaced objects, "name" otherwise.
func objectRef(obj *unstructured.Unstructured) string {
	if ns := obj.GetNamespace(); ns != "" {
		return ns + "/" + obj.GetName()
	}
	return obj.GetName()
}

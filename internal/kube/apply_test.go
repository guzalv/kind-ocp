package kube

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	k8stesting "k8s.io/client-go/testing"
)

// appliedCall is one server-side apply the recording client saw.
type appliedCall struct {
	gvr  schema.GroupVersionResource
	ns   string
	name string
	obj  *unstructured.Unstructured
	opts metav1.ApplyOptions
}

// createdCall is one create the recording client saw.
type createdCall struct {
	gvr  schema.GroupVersionResource
	name string
	obj  *unstructured.Unstructured
}

// recordingDynamic records Apply and Create calls. It implements only
// what Client.Apply and Client.CreateAbsentCRD use; unimplemented
// ResourceInterface methods panic via the nil embedded interface.
type recordingDynamic struct {
	dynamic.Interface
	applies       []appliedCall
	statusApplies []appliedCall
	creates       []createdCall
	createErr     error // when set, every Create fails with it
}

func (d *recordingDynamic) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	return &recordingResource{dyn: d, gvr: gvr}
}

type recordingResource struct {
	dynamic.ResourceInterface // unimplemented methods panic; Apply and Namespace are overridden
	dyn                       *recordingDynamic
	gvr                       schema.GroupVersionResource
	ns                        string
}

func (r *recordingResource) Namespace(ns string) dynamic.ResourceInterface {
	return &recordingResource{dyn: r.dyn, gvr: r.gvr, ns: ns}
}

func (r *recordingResource) Apply(_ context.Context, name string, obj *unstructured.Unstructured, opts metav1.ApplyOptions, _ ...string) (*unstructured.Unstructured, error) {
	r.dyn.applies = append(r.dyn.applies, appliedCall{gvr: r.gvr, ns: r.ns, name: name, obj: obj, opts: opts})
	return obj, nil
}

func (r *recordingResource) Create(_ context.Context, obj *unstructured.Unstructured, _ metav1.CreateOptions, _ ...string) (*unstructured.Unstructured, error) {
	if r.dyn.createErr != nil {
		return nil, r.dyn.createErr
	}
	r.dyn.creates = append(r.dyn.creates, createdCall{gvr: r.gvr, name: obj.GetName(), obj: obj})
	return obj, nil
}

func (r *recordingResource) ApplyStatus(_ context.Context, name string, obj *unstructured.Unstructured, opts metav1.ApplyOptions) (*unstructured.Unstructured, error) {
	r.dyn.statusApplies = append(r.dyn.statusApplies, appliedCall{gvr: r.gvr, ns: r.ns, name: name, obj: obj, opts: opts})
	return obj, nil
}

// testDiscovery serves a small API surface: core namespaces (cluster-
// scoped), apps deployments (namespaced), rbac cluster roles (cluster-
// scoped), apiextensions CRDs, and the config.openshift.io placeholder
// resources.
func testDiscovery() *fakediscovery.FakeDiscovery {
	return &fakediscovery.FakeDiscovery{Fake: &k8stesting.Fake{
		Resources: []*metav1.APIResourceList{
			{GroupVersion: "v1", APIResources: []metav1.APIResource{
				{Name: "namespaces", Kind: "Namespace", Namespaced: false},
			}},
			{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
				{Name: "deployments", Kind: "Deployment", Namespaced: true},
			}},
			{GroupVersion: "rbac.authorization.k8s.io/v1", APIResources: []metav1.APIResource{
				{Name: "clusterroles", Kind: "ClusterRole", Namespaced: false},
			}},
			{GroupVersion: "apiextensions.k8s.io/v1", APIResources: []metav1.APIResource{
				{Name: "customresourcedefinitions", Kind: "CustomResourceDefinition", Namespaced: false},
			}},
			{GroupVersion: "config.openshift.io/v1", APIResources: []metav1.APIResource{
				{Name: "apiservers", Kind: "APIServer", Namespaced: false},
				{Name: "infrastructures", Kind: "Infrastructure", Namespaced: false},
				{Name: "clusterversions", Kind: "ClusterVersion", Namespaced: false},
			}},
		},
	}}
}

// applyClient builds a Client over the recording dynamic client and the
// test discovery surface.
func applyClient() (*Client, *recordingDynamic) {
	dyn := &recordingDynamic{}
	return &Client{Dynamic: dyn, Discovery: testDiscovery()}, dyn
}

func mustDecode(t *testing.T, manifest string) []*unstructured.Unstructured {
	t.Helper()
	objs, err := DecodeManifests(strings.NewReader(manifest))
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

func TestApply(t *testing.T) {
	t.Run("applies objects in order with scope, manager, and force", func(t *testing.T) {
		client, dyn := applyClient()
		objs := mustDecode(t, `
apiVersion: v1
kind: Namespace
metadata:
  name: olm
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: olm-operator
  namespace: olm
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: system:controller:olm
`)
		if err := client.Apply(context.Background(), LayerFieldManager("some-layer"), objs); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if len(dyn.applies) != 3 {
			t.Fatalf("applies = %d, want 3", len(dyn.applies))
		}

		want := []struct {
			gvr  schema.GroupVersionResource
			ns   string
			name string
		}{
			{schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}, "", "olm"},
			{schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}, "olm", "olm-operator"},
			{schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}, "", "system:controller:olm"},
		}
		for i, w := range want {
			got := dyn.applies[i]
			if got.gvr != w.gvr || got.ns != w.ns || got.name != w.name {
				t.Errorf("apply %d = %s %s/%s, want %s %s/%s", i, got.gvr, got.ns, got.name, w.gvr, w.ns, w.name)
			}
			if got.obj != objs[i] {
				t.Errorf("apply %d sent a different object than the input", i)
			}
			// The manager the caller supplies is the one every apply
			// runs under — there is no default to fall back to.
			if got.opts.FieldManager != LayerFieldManager("some-layer") || !got.opts.Force {
				t.Errorf("apply %d options = %+v, want field manager %q with force", i, got.opts, LayerFieldManager("some-layer"))
			}
		}
	})

	t.Run("no objects is an error", func(t *testing.T) {
		client, dyn := applyClient()
		if err := client.Apply(context.Background(), FieldManager, nil); err == nil ||
			!strings.Contains(err.Error(), "no objects") {
			t.Fatalf("Apply(nil) = %v, want a no-objects error", err)
		}
		if len(dyn.applies) != 0 {
			t.Errorf("no applies may be issued, got %d", len(dyn.applies))
		}
	})

	t.Run("no field manager is an error", func(t *testing.T) {
		client, dyn := applyClient()
		objs := mustDecode(t, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: olm\n")
		if err := client.Apply(context.Background(), "", objs); err == nil ||
			!strings.Contains(err.Error(), "no field manager") {
			t.Fatalf("Apply(no manager) = %v, want a no-field-manager error", err)
		}
		if len(dyn.applies) != 0 {
			t.Errorf("no applies may be issued, got %d", len(dyn.applies))
		}
	})

	t.Run("missing apiVersion/kind is an error", func(t *testing.T) {
		client, _ := applyClient()
		obj := &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "anonymous"},
		}}
		err := client.Apply(context.Background(), FieldManager, []*unstructured.Unstructured{obj})
		if err == nil || !strings.Contains(err.Error(), "no apiVersion/kind") {
			t.Fatalf("Apply(kindless) = %v, want a kind error", err)
		}
	})

	t.Run("missing name is an error", func(t *testing.T) {
		client, _ := applyClient()
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Namespace",
		}}
		err := client.Apply(context.Background(), FieldManager, []*unstructured.Unstructured{obj})
		if err == nil || !strings.Contains(err.Error(), "no metadata.name") {
			t.Fatalf("Apply(nameless) = %v, want a name error", err)
		}
	})

	t.Run("namespaced object without namespace is an error", func(t *testing.T) {
		client, dyn := applyClient()
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata":   map[string]any{"name": "olm-operator"},
		}}
		err := client.Apply(context.Background(), FieldManager, []*unstructured.Unstructured{obj})
		if err == nil || !strings.Contains(err.Error(), "no namespace") {
			t.Fatalf("Apply(namespaceless) = %v, want a namespace error", err)
		}
		if len(dyn.applies) != 0 {
			t.Errorf("no applies may be issued, got %d", len(dyn.applies))
		}
	})

	t.Run("unknown kind names the CRD prerequisite", func(t *testing.T) {
		client, dyn := applyClient()
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "operators.coreos.com/v1alpha1",
			"kind":       "CatalogSource",
			"metadata":   map[string]any{"name": "operatorhubio", "namespace": "olm"},
		}}
		err := client.Apply(context.Background(), FieldManager, []*unstructured.Unstructured{obj})
		if err == nil || !strings.Contains(err.Error(), "CRD defining the kind") {
			t.Fatalf("Apply(unknown kind) = %v, want the CRD hint", err)
		}
		if len(dyn.applies) != 0 {
			t.Errorf("no applies may be issued, got %d", len(dyn.applies))
		}
	})

	t.Run("discovery failure is an error", func(t *testing.T) {
		disc := testDiscovery()
		disc.PrependReactor("get", "group", func(_ k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errDiscoveryDown
		})
		client := &Client{Dynamic: &recordingDynamic{}, Discovery: disc}
		objs := mustDecode(t, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: olm\n")
		err := client.Apply(context.Background(), FieldManager, objs)
		if err == nil || !strings.Contains(err.Error(), "discovering") {
			t.Fatalf("Apply(discovery down) = %v, want a discovery error", err)
		}
	})
}

func TestApplyStatus(t *testing.T) {
	infrastructureStatus := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "config.openshift.io/v1",
			"kind":       "Infrastructure",
			"metadata":   map[string]any{"name": "cluster"},
			"status":     map[string]any{"platformStatus": map[string]any{"type": "None"}},
		}}
	}

	t.Run("applies the status with scope, manager, and force", func(t *testing.T) {
		client, dyn := applyClient()
		obj := infrastructureStatus()
		if err := client.ApplyStatus(context.Background(), FieldManager, obj); err != nil {
			t.Fatalf("ApplyStatus: %v", err)
		}
		if len(dyn.statusApplies) != 1 {
			t.Fatalf("status applies = %d, want 1", len(dyn.statusApplies))
		}
		if len(dyn.applies) != 0 {
			t.Errorf("ApplyStatus issued %d main-resource applies, want 0", len(dyn.applies))
		}
		got := dyn.statusApplies[0]
		wantGVR := schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: "infrastructures"}
		if got.gvr != wantGVR || got.ns != "" || got.name != "cluster" {
			t.Errorf("status apply = %s %s/%s, want %s cluster", got.gvr, got.ns, got.name, wantGVR)
		}
		if got.obj != obj {
			t.Errorf("status apply sent a different object than the input")
		}
		if got.opts.FieldManager != FieldManager || !got.opts.Force {
			t.Errorf("status apply options = %+v, want field manager %q with force", got.opts, FieldManager)
		}
	})

	t.Run("no object is an error", func(t *testing.T) {
		client, dyn := applyClient()
		if err := client.ApplyStatus(context.Background(), FieldManager, nil); err == nil ||
			!strings.Contains(err.Error(), "no object") {
			t.Fatalf("ApplyStatus(nil) = %v, want a no-object error", err)
		}
		if len(dyn.statusApplies) != 0 {
			t.Errorf("no applies may be issued, got %d", len(dyn.statusApplies))
		}
	})

	t.Run("missing name is an error", func(t *testing.T) {
		client, dyn := applyClient()
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "config.openshift.io/v1",
			"kind":       "Infrastructure",
		}}
		err := client.ApplyStatus(context.Background(), FieldManager, obj)
		if err == nil || !strings.Contains(err.Error(), "no metadata.name") {
			t.Fatalf("ApplyStatus(nameless) = %v, want a name error", err)
		}
		if len(dyn.statusApplies) != 0 {
			t.Errorf("no applies may be issued, got %d", len(dyn.statusApplies))
		}
	})

	t.Run("namespaced object without namespace is an error", func(t *testing.T) {
		client, dyn := applyClient()
		obj := mustDecode(t, "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: olm-operator\n")[0]
		err := client.ApplyStatus(context.Background(), FieldManager, obj)
		if err == nil || !strings.Contains(err.Error(), "no namespace") {
			t.Fatalf("ApplyStatus(namespaceless) = %v, want a namespace error", err)
		}
		if len(dyn.statusApplies) != 0 {
			t.Errorf("no applies may be issued, got %d", len(dyn.statusApplies))
		}
	})

	t.Run("unknown kind names the CRD prerequisite", func(t *testing.T) {
		client, dyn := applyClient()
		obj := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "config.openshift.io/v1",
			"kind":       "Ingress",
			"metadata":   map[string]any{"name": "cluster"},
		}}
		err := client.ApplyStatus(context.Background(), FieldManager, obj)
		if err == nil || !strings.Contains(err.Error(), "CRD defining the kind") {
			t.Fatalf("ApplyStatus(unknown kind) = %v, want the CRD hint", err)
		}
		if len(dyn.statusApplies) != 0 {
			t.Errorf("no applies may be issued, got %d", len(dyn.statusApplies))
		}
	})
}

var errDiscoveryDown = errors.New("apiserver unreachable")

func TestCreateAbsentCRD(t *testing.T) {
	crd := mustDecode(t, `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: infrastructures.config.openshift.io
`)[0]
	wantGVR := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}

	// A client with no discovery at all: the create path must not need
	// it — the CRD resource path is fixed by the API, so a batch of
	// bootstrap CRDs costs one POST each, not a mapper per object.
	client := func(dyn *recordingDynamic) *Client {
		return &Client{Dynamic: dyn}
	}

	t.Run("creates at the fixed CRD resource path, no discovery", func(t *testing.T) {
		dyn := &recordingDynamic{}
		created, err := client(dyn).CreateAbsentCRD(context.Background(), crd)
		if err != nil {
			t.Fatalf("CreateAbsentCRD: %v", err)
		}
		if !created {
			t.Error("created = false, want true")
		}
		if len(dyn.creates) != 1 {
			t.Fatalf("creates = %d, want 1", len(dyn.creates))
		}
		got := dyn.creates[0]
		if got.gvr != wantGVR || got.name != "infrastructures.config.openshift.io" || got.obj != crd {
			t.Errorf("create = %s %s (obj %p), want %s infrastructures.config.openshift.io with the input object",
				got.gvr, got.name, got.obj, wantGVR)
		}
		if len(dyn.applies) != 0 {
			t.Errorf("CreateAbsentCRD issued %d applies, want 0 (it creates; it never converges)", len(dyn.applies))
		}
	})

	t.Run("AlreadyExists reports false and succeeds", func(t *testing.T) {
		dyn := &recordingDynamic{
			createErr: apierrors.NewAlreadyExists(
				schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"},
				"infrastructures.config.openshift.io"),
		}
		created, err := client(dyn).CreateAbsentCRD(context.Background(), crd)
		if err != nil {
			t.Fatalf("CreateAbsentCRD(AlreadyExists) = %v, want nil — an existing CRD is skipped, not an error", err)
		}
		if created {
			t.Error("created = true on AlreadyExists, want false")
		}
	})

	t.Run("any other create error is wrapped with the CRD named", func(t *testing.T) {
		dyn := &recordingDynamic{createErr: errDiscoveryDown}
		_, err := client(dyn).CreateAbsentCRD(context.Background(), crd)
		if err == nil || !strings.Contains(err.Error(), "infrastructures.config.openshift.io") || !errors.Is(err, errDiscoveryDown) {
			t.Fatalf("CreateAbsentCRD(other error) = %v, want it wrapped and naming the CRD", err)
		}
	})

	t.Run("guards: nil, wrong kind, missing name", func(t *testing.T) {
		dyn := &recordingDynamic{}
		if _, err := client(dyn).CreateAbsentCRD(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "no object") {
			t.Errorf("CreateAbsentCRD(nil) = %v, want a no-object error", err)
		}
		notACRD := mustDecode(t, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: x\n")[0]
		if _, err := client(dyn).CreateAbsentCRD(context.Background(), notACRD); err == nil || !strings.Contains(err.Error(), "not a CustomResourceDefinition") {
			t.Errorf("CreateAbsentCRD(Namespace) = %v, want a kind error", err)
		}
		nameless := mustDecode(t, "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n")[0]
		if _, err := client(dyn).CreateAbsentCRD(context.Background(), nameless); err == nil || !strings.Contains(err.Error(), "no metadata.name") {
			t.Errorf("CreateAbsentCRD(nameless) = %v, want a name error", err)
		}
		if len(dyn.creates) != 0 {
			t.Errorf("guards issued %d creates, want 0", len(dyn.creates))
		}
	})
}

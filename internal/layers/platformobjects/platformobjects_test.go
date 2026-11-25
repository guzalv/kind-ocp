package platformobjects

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/stackrox/kind-ocp/internal/kube"
	"github.com/stackrox/kind-ocp/internal/layers/openshiftapicrds"
	"github.com/stackrox/kind-ocp/layers"
)

// The ported object set, pinned. These are the exact contents reviewed
// in the PoC (stackrox PR #23841) with the two upstream fields
// regenerated at its pinned commits; if an edit changes the set, the
// change must be deliberate — revisit the layer docs with it.
var wantObjectRefs = []string{
	"Namespace openshift-kube-apiserver",
	"Namespace openshift-apiserver",
	"Namespace openshift-kube-controller-manager",
	"Namespace openshift-etcd",
	"Namespace openshift-sdn",
	"Namespace openshift-ingress-operator",
	"APIServer cluster",
	"Infrastructure cluster",
	"Network cluster",
	"OAuth cluster",
	"Image cluster",
	"ClusterOperator kube-apiserver",
	"Network cluster",
	"KubeAPIServer cluster",
	"OpenShiftAPIServer cluster",
	"IngressController openshift-ingress-operator/default",
	"ClusterRole cluster-debugger",
	"DaemonSet openshift-sdn/sdn",
	"ConfigMap openshift-kube-apiserver/config",
	"ConfigMap openshift-apiserver/config",
	"ConfigMap openshift-kube-controller-manager/config",
	"ConfigMap openshift-etcd/etcd-pod",
}

// wantSCCRefs is the SCC asset's object set: the two real SCCs, in the
// fixed order the regeneration tool writes them.
var wantSCCRefs = []string{"SecurityContextConstraints restricted-v2", "SecurityContextConstraints privileged"}

// wantStatusRefs is the set of objects the asset gives a status to.
var wantStatusRefs = []string{"Infrastructure cluster", "Network cluster", "Image cluster", "ClusterOperator kube-apiserver"}

func TestLayerName(t *testing.T) {
	l := Layer{}
	if got := l.Name(); got != Name {
		t.Errorf("Name() = %q, want %q", got, Name)
	}
	if Name != "platform-objects" {
		t.Errorf("Name = %q, want the documented layer name platform-objects", Name)
	}
}

// loadAssets is the production decode path; pinning its output pins the
// embedded assets themselves.
func TestLoadAssets(t *testing.T) {
	objects, sccs, err := loadAssets()
	if err != nil {
		t.Fatalf("loadAssets: %v", err)
	}

	t.Run("decodes the ported objects in document order", func(t *testing.T) {
		if len(objects) != len(wantObjectRefs) {
			t.Fatalf("decoded %d objects, want %d (%v)", len(objects), len(wantObjectRefs), refs(objects))
		}
		if !slices.Equal(refs(objects), wantObjectRefs) {
			t.Errorf("object refs = %v, want %v in document order", refs(objects), wantObjectRefs)
		}
	})

	t.Run("decodes the two real SCCs", func(t *testing.T) {
		if !slices.Equal(refs(sccs), wantSCCRefs) {
			t.Errorf("SCC refs = %v, want %v", refs(sccs), wantSCCRefs)
		}
		for _, scc := range sccs {
			if scc.GetAPIVersion() != "security.openshift.io/v1" {
				t.Errorf("SCC %s apiVersion = %q, want security.openshift.io/v1", scc.GetName(), scc.GetAPIVersion())
			}
			// Every field the pinned CRD marks required: present as
			// keys in the asset, whatever their value, because the
			// create must send them as-is — the nullable ones even
			// when null.
			for _, field := range RequiredSCCFields {
				if _, present := scc.Object[field]; !present {
					t.Errorf("SCC %s carries no %q field; the real CRD requires it", scc.GetName(), field)
				}
			}
		}
	})

	t.Run("namespaces precede every object that lives in them", func(t *testing.T) {
		seen := map[string]bool{}
		for _, obj := range objects {
			if obj.GetKind() == "Namespace" {
				seen[obj.GetName()] = true
				continue
			}
			if ns := obj.GetNamespace(); ns != "" && !seen[ns] {
				t.Errorf("%s references namespace %q that no earlier Namespace document created", objectRef(obj), ns)
			}
		}
	})

	t.Run("exactly the four status-bearing singletons carry a status", func(t *testing.T) {
		var got []string
		for _, obj := range objects {
			if _, has := obj.Object["status"]; has {
				got = append(got, objectRef(obj))
			}
		}
		slices.Sort(got)
		want := slices.Clone(wantStatusRefs)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("status-bearing objects = %v, want %v", got, want)
		}
	})

	t.Run("the ConfigMaps carry the data keys the checks read", func(t *testing.T) {
		wantDataKeys := map[string]string{
			"ConfigMap openshift-kube-apiserver/config":          "config.yaml",
			"ConfigMap openshift-apiserver/config":               "config.yaml",
			"ConfigMap openshift-kube-controller-manager/config": "config.yaml",
			"ConfigMap openshift-etcd/etcd-pod":                  "pod.yaml",
		}
		for _, obj := range objects {
			if obj.GetKind() != "ConfigMap" {
				continue
			}
			want := wantDataKeys[objectRef(obj)]
			if want == "" {
				t.Errorf("unexpected ConfigMap %s in the asset", objectRef(obj))
				continue
			}
			data, found, _ := unstructured.NestedMap(obj.Object, "data")
			if !found || len(data) == 0 {
				t.Errorf("%s carries no data", objectRef(obj))
				continue
			}
			value, found := data[want]
			s, isString := value.(string)
			if !found || !isString || s == "" {
				t.Errorf("%s carries no non-empty %q data key", objectRef(obj), want)
			}
		}
	})

	t.Run("the DaemonSet is the deliberate zero-pod object", func(t *testing.T) {
		var daemonSet *unstructured.Unstructured
		for _, obj := range objects {
			if obj.GetKind() == "DaemonSet" {
				daemonSet = obj
			}
		}
		if daemonSet == nil {
			t.Fatal("the asset carries no DaemonSet")
		}
		selector, found, _ := unstructured.NestedMap(daemonSet.Object, "spec", "template", "spec", "nodeSelector")
		if !found || len(selector) == 0 {
			t.Fatalf("DaemonSet carries no nodeSelector; its zero-pod guarantee is the point of the object")
		}
		if value, _, _ := unstructured.NestedString(daemonSet.Object, "spec", "template", "spec", "nodeSelector", "node-role.kubernetes.io/platform-shim-unscheduled"); value != "true" {
			t.Errorf("DaemonSet nodeSelector = %v, want the platform-shim-unscheduled label no kind node carries", selector)
		}
	})

	t.Run("every object's kind resolves through the verify path table", func(t *testing.T) {
		for _, obj := range append(slices.Clone(objects), sccs...) {
			if _, err := gvrFor(obj); err != nil {
				t.Errorf("object %s: %v (a new kind in the asset needs its resource in objectResources, or Verify cannot read it)", objectRef(obj), err)
			}
		}
	})
}

func TestGVRForUnknownKind(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "example.com/v1", "kind": "Widget",
		"metadata": map[string]any{"name": "w"},
	}}
	if _, err := gvrFor(obj); err == nil || !strings.Contains(err.Error(), "no resource path known") {
		t.Errorf("gvrFor(unknown kind) = %v, want a no-resource-path error", err)
	}
}

// TestRequiredSCCFieldsAgainstPinnedCRD pins RequiredSCCFields against
// the pinned openshift/api module's SecurityContextConstraints CRD —
// the module in the local cache, the same resolution the
// openshiftapicrds pin tests use. The list must be exactly the CRD's
// v1 required set, in the CRD's own order: a module bump that adds,
// removes, or reorders a required key fails here, at build time,
// instead of as a create-run CRD validation failure — or, worse, a
// field nothing checks.
func TestRequiredSCCFieldsAgainstPinnedCRD(t *testing.T) {
	var manifest string
	for _, src := range openshiftapicrds.CRDSources {
		if src.Name == "securitycontextconstraints.security.openshift.io" {
			manifest = src.Path
		}
	}
	if manifest == "" {
		t.Fatal("openshiftapicrds.CRDSources does not list the SecurityContextConstraints CRD")
	}
	dir, err := goListModuleDir("github.com/openshift/api")
	if err != nil {
		t.Fatalf("resolving the pinned openshift/api module: %v (is the module cache warm? it is, if the build ran)", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(manifest)))
	if err != nil {
		t.Fatalf("reading the pinned CRD manifest %s: %v (module layout changed?)", manifest, err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Name   string `yaml:"name"`
				Schema struct {
					OpenAPIV3Schema struct {
						Required []string `yaml:"required"`
					} `yaml:"openAPIV3Schema"`
				} `yaml:"schema"`
			} `yaml:"versions"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(b, &crd); err != nil {
		t.Fatalf("decoding the pinned CRD manifest: %v", err)
	}
	var required []string
	for _, version := range crd.Spec.Versions {
		if version.Name == "v1" {
			required = version.Schema.OpenAPIV3Schema.Required
		}
	}
	if required == nil {
		t.Fatal("the pinned SCC CRD carries no v1 version with a required list")
	}
	if !slices.Equal(required, RequiredSCCFields) {
		t.Errorf("RequiredSCCFields =\n%v\nwant the pinned CRD's v1 required set\n%v\n— update the list with the module bump", RequiredSCCFields, required)
	}
}

// goListModuleDir resolves one module's directory in the local module
// cache with `go list -m`. It runs from inside this module and needs
// no network: the module is in the build graph, so it is already
// downloaded.
func goListModuleDir(path string) (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", path).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go list -m %s: %v: %s", path, err, strings.TrimSpace(string(out)))
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", fmt.Errorf("go list -m recorded no directory for %s; the module is not in the local cache", path)
	}
	return dir, nil
}

func TestStatusObject(t *testing.T) {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "config.openshift.io/v1",
		"kind":       "Infrastructure",
		"metadata":   map[string]any{"name": "cluster"},
		"spec":       map[string]any{"platformSpec": map[string]any{"type": "None"}},
		"status":     map[string]any{"platform": "None"},
	}}
	status := statusObject(obj)
	if status.GetAPIVersion() != "config.openshift.io/v1" || status.GetKind() != "Infrastructure" || status.GetName() != "cluster" {
		t.Fatalf("status object = %s %s/%s, want the identity of its source", status.GetAPIVersion(), status.GetKind(), status.GetName())
	}
	if _, found := status.Object["spec"]; found {
		t.Error("status object carries a spec; the subresource write must stay status-only")
	}
	if _, found := status.Object["status"]; !found {
		t.Error("status object carries no status")
	}

	namespaced := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "operator.openshift.io/v1",
		"kind":       "IngressController",
		"metadata":   map[string]any{"name": "default", "namespace": "openshift-ingress-operator"},
		"status":     map[string]any{"domains": []any{}}},
	}
	if got := statusObject(namespaced).GetNamespace(); got != "openshift-ingress-operator" {
		t.Errorf("status object namespace = %q, want the source object's", got)
	}
}

// fakeAPIServer is an in-memory apiserver carrying the API surface the
// layer touches. It stores what applies and creates write, serves the
// stores on get, refuses creates of existing names with 409
// AlreadyExists (the re-run the layer's SCC semantics must handle), and
// models the one real-CRD fact the status writes depend on:
// config.openshift.io networks serves no status subresource, so a
// status patch on it is a 404, exactly as the pinned openshift/api CRD
// behaves.
type fakeAPIServer struct {
	server *httptest.Server

	mu             sync.Mutex
	store          map[string]*unstructured.Unstructured // object path → object
	calls          []string                              // "METHOD path[?query]" of every object request, in order
	sccCreates     int
	sccDeletes     int
	failSCCCreate  bool   // when set, SCC creates fail with a 500
	statusNotFound string // when set, this object key's status subresource answers 404 like a kind that serves none
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	api := &fakeAPIServer{store: map[string]*unstructured.Unstructured{}}
	mux := http.NewServeMux()

	// Discovery: the groups and resources the layer touches, with the
	// scoping the real APIs carry (the REST mapper derives it).
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		api.write(w, `{"kind":"APIVersions","versions":["v1"],
		"serverAddressByClientCIDRs":[{"clientCIDR":"0.0.0.0/0","serverAddress":"127.0.0.1"}]}`)
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, r *http.Request) {
		api.write(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[
		{"name":"apps","versions":[{"groupVersion":"apps/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apps/v1","version":"v1"}},
		{"name":"config.openshift.io","versions":[{"groupVersion":"config.openshift.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"config.openshift.io/v1","version":"v1"}},
		{"name":"operator.openshift.io","versions":[{"groupVersion":"operator.openshift.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"operator.openshift.io/v1","version":"v1"}},
		{"name":"rbac.authorization.k8s.io","versions":[{"groupVersion":"rbac.authorization.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"rbac.authorization.k8s.io/v1","version":"v1"}},
		{"name":"security.openshift.io","versions":[{"groupVersion":"security.openshift.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"security.openshift.io/v1","version":"v1"}}]}`)
	})
	mux.HandleFunc("/api/v1", func(w http.ResponseWriter, r *http.Request) {
		api.write(w, `{"kind":"APIResourceList","groupVersion":"v1","resources":[
		{"name":"configmaps","singularName":"configmap","namespaced":true,"kind":"ConfigMap","verbs":["get","list","create","patch","delete"]},
		{"name":"namespaces","singularName":"namespace","namespaced":false,"kind":"Namespace","verbs":["get","list","create","patch","delete"]}]}`)
	})
	groupResources := map[string]string{
		"/apis/apps/v1": `{"kind":"APIResourceList","groupVersion":"apps/v1","resources":[
		{"name":"daemonsets","singularName":"daemonset","namespaced":true,"kind":"DaemonSet","verbs":["get","list","create","patch","delete"]}]}`,
		"/apis/config.openshift.io/v1": `{"kind":"APIResourceList","groupVersion":"config.openshift.io/v1","resources":[
		{"name":"apiservers","singularName":"apiserver","namespaced":false,"kind":"APIServer","verbs":["get","patch"]},
		{"name":"clusteroperators","singularName":"clusteroperator","namespaced":false,"kind":"ClusterOperator","verbs":["get","patch"]},
		{"name":"images","singularName":"image","namespaced":false,"kind":"Image","verbs":["get","patch"]},
		{"name":"infrastructures","singularName":"infrastructure","namespaced":false,"kind":"Infrastructure","verbs":["get","patch"]},
		{"name":"networks","singularName":"network","namespaced":false,"kind":"Network","verbs":["get","patch"]},
		{"name":"oauths","singularName":"oauth","namespaced":false,"kind":"OAuth","verbs":["get","patch"]}]}`,
		"/apis/operator.openshift.io/v1": `{"kind":"APIResourceList","groupVersion":"operator.openshift.io/v1","resources":[
		{"name":"ingresscontrollers","singularName":"ingresscontroller","namespaced":true,"kind":"IngressController","verbs":["get","patch"]},
		{"name":"kubeapiservers","singularName":"kubeapiserver","namespaced":false,"kind":"KubeAPIServer","verbs":["get","patch"]},
		{"name":"networks","singularName":"network","namespaced":false,"kind":"Network","verbs":["get","patch"]},
		{"name":"openshiftapiservers","singularName":"openshiftapiserver","namespaced":false,"kind":"OpenShiftAPIServer","verbs":["get","patch"]}]}`,
		"/apis/rbac.authorization.k8s.io/v1": `{"kind":"APIResourceList","groupVersion":"rbac.authorization.k8s.io/v1","resources":[
		{"name":"clusterroles","singularName":"clusterrole","namespaced":false,"kind":"ClusterRole","verbs":["get","patch"]}]}`,
		"/apis/security.openshift.io/v1": `{"kind":"APIResourceList","groupVersion":"security.openshift.io/v1","resources":[
		{"name":"securitycontextconstraints","singularName":"securitycontextconstraint","namespaced":false,"kind":"SecurityContextConstraints","verbs":["get","create","patch","delete"]}]}`,
	}
	mux.HandleFunc("/apis/", func(w http.ResponseWriter, r *http.Request) {
		if body, ok := groupResources[r.URL.Path]; ok {
			api.write(w, body)
			return
		}
		api.handleObject(w, r)
	})
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		api.handleObject(w, r)
	})

	api.server = httptest.NewServer(mux)
	t.Cleanup(api.server.Close)
	return api
}

// handleObject serves one object request against the store.
func (a *fakeAPIServer) handleObject(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.calls = append(a.calls, r.Method+" "+r.URL.RequestURI())
	a.mu.Unlock()

	body := func() *unstructured.Unstructured {
		var obj unstructured.Unstructured
		if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
			http.Error(w, fmt.Sprintf("decoding request body: %v", err), http.StatusBadRequest)
			return nil
		}
		return &obj
	}
	isSCC := strings.Contains(r.URL.Path, "/securitycontextconstraints")

	switch r.Method {
	case http.MethodPatch:
		obj := body()
		if obj == nil {
			return
		}
		key, subresource := objectPathKey(r.URL.Path)
		a.mu.Lock()
		defer a.mu.Unlock()
		if subresource == "status" {
			// The one real-CRD fact the status writes depend on: the
			// config.openshift.io networks CRD serves no status
			// subresource, so the subresource path does not exist.
			// statusNotFound simulates the same 404 for another kind,
			// which the layer must treat as an error, not the
			// no-subresource exception.
			if key == "/apis/config.openshift.io/v1/networks/cluster" || key == a.statusNotFound {
				a.notFound(w, "the server could not find the requested resource")
				return
			}
			stored, ok := a.store[key]
			if !ok {
				a.notFound(w, "no such object")
				return
			}
			stored.Object["status"] = obj.Object["status"]
			a.write(w, rawJSON(stored))
			return
		}
		a.store[key] = obj
		a.write(w, rawJSON(obj))
	case http.MethodPost:
		obj := body()
		if obj == nil {
			return
		}
		key := strings.TrimSuffix(r.URL.Path, "/") + "/" + obj.GetName()
		a.mu.Lock()
		defer a.mu.Unlock()
		if isSCC {
			if a.failSCCCreate {
				http.Error(w, "injected failure", http.StatusInternalServerError)
				return
			}
			a.sccCreates++
		}
		if _, exists := a.store[key]; exists {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"AlreadyExists","code":409,
				"details":{"name":%q},"message":"securitycontextconstraints %q already exists"}`, obj.GetName(), obj.GetName())
			return
		}
		a.store[key] = obj
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		a.write(w, rawJSON(obj))
	case http.MethodDelete:
		key, _ := objectPathKey(r.URL.Path)
		a.mu.Lock()
		defer a.mu.Unlock()
		if _, exists := a.store[key]; !exists {
			a.notFound(w, "no such object")
			return
		}
		delete(a.store, key)
		if isSCC {
			a.sccDeletes++
		}
		a.write(w, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
	case http.MethodGet:
		key, _ := objectPathKey(r.URL.Path)
		a.mu.Lock()
		defer a.mu.Unlock()
		stored, ok := a.store[key]
		if !ok {
			a.notFound(w, "no such object")
			return
		}
		a.write(w, rawJSON(stored))
	default:
		http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)
	}
}

func (a *fakeAPIServer) notFound(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404,"message":%q}`, message)
}

func (a *fakeAPIServer) write(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// rawJSON renders a stored object back as the JSON the apiserver would.
func rawJSON(obj *unstructured.Unstructured) string {
	b, err := obj.MarshalJSON()
	if err != nil {
		return "{}"
	}
	return string(b)
}

// objectPathKey converts a request path to the store key of the object
// it addresses (the subresource stripped) plus the subresource, if any.
// An object's path is its collection root plus either [name] or
// [namespaces, ns, resource, name]; one segment beyond that is a
// subresource.
func objectPathKey(path string) (key, subresource string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	plain, namespaced := 5, 7 // /apis/group/version/resource/name and its namespaced form
	if parts[0] == "api" {
		plain, namespaced = 4, 6 // /api/version/resource/name and its namespaced form
	}
	switch len(parts) {
	case plain, namespaced:
		return "/" + strings.Join(parts, "/"), ""
	case plain + 1, namespaced + 1:
		return "/" + strings.Join(parts[:len(parts)-1], "/"), parts[len(parts)-1]
	}
	return "/" + strings.Join(parts, "/"), ""
}

// calls returns the recorded object requests in order.
func (a *fakeAPIServer) objectCalls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.calls)
}

// stored returns the object stored at key, if any.
func (a *fakeAPIServer) stored(key string) (*unstructured.Unstructured, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	obj, ok := a.store[key]
	return obj, ok
}

// seed stores an object as though a previous owner had created it.
func (a *fakeAPIServer) seed(key string, obj *unstructured.Unstructured) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.store[key] = obj
}

// writeKubeconfig writes a kubeconfig pointing at serverURL, the same
// shape the state store exports.
func writeKubeconfig(t *testing.T, serverURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: fake
  cluster:
    server: %s
contexts:
- name: fake
  context:
    cluster: fake
    user: fake
current-context: fake
users:
- name: fake
  user: {}
`, serverURL)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func testEnv(kubeconfig string) layers.Env {
	return layers.Env{KubeconfigPath: kubeconfig, Log: slog.New(slog.DiscardHandler)}
}

// applyLayer runs the layer's Apply against the fake apiserver.
func applyLayer(t *testing.T, api *fakeAPIServer) {
	t.Helper()
	env := testEnv(writeKubeconfig(t, api.server.URL))
	if err := (Layer{}).Apply(context.Background(), env); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

func TestApply(t *testing.T) {
	api := newFakeAPIServer(t)
	applyLayer(t, api)

	calls := api.objectCalls()

	t.Run("applies every object in document order, namespaces first", func(t *testing.T) {
		var applies []string
		for _, call := range calls {
			if strings.HasPrefix(call, "PATCH ") && !strings.HasSuffix(strings.SplitN(call, "?", 2)[0], "/status") {
				applies = append(applies, call)
			}
		}
		if len(applies) != len(wantObjectRefs) {
			t.Fatalf("main applies = %d, want %d (%v)", len(applies), len(wantObjectRefs), applies)
		}
		// The six namespaces are the asset's first six documents and
		// must be the first six applies: nothing namespaced can apply
		// before its namespace exists.
		for i := 0; i < 6; i++ {
			if !strings.Contains(applies[i], "/api/v1/namespaces/") {
				t.Errorf("apply %d = %q, want a Namespace create before any namespaced object", i, applies[i])
			}
		}
		if strings.Contains(applies[6], "/api/v1/namespaces/") && !strings.Contains(applies[6], "/apis/") {
			t.Errorf("apply 6 = %q, want the singletons to follow the six namespaces", applies[6])
		}
	})

	t.Run("writes the subresource-served statuses under the layer's field manager", func(t *testing.T) {
		for _, ref := range []string{"infrastructures/cluster", "clusteroperators/kube-apiserver", "images/cluster"} {
			var statusApply string
			for _, call := range calls {
				if strings.HasPrefix(call, "PATCH /apis/config.openshift.io/v1/"+ref+"?") ||
					strings.HasPrefix(call, "PATCH /apis/config.openshift.io/v1/"+ref+"/status") {
					if strings.HasSuffix(strings.SplitN(call, "?", 2)[0], "/status") {
						statusApply = call
					}
				}
			}
			if statusApply == "" {
				t.Errorf("no status apply recorded for %s", ref)
				continue
			}
			if !strings.Contains(statusApply, "fieldManager="+url.QueryEscape(kube.LayerFieldManager(Name))) {
				t.Errorf("status apply %q does not carry the layer's field manager %q", statusApply, kube.LayerFieldManager(Name))
			}
		}
		// The stored objects carry the statuses the asset defines.
		infra, ok := api.stored("/apis/config.openshift.io/v1/infrastructures/cluster")
		if !ok {
			t.Fatal("Infrastructure/cluster was not applied")
		}
		if platformStatus, _, _ := unstructured.NestedString(infra.Object, "status", "platformStatus", "type"); platformStatus != "None" {
			t.Errorf("Infrastructure status.platformStatus.type = %q, want None (the asset's status)", platformStatus)
		}
		if mtu, _, _ := unstructured.NestedInt64(api.mustStored(t, "/apis/config.openshift.io/v1/networks/cluster").Object, "status", "clusterNetworkMTU"); mtu != 1400 {
			t.Errorf("Network status.clusterNetworkMTU = %d, want 1400 (carried by the main apply: the kind serves no status subresource)", mtu)
		}
	})

	t.Run("creates both SCCs with their nulls intact", func(t *testing.T) {
		api.mu.Lock()
		creates, deletes := api.sccCreates, api.sccDeletes
		api.mu.Unlock()
		if creates != 2 || deletes != 0 {
			t.Fatalf("SCC creates = %d, deletes = %d, want 2 creates and no deletes on a fresh cluster", creates, deletes)
		}
		scc := api.mustStored(t, "/apis/security.openshift.io/v1/securitycontextconstraints/restricted-v2")
		for _, field := range RequiredSCCFields {
			if _, present := scc.Object[field]; !present {
				t.Errorf("created restricted-v2 carries no %q field; the create must send every field the CRD requires, explicit nulls included", field)
			}
		}
	})
}

// A 404 on a status write is the no-subresource exception for exactly
// one known kind (networks.config.openshift.io). For any other kind it
// must stay an error: the object or its subresource is missing, and
// treating that as convergence would leave Verify to discover it.
func TestApplyRefusesAStatus404OutsideTheKnownException(t *testing.T) {
	api := newFakeAPIServer(t)
	api.statusNotFound = "/apis/config.openshift.io/v1/infrastructures/cluster"
	env := testEnv(writeKubeconfig(t, api.server.URL))
	err := (Layer{}).Apply(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "Infrastructure/cluster status") {
		t.Fatalf("Apply = %v, want the Infrastructure status 404 to be an error naming the object, not the no-subresource exception", err)
	}
}

// mustStored returns the object stored at key or fails the test.
func (a *fakeAPIServer) mustStored(t *testing.T, key string) *unstructured.Unstructured {
	t.Helper()
	obj, ok := a.stored(key)
	if !ok {
		t.Fatalf("no object stored at %s", key)
	}
	return obj
}

// The re-run path: an existing SCC (this layer's own earlier run, or a
// foreign owner) is deleted and re-created, never updated — the update
// paths are the ones that strip the nulls the CRD requires.
func TestApplyRecreatesExistingSCCs(t *testing.T) {
	api := newFakeAPIServer(t)
	api.seed("/apis/security.openshift.io/v1/securitycontextconstraints/restricted-v2", &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "security.openshift.io/v1",
		"kind":       "SecurityContextConstraints",
		"metadata":   map[string]any{"name": "restricted-v2"},
		// A drifted shape: the priority null an update path would strip.
		"allowHostIPC": false,
	}})
	applyLayer(t, api)

	api.mu.Lock()
	creates, deletes := api.sccCreates, api.sccDeletes
	api.mu.Unlock()
	// restricted-v2: create (409) → delete → create; privileged: create.
	if creates != 3 || deletes != 1 {
		t.Fatalf("SCC creates = %d, deletes = %d, want the existing SCC deleted (1) and both created (3 creates: the refused one, its re-create, and the fresh privileged)", creates, deletes)
	}
	var deleteCall bool
	for _, call := range api.objectCalls() {
		if strings.HasPrefix(call, "DELETE /apis/security.openshift.io/v1/securitycontextconstraints/restricted-v2") {
			deleteCall = true
		}
	}
	if !deleteCall {
		t.Error("no DELETE recorded for the existing restricted-v2 SCC")
	}
	// The re-created object carries the asset's content, nulls included.
	scc := api.mustStored(t, "/apis/security.openshift.io/v1/securitycontextconstraints/restricted-v2")
	if _, present := scc.Object["priority"]; !present {
		t.Error("re-created restricted-v2 carries no priority field; the re-create must restore the nulls")
	}
}

// A create failure that is not AlreadyExists is an error, not a
// delete+recreate attempt.
func TestApplySCCCreateFailureIsAnError(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.failSCCCreate = true
	api.mu.Unlock()
	env := testEnv(writeKubeconfig(t, api.server.URL))
	err := (Layer{}).Apply(context.Background(), env)
	if err == nil || !strings.Contains(err.Error(), "creating the SecurityContextConstraints restricted-v2") {
		t.Fatalf("Apply with a failing SCC create = %v, want an error naming the SCC", err)
	}
}

func TestVerify(t *testing.T) {
	t.Run("passes after Apply", func(t *testing.T) {
		api := newFakeAPIServer(t)
		applyLayer(t, api)
		if err := (Layer{}).Verify(context.Background(), testEnv(writeKubeconfig(t, api.server.URL))); err != nil {
			t.Fatalf("Verify after Apply = %v, want nil", err)
		}
	})

	t.Run("a missing ConfigMap fails naming it", func(t *testing.T) {
		api := newFakeAPIServer(t)
		applyLayer(t, api)
		api.mu.Lock()
		delete(api.store, "/api/v1/namespaces/openshift-kube-apiserver/configmaps/config")
		api.mu.Unlock()
		err := (Layer{}).Verify(context.Background(), testEnv(writeKubeconfig(t, api.server.URL)))
		if err == nil || !strings.Contains(err.Error(), "ConfigMap openshift-kube-apiserver/config") {
			t.Fatalf("Verify(missing ConfigMap) = %v, want a failure naming the ConfigMap", err)
		}
	})

	t.Run("an emptied ConfigMap data key fails naming the key", func(t *testing.T) {
		api := newFakeAPIServer(t)
		applyLayer(t, api)
		cm := api.mustStored(t, "/api/v1/namespaces/openshift-etcd/configmaps/etcd-pod")
		api.mu.Lock()
		cm.Object["data"] = map[string]any{}
		api.mu.Unlock()
		err := (Layer{}).Verify(context.Background(), testEnv(writeKubeconfig(t, api.server.URL)))
		if err == nil || !strings.Contains(err.Error(), `"pod.yaml"`) {
			t.Fatalf("Verify(emptied data) = %v, want a failure naming the pod.yaml key", err)
		}
	})

	t.Run("a lost singleton status fails", func(t *testing.T) {
		api := newFakeAPIServer(t)
		applyLayer(t, api)
		infra := api.mustStored(t, "/apis/config.openshift.io/v1/infrastructures/cluster")
		api.mu.Lock()
		delete(infra.Object, "status")
		api.mu.Unlock()
		err := (Layer{}).Verify(context.Background(), testEnv(writeKubeconfig(t, api.server.URL)))
		if err == nil || !strings.Contains(err.Error(), "Infrastructure cluster carries no populated status") {
			t.Fatalf("Verify(lost status) = %v, want a failure naming the singleton and its status", err)
		}
	})

	t.Run("a missing SCC fails naming it", func(t *testing.T) {
		api := newFakeAPIServer(t)
		applyLayer(t, api)
		api.mu.Lock()
		delete(api.store, "/apis/security.openshift.io/v1/securitycontextconstraints/privileged")
		api.mu.Unlock()
		err := (Layer{}).Verify(context.Background(), testEnv(writeKubeconfig(t, api.server.URL)))
		if err == nil || !strings.Contains(err.Error(), "SecurityContextConstraints privileged") {
			t.Fatalf("Verify(missing SCC) = %v, want a failure naming the SCC", err)
		}
	})

	t.Run("an SCC that lost a required null fails naming the field", func(t *testing.T) {
		api := newFakeAPIServer(t)
		applyLayer(t, api)
		scc := api.mustStored(t, "/apis/security.openshift.io/v1/securitycontextconstraints/restricted-v2")
		api.mu.Lock()
		delete(scc.Object, "priority")
		api.mu.Unlock()
		err := (Layer{}).Verify(context.Background(), testEnv(writeKubeconfig(t, api.server.URL)))
		if err == nil || !strings.Contains(err.Error(), `"priority"`) || !strings.Contains(err.Error(), "restricted-v2") {
			t.Fatalf("Verify(stripped null) = %v, want a failure naming the SCC and the priority field", err)
		}
	})

	t.Run("a missing singleton CR fails naming it", func(t *testing.T) {
		api := newFakeAPIServer(t)
		applyLayer(t, api)
		api.mu.Lock()
		delete(api.store, "/apis/operator.openshift.io/v1/namespaces/openshift-ingress-operator/ingresscontrollers/default")
		api.mu.Unlock()
		err := (Layer{}).Verify(context.Background(), testEnv(writeKubeconfig(t, api.server.URL)))
		if err == nil || !strings.Contains(err.Error(), "IngressController openshift-ingress-operator/default") {
			t.Fatalf("Verify(missing IngressController) = %v, want a failure naming it", err)
		}
	})
}

// The same regression class the earlier gates found in other layers:
// Verify's reads ran under the caller's unbounded context, so a wedged
// apiserver hung `kind-ocp verify` indefinitely (the CLI hands it
// context.Background). The fake apiserver stalls every object read far
// beyond the read budget; Verify must fail within the bound.
func TestVerifyReadIsBounded(t *testing.T) {
	const readBudget = 200 * time.Millisecond
	const stall = 3 * time.Second

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(stall):
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)

	start := time.Now()
	err := (Layer{}).verify(context.Background(), testEnv(writeKubeconfig(t, srv.URL)), readBudget)
	if err == nil {
		t.Fatal("Verify against a wedged apiserver = nil, want a bounded failure")
	}
	if elapsed := time.Since(start); elapsed > stall {
		t.Fatalf("Verify took %s, want it bounded by the %s read budget", elapsed, readBudget)
	}
}

// refs renders the object references of a decoded set.
func refs(objs []*unstructured.Unstructured) []string {
	out := make([]string, 0, len(objs))
	for _, obj := range objs {
		out = append(out, objectRef(obj))
	}
	return out
}

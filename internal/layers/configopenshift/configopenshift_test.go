package configopenshift

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/guzalv/kind-ocp/layers"
)

// The ported asset sets, pinned. These are the exact contents reviewed
// in the PoC (stackrox PR #23841); if an edit changes either set, the
// change must be deliberate — revisit the layer docs with it.
var wantCRDNames = []string{
	"apiservers.config.openshift.io",
	"infrastructures.config.openshift.io",
	"clusterversions.config.openshift.io",
	"clusteroperators.config.openshift.io",
	"networks.config.openshift.io",
	"oauths.config.openshift.io",
	"images.config.openshift.io",
	"proxies.config.openshift.io",
	"schedulers.config.openshift.io",
	"consoles.config.openshift.io",
	"ingresses.config.openshift.io",
	"authentications.config.openshift.io",
	"featuregates.config.openshift.io",
}

var wantSingletons = []string{"APIServer/cluster", "Infrastructure/cluster", "ClusterVersion/version"}

func TestLayerName(t *testing.T) {
	l := Layer{}
	if got := l.Name(); got != Name {
		t.Errorf("Name() = %q, want %q", got, Name)
	}
	if Name != "config-openshift" {
		t.Errorf("Name = %q, want the documented layer name config-openshift", Name)
	}
}

// loadAssets is the production decode path; pinning its output pins the
// embedded assets themselves.
func TestLoadAssets(t *testing.T) {
	crds, singletons, err := loadAssets()
	if err != nil {
		t.Fatalf("loadAssets: %v", err)
	}

	t.Run("decodes the 13 ported placeholder CRDs in document order", func(t *testing.T) {
		if len(crds) != len(wantCRDNames) {
			t.Fatalf("decoded %d CRDs, want %d (%v)", len(crds), len(wantCRDNames), crdNames(crds))
		}
		if !slices.Equal(crdNames(crds), wantCRDNames) {
			t.Errorf("CRD names = %v, want %v in document order", crdNames(crds), wantCRDNames)
		}
	})

	// The structural contract the apply order and the Established wait
	// depend on: every object in the CRD manifest is a CustomResourceDefinition
	// of the config.openshift.io group, so a stray object or a foreign
	// group fails here instead of surfacing as a two-minute wait for a
	// CRD that does not exist.
	t.Run("every CRD is a cluster-scoped served v1 with the status subresource and a schema-less root", func(t *testing.T) {
		// The status subresource is what the Infrastructure status
		// write goes through, and the preserve-unknown-fields root is
		// what makes these CRDs placeholders: any spec shape validates.
		for _, crd := range crds {
			if crd.GetKind() != "CustomResourceDefinition" {
				t.Errorf("the CRD asset contains a %s (%s); only CustomResourceDefinitions belong there",
					crd.GetKind(), crd.GetName())
			}
			if group, _, _ := unstructured.NestedString(crd.Object, "spec", "group"); group != configAPIGroup {
				t.Errorf("CRD %s defines group %q, want %q", crd.GetName(), group, configAPIGroup)
			}
			if scope, _, _ := unstructured.NestedString(crd.Object, "spec", "scope"); scope != "Cluster" {
				t.Errorf("CRD %s scope = %q, want Cluster", crd.GetName(), scope)
			}
			versions, _, _ := unstructured.NestedSlice(crd.Object, "spec", "versions")
			if len(versions) != 1 {
				t.Errorf("CRD %s has %d versions, want exactly v1", crd.GetName(), len(versions))
				continue
			}
			version, ok := versions[0].(map[string]any)
			if !ok {
				t.Errorf("CRD %s version is not a map", crd.GetName())
				continue
			}
			if version["name"] != "v1" || version["served"] != true || version["storage"] != true {
				t.Errorf("CRD %s version = %v, want v1 served and stored", crd.GetName(), version)
			}
			subresources, found, _ := unstructured.NestedMap(version, "subresources")
			if !found {
				t.Errorf("CRD %s v1 has no subresources", crd.GetName())
			} else if _, ok := subresources["status"]; !ok {
				t.Errorf("CRD %s v1 does not enable the status subresource", crd.GetName())
			}
			preserve, found, _ := unstructured.NestedBool(version, "schema", "openAPIV3Schema", "x-kubernetes-preserve-unknown-fields")
			if !found || !preserve {
				t.Errorf("CRD %s v1 schema does not preserve unknown fields", crd.GetName())
			}
		}
	})

	t.Run("decodes the three ported singletons in document order", func(t *testing.T) {
		// The exact set pins the Infrastructure/cluster object the
		// status subresource write targets: the equality cannot pass
		// without it.
		var got []string
		for _, obj := range singletons {
			got = append(got, obj.GetKind()+"/"+obj.GetName())
		}
		if !slices.Equal(got, wantSingletons) {
			t.Fatalf("singletons = %v, want %v in document order", got, wantSingletons)
		}
		for _, obj := range singletons {
			if obj.GetAPIVersion() != configAPIVersion {
				t.Errorf("singleton %s/%s apiVersion = %q, want %s",
					obj.GetKind(), obj.GetName(), obj.GetAPIVersion(), configAPIVersion)
			}
			// Spec-only: the status is the layer's to write through
			// the subresource, never part of the asset.
			if _, found, _ := unstructured.NestedMap(obj.Object, "status"); found {
				t.Errorf("singleton %s/%s carries a status in the asset; status belongs to the subresource write",
					obj.GetKind(), obj.GetName())
			}
		}
	})
}

// The status payload is ported verbatim from the PoC's
// `kubectl patch infrastructure cluster --subresource=status`; Sensor's
// OpenShift detection reads status.platformStatus, so every field here
// is load-bearing and none may drift silently.
func TestInfrastructureStatusObject(t *testing.T) {
	obj := infrastructureStatusObject()
	if obj.GetAPIVersion() != configAPIVersion || obj.GetKind() != "Infrastructure" || obj.GetName() != "cluster" {
		t.Fatalf("object = %s %s/%s, want %s Infrastructure/cluster",
			obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), configAPIVersion)
	}
	want := map[string]string{
		"infrastructureName":   "kind",
		"controlPlaneTopology": "HighlyAvailable",
		"platform":             "None",
	}
	status, found, _ := unstructured.NestedMap(obj.Object, "status")
	if !found {
		t.Fatal("object carries no status")
	}
	for k, v := range want {
		got, found := status[k]
		if !found {
			t.Errorf("status.%s is missing; the PoC's patch payload carried it", k)
			continue
		}
		if got != v {
			t.Errorf("status.%s = %v, want %v", k, got, v)
		}
	}
	platformStatus, found := status["platformStatus"]
	if !found {
		t.Fatal("status.platformStatus is missing; it is the input Sensor's detection reads")
	}
	if ps, ok := platformStatus.(map[string]any); !ok || ps["type"] != "None" {
		t.Errorf("status.platformStatus = %v, want {type: None}", platformStatus)
	}
	// Spec-less by design: the subresource ignores it and the spec
	// belongs to the asset.
	if _, found := obj.Object["spec"]; found {
		t.Error("status object carries a spec; the subresource write must stay status-only")
	}
}

func TestCheckInfrastructureStatus(t *testing.T) {
	infra := func(status map[string]any) *unstructured.Unstructured {
		obj := map[string]any{
			"apiVersion": configAPIVersion,
			"kind":       "Infrastructure",
			"metadata":   map[string]any{"name": "cluster"},
		}
		if status != nil {
			obj["status"] = status
		}
		return &unstructured.Unstructured{Object: obj}
	}

	t.Run("platformStatus present passes", func(t *testing.T) {
		err := checkInfrastructureStatus(infra(map[string]any{
			"platformStatus": map[string]any{"type": "None"},
		}))
		if err != nil {
			t.Fatalf("checkInfrastructureStatus(platformStatus) = %v, want nil", err)
		}
	})

	t.Run("no status fails", func(t *testing.T) {
		err := checkInfrastructureStatus(infra(nil))
		if err == nil || !strings.Contains(err.Error(), "status.platformStatus") {
			t.Fatalf("checkInfrastructureStatus(no status) = %v, want a platformStatus error", err)
		}
	})

	t.Run("empty platformStatus fails", func(t *testing.T) {
		err := checkInfrastructureStatus(infra(map[string]any{"platformStatus": map[string]any{}}))
		if err == nil || !strings.Contains(err.Error(), "status.platformStatus") {
			t.Fatalf("checkInfrastructureStatus(empty platformStatus) = %v, want a platformStatus error", err)
		}
	})
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

// The Infrastructure object the fake apiserver serves once the layer
// has run: the singleton with the layer's status on it.
const infraWithPlatformStatus = `{"apiVersion":"config.openshift.io/v1","kind":"Infrastructure",
  "metadata":{"name":"cluster"},"spec":{"platformSpec":{"type":"None"}},
  "status":{"infrastructureName":"kind","controlPlaneTopology":"HighlyAvailable",
            "platform":"None","platformStatus":{"type":"None"}}}`

func TestVerify(t *testing.T) {
	t.Run("readable Infrastructure with platformStatus passes", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/apis/config.openshift.io/v1/infrastructures/cluster" {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(infraWithPlatformStatus))
		}))
		t.Cleanup(srv.Close)

		if err := (Layer{}).Verify(context.Background(), testEnv(writeKubeconfig(t, srv.URL))); err != nil {
			t.Fatalf("Verify = %v, want nil", err)
		}
	})

	t.Run("Infrastructure without platformStatus fails", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"apiVersion":"config.openshift.io/v1","kind":"Infrastructure",
				"metadata":{"name":"cluster"},"spec":{}}`))
		}))
		t.Cleanup(srv.Close)

		err := (Layer{}).Verify(context.Background(), testEnv(writeKubeconfig(t, srv.URL)))
		if err == nil || !strings.Contains(err.Error(), "status.platformStatus") {
			t.Fatalf("Verify(no platformStatus) = %v, want a platformStatus error", err)
		}
	})

	t.Run("missing Infrastructure fails with the read named", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		}))
		t.Cleanup(srv.Close)

		err := (Layer{}).Verify(context.Background(), testEnv(writeKubeconfig(t, srv.URL)))
		if err == nil || !strings.Contains(err.Error(), "reading Infrastructure/cluster") {
			t.Fatalf("Verify(no Infrastructure) = %v, want a read error naming the object", err)
		}
	})
}

// The same regression class the increment-2 gates found in the olm
// layer: Verify's read ran under the caller's unbounded context, so a
// wedged apiserver hung `kind-ocp verify` indefinitely (the CLI hands
// it context.Background). The fake apiserver stalls the Infrastructure
// read far beyond the read budget; the read must fail within the bound.
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
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Verify must fail: the apiserver never answered")
	}
	if !strings.Contains(err.Error(), "reading Infrastructure/cluster") {
		t.Errorf("error = %v, want the bounded read to fail it", err)
	}
	if elapsed > time.Second {
		t.Errorf("%s read budget took %s; the stalled read was not cancelled at the deadline", readBudget, elapsed)
	}
}

// fakeClusterAPIServer serves enough of the Kubernetes API for the
// layer's Apply: discovery for the REST mapper, the CRD creates and
// reads (Established), the singleton applies, and the status
// subresource apply. It records every call in order, answers creates
// for CRD names in existing with 409 AlreadyExists (a cluster that
// already has those CRDs — the layer's bootstrap must skip them, not
// overwrite them), echoes applied and created bodies back, the way a
// real apiserver returns the stored object.
type fakeClusterAPIServer struct {
	server *httptest.Server

	mu          sync.Mutex
	calls       []string        // "METHOD path" of every API call, in order
	statusBody  string          // body of the status-subresource apply
	established int             // CRD reads served for the Established wait
	existing    map[string]bool // CRD names that already exist (409 on create)
	created     []string        // CRD names whose create succeeded (201)
}

func newFakeClusterAPIServer(t *testing.T) *fakeClusterAPIServer {
	t.Helper()
	api := &fakeClusterAPIServer{existing: map[string]bool{}}
	mux := http.NewServeMux()

	// Discovery: core v1 (empty) plus the two groups the layer touches.
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		api.write(w, `{"kind":"APIVersions","versions":["v1"],
		"serverAddressByClientCIDRs":[{"clientCIDR":"0.0.0.0/0","serverAddress":"127.0.0.1"}]}`)
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, r *http.Request) {
		api.write(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[
		{"name":"apiextensions.k8s.io","versions":[{"groupVersion":"apiextensions.k8s.io/v1","version":"v1"}],
		 "preferredVersion":{"groupVersion":"apiextensions.k8s.io/v1","version":"v1"}},
		{"name":"config.openshift.io","versions":[{"groupVersion":"config.openshift.io/v1","version":"v1"}],
		 "preferredVersion":{"groupVersion":"config.openshift.io/v1","version":"v1"}}]}`)
	})
	mux.HandleFunc("/api/v1", func(w http.ResponseWriter, r *http.Request) {
		api.write(w, `{"kind":"APIResourceList","apiVersion":"v1","groupVersion":"v1","resources":[]}`)
	})
	mux.HandleFunc("/apis/apiextensions.k8s.io/v1", func(w http.ResponseWriter, r *http.Request) {
		api.write(w, `{"kind":"APIResourceList","apiVersion":"v1",
		"groupVersion":"apiextensions.k8s.io/v1","resources":[
		{"name":"customresourcedefinitions","singularName":"customresourcedefinition","namespaced":false,
		 "kind":"CustomResourceDefinition","verbs":["get","list","create","patch"]}]}`)
	})
	mux.HandleFunc("/apis/config.openshift.io/v1", func(w http.ResponseWriter, r *http.Request) {
		api.write(w, `{"kind":"APIResourceList","apiVersion":"v1",
		"groupVersion":"config.openshift.io/v1","resources":[
		{"name":"apiservers","singularName":"apiserver","namespaced":false,"kind":"APIServer","verbs":["get","patch"]},
		{"name":"infrastructures","singularName":"infrastructure","namespaced":false,"kind":"Infrastructure","verbs":["get","patch"]},
		{"name":"clusterversions","singularName":"clusterversion","namespaced":false,"kind":"ClusterVersion","verbs":["get","patch"]}]}`)
	})

	// CRD endpoints: creates go to the collection and are echoed back
	// (201) or refused with 409 AlreadyExists for names in existing;
	// per-name gets answer the Established wait with an Established
	// condition. There is deliberately no per-name patch or put handler:
	// the bootstrap must never overwrite an existing CRD, and a request
	// of that shape fails the test with a 404.
	crdCollection := "/apis/apiextensions.k8s.io/v1/customresourcedefinitions"
	mux.HandleFunc(crdCollection, func(w http.ResponseWriter, r *http.Request) {
		api.record(r)
		if r.Method != http.MethodPost {
			http.Error(w, "unexpected method on the CRD collection", http.StatusMethodNotAllowed)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var obj unstructured.Unstructured
		if err := json.Unmarshal(body, &obj); err != nil {
			http.Error(w, fmt.Sprintf("decoding create body: %v", err), http.StatusBadRequest)
			return
		}
		api.mu.Lock()
		defer api.mu.Unlock()
		if api.existing[obj.GetName()] {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure",
				"reason":"AlreadyExists","code":409,
				"details":{"name":%q,"group":"apiextensions.k8s.io","kind":"customresourcedefinitions"},
				"message":"customresourcedefinitions.apiextensions.k8s.io %q already exists"}`,
				obj.GetName(), obj.GetName())
			return
		}
		api.created = append(api.created, obj.GetName())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	})
	mux.HandleFunc(crdCollection+"/", func(w http.ResponseWriter, r *http.Request) {
		api.record(r)
		name := strings.TrimPrefix(r.URL.Path, crdCollection+"/")
		if r.Method != http.MethodGet {
			http.Error(w, fmt.Sprintf("unexpected %s on an existing CRD: the bootstrap never overwrites one", r.Method), http.StatusMethodNotAllowed)
			return
		}
		api.mu.Lock()
		api.established++
		api.mu.Unlock()
		api.write(w, fmt.Sprintf(`{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition",
			"metadata":{"name":%q},"status":{"conditions":[
			{"type":"NamesAccepted","status":"True"},{"type":"Established","status":"True"}]}}`, name))
	})

	// The config.openshift.io resources: singleton applies, and the
	// status-subresource apply whose body the test inspects.
	mux.HandleFunc("/apis/config.openshift.io/v1/", func(w http.ResponseWriter, r *http.Request) {
		api.record(r)
		if r.Method == http.MethodPatch {
			if strings.HasSuffix(r.URL.Path, "/status") {
				body, _ := io.ReadAll(r.Body)
				api.mu.Lock()
				api.statusBody = string(body)
				api.mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(infraWithPlatformStatus))
				return
			}
			api.echo(w, r)
			return
		}
		http.NotFound(w, r)
	})

	api.server = httptest.NewServer(mux)
	t.Cleanup(api.server.Close)
	return api
}

// record appends one API call to the ordered log.
func (a *fakeClusterAPIServer) record(r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, r.Method+" "+r.URL.Path)
}

// echo writes the request body back, the way the apiserver returns the
// stored object for an apply.
func (a *fakeClusterAPIServer) echo(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func (a *fakeClusterAPIServer) write(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(body))
}

// applyAssertions checks the shared shape of every Apply run against
// the fake apiserver: the singletons only after the Established wait
// read every CRD, and the Infrastructure status on the status
// subresource with the ported payload.
func applyAssertions(t *testing.T, api *fakeClusterAPIServer) {
	t.Helper()
	api.mu.Lock()
	defer api.mu.Unlock()

	// The Established wait ran, and ran before the singletons: every
	// CRD was read, and no singleton apply precedes the last CRD read.
	if api.established < len(wantCRDNames) {
		t.Errorf("Established wait read %d CRDs, want at least %d", api.established, len(wantCRDNames))
	}
	firstSingleton := slices.Index(api.calls, "PATCH /apis/config.openshift.io/v1/apiservers/cluster")
	lastCRDRead := -1
	for i, call := range api.calls {
		if strings.HasPrefix(call, "GET /apis/apiextensions.k8s.io/v1/customresourcedefinitions/") {
			lastCRDRead = i
		}
	}
	if firstSingleton == -1 || lastCRDRead == -1 || lastCRDRead > firstSingleton {
		t.Errorf("singleton applies (index %d) ran before the Established reads finished (last read index %d)",
			firstSingleton, lastCRDRead)
	}

	// The status apply carried the ported payload, under kind-ocp's
	// field manager (the query the dynamic client sends with applies).
	var statusObj map[string]any
	if err := json.Unmarshal([]byte(api.statusBody), &statusObj); err != nil {
		t.Fatalf("status apply body is not the JSON the client sends: %v (body: %s)", err, api.statusBody)
	}
	platformStatus, found, _ := unstructured.NestedMap(statusObj, "status", "platformStatus")
	if !found || platformStatus["type"] != "None" {
		t.Errorf("status apply body = %s, want the ported platformStatus payload", api.statusBody)
	}
}

// Apply is the layer's whole converge, driven against a fake apiserver:
// on a fresh cluster every placeholder CRD is created (never applied),
// in document order, the singletons only after the Established wait
// read every CRD, and the Infrastructure status lands on the status
// subresource with the ported payload.
func TestApply(t *testing.T) {
	api := newFakeClusterAPIServer(t)
	if err := (Layer{}).Apply(context.Background(), testEnv(writeKubeconfig(t, api.server.URL))); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	api.mu.Lock()
	created := slices.Clone(api.created)
	calls := slices.Clone(api.calls)
	api.mu.Unlock()

	// Exactly one create per placeholder CRD, in document order.
	if !slices.Equal(created, wantCRDNames) {
		t.Fatalf("created CRDs = %v, want %v in document order", created, wantCRDNames)
	}
	// No CRD was patched or put: the bootstrap creates, it never
	// overwrites. (The fake answers those with 405, so their presence
	// would already have failed Apply; this makes the contract explicit.)
	for _, call := range calls {
		if strings.Contains(call, "/customresourcedefinitions/") &&
			!strings.HasPrefix(call, http.MethodGet) {
			t.Errorf("call %s touches an existing CRD; the bootstrap must never overwrite one", call)
		}
	}
	// The singletons were applied, in document order, after the wait.
	var singletonApplies []string
	for _, call := range calls {
		if strings.HasPrefix(call, http.MethodPatch+" /apis/config.openshift.io/v1/") {
			singletonApplies = append(singletonApplies, call)
		}
	}
	want := []string{
		"PATCH /apis/config.openshift.io/v1/apiservers/cluster",
		"PATCH /apis/config.openshift.io/v1/infrastructures/cluster",
		"PATCH /apis/config.openshift.io/v1/clusterversions/version",
	}
	// The status apply is also a PATCH under this prefix; drop it from
	// the singleton comparison.
	statusApply := "PATCH /apis/config.openshift.io/v1/infrastructures/cluster/status"
	if i := slices.Index(singletonApplies, statusApply); i >= 0 {
		singletonApplies = slices.Delete(singletonApplies, i, i+1)
	}
	if !slices.Equal(singletonApplies, want) {
		t.Errorf("singleton applies = %v, want %v", singletonApplies, want)
	}
	applyAssertions(t, api)
}

// The increment-4 review's finding, at unit level: a cluster that
// already has CRDs under these names — the real openshift/api CRDs the
// openshift-api-crds layer installs, or any foreign form — must not be
// overwritten by the placeholder bootstrap. Existing CRDs are skipped,
// the missing ones are created, and the singletons still converge.
func TestApplyDoesNotOverwriteExistingCRDs(t *testing.T) {
	api := newFakeClusterAPIServer(t)
	// Half the placeholder names already exist on the cluster, in the
	// real shape a later layer (or a foreign owner) left them in.
	for _, name := range []string{
		"apiservers.config.openshift.io",
		"infrastructures.config.openshift.io",
		"clusterversions.config.openshift.io",
		"clusteroperators.config.openshift.io",
		"networks.config.openshift.io",
		"oauths.config.openshift.io",
	} {
		api.existing[name] = true
	}

	if err := (Layer{}).Apply(context.Background(), testEnv(writeKubeconfig(t, api.server.URL))); err != nil {
		t.Fatalf("Apply over existing CRDs: %v", err)
	}

	api.mu.Lock()
	created := slices.Clone(api.created)
	calls := slices.Clone(api.calls)
	api.mu.Unlock()

	// Exactly the absent placeholders were created — the existing
	// names were skipped, not created and not patched.
	wantCreated := []string{
		"images.config.openshift.io",
		"proxies.config.openshift.io",
		"schedulers.config.openshift.io",
		"consoles.config.openshift.io",
		"ingresses.config.openshift.io",
		"authentications.config.openshift.io",
		"featuregates.config.openshift.io",
	}
	if !slices.Equal(created, wantCreated) {
		t.Fatalf("created CRDs = %v, want exactly the absent ones %v", created, wantCreated)
	}
	for _, call := range calls {
		if strings.Contains(call, "/customresourcedefinitions/") && !strings.HasPrefix(call, http.MethodGet) {
			t.Errorf("call %s touches an existing CRD; the bootstrap must never overwrite one", call)
		}
	}
	applyAssertions(t, api)
}

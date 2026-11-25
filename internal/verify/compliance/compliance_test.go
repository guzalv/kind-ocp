package compliance

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// fakeAPIServer is an in-memory apiserver carrying the API surface the
// proof touches, plus the actors the proof depends on: OLM resolving
// the Subscription (installing a CSV whose name deliberately disagrees
// with the Subscription's status.currentCSV — the channel-head
// mismatch the fallback exists for), OLM driving the CSV to Succeeded
// and rolling out the operator Deployment, the operator creating its
// default ProfileBundles, the binding controller marking the binding
// READY, and the scan producing ComplianceCheckResults. Every actor
// advances inside the request handlers, so the proof's polls drive the
// world forward exactly as they would against the real controllers,
// and every request is recorded so the tests can assert ordering.
type fakeAPIServer struct {
	server *httptest.Server

	mu    sync.Mutex
	store map[string]*unstructured.Unstructured // object path → object
	calls []string                              // "METHOD path" of every object request, in order

	// serveOLM lists operators.coreos.com in discovery. Clearing it
	// models a cluster without the olm layer — the preflight refusal.
	serveOLM bool
	// catalogPodReady advances the kubelet once the catalog server pod
	// is applied: the pod runs and its gRPC health probe passes, which
	// is the Ready condition the proof waits on. Clearing it models a
	// catalog the cluster cannot serve — an image that will not pull,
	// or an opm that refuses the content.
	catalogPodReady bool
	// resolveSubscription advances OLM's resolution once the
	// Subscription is applied: status.currentCSV is set to a name no
	// CSV will ever carry (csvGhostName) and one real CSV
	// (csvInstalledName) is installed with no phase yet. Clearing it
	// models a Subscription that never resolves.
	resolveSubscription bool
	// csvSucceeds advances the installed CSV to phase Succeeded and
	// rolls out the operator Deployment.
	csvSucceeds bool
	// createBundles advances the operator to create its default
	// ProfileBundles, VALID.
	createBundles bool
	// bindingReadiness advances the binding controller to mark the
	// proof's binding READY.
	bindingReadiness bool
	// passResults and failResults are the ComplianceCheckResults the
	// scan produces once the binding is READY. Zero and zero models a
	// scan whose results never arrive (or arrive NOT-APPLICABLE).
	passResults int
	failResults int
}

// The CSV names the fake's OLM installs: one real CSV, and one
// status.currentCSV value that names nothing — the exact shape the
// PoC's fallback was written for.
const (
	csvInstalledName = "compliance-operator.v1.1.1-fake"
	csvGhostName     = "compliance-operator.v9.9.9-ghost"
)

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	api := &fakeAPIServer{
		store:               map[string]*unstructured.Unstructured{},
		serveOLM:            true,
		catalogPodReady:     true,
		resolveSubscription: true,
		csvSucceeds:         true,
		createBundles:       true,
		bindingReadiness:    true,
		passResults:         2,
		failResults:         1,
	}
	mux := http.NewServeMux()

	// Discovery: the groups and resources the proof touches, with the
	// scoping the real APIs carry (the REST mapper derives it).
	mux.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		api.write(w, `{"kind":"APIVersions","versions":["v1"],
		"serverAddressByClientCIDRs":[{"clientCIDR":"0.0.0.0/0","serverAddress":"127.0.0.1"}]}`)
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		groups := `{"name":"apps","versions":[{"groupVersion":"apps/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apps/v1","version":"v1"}},` +
			`{"name":"compliance.openshift.io","versions":[{"groupVersion":"compliance.openshift.io/v1alpha1","version":"v1alpha1"}],"preferredVersion":{"groupVersion":"compliance.openshift.io/v1alpha1","version":"v1alpha1"}}`
		if api.serveOLM {
			groups += `,{"name":"operators.coreos.com","versions":[{"groupVersion":"operators.coreos.com/v1alpha1","version":"v1alpha1"},{"groupVersion":"operators.coreos.com/v1","version":"v1"}],"preferredVersion":{"groupVersion":"operators.coreos.com/v1alpha1","version":"v1alpha1"}}`
		}
		api.write(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[`+groups+`]}`)
	})
	mux.HandleFunc("/api/v1", func(w http.ResponseWriter, _ *http.Request) {
		api.write(w, `{"kind":"APIResourceList","groupVersion":"v1","resources":[
		{"name":"namespaces","singularName":"namespace","namespaced":false,"kind":"Namespace","verbs":["get","patch"]},
		{"name":"configmaps","singularName":"configmap","namespaced":true,"kind":"ConfigMap","verbs":["get","patch"]},
		{"name":"pods","singularName":"pod","namespaced":true,"kind":"Pod","verbs":["get","patch"]},
		{"name":"secrets","singularName":"secret","namespaced":true,"kind":"Secret","verbs":["get","create","update"]},
		{"name":"services","singularName":"service","namespaced":true,"kind":"Service","verbs":["get","patch"]}]}`)
	})
	groupResources := map[string]string{
		"/apis/apps/v1": `{"kind":"APIResourceList","groupVersion":"apps/v1","resources":[
		{"name":"deployments","singularName":"deployment","namespaced":true,"kind":"Deployment","verbs":["get"]}]}`,
		"/apis/compliance.openshift.io/v1alpha1": `{"kind":"APIResourceList","groupVersion":"compliance.openshift.io/v1alpha1","resources":[
		{"name":"profilebundles","singularName":"profilebundle","namespaced":true,"kind":"ProfileBundle","verbs":["get"]},
		{"name":"scansettings","singularName":"scansetting","namespaced":true,"kind":"ScanSetting","verbs":["get","patch"]},
		{"name":"scansettingbindings","singularName":"scansettingbinding","namespaced":true,"kind":"ScanSettingBinding","verbs":["get","patch"]},
		{"name":"compliancecheckresults","singularName":"compliancecheckresult","namespaced":true,"kind":"ComplianceCheckResult","verbs":["list"]}]}`,
		"/apis/operators.coreos.com/v1alpha1": `{"kind":"APIResourceList","groupVersion":"operators.coreos.com/v1alpha1","resources":[
		{"name":"catalogsources","singularName":"catalogsource","namespaced":true,"kind":"CatalogSource","verbs":["get","patch"]},
		{"name":"clusterserviceversions","singularName":"clusterserviceversion","namespaced":true,"kind":"ClusterServiceVersion","verbs":["get","list"]},
		{"name":"operatorgroups","singularName":"operatorgroup","namespaced":true,"kind":"OperatorGroup","verbs":["get","patch"]},
		{"name":"subscriptions","singularName":"subscription","namespaced":true,"kind":"Subscription","verbs":["get","patch"]}]}`,
		"/apis/operators.coreos.com/v1": `{"kind":"APIResourceList","groupVersion":"operators.coreos.com/v1","resources":[
		{"name":"operatorgroups","singularName":"operatorgroup","namespaced":true,"kind":"OperatorGroup","verbs":["get","patch"]}]}`,
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

// handleObject serves one object request against the store, advancing
// the simulated world first: the actors move inside the reads the
// proof's waits issue, so a poll that observes a precondition also
// causes its consequence — the shape a real reconcile loop has.
func (a *fakeAPIServer) handleObject(w http.ResponseWriter, r *http.Request) {
	body := func() *unstructured.Unstructured {
		var obj unstructured.Unstructured
		if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
			http.Error(w, fmt.Sprintf("decoding request body: %v", err), http.StatusBadRequest)
			return nil
		}
		return &obj
	}
	key := strings.TrimSuffix(r.URL.Path, "/")

	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, r.Method+" "+strings.SplitN(r.URL.RequestURI(), "?", 2)[0])

	// The actors, in the order the real system moves: the kubelet runs
	// the catalog server pod once it is applied, OLM resolves the
	// Subscription (installing a CSV whose name disagrees with
	// status.currentCSV), OLM drives the CSV to Succeeded and rolls
	// out the operator, the operator creates its ProfileBundles, the
	// binding controller marks the binding READY, and the scan lands
	// its results.
	podKey := "/api/v1/namespaces/" + catalogNamespace + "/pods/" + catalogServerName
	if a.catalogPodReady {
		if pod, ok := a.store[podKey]; ok {
			if _, found, _ := unstructured.NestedString(pod.Object, "status", "phase"); !found {
				_ = unstructured.SetNestedField(pod.Object, "Running", "status", "phase")
				_ = unstructured.SetNestedSlice(pod.Object, []any{
					map[string]any{"type": "Ready", "status": "True"},
				}, "status", "conditions")
			}
		}
	}
	subKey := "/apis/operators.coreos.com/v1alpha1/namespaces/" + operatorNamespace + "/subscriptions/" + subscriptionName
	if a.resolveSubscription {
		if sub, ok := a.store[subKey]; ok {
			if _, found, _ := unstructured.NestedString(sub.Object, "status", "currentCSV"); !found {
				_ = unstructured.SetNestedField(sub.Object, csvGhostName, "status", "currentCSV")
				a.store[csvKey(csvInstalledName)] = &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "operators.coreos.com/v1alpha1",
					"kind":       "ClusterServiceVersion",
					"metadata":   map[string]any{"name": csvInstalledName, "namespace": operatorNamespace},
				}}
			}
		}
	}
	if a.csvSucceeds {
		if csv, ok := a.store[csvKey(csvInstalledName)]; ok {
			if _, found, _ := unstructured.NestedString(csv.Object, "status", "phase"); !found {
				_ = unstructured.SetNestedField(csv.Object, "Succeeded", "status", "phase")
				// OLM's install plan also applied the operator's
				// Deployment, rolled out.
				a.store[deploymentKey()] = &unstructured.Unstructured{Object: map[string]any{
					"apiVersion": "apps/v1",
					"kind":       "Deployment",
					"metadata":   map[string]any{"name": operatorDeployment, "namespace": operatorNamespace},
					"status":     map[string]any{"updatedReplicas": int64(1), "availableReplicas": int64(1), "replicas": int64(1)},
				}}
			}
		}
	}
	if a.createBundles {
		if csv, ok := a.store[csvKey(csvInstalledName)]; ok {
			if phase, _, _ := unstructured.NestedString(csv.Object, "status", "phase"); phase == "Succeeded" {
				for _, bundle := range profileBundleNames() {
					if _, ok := a.store[bundleKey(bundle)]; !ok {
						a.store[bundleKey(bundle)] = &unstructured.Unstructured{Object: map[string]any{
							"apiVersion": "compliance.openshift.io/v1alpha1",
							"kind":       "ProfileBundle",
							"metadata":   map[string]any{"name": bundle, "namespace": operatorNamespace},
							"status":     map[string]any{"dataStreamStatus": "VALID"},
						}}
					}
				}
			}
		}
	}
	bindingKey := "/apis/compliance.openshift.io/v1alpha1/namespaces/" + operatorNamespace + "/scansettingbindings/" + bindingName
	if a.bindingReadiness {
		if binding, ok := a.store[bindingKey]; ok {
			if _, found, _ := unstructured.NestedString(binding.Object, "status", "phase"); !found {
				_ = unstructured.SetNestedField(binding.Object, "READY", "status", "phase")
			}
		}
	}
	if binding, ok := a.store[bindingKey]; ok {
		if phase, _, _ := unstructured.NestedString(binding.Object, "status", "phase"); phase == "READY" {
			for i := 0; i < a.passResults; i++ {
				a.store[resultKey(fmt.Sprintf("pass-%d", i))] = checkResult(fmt.Sprintf("pass-%d", i), checkStatusPass)
			}
			for i := 0; i < a.failResults; i++ {
				a.store[resultKey(fmt.Sprintf("fail-%d", i))] = checkResult(fmt.Sprintf("fail-%d", i), checkStatusFail)
			}
		}
	}

	switch r.Method {
	case http.MethodPatch:
		obj := body()
		if obj == nil {
			return
		}
		a.store[key] = obj
		a.write(w, rawJSON(obj))
	case http.MethodPost:
		obj := body()
		if obj == nil {
			return
		}
		if obj.GetUID() == "" {
			obj.SetUID(types.UID("fake-uid-" + obj.GetName()))
		}
		a.store[key+"/"+obj.GetName()] = obj
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		a.write(w, rawJSON(obj))
	case http.MethodGet:
		if obj, ok := a.store[key]; ok {
			a.write(w, rawJSON(obj))
			return
		}
		// A GET on a path no object sits at is a collection read
		// (the dynamic client's List); serve the store's children.
		if a.hasCollection(key) {
			a.list(w, r, key)
			return
		}
		a.notFound(w, key)
	default:
		http.Error(w, "method not handled: "+r.Method, http.StatusMethodNotAllowed)
	}
}

// hasCollection reports whether a GET path is a collection read (the
// dynamic client's List): either stored objects sit under it, or it
// is one of the known collection paths the proof lists — an empty
// collection must list as empty, not 404.
func (a *fakeAPIServer) hasCollection(collection string) bool {
	for _, known := range []string{csvCollectionKey(), resultCollectionKey()} {
		if collection == known {
			return true
		}
	}
	for key := range a.store {
		if strings.HasPrefix(key, collection+"/") {
			return true
		}
	}
	return false
}

func csvCollectionKey() string {
	return "/apis/operators.coreos.com/v1alpha1/namespaces/" + operatorNamespace + "/clusterserviceversions"
}

func resultCollectionKey() string {
	return "/apis/compliance.openshift.io/v1alpha1/namespaces/" + operatorNamespace + "/compliancecheckresults"
}

// list serves one collection request: the objects under key's
// collection, filtered by the proof's equality label selector when
// one is carried.
func (a *fakeAPIServer) list(w http.ResponseWriter, r *http.Request, collection string) {
	items := make([]any, 0)
	selector := map[string]string{}
	for _, term := range strings.Split(r.URL.Query().Get("labelSelector"), ",") {
		if k, v, found := strings.Cut(term, "="); found && k != "" {
			selector[k] = v
		}
	}
	for key, obj := range a.store {
		if !strings.HasPrefix(key, collection+"/") {
			continue
		}
		if len(selector) > 0 {
			match := true
			for k, v := range selector {
				if obj.GetLabels()[k] != v {
					match = false
					break
				}
			}
			if !match {
				continue
			}
		}
		items = append(items, obj.Object)
	}
	a.write(w, fmt.Sprintf(`{"kind":"List","apiVersion":"v1","items":[%s]}`, joinRawObjects(items)))
}

func joinRawObjects(items []any) string {
	encoded := make([]string, 0, len(items))
	for _, item := range items {
		b, err := json.Marshal(item)
		if err != nil {
			continue
		}
		encoded = append(encoded, string(b))
	}
	return strings.Join(encoded, ",")
}

func checkResult(name, status string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "compliance.openshift.io/v1alpha1",
		"kind":       "ComplianceCheckResult",
		"metadata": map[string]any{
			"name":      name,
			"namespace": operatorNamespace,
			"labels":    map[string]any{suiteLabel: bindingName, checkStatusLabel: status},
		},
	}}
}

func csvKey(name string) string {
	return "/apis/operators.coreos.com/v1alpha1/namespaces/" + operatorNamespace + "/clusterserviceversions/" + name
}

func deploymentKey() string {
	return "/apis/apps/v1/namespaces/" + operatorNamespace + "/deployments/" + operatorDeployment
}

func bundleKey(bundle string) string {
	return "/apis/compliance.openshift.io/v1alpha1/namespaces/" + operatorNamespace + "/profilebundles/" + bundle
}

func resultKey(name string) string {
	return "/apis/compliance.openshift.io/v1alpha1/namespaces/" + operatorNamespace + "/compliancecheckresults/" + name
}

func secretKey() string {
	return "/api/v1/namespaces/" + operatorNamespace + "/secrets/" + servingCertSecret
}

func catalogSourceKey() string {
	return "/apis/operators.coreos.com/v1alpha1/namespaces/" + catalogNamespace + "/catalogsources/" + catalogSourceName
}

func catalogConfigMapKey() string {
	return "/api/v1/namespaces/" + catalogNamespace + "/configmaps/" + catalogServerName
}

func catalogPodKey() string {
	return "/api/v1/namespaces/" + catalogNamespace + "/pods/" + catalogServerName
}

func catalogServiceKey() string {
	return "/api/v1/namespaces/" + catalogNamespace + "/services/" + catalogServerName
}

func subscriptionObjKey() string {
	return "/apis/operators.coreos.com/v1alpha1/namespaces/" + operatorNamespace + "/subscriptions/" + subscriptionName
}

func (a *fakeAPIServer) notFound(w http.ResponseWriter, key string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404,"message":%q}`, key+" not found")
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

// recordedCalls returns the recorded object requests, in order.
func (a *fakeAPIServer) recordedCalls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls...)
}

// stored returns the object stored at key.
func (a *fakeAPIServer) stored(key string) *unstructured.Unstructured {
	a.mu.Lock()
	defer a.mu.Unlock()
	obj, ok := a.store[key]
	if !ok {
		return nil
	}
	return obj
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

// testBudgets are short enough for a failing wait to give up within
// the test's patience, and every successful stage advances on its
// first poll, so the happy path does not pay them.
func testBudgets() budgets {
	return budgets{
		preflight:     5 * time.Second,
		catalogServer: 5 * time.Second,
		csvResolve:    5 * time.Second,
		csvSucceeded:  5 * time.Second,
		rollout:       5 * time.Second,
		profileBundle: 5 * time.Second,
		bindingReady:  5 * time.Second,
		scanResults:   5 * time.Second,
	}
}

// runProve drives the proof against the fake apiserver with the given
// knobs and budgets, returning its error.
func runProve(t *testing.T, api *fakeAPIServer, b budgets) error {
	t.Helper()
	kubeconfig := writeKubeconfig(t, api.server.URL)
	return prove(context.Background(), Options{
		KubeconfigPath: kubeconfig,
		Log:            slogDiscard(),
	}, b)
}

func slogDiscard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestProveInstallsTheOperatorAndScans is the proof's happy path
// against the fake apiserver — with the CSV fallback exercised (the
// Subscription's currentCSV names a CSV that never exists), every wait
// passing, and the scan producing both statuses. It asserts the
// objects the proof applied (the gotchas: CatalogSource in olm, no
// config.env on the Subscription, raw result storage off) and the
// ordering the serving-cert gotcha needs: the secret exists before
// the Subscription that starts the operator.
func TestProveInstallsTheOperatorAndScans(t *testing.T) {
	api := newFakeAPIServer(t)
	if err := runProve(t, api, testBudgets()); err != nil {
		t.Fatalf("prove: %v", err)
	}

	// The serving-cert secret: present, and carrying a usable
	// keypair.
	secret := api.stored(secretKey())
	if secret == nil {
		t.Fatalf("no %s/%s secret stored", operatorNamespace, servingCertSecret)
	}
	data := map[string][]byte{}
	for k, v := range secret.Object["data"].(map[string]any) {
		decoded, err := base64Decode(v.(string))
		if err != nil {
			t.Fatalf("secret field %s does not decode: %v", k, err)
		}
		data[k] = decoded
	}
	if err := validateServingCert(data, time.Now()); err != nil {
		t.Errorf("the stored serving-cert secret is not usable: %v", err)
	}
	if secret.Object["type"] != string(corev1.SecretTypeTLS) {
		t.Errorf("secret type = %v, want %s", secret.Object["type"], corev1.SecretTypeTLS)
	}

	// The gotcha ordering: the secret was created before the
	// Subscription (and its CatalogSource) was applied — the operator
	// the Subscription starts requires the secret to exist — and the
	// catalog server was staged before the CatalogSource that
	// addresses it was applied — the install resolves against a
	// server that is already serving, or the resolution waits on a
	// catalog that may never come up.
	calls := api.recordedCalls()
	secretAt, configMapAt, podAt, catalogAt, subscriptionAt := -1, -1, -1, -1, -1
	for i, call := range calls {
		switch {
		case strings.HasPrefix(call, "POST /api/v1/namespaces/"+operatorNamespace+"/secrets"):
			if secretAt < 0 {
				secretAt = i
			}
		case strings.HasPrefix(call, "PATCH "+catalogConfigMapKey()):
			if configMapAt < 0 {
				configMapAt = i
			}
		case strings.HasPrefix(call, "PATCH "+catalogPodKey()):
			if podAt < 0 {
				podAt = i
			}
		case strings.HasPrefix(call, "PATCH "+catalogSourceKey()):
			catalogAt = i
		case strings.HasPrefix(call, "PATCH "+subscriptionObjKey()):
			subscriptionAt = i
		}
	}
	if secretAt < 0 || configMapAt < 0 || podAt < 0 || catalogAt < 0 || subscriptionAt < 0 {
		t.Fatalf("calls = %v, want a secret create and catalog staging before the CatalogSource and Subscription applies", calls)
	}
	if secretAt > catalogAt || secretAt > subscriptionAt {
		t.Errorf("the serving-cert secret (call %d) was not created before the CatalogSource (%d) and Subscription (%d) applies: the operator the Subscription starts exits without the secret", secretAt, catalogAt, subscriptionAt)
	}
	if configMapAt > catalogAt || podAt > catalogAt {
		t.Errorf("the catalog server (ConfigMap %d, pod %d) was not staged before the CatalogSource apply (%d): the install must resolve against a serving catalog", configMapAt, podAt, catalogAt)
	}

	// The staged catalog: the ConfigMap carries exactly the assembled
	// catalog, the pod serves it from the pinned image with the
	// health-probe readiness OLM's own catalog pods carry, and the
	// Service selects the pod.
	configMap := api.stored(catalogConfigMapKey())
	if configMap == nil {
		t.Fatalf("no catalog ConfigMap stored in %s", catalogNamespace)
	}
	wantCatalog, err := renderCatalog()
	if err != nil {
		t.Fatalf("rendering the catalog: %v", err)
	}
	if got := configMap.Object["data"].(map[string]any)[catalogDataKey]; got != wantCatalog {
		t.Errorf("the catalog ConfigMap carries something other than the assembled catalog (lengths %d vs %d)", len(got.(string)), len(wantCatalog))
	}
	pod := api.stored(catalogPodKey())
	if pod == nil {
		t.Fatalf("no catalog server pod stored in %s", catalogNamespace)
	}
	containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
	if len(containers) != 1 {
		t.Fatalf("the catalog server pod has %d containers, want 1", len(containers))
	}
	container := containers[0].(map[string]any)
	if container["image"] != DefaultCatalogServerImage {
		t.Errorf("catalog server image = %q, want the pinned %q", container["image"], DefaultCatalogServerImage)
	}
	if container["imagePullPolicy"] != "IfNotPresent" {
		t.Errorf("catalog server imagePullPolicy = %v, want IfNotPresent (a digest-pinned image the node may already carry)", container["imagePullPolicy"])
	}
	if token, found, _ := unstructured.NestedBool(pod.Object, "spec", "automountServiceAccountToken"); !found || token {
		t.Errorf("catalog server automountServiceAccountToken = %v (found %v), want false (the server needs no API access)", token, found)
	}
	// The serving pod carries OLM's catalog label: the olm namespace is
	// default-deny, and OLM's own ingress policy admits catalog-port
	// traffic only to pods labeled olm.catalogSource.
	if label := pod.GetLabels()["olm.catalogSource"]; label != catalogSourceName {
		t.Errorf("catalog server pod label olm.catalogSource = %q, want %q (OLM's ingress policy for the olm namespace admits the catalog port only to labeled pods)", label, catalogSourceName)
	}

	// The olm namespace enforces restricted PodSecurity (OLM's release
	// labels it), so the pod must carry the full restricted context.
	if runAsNonRoot, _, _ := unstructured.NestedBool(pod.Object, "spec", "securityContext", "runAsNonRoot"); !runAsNonRoot {
		t.Error("catalog server pod does not set runAsNonRoot, want true (restricted PodSecurity in olm)")
	}
	if seccomp, _, _ := unstructured.NestedString(pod.Object, "spec", "securityContext", "seccompProfile", "type"); seccomp != "RuntimeDefault" {
		t.Errorf("catalog server seccompProfile.type = %q, want RuntimeDefault (restricted PodSecurity in olm)", seccomp)
	}
	if escalate, _, _ := unstructured.NestedBool(containers[0].(map[string]any), "securityContext", "allowPrivilegeEscalation"); escalate {
		t.Error("catalog server container allows privilege escalation, want false (restricted PodSecurity in olm)")
	}
	// The catalog reaches the server as one subPath-mounted file inside
	// an otherwise-empty directory: a whole ConfigMap volume's
	// kubelet-managed layout reads as duplicate content to opm's
	// walker, so the served directory must contain the file and
	// nothing else.
	mounts, _, _ := unstructured.NestedSlice(containers[0].(map[string]any), "volumeMounts")
	mountShapes := map[string]string{}
	for _, m := range mounts {
		mount := m.(map[string]any)
		mountShapes[mount["name"].(string)] = fmt.Sprint(mount["mountPath"], " subPath=", mount["subPath"])
	}
	if mountShapes["serving"] != catalogMountPath+" subPath=<nil>" {
		t.Errorf("the serving volume mount = %v, want the emptyDir at %s with no subPath", mountShapes["serving"], catalogMountPath)
	}
	if mountShapes["catalog"] != catalogMountPath+"/"+catalogDataKey+" subPath="+catalogDataKey {
		t.Errorf("the catalog volume mount = %v, want the ConfigMap's %s key mounted as one file inside %s", mountShapes["catalog"], catalogDataKey, catalogMountPath)
	}
	volumes, _, _ := unstructured.NestedSlice(pod.Object, "spec", "volumes")
	volumeKinds := map[string]bool{}
	for _, v := range volumes {
		volume := v.(map[string]any)
		_, hasEmptyDir := volume["emptyDir"]
		_, hasConfigMap := volume["configMap"]
		volumeKinds[volume["name"].(string)] = hasEmptyDir || hasConfigMap
	}
	if !volumeKinds["serving"] || !volumeKinds["catalog"] {
		t.Errorf("the pod's volumes = %v, want an emptyDir (serving) and a configMap (catalog)", volumes)
	}
	service := api.stored(catalogServiceKey())
	if service == nil {
		t.Fatalf("no catalog Service stored in %s", catalogNamespace)
	}

	// The CatalogSource: in olm, addressing the proof's catalog
	// service — no image, because the catalog is committed content
	// served in-cluster, not a registry artifact.
	catalog := api.stored(catalogSourceKey())
	if catalog == nil {
		t.Fatalf("no CatalogSource stored in %s", catalogNamespace)
	}
	if image, found := catalog.Object["spec"].(map[string]any)["image"]; found {
		t.Errorf("CatalogSource carries spec.image = %v, want none (the catalog is served from the committed manifests, not pulled)", image)
	}
	if address, _ := catalog.Object["spec"].(map[string]any)["address"]; address != catalogAddress() {
		t.Errorf("CatalogSource address = %v, want %q", address, catalogAddress())
	}
	if st, _, _ := unstructured.NestedString(catalog.Object, "spec", "sourceType"); st != "grpc" {
		t.Errorf("CatalogSource sourceType = %q, want grpc", st)
	}

	// The Subscription: in the operator namespace, resolving the
	// proof's source, and carrying no config.env — the upstream CSV
	// ignores those overrides (BRIEF §7.2), and the operator's own
	// platform default is OpenShift.
	subscription := api.stored(subscriptionObjKey())
	if subscription == nil {
		t.Fatalf("no Subscription stored in %s", operatorNamespace)
	}
	if _, found := subscription.Object["spec"].(map[string]any)["config"]; found {
		t.Errorf("the Subscription carries a config section (config.env); the proof must not wire the overrides the upstream CSV ignores")
	}
	if source, _, _ := unstructured.NestedString(subscription.Object, "spec", "source"); source != catalogSourceName {
		t.Errorf("Subscription source = %q, want %q", source, catalogSourceName)
	}
	if sourceNS, _, _ := unstructured.NestedString(subscription.Object, "spec", "sourceNamespace"); sourceNS != catalogNamespace {
		t.Errorf("Subscription sourceNamespace = %q, want %q", sourceNS, catalogNamespace)
	}
	// The package and channel must be the catalog's own constants, so
	// the Subscription cannot drift from the catalog the proof emits.
	if channel, _, _ := unstructured.NestedString(subscription.Object, "spec", "channel"); channel != catalogChannel {
		t.Errorf("Subscription channel = %q, want the catalog's own %q", channel, catalogChannel)
	}
	if pkg, _, _ := unstructured.NestedString(subscription.Object, "spec", "name"); pkg != catalogPackageName {
		t.Errorf("Subscription package = %q, want the catalog's own %q", pkg, catalogPackageName)
	}

	// The OperatorGroup: scoped to the operator's own namespace.
	group := api.stored("/apis/operators.coreos.com/v1/namespaces/" + operatorNamespace + "/operatorgroups/" + operatorGroupName)
	if group == nil {
		t.Fatalf("no OperatorGroup stored in %s", operatorNamespace)
	}
	if targets, _, _ := unstructured.NestedStringSlice(group.Object, "spec", "targetNamespaces"); !slices.Equal(targets, []string{operatorNamespace}) {
		t.Errorf("OperatorGroup targetNamespaces = %v, want [%s]", targets, operatorNamespace)
	}

	// The scan objects: raw result storage off, the ocp4-cis profile
	// bound to the proof's own ScanSetting.
	setting := api.stored("/apis/compliance.openshift.io/v1alpha1/namespaces/" + operatorNamespace + "/scansettings/" + scanSettingName)
	if setting == nil {
		t.Fatalf("no ScanSetting stored")
	}
	if enabled, _, _ := unstructured.NestedBool(setting.Object, "rawResultStorage", "enabled"); enabled {
		t.Errorf("ScanSetting rawResultStorage.enabled = true, want false (no PVC, no result server on a proof cluster)")
	}
	binding := api.stored("/apis/compliance.openshift.io/v1alpha1/namespaces/" + operatorNamespace + "/scansettingbindings/" + bindingName)
	if binding == nil {
		t.Fatalf("no ScanSettingBinding stored")
	}
	profiles, _, _ := unstructured.NestedSlice(binding.Object, "profiles")
	if len(profiles) != 1 || profiles[0].(map[string]any)["name"] != profileName {
		t.Errorf("binding profiles = %v, want exactly the %s profile", profiles, profileName)
	}
	if ref, _, _ := unstructured.NestedString(binding.Object, "settingsRef", "name"); ref != scanSettingName {
		t.Errorf("binding settingsRef.name = %q, want %q", ref, scanSettingName)
	}

	// The results the fake's scan produced were counted through to
	// success (prove returned nil); pin that the wait saw both
	// statuses by checking the store carries them.
	var statuses []string
	for _, result := range []string{"pass-0", "fail-0"} {
		obj := api.stored(resultKey(result))
		if obj == nil {
			t.Fatalf("no ComplianceCheckResult %s stored", result)
		}
		statuses = append(statuses, obj.GetLabels()[checkStatusLabel])
	}
	if statuses[0] != checkStatusPass || statuses[1] != checkStatusFail {
		t.Errorf("result statuses = %v, want a PASS and a FAIL", statuses)
	}
}

// TestProveFailsFastWithoutOLM pins the preflight: a cluster without
// the olm layer fails in the discovery read, naming the layer,
// instead of at the CSV wait's timeout.
func TestProveFailsFastWithoutOLM(t *testing.T) {
	api := newFakeAPIServer(t)
	api.serveOLM = false
	err := runProve(t, api, testBudgets())
	if err == nil {
		t.Fatal("prove succeeded without OLM, want the preflight refusal")
	}
	if !strings.Contains(err.Error(), operatorsAPI) || !strings.Contains(err.Error(), "olm layer") {
		t.Errorf("error = %v, want it to name the unserved API and the olm layer", err)
	}
}

// TestProveTimesOutWithoutResults pins the conclusion's failure: a
// scan that produces no results fails the wait with the counts and
// the platform-layers hint, never a vacuous pass.
func TestProveTimesOutWithoutResults(t *testing.T) {
	api := newFakeAPIServer(t)
	api.passResults, api.failResults = 0, 0
	b := testBudgets()
	b.scanResults = 1500 * time.Millisecond // one poll interval, no more
	err := runProve(t, api, b)
	if err == nil {
		t.Fatal("prove succeeded without scan results, want the wait to fail")
	}
	if !strings.Contains(err.Error(), "did not produce both a PASS and a FAIL") {
		t.Errorf("error = %v, want the results wait's failure", err)
	}
	if !strings.Contains(err.Error(), "NOT-APPLICABLE") {
		t.Errorf("error = %v, want the all-NOT-APPLICABLE hint naming the platform layers", err)
	}
}

// TestProveTimesOutWhenTheSubscriptionNeverResolves pins the first
// wait: a Subscription OLM never resolves fails the proof naming the
// Subscription, not a later stage.
func TestProveTimesOutWhenTheSubscriptionNeverResolves(t *testing.T) {
	api := newFakeAPIServer(t)
	api.resolveSubscription = false
	b := testBudgets()
	b.csvResolve = 1500 * time.Millisecond
	err := runProve(t, api, b)
	if err == nil {
		t.Fatal("prove succeeded without a resolved CSV, want the wait to fail")
	}
	if !strings.Contains(err.Error(), "did not resolve an installable CSV") {
		t.Errorf("error = %v, want the CSV resolution wait's failure", err)
	}
	// Nothing past the install objects: no scan was bound on a
	// cluster whose operator never installed.
	for _, call := range api.recordedCalls() {
		if strings.Contains(call, "scansettingbindings") {
			t.Errorf("a scan binding was applied though the operator never installed: %s", call)
		}
	}
}

// TestProveTimesOutWhenTheCatalogServerNeverReadies pins the staging
// gate: a catalog server whose pod never reports Ready fails the proof
// there, naming the pod and where to look, before any OLM object is
// applied — the install must never resolve against a catalog that is
// not being served.
func TestProveTimesOutWhenTheCatalogServerNeverReadies(t *testing.T) {
	api := newFakeAPIServer(t)
	api.catalogPodReady = false
	b := testBudgets()
	b.catalogServer = 1500 * time.Millisecond
	err := runProve(t, api, b)
	if err == nil {
		t.Fatal("prove succeeded without a serving catalog, want the staging wait to fail")
	}
	if !strings.Contains(err.Error(), "catalog server pod") || !strings.Contains(err.Error(), "did not become Ready") {
		t.Errorf("error = %v, want the catalog server wait's failure", err)
	}
	// Nothing past the staging: no CatalogSource or Subscription was
	// applied against a catalog that never served.
	for _, call := range api.recordedCalls() {
		if strings.Contains(call, "catalogsources") || strings.Contains(call, "subscriptions") {
			t.Errorf("an OLM install object was applied though the catalog never served: %s", call)
		}
	}
}

// TestCountCheckResults pins the conclusion's counting: PASS and FAIL
// counted, everything else (the NOT-APPLICABLE an unshimmed cluster
// produces among them) reported as neither.
func TestCountCheckResults(t *testing.T) {
	item := func(status string) unstructured.Unstructured {
		obj := checkResult("x", status)
		return *obj
	}
	tests := []struct {
		name       string
		statuses   []string
		pass, fail int
	}{
		{"empty", nil, 0, 0},
		{"one of each", []string{checkStatusPass, checkStatusFail}, 1, 1},
		{"not applicable is neither", []string{"NOT-APPLICABLE", "INFO", ""}, 0, 0},
		{"mixed", []string{checkStatusPass, checkStatusPass, "ERROR", checkStatusFail}, 2, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items := make([]unstructured.Unstructured, 0, len(tt.statuses))
			for _, s := range tt.statuses {
				items = append(items, item(s))
			}
			pass, fail, _ := countCheckResults(items)
			if pass != tt.pass || fail != tt.fail {
				t.Errorf("countCheckResults(%v) = %d PASS, %d FAIL, want %d, %d", tt.statuses, pass, fail, tt.pass, tt.fail)
			}
		})
	}
}

// TestDefaultCatalogServerImageIsDigestPinned pins the shape of the proof's
// one external input: the catalog reference must carry a sha256
// digest, because the catalog bundles the operator, scanner, and
// content images — a floating tag here would float all three behind a
// green proof (AC-4.1). The pin's value is BRIEF §7.2's, carried
// verbatim; only its shape is asserted, so a digest bump does not
// touch this test.
// TestDefaultCatalogServerImageIsDigestPinned pins the shape of the
// proof's one pinned serving image: the reference must carry a sha256
// digest, because the image contributes the opm binary the proof's
// catalog resolution runs through — a floating tag here would float
// the server behind a green proof (AC-4.1). The pin's value is BRIEF
// §7.2's, carried verbatim; only its shape is asserted, so a digest
// bump does not touch this test.
func TestDefaultCatalogServerImageIsDigestPinned(t *testing.T) {
	_, digest, found := strings.Cut(DefaultCatalogServerImage, "@sha256:")
	if !found {
		t.Fatalf("DefaultCatalogServerImage %q carries no @sha256: digest", DefaultCatalogServerImage)
	}
	if len(digest) != 64 {
		t.Fatalf("DefaultCatalogServerImage's digest %q is %d characters, want 64", digest, len(digest))
	}
	for _, r := range digest {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			t.Fatalf("DefaultCatalogServerImage's digest %q carries %q, want lowercase hex only", digest, r)
		}
	}
}

// TestServingCertRoundTrip pins the keypair's usability contract: a
// generated pair validates as-is, an expired or mismatched or
// malformed pair is refused. The proof creates the secret once and
// never re-reads it; this oracle is what the happy-path test holds
// the landed secret against.
func TestServingCertRoundTrip(t *testing.T) {
	certPEM, keyPEM, err := generateServingCert(time.Now())
	if err != nil {
		t.Fatalf("generateServingCert: %v", err)
	}
	if err := validateServingCert(map[string][]byte{
		corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: keyPEM,
	}, time.Now()); err != nil {
		t.Errorf("a freshly generated keypair does not validate: %v", err)
	}

	// Expired: a certificate generated a validity and a day ago.
	expiredCert, expiredKey, err := generateServingCert(time.Now().Add(-servingCertValidity - 24*time.Hour))
	if err != nil {
		t.Fatalf("generateServingCert (aged): %v", err)
	}
	if err := validateServingCert(map[string][]byte{
		corev1.TLSCertKey: expiredCert, corev1.TLSPrivateKeyKey: expiredKey,
	}, time.Now()); err == nil {
		t.Error("an expired keypair validates, want the expiry refusal")
	}

	// A mismatched pair: the certificate of one keypair, the key of
	// another.
	_, otherKey, err := generateServingCert(time.Now())
	if err != nil {
		t.Fatalf("generateServingCert (second): %v", err)
	}
	if err := validateServingCert(map[string][]byte{
		corev1.TLSCertKey: certPEM, corev1.TLSPrivateKeyKey: otherKey,
	}, time.Now()); err == nil {
		t.Error("a certificate paired with another keypair's key validates, want the mismatch refusal")
	}

	for name, data := range map[string]map[string][]byte{
		"empty":            {},
		"no certificate":   {corev1.TLSPrivateKeyKey: keyPEM},
		"garbage":          {corev1.TLSCertKey: []byte("not pem"), corev1.TLSPrivateKeyKey: keyPEM},
		"not an x509 cert": {corev1.TLSCertKey: pemEncode([]byte("junk")), corev1.TLSPrivateKeyKey: keyPEM},
	} {
		if err := validateServingCert(data, time.Now()); err == nil {
			t.Errorf("%s: validateServingCert = nil, want a refusal", name)
		}
	}
}

// TestProveIsOneShot pins the one-shot contract: a second Prove on
// the same cluster is refused, not satisfied. The fake keeps
// everything the first proof left behind — the serving-cert secret,
// the resolved Subscription, the Succeeded CSV, the rolled-out
// Deployment, the READY binding, and its PASS/FAIL results — and no
// new results would ever arrive (the counters are zeroed), so without
// the refusal this second run would count the first run's scan as a
// fresh proof and return nil.
func TestProveIsOneShot(t *testing.T) {
	api := newFakeAPIServer(t)
	if err := runProve(t, api, testBudgets()); err != nil {
		t.Fatalf("first prove: %v", err)
	}
	api.passResults, api.failResults = 0, 0

	before := len(api.recordedCalls())
	err := runProve(t, api, testBudgets())
	if err == nil {
		t.Fatal("a second prove on the same cluster succeeded, want the one-shot refusal — it accepted the first run's results as a new proof")
	}
	if !strings.Contains(err.Error(), "one-shot") || !strings.Contains(err.Error(), servingCertSecret) {
		t.Errorf("error = %v, want the one-shot refusal naming the %s secret", err, servingCertSecret)
	}
	// The refusal precedes every mutation: the refused run may read
	// (the guard's secret GET), never write.
	for _, call := range api.recordedCalls()[before:] {
		if !strings.HasPrefix(call, "GET ") {
			t.Errorf("the refused second run mutated the cluster: %s", call)
		}
	}
}

// base64Decode decodes the standard-encoding payload the apiserver's
// JSON encoding of secret data carries.
func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// pemEncode wraps raw bytes in a PEM block, for the not-an-x509
// refusal.
func pemEncode(b []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b})
}

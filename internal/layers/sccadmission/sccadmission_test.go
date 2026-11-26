package sccadmission

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"

	"github.com/guzalv/kind-ocp/internal/kube"
	"github.com/guzalv/kind-ocp/internal/shell"
	"github.com/guzalv/kind-ocp/layers"
)

// fakeAPIServer is an in-memory apiserver carrying the API surface the
// layer touches. It stores what applies and creates write, answers the
// deployment reads with a rolled-out status (the rollout wait's
// condition), simulates the two actors the layer depends on — the
// admission webhook (mutating pod creates in a watched namespace) and
// the kubelet (running the mutated pod) — and records every request so
// the tests can assert the layer's cluster behavior without Docker.
type fakeAPIServer struct {
	server *httptest.Server

	mu                sync.Mutex
	store             map[string]*unstructured.Unstructured // object path → object
	calls             []string                              // "METHOD path" of every object request, in order
	podCreates        int
	podDeletes        int
	mutatePods        bool  // simulate the webhook: inject the UID on pod creates in selected namespaces
	mutateFromAttempt int   // when > 0, model the apiserver's config-propagation delay: creates before this attempt pass through unmutated
	podRuns           bool  // simulate the kubelet: answer pod reads with Running
	injectUID         int64 // what the simulated webhook injects
	failPuts          bool  // when set, secret writes fail with a 500
	stallPodCreates   bool  // when set, pod creates hang until the caller gives up (a stalled apiserver)
	failPodDeletes    bool  // when set, individual pod deletes fail with a 500
	// persistThenFailCreates, when > 0, persists the next that many
	// pod creates and then answers them with a 502: the client cannot
	// know whether its create landed — the ambiguous outcome the
	// probe must resolve by the name it retained.
	persistThenFailCreates int
	// delayedCommitCreates, when > 0, answers that many pod creates
	// with a 502 while holding the pod back: the object commits only
	// when its name is next read, so the resolving read finds the
	// name free and the create lands behind it — the shape of an
	// in-flight create committing after its own failure response.
	delayedCommitCreates int
	// pendingDelayedCommits holds the pods delayedCommitCreates held
	// back, keyed by their object path, until the read that commits
	// them.
	pendingDelayedCommits map[string]*unstructured.Unstructured
	// conflictCreatesWithDecoy, when > 0, plants a decoy under the
	// next that many creates' attempted name — a pod carrying the
	// probe's static app label but another attempt's ownership token,
	// the shape of an earlier run's leftover — and lets the held name
	// answer the create with AlreadyExists, the way a real apiserver
	// refuses a create onto a held name.
	conflictCreatesWithDecoy int
	// foreignSwapCreates, when > 0, answers that many pod creates
	// with a 502 while planting a foreign, probe-shaped pod under
	// the attempted name; the first read of that name answers with
	// the foreign holder and then swaps in the create's own pod —
	// the shape of a foreign holder going away between the read and
	// a still-pending create's commit.
	foreignSwapCreates int
	foreignSwaps       map[string]*unstructured.Unstructured
	// retryAfterCreates, when > 0, answers that many pod creates
	// with a 502 carrying Retry-After: 0 — the response client-go's
	// default retry policy replays, which would make one typed
	// Create call two wire POSTs under one name. The create is not
	// persisted: what the probe must do is surface the failure as
	// one POST and resolve the attempt it never saw answered.
	retryAfterCreates int
	// redirectPodCreates, when > 0, answers that many pod creates
	// with a 307 pointing at redirectTargetPath: Go's http.Client
	// follows such an answer by re-sending the original POST and
	// body to the target, no retry policy consulted. Nothing is
	// persisted by the redirect itself: what the probe must do is
	// refuse the redirect — one POST, nothing sent to the target,
	// the attempt resolved like any other unanswered create.
	redirectPodCreates int
	// redirectTargetHits counts the requests that reached the
	// redirect target: any hit at all means some client followed
	// the 307, which the probe's create must never.
	redirectTargetHits int
	// failPodReads, when set, fails pod reads with a 500 — an
	// apiserver too broken to resolve an ambiguous attempt.
	failPodReads bool
	// acceptWithoutDeleting, when set, answers pod deletes with an
	// accepted 202 while the object stays: deletion requested is not
	// deletion done.
	acceptWithoutDeleting bool
	// onDeploymentGet, when set, runs when the webhook Deployment is
	// read — the seam the kubeconfig-swap regressions use to rewrite
	// the kubeconfig file under a run that has already loaded it (the
	// rollout wait in converge and the first read in verify both land
	// here). It is set before the run starts, runs without the fake's
	// lock, and must not touch the fake's state.
	onDeploymentGet func()
	// mwcForeignConditions, when set, is the matchConditions list a
	// foreign field manager owns on the stored registration. The
	// layer's manifest carries no matchConditions, so a real
	// apiserver's server-side apply preserves that list across the
	// layer's re-apply; the fake's PATCH handler re-attaches it to the
	// registration's one webhook, which is the single SSA behavior the
	// fake models — scoped to this one object, because only the drift
	// regression needs a foreign field to outlive an apply. The
	// real-cluster integration test exercises the actual semantics.
	mwcForeignConditions []any
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	api := &fakeAPIServer{
		store:                 map[string]*unstructured.Unstructured{},
		pendingDelayedCommits: map[string]*unstructured.Unstructured{},
		foreignSwaps:          map[string]*unstructured.Unstructured{},
		mutatePods:            true,
		podRuns:               true,
		injectUID:             defaultRunAsUser,
	}
	mux := http.NewServeMux()

	// Discovery: the groups and resources the layer touches, with the
	// scoping the real APIs carry (the REST mapper derives it).
	mux.HandleFunc("/api", func(w http.ResponseWriter, _ *http.Request) {
		api.write(w, `{"kind":"APIVersions","versions":["v1"],
		"serverAddressByClientCIDRs":[{"clientCIDR":"0.0.0.0/0","serverAddress":"127.0.0.1"}]}`)
	})
	mux.HandleFunc("/apis", func(w http.ResponseWriter, _ *http.Request) {
		api.write(w, `{"kind":"APIGroupList","apiVersion":"v1","groups":[
		{"name":"apps","versions":[{"groupVersion":"apps/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apps/v1","version":"v1"}},
		{"name":"admissionregistration.k8s.io","versions":[{"groupVersion":"admissionregistration.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"admissionregistration.k8s.io/v1","version":"v1"}}]}`)
	})
	mux.HandleFunc("/api/v1", func(w http.ResponseWriter, _ *http.Request) {
		api.write(w, `{"kind":"APIResourceList","groupVersion":"v1","resources":[
		{"name":"namespaces","singularName":"namespace","namespaced":false,"kind":"Namespace","verbs":["get","patch"]},
		{"name":"pods","singularName":"pod","namespaced":true,"kind":"Pod","verbs":["get","create","delete","deletecollection"]},
		{"name":"secrets","singularName":"secret","namespaced":true,"kind":"Secret","verbs":["get","create","update"]},
		{"name":"services","singularName":"service","namespaced":true,"kind":"Service","verbs":["get","patch"]}]}`)
	})
	groupResources := map[string]string{
		"/apis/apps/v1": `{"kind":"APIResourceList","groupVersion":"apps/v1","resources":[
		{"name":"deployments","singularName":"deployment","namespaced":true,"kind":"Deployment","verbs":["get","patch"]}]}`,
		"/apis/admissionregistration.k8s.io/v1": `{"kind":"APIResourceList","groupVersion":"admissionregistration.k8s.io/v1","resources":[
		{"name":"mutatingwebhookconfigurations","singularName":"mutatingwebhookconfiguration","namespaced":false,"kind":"MutatingWebhookConfiguration","verbs":["get","patch"]}]}`,
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
	// The redirect target: a 307 from the pods endpoint points here.
	// Reaching it at all means some client followed the redirect,
	// which the probe's create must never.
	mux.HandleFunc(redirectTargetPath+"/", api.handleRedirectTarget)

	api.server = httptest.NewServer(mux)
	t.Cleanup(api.server.Close)
	return api
}

// redirectTargetPath is where redirectPodCreates points a followed
// POST. It is served the way the pods endpoint would serve the create
// — mutated, UID stamped, stored, 201 — which is what makes following
// the redirect dangerous: a client that re-sends the create lands the
// pod and passes the probe.
const redirectTargetPath = "/redirected-pod-create"

// handleRedirectTarget completes a create whose POST followed the 307.
// The regression counts hits (redirectTargetHits): the probe's create
// must never send this request.
func (a *fakeAPIServer) handleRedirectTarget(w http.ResponseWriter, r *http.Request) {
	var obj unstructured.Unstructured
	if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
		http.Error(w, fmt.Sprintf("decoding request body: %v", err), http.StatusBadRequest)
		return
	}
	ns := strings.TrimPrefix(r.URL.Path, redirectTargetPath+"/")
	key := "/api/v1/namespaces/" + ns + "/pods"
	a.mu.Lock()
	defer a.mu.Unlock()
	a.redirectTargetHits++
	// A UID of its own sequence, so a followed create's pod is never
	// confused with one the pods endpoint stamped.
	obj.SetUID(types.UID(fmt.Sprintf("fake-redirect-pod-uid-%d", a.redirectTargetHits)))
	a.admitPodCreate(key, &obj)
	a.store[key+"/"+obj.GetName()] = &obj
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	a.write(w, rawJSON(&obj))
}

// admitPodCreate applies the two actors to one pod create: the
// webhook's mutation (in a namespace the stored configuration
// selects, once the configuration has loaded — mutateFromAttempt
// models that delay, and on an object the stored objectSelector
// selects) and the apiserver's UID stamping. The caller holds a.mu.
func (a *fakeAPIServer) admitPodCreate(key string, obj *unstructured.Unstructured) {
	loaded := a.mutateFromAttempt == 0 || a.podCreates >= a.mutateFromAttempt
	if a.mutatePods && loaded && a.mwcSelectsNamespace(podNamespace(key)) && a.objectSelectsPod(obj) {
		_ = unstructured.SetNestedField(obj.Object, a.injectUID, "spec", "securityContext", "runAsUser")
		_ = unstructured.SetNestedField(obj.Object, a.injectUID, "spec", "securityContext", "runAsGroup")
		_ = unstructured.SetNestedField(obj.Object, a.injectUID, "spec", "securityContext", "fsGroup")
	}
	if obj.GetUID() == "" {
		obj.SetUID(types.UID(fmt.Sprintf("fake-pod-uid-%d", a.podCreates)))
	}
}

// handleObject serves one object request against the store.
func (a *fakeAPIServer) handleObject(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	a.calls = append(a.calls, r.Method+" "+strings.SplitN(r.URL.RequestURI(), "?", 2)[0])
	a.mu.Unlock()

	body := func() *unstructured.Unstructured {
		var obj unstructured.Unstructured
		if err := json.NewDecoder(r.Body).Decode(&obj); err != nil {
			http.Error(w, fmt.Sprintf("decoding request body: %v", err), http.StatusBadRequest)
			return nil
		}
		return &obj
	}
	key := strings.TrimSuffix(r.URL.Path, "/")

	switch r.Method {
	case http.MethodPatch:
		obj := body()
		if obj == nil {
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		// A patch stores what was applied, wholesale. The fake does
		// not model server-side apply's field-manager merge; the one
		// SSA behavior a regression needs — a foreign manager's field
		// surviving the layer's apply — is carried by
		// mwcForeignConditions on the registration alone, and the
		// real-cluster integration test covers the actual semantics.
		if len(a.mwcForeignConditions) > 0 && key == mwcKey {
			// The raw map walk, like the test tampers:
			// unstructured's Nested* helpers hand back copies, and
			// the planted field must reach the stored object.
			webhook := obj.Object["webhooks"].([]any)[0].(map[string]any)
			webhook["matchConditions"] = append([]any(nil), a.mwcForeignConditions...)
		}
		a.store[key] = obj
		a.write(w, rawJSON(obj))
	case http.MethodPost:
		if a.stallPodCreates && strings.HasSuffix(key, "/pods") {
			// A stalled apiserver: the create answers only when the
			// caller disconnects, which the probe's deadline context
			// forces — this is the regression for calls that must not
			// outlive the probe's budget. The body is drained first
			// so the server's background read can see the client go
			// and cancel the request context; an unread body would
			// leave the handler blocked past the test's cleanup.
			_, _ = io.Copy(io.Discard, r.Body)
			<-r.Context().Done()
			return
		}
		obj := body()
		if obj == nil {
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.failPuts && strings.Contains(key, "/secrets") {
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
		if strings.HasSuffix(key, "/pods") {
			// The simulated admission path: the create response is
			// the object after admission, carrying the injected UID.
			// The apiserver evaluates the MutatingWebhookConfiguration's
			// namespaceSelector against the pod's namespace (In on the
			// built-in name label); admitPodCreate models its delay
			// in loading a fresh configuration: below that attempt
			// number, creates pass through unmutated.
			a.podCreates++
			a.admitPodCreate(key, obj)
			if a.persistThenFailCreates > 0 {
				// Persisted, then answered with a 502 the client
				// cannot interpret: the create's outcome is unknown.
				a.persistThenFailCreates--
				a.store[key+"/"+obj.GetName()] = obj
				http.Error(w, "injected ambiguous failure", http.StatusBadGateway)
				return
			}
			if a.delayedCommitCreates > 0 {
				// Held back, then answered with the same 502: the pod
				// commits only when its name is next read — behind the
				// resolving read that finds the name free.
				a.delayedCommitCreates--
				a.pendingDelayedCommits[key+"/"+obj.GetName()] = obj
				http.Error(w, "injected ambiguous failure", http.StatusBadGateway)
				return
			}
			if a.foreignSwapCreates > 0 {
				// The ambiguous create is held back while a foreign,
				// probe-shaped pod holds the attempted name: the read
				// that would resolve the attempt observes the holder,
				// and the create commits behind it — the shape of a
				// holder going away between the read and a pending
				// create's commit.
				a.foreignSwapCreates--
				foreign := obj.DeepCopy()
				labels := foreign.GetLabels()
				labels[probeAttemptLabelKey] = decoyAttemptToken
				foreign.SetLabels(labels)
				foreign.SetUID(types.UID("fake-foreign-holder-uid"))
				a.store[key+"/"+obj.GetName()] = foreign
				a.foreignSwaps[key+"/"+obj.GetName()] = obj
				http.Error(w, "injected ambiguous failure", http.StatusBadGateway)
				return
			}
			if a.retryAfterCreates > 0 {
				// The failure client-go's default retry policy replays:
				// 5xx plus a Retry-After header. The probe's create
				// must surface it as one wire POST.
				a.retryAfterCreates--
				w.Header().Set("Retry-After", "0")
				http.Error(w, "injected retryable failure", http.StatusBadGateway)
				return
			}
			if a.redirectPodCreates > 0 {
				// The apiserver-shaped redirect: the POST's answer
				// points elsewhere. A client that follows it re-sends
				// the same create body to the target — the replay the
				// probe's create must refuse.
				a.redirectPodCreates--
				w.Header().Set("Location", redirectTargetPath+"/"+podNamespace(key))
				w.WriteHeader(http.StatusTemporaryRedirect)
				return
			}
			if a.conflictCreatesWithDecoy > 0 {
				// The decoy: probe-shaped, down to the static app
				// label, but carrying another attempt's token. The
				// exists-check below answers the create with the
				// conflict the held name earns.
				a.conflictCreatesWithDecoy--
				decoy := obj.DeepCopy()
				labels := decoy.GetLabels()
				labels[probeAttemptLabelKey] = decoyAttemptToken
				decoy.SetLabels(labels)
				decoy.SetUID(types.UID("fake-decoy-uid"))
				a.store[key+"/"+obj.GetName()] = decoy
			}
		}
		if _, exists := a.store[key+"/"+obj.GetName()]; exists {
			a.conflict(w, obj.GetName())
			return
		}
		a.store[key+"/"+obj.GetName()] = obj
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		a.write(w, rawJSON(obj))
	case http.MethodPut:
		obj := body()
		if obj == nil {
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.failPuts {
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
		a.store[key] = obj
		a.write(w, rawJSON(obj))
	case http.MethodGet:
		if a.onDeploymentGet != nil && key == deployKey {
			a.onDeploymentGet()
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.failPodReads && strings.Contains(key, "/pods/") {
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
		if pending, ok := a.pendingDelayedCommits[key]; ok {
			// The read that would resolve the ambiguous create sees
			// the name free; the create commits behind it.
			delete(a.pendingDelayedCommits, key)
			a.notFound(w, "no such object")
			a.store[key] = pending
			return
		}
		if ours, ok := a.foreignSwaps[key]; ok {
			// The resolving read observes the foreign holder under
			// the name; the holder then goes away and the
			// still-pending create commits behind the read.
			delete(a.foreignSwaps, key)
			if foreign, held := a.store[key]; held {
				a.store[key] = ours
				a.write(w, rawJSON(foreign))
				return
			}
		}
		stored, ok := a.store[key]
		if !ok {
			a.notFound(w, "no such object")
			return
		}
		switch {
		case strings.Contains(key, "/deployments/"):
			// The rollout wait's and Verify's condition: a deployment
			// the controller has fully rolled out and made available.
			synthesized := stored.DeepCopy()
			_ = unstructured.SetNestedField(synthesized.Object, synthesized.GetGeneration(), "status", "observedGeneration")
			for _, field := range []string{"updatedReplicas", "availableReplicas", "replicas"} {
				_ = unstructured.SetNestedField(synthesized.Object, int64(1), "status", field)
			}
			_ = unstructured.SetNestedSlice(synthesized.Object, []any{
				map[string]any{"type": "Available", "status": "True"},
			}, "status", "conditions")
			a.write(w, rawJSON(synthesized))
		case strings.Contains(key, "/pods/"):
			// The kubelet's answer: a mutated pod runs; an unmutated
			// one stays Pending with the refusal the kubelet records.
			synthesized := stored.DeepCopy()
			if a.podRuns {
				_ = unstructured.SetNestedField(synthesized.Object, "Running", "status", "phase")
			} else {
				_ = unstructured.SetNestedField(synthesized.Object, "Pending", "status", "phase")
				_ = unstructured.SetNestedSlice(synthesized.Object, []any{
					map[string]any{
						"name": "probe",
						"state": map[string]any{
							"waiting": map[string]any{
								"reason":  "CreateContainerError",
								"message": "container has runAsNonRoot and image will run as root",
							},
						},
					},
				}, "status", "containerStatuses")
			}
			a.write(w, rawJSON(synthesized))
		default:
			a.write(w, rawJSON(stored))
		}
	case http.MethodDelete:
		// Delete options travel in the body; the preconditions are
		// what the probe's cleanup pins its ownership on.
		var opts struct {
			Preconditions *struct {
				UID types.UID `json:"uid"`
			} `json:"preconditions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&opts)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.failPodDeletes && strings.Contains(key, "/pods/") {
			http.Error(w, "injected failure", http.StatusInternalServerError)
			return
		}
		if strings.HasSuffix(key, "/pods") {
			// A pod-collection delete: nothing in the layer issues one
			// anymore (the deployment template's fingerprint rolls the
			// pods), but the handler keeps the shape honest.
			for k := range a.store {
				if strings.HasPrefix(k, key+"/") {
					delete(a.store, k)
					a.podDeletes++
				}
			}
			a.write(w, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
			return
		}
		stored, exists := a.store[key]
		if !exists {
			a.notFound(w, "no such object")
			return
		}
		if opts.Preconditions != nil && opts.Preconditions.UID != "" && stored.GetUID() != opts.Preconditions.UID {
			// The apiserver's UID precondition: the name is held by a
			// different object than the one the caller read.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Conflict","code":409,
				"message":"the UID in the precondition does not match the stored object"}`)
			return
		}
		if a.acceptWithoutDeleting && strings.Contains(key, "/pods/") {
			// The delete was accepted (202) and the object stays:
			// whatever holds it has not let go.
			a.podDeletes++
			w.WriteHeader(http.StatusAccepted)
			a.write(w, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
			return
		}
		delete(a.store, key)
		if strings.Contains(key, "/pods/") {
			a.podDeletes++
		}
		a.write(w, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
	default:
		http.Error(w, "unexpected method "+r.Method, http.StatusMethodNotAllowed)
	}
}

// mwcSelectsNamespace models the apiserver's evaluation of the stored
// MutatingWebhookConfiguration's namespaceSelector: the built-in
// kubernetes.io/metadata.name label matched with In against the
// configured list. Reading the selector from the stored object — what
// the layer applied — is what makes the namespace-switch regressions
// meaningful: the fake admits by what the configuration actually says,
// not by what the test believes it says. The caller holds a.mu.
func (a *fakeAPIServer) mwcSelectsNamespace(namespace string) bool {
	mwc, ok := a.store[mwcKey]
	if !ok {
		return false
	}
	webhooks, _, _ := unstructured.NestedSlice(mwc.Object, "webhooks")
	if len(webhooks) == 0 {
		return false
	}
	first, ok := webhooks[0].(map[string]any)
	if !ok {
		return false
	}
	expressions, _, _ := unstructured.NestedSlice(first, "namespaceSelector", "matchExpressions")
	for _, raw := range expressions {
		expr, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if key, _ := expr["key"].(string); key != "kubernetes.io/metadata.name" {
			continue
		}
		if op, _ := expr["operator"].(string); op != "In" {
			continue
		}
		values, _ := expr["values"].([]any)
		for _, raw := range values {
			if value, _ := raw.(string); value == namespace {
				return true
			}
		}
	}
	return false
}

// objectSelectsPod models the apiserver's evaluation of the stored
// registration's objectSelector, which runs against the created
// object before the webhook is called: a missing or empty selector
// matches every object (the API defaults to the empty selector), while
// a nonempty one requires every matchLabel on the object — the shape a
// drifted registration can use to select the probe's own pods while
// ordinary pods pass unmutated. Only matchLabels is modeled; nothing
// the tests plant needs expressions. The caller holds a.mu.
func (a *fakeAPIServer) objectSelectsPod(obj *unstructured.Unstructured) bool {
	mwc, ok := a.store[mwcKey]
	if !ok {
		return false
	}
	webhooks, _, _ := unstructured.NestedSlice(mwc.Object, "webhooks")
	if len(webhooks) == 0 {
		return false
	}
	first, ok := webhooks[0].(map[string]any)
	if !ok {
		return false
	}
	selector, found, _ := unstructured.NestedStringMap(first, "objectSelector", "matchLabels")
	if !found || len(selector) == 0 {
		return true
	}
	labels := obj.GetLabels()
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

// podNamespace extracts the namespace from one object path
// (/api/v1/namespaces/<ns>/pods).
func podNamespace(key string) string {
	rest := strings.TrimPrefix(key, "/api/v1/namespaces/")
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[:i]
	}
	return rest
}

func (a *fakeAPIServer) conflict(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"AlreadyExists","code":409,
		"details":{"name":%q},"message":%q}`, name, name+" already exists")
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

// callsFrom returns the recorded object requests from the given index
// on, so a test can look at only one converge's requests.
func (a *fakeAPIServer) callsFrom(start int) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.calls[start:]...)
}

// callCount returns the number of requests recorded so far.
func (a *fakeAPIServer) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

// mustStored returns the object stored at key or fails the test.
func (a *fakeAPIServer) mustStored(t *testing.T, key string) *unstructured.Unstructured {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	obj, ok := a.store[key]
	if !ok {
		t.Fatalf("no object stored at %s", key)
	}
	return obj
}

// storedOK reports whether an object is stored at key.
func (a *fakeAPIServer) storedOK(key string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.store[key]
	return ok
}

// replaceStored overwrites the object stored at key.
func (a *fakeAPIServer) replaceStored(key string, obj *unstructured.Unstructured) {
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
	return layers.Env{
		ClusterName:    "kind-ocp-1",
		KubeconfigPath: kubeconfig,
		Log:            slog.New(slog.DiscardHandler),
	}
}

// plantKubeSystem stores a kube-system namespace carrying the given
// instance UID in the fake apiserver — the cluster identity the
// instance binding reads (kube.ClusterInstanceID).
func plantKubeSystem(t *testing.T, api *fakeAPIServer, uid string) {
	t.Helper()
	api.replaceStored("/api/v1/namespaces/kube-system", mustUnstructured(&corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(uid)},
	}))
}

// rewriteKubeconfig points an existing kubeconfig file at serverURL —
// the same file, atomically replaced: the shape of a same-name cluster
// replacement re-exporting a different cluster's credential over the
// state path mid-run.
func rewriteKubeconfig(t *testing.T, path, serverURL string) {
	t.Helper()
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
}

// convergeLayer runs the layer's converge against the fake apiserver,
// with a probe budget short enough for a failing test but long enough
// for the probe's one-second retry interval to elapse once — the
// propagation-delay regression needs two attempts. It returns the
// number of recorded requests before the run, so the caller can look
// at only this run's requests.
func convergeLayer(t *testing.T, api *fakeAPIServer, s settings) (callsBefore int, err error) {
	t.Helper()
	kubeconfig := writeKubeconfig(t, api.server.URL)
	// The production shape: one load of the kubeconfig builds the one
	// no-redirect client set the run uses.
	cfg, err := kube.RESTConfig(kubeconfig)
	if err != nil {
		t.Fatalf("loading the REST config: %v", err)
	}
	client, err := kube.NewNoRedirectClientFromConfig(cfg)
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	callsBefore = api.callCount()
	return callsBefore, (Layer{}).converge(context.Background(), testEnv(kubeconfig), client, s, imageRef(s), 3*time.Second)
}

// The paths the tests read objects at.
const (
	secretKey  = "/api/v1/namespaces/scc-admission/secrets/scc-admission-tls"
	mwcKey     = "/apis/admissionregistration.k8s.io/v1/mutatingwebhookconfigurations/scc-admission"
	deployKey  = "/apis/apps/v1/namespaces/scc-admission/deployments/scc-admission-webhook"
	watchedNS  = "/api/v1/namespaces/openshift-compliance"
	webhookNS  = "/api/v1/namespaces/scc-admission"
	probePodNS = "/api/v1/namespaces/openshift-compliance/pods"
)

func TestConverge(t *testing.T) {
	api := newFakeAPIServer(t)
	s := defaultSettings()
	if _, err := convergeLayer(t, api, s); err != nil {
		t.Fatalf("converge: %v", err)
	}

	t.Run("creates the namespaces", func(t *testing.T) {
		if !api.storedOK(watchedNS) {
			t.Error("the watched namespace was not created")
		}
		if !api.storedOK(webhookNS) {
			t.Error("the webhook's own namespace was not created")
		}
	})

	t.Run("writes a usable TLS secret", func(t *testing.T) {
		if err := validateSecretData(storedSecretData(t, api), time.Now()); err != nil {
			t.Errorf("the created secret does not validate: %v", err)
		}
	})

	t.Run("applies the deployment, service, and webhook configuration", func(t *testing.T) {
		deploy := api.mustStored(t, deployKey)
		container := firstContainer(t, deploy.Object)
		if image, _ := container["image"].(string); image != imageRef(s) {
			t.Errorf("deployment image = %q, want the locally built %q", image, imageRef(s))
		}
		if policy, _ := container["imagePullPolicy"].(string); policy != "Never" {
			t.Errorf("imagePullPolicy = %q, want Never (the image is only on the node)", policy)
		}
		if uid := containerEnvValue(t, container, envRunAsUserName); uid != "1001" {
			t.Errorf("%s = %q, want 1001", envRunAsUserName, uid)
		}
		// The pod template carries the fingerprint of the secret's
		// material: the roll trigger for a changed secret.
		fingerprint, _, _ := unstructured.NestedString(deploy.Object, "spec", "template", "metadata", "annotations", tlsFingerprintAnnotation)
		if fingerprint != tlsFingerprint(storedSecretData(t, api)) {
			t.Errorf("pod template TLS fingerprint = %q, want the secret's %q", fingerprint, tlsFingerprint(storedSecretData(t, api)))
		}
		if !api.storedOK("/api/v1/namespaces/scc-admission/services/scc-admission-webhook") {
			t.Error("the webhook Service was not applied; its shape is pinned by TestRenderedObjects")
		}
		mwc := api.mustStored(t, mwcKey)
		webhook := firstWebhook(t, mwc.Object)
		if policy, _ := webhook["failurePolicy"].(string); policy != "Ignore" {
			t.Errorf("failurePolicy = %q, want Ignore (the documented trade-off)", policy)
		}
		// The bundled CA is the one the secret carries: the apiserver
		// must trust exactly what the webhook serves.
		caBundle, _ := nestedValue(t, webhook, "clientConfig", "caBundle").(string)
		decoded, err := base64.StdEncoding.DecodeString(caBundle)
		if err != nil || string(decoded) != string(storedSecretData(t, api)[secretCAKey]) {
			t.Errorf("the caBundle does not match the secret's CA: %d bytes vs %d", len(decoded), len(storedSecretData(t, api)[secretCAKey]))
		}
	})

	t.Run("probes the watched namespace and cleans up", func(t *testing.T) {
		api.mu.Lock()
		creates, deletes := api.podCreates, api.podDeletes
		api.mu.Unlock()
		if creates == 0 {
			t.Fatal("no probe pod was created")
		}
		if deletes != creates {
			t.Errorf("probe pod deletes = %d, want %d (every probe pod is deleted)", deletes, creates)
		}
	})
}

// A re-run keeps a still-valid secret: no update, no template change
// (so no rollout), no webhook pod churn. This is the PoC bug the layer
// exists not to have — the PoC regenerated the certificate on every
// run, leaving the running webhook serving a key the configuration no
// longer trusted.
func TestConvergeKeepsAValidSecret(t *testing.T) {
	api := newFakeAPIServer(t)
	s := defaultSettings()
	if _, err := convergeLayer(t, api, s); err != nil {
		t.Fatalf("first converge: %v", err)
	}
	fingerprint := storedTemplateFingerprint(t, api)
	before, err := convergeLayer(t, api, s)
	if err != nil {
		t.Fatalf("second converge: %v", err)
	}
	for _, call := range api.callsFrom(before) {
		if strings.HasPrefix(call, "PUT") || strings.HasPrefix(call, "POST /api/v1/namespaces/scc-admission/secrets") {
			t.Errorf("the re-run rewrote the secret: %s (a valid secret must be kept)", call)
		}
		if strings.HasPrefix(call, "DELETE /api/v1/namespaces/scc-admission/pods") {
			t.Errorf("the re-run restarted the webhook: %s", call)
		}
	}
	if storedTemplateFingerprint(t, api) != fingerprint {
		t.Error("the re-run changed the pod template's TLS fingerprint; a kept secret must not roll the deployment")
	}
}

// An unusable secret is regenerated, and the regeneration reaches the
// cluster only through the deployment template's fingerprint: the layer
// never deletes the webhook's pods itself — the Deployment controller
// rolls them onto the changed template.
func TestConvergeRegeneratesAnUnusableSecret(t *testing.T) {
	api := newFakeAPIServer(t)
	s := defaultSettings()
	if _, err := convergeLayer(t, api, s); err != nil {
		t.Fatalf("first converge: %v", err)
	}
	oldFingerprint := storedTemplateFingerprint(t, api)
	// Tamper: a secret whose key no longer matches its certificate.
	tampered := api.mustStored(t, secretKey).DeepCopy()
	data := storedSecretData(t, api)
	data["tls.key"] = []byte("not a key")
	writeStoredSecretData(t, tampered, data)
	api.replaceStored(secretKey, tampered)

	before, err := convergeLayer(t, api, s)
	if err != nil {
		t.Fatalf("converge after tampering: %v", err)
	}
	var sawUpdate bool
	for _, call := range api.callsFrom(before) {
		if call == "PUT "+secretKey {
			sawUpdate = true
		}
		if strings.HasPrefix(call, "DELETE /api/v1/namespaces/scc-admission/pods") {
			t.Errorf("the layer deleted the webhook's pods itself (%s); the template fingerprint is the roll mechanism", call)
		}
	}
	if !sawUpdate {
		t.Error("the unusable secret was not rewritten")
	}
	if storedTemplateFingerprint(t, api) == oldFingerprint {
		t.Error("the regenerated secret left the pod template's fingerprint unchanged; the pods would keep serving the replaced material")
	}
	if storedTemplateFingerprint(t, api) != tlsFingerprint(storedSecretData(t, api)) {
		t.Error("the pod template does not carry the regenerated secret's fingerprint")
	}
	if err := validateSecretData(storedSecretData(t, api), time.Now()); err != nil {
		t.Errorf("the regenerated secret does not validate: %v", err)
	}
}

// A valid replacement secret written by an interrupted run — the secret
// was rewritten but the rollout never happened, so the pod template
// still carries the old fingerprint while the running pod serves the
// old material — is adopted on retry: not rewritten again, and the
// template now carries its fingerprint, which is exactly what rolls the
// still-serving-old-material pod in a real cluster.
func TestConvergeAdoptsAReplacementSecret(t *testing.T) {
	api := newFakeAPIServer(t)
	s := defaultSettings()
	if _, err := convergeLayer(t, api, s); err != nil {
		t.Fatalf("first converge: %v", err)
	}
	oldFingerprint := storedTemplateFingerprint(t, api)

	// The interruption: a fresh, valid secret in place of the old one,
	// with the deployment template still carrying the old fingerprint.
	replacement, err := generateTLS(time.Now())
	if err != nil {
		t.Fatalf("generating the replacement material: %v", err)
	}
	secret := api.mustStored(t, secretKey).DeepCopy()
	writeStoredSecretData(t, secret, map[string][]byte{
		secretCAKey: replacement.caCertPEM,
		"tls.crt":   replacement.tlsCertPEM,
		"tls.key":   replacement.tlsKeyPEM,
	})
	api.replaceStored(secretKey, secret)

	before, err := convergeLayer(t, api, s)
	if err != nil {
		t.Fatalf("converge after the replacement: %v", err)
	}
	for _, call := range api.callsFrom(before) {
		if strings.HasPrefix(call, "PUT") || strings.HasPrefix(call, "POST /api/v1/namespaces/scc-admission/secrets") {
			t.Errorf("the re-run rewrote a valid replacement secret: %s", call)
		}
	}
	newFingerprint := storedTemplateFingerprint(t, api)
	if newFingerprint == oldFingerprint {
		t.Error("the pod template kept the old fingerprint; the interrupted rollout never converges")
	}
	if newFingerprint != tlsFingerprint(storedSecretData(t, api)) {
		t.Error("the pod template does not carry the replacement secret's fingerprint")
	}
}

// Switching watchedNamespaces re-scopes the webhook: the new namespace
// is created and mutated, the old one is left in place and admitted
// unmutated — the scope is the configuration, not a label the old
// namespace still carries.
func TestConvergeSwitchesWatchedNamespaces(t *testing.T) {
	api := newFakeAPIServer(t)
	first := defaultSettings()
	first.WatchedNamespaces = []string{"scc-watch-a"}
	if _, err := convergeLayer(t, api, first); err != nil {
		t.Fatalf("converge watching scc-watch-a: %v", err)
	}

	second := defaultSettings()
	second.WatchedNamespaces = []string{"scc-watch-b"}
	if _, err := convergeLayer(t, api, second); err != nil {
		t.Fatalf("converge watching scc-watch-b: %v", err)
	}

	// The stored configuration selects exactly the new list…
	mwc := api.mustStored(t, mwcKey)
	webhook := firstWebhook(t, mwc.Object)
	expression, _, _ := unstructured.NestedSlice(webhook, "namespaceSelector", "matchExpressions")
	var selected []string
	if len(expression) == 1 {
		if expr, ok := expression[0].(map[string]any); ok {
			if raw, ok := expr["values"].([]any); ok {
				for _, v := range raw {
					if s, ok := v.(string); ok {
						selected = append(selected, s)
					}
				}
			}
		}
	}
	if len(selected) != 1 || selected[0] != "scc-watch-b" {
		t.Errorf("namespaceSelector values = %v, want exactly [scc-watch-b]", selected)
	}
	// …the old namespace is left in place, merely unwatched…
	if !api.storedOK("/api/v1/namespaces/scc-watch-a") {
		t.Error("the previously watched namespace was deleted; the layer must not remove namespaces")
	}
	// …and the simulated apiserver, evaluating the stored selector,
	// admits by it: the old namespace unmutated, the new one mutated.
	if pod := createPodThrough(t, api, "scc-watch-a", "switch-check-old"); pod.Spec.SecurityContext != nil && pod.Spec.SecurityContext.RunAsUser != nil {
		t.Errorf("a pod in the removed namespace was mutated to runAsUser %d; the scope must follow the configuration", *pod.Spec.SecurityContext.RunAsUser)
	}
	if pod := createPodThrough(t, api, "scc-watch-b", "switch-check-new"); pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.RunAsUser == nil || *pod.Spec.SecurityContext.RunAsUser != defaultRunAsUser {
		t.Errorf("a pod in the newly watched namespace carries securityContext %+v, want the injected UID %d", pod.Spec.SecurityContext, defaultRunAsUser)
	}
}

// A registration field another field manager owns survives the
// layer's apply — server-side apply preserves it, because the layer's
// manifest does not carry it — so Apply must not report convergence
// from a passing probe alone: the stored registration is read back
// after the apply, and any drift fails the create before a probe pod
// exists. The field is left in place (the layer never removes fields
// it does not own), and the error names the drift and the recovery.
func TestConvergeFailsWhenTheApplyCannotConvergeTheRegistration(t *testing.T) {
	api := newFakeAPIServer(t)
	s := defaultSettings()
	if _, err := convergeLayer(t, api, s); err != nil {
		t.Fatalf("first converge: %v", err)
	}
	// A separately managed matchCondition, the shape a foreign field
	// manager leaves: absent from the layer's manifest, so the apply
	// that follows preserves it — the fake models exactly that for
	// this object (mwcForeignConditions re-attaches the list at the
	// apply), and the real-cluster integration test exercises the
	// actual server-side-apply semantics.
	api.mu.Lock()
	api.mwcForeignConditions = []any{map[string]any{"name": "foreign.example", "expression": "true"}}
	api.mu.Unlock()

	before := api.callCount()
	_, err := convergeLayer(t, api, s)
	if err == nil {
		t.Fatal("converge succeeded over a registration another field manager had drifted, want the drift to fail it")
	}
	for _, want := range []string{
		"is not the registration the layer applies",
		"matchConditions",
		"never removes fields it does not own",
		"re-run create",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("converge = %v, want %q in the error", err, want)
		}
	}
	if posts := countPodPosts(api.callsFrom(before)); posts != 0 {
		t.Errorf("probe creates = %d, want 0 (the drift gate fires before the probe)", posts)
	}
	// The field survived the apply: removing another manager's work is
	// the recovery's call, not the layer's.
	if conditions, ok := api.mustStored(t, mwcKey).Object["webhooks"].([]any)[0].(map[string]any)["matchConditions"].([]any); !ok || len(conditions) != 1 {
		t.Errorf("the stored matchConditions = %v, want the one foreign condition preserved (the layer must not remove fields it does not own)", conditions)
	}

	// The drift is invisible to the probe — the demonstration that the
	// gate is load-bearing: under the drifted registration a
	// probe-shaped pod in the watched namespace is still mutated (the
	// fake does not evaluate CEL, and an expression of true narrows
	// nothing on a real apiserver either), so without the gate the
	// create would have reported success and left the drift for Verify
	// to find.
	if pod := createPodThrough(t, api, "openshift-compliance", "foreign-condition-probe-shaped"); pod.Spec.SecurityContext == nil ||
		pod.Spec.SecurityContext.RunAsUser == nil || *pod.Spec.SecurityContext.RunAsUser != defaultRunAsUser {
		t.Fatal("the fixture lost the false-pass shape: a probe-shaped pod under the drifted registration must still be mutated")
	}
	// Keep the demonstration pod out of the store.
	api.mu.Lock()
	delete(api.store, "/api/v1/namespaces/openshift-compliance/pods/foreign-condition-probe-shaped")
	api.mu.Unlock()
}

// The drift failure's recovery must be scoped to the failed cluster.
// A prefix can carry several clusters, and the operator's ambient
// KUBECONFIG can point at any of them: an unscoped "kubectl delete
// mutatingwebhookconfiguration" pasted into that shell deletes a
// healthy cluster's registration while the drifted cluster keeps its
// foreign field. The error therefore names the failed cluster and
// renders the delete against the kubeconfig that run used — the
// credential of exactly that cluster, the one the drift was read
// through — and prints no kubectl command that would follow the
// ambient context.
func TestConvergeDriftErrorIsScopedToTheFailedCluster(t *testing.T) {
	// The healthy cluster the ambient KUBECONFIG points at: converged,
	// carrying the layer's own registration.
	healthy := newFakeAPIServer(t)
	if _, err := convergeLayer(t, healthy, defaultSettings()); err != nil {
		t.Fatalf("converging the healthy cluster: %v", err)
	}
	t.Setenv("KUBECONFIG", writeKubeconfig(t, healthy.server.URL))

	// The drifted cluster: converged once, then a foreign field
	// manager's matchCondition on its registration.
	drifted := newFakeAPIServer(t)
	s := defaultSettings()
	if _, err := convergeLayer(t, drifted, s); err != nil {
		t.Fatalf("first converge: %v", err)
	}
	drifted.mu.Lock()
	drifted.mwcForeignConditions = []any{map[string]any{"name": "foreign.example", "expression": "true"}}
	drifted.mu.Unlock()

	kubeconfig := writeKubeconfig(t, drifted.server.URL)
	cfg, err := kube.RESTConfig(kubeconfig)
	if err != nil {
		t.Fatalf("loading the REST config: %v", err)
	}
	client, err := kube.NewNoRedirectClientFromConfig(cfg)
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	env := testEnv(kubeconfig)
	err = (Layer{}).converge(context.Background(), env, client, s, imageRef(s), 3*time.Second)
	if err == nil {
		t.Fatal("converge succeeded over a registration another field manager had drifted, want the drift to fail it")
	}
	// The failed cluster is named, and the delete the error renders
	// runs through that cluster's kubeconfig — the one kubeconfig the
	// run resolved, never the ambient one the shell carries.
	scoped := "kubectl --kubeconfig " + shell.Quote(kubeconfig) + " delete mutatingwebhookconfiguration " + webhookConfig
	for _, want := range []string{"on cluster " + env.ClusterName, scoped} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("converge = %v\nwant %q in the error (the recovery must be scoped to the failed cluster)", err, want)
		}
	}
	if strings.Contains(err.Error(), healthy.server.URL) {
		t.Errorf("converge = %v\nthe error names the healthy cluster the ambient KUBECONFIG points at; the recovery must not reach it", err)
	}
	if strings.Contains(err.Error(), "kubectl delete mutatingwebhookconfiguration") {
		t.Errorf("converge = %v\nthe error prints an unscoped kubectl delete, which follows the ambient KUBECONFIG and can remove another cluster's registration", err)
	}
}

// TestConvergeDriftErrorIsScopedToTheFailedClusterWithQuotingRequiringPath
// re-runs the scoped-recovery regression with a kubeconfig path that
// contains a space and an apostrophe: the error's recovery command
// interpolates the path through POSIX shell quoting, and a shell-safe
// fixture would not notice the quoting being removed.
func TestConvergeDriftErrorQuotesAPathNeedingIt(t *testing.T) {
	// The healthy cluster the ambient KUBECONFIG points at.
	healthy := newFakeAPIServer(t)
	if _, err := convergeLayer(t, healthy, defaultSettings()); err != nil {
		t.Fatalf("converging the healthy cluster: %v", err)
	}
	t.Setenv("KUBECONFIG", writeKubeconfig(t, healthy.server.URL))

	drifted := newFakeAPIServer(t)
	s := defaultSettings()
	if _, err := convergeLayer(t, drifted, s); err != nil {
		t.Fatalf("first converge: %v", err)
	}
	drifted.mu.Lock()
	drifted.mwcForeignConditions = []any{map[string]any{"name": "foreign.example", "expression": "true"}}
	drifted.mu.Unlock()

	// The path that needs quoting: a space and an apostrophe, both of
	// which break an unquoted shell word.
	dir := filepath.Join(t.TempDir(), "state root's")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	kubeconfig := filepath.Join(dir, "kubeconfig")
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
`, drifted.server.URL)
	if err := os.WriteFile(kubeconfig, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := kube.RESTConfig(kubeconfig)
	if err != nil {
		t.Fatalf("loading the REST config: %v", err)
	}
	client, err := kube.NewNoRedirectClientFromConfig(cfg)
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	env := testEnv(kubeconfig)
	err = (Layer{}).converge(context.Background(), env, client, s, imageRef(s), 3*time.Second)
	if err == nil {
		t.Fatal("converge succeeded over a registration another field manager had drifted, want the drift to fail it")
	}
	// The scoped command must carry the path intact and quoted: the
	// apostrophe alone breaks single-quoting that does not escape it.
	scoped := "kubectl --kubeconfig " + shell.Quote(kubeconfig) + " delete mutatingwebhookconfiguration " + webhookConfig
	if !strings.Contains(err.Error(), scoped) {
		t.Errorf("converge = %v\nwant the POSIX-quoted %q in the error (a path with a space and an apostrophe must survive copy-paste)", err, scoped)
	}
}

// storedTemplateFingerprint returns the TLS fingerprint the stored
// deployment's pod template carries.
func storedTemplateFingerprint(t *testing.T, api *fakeAPIServer) string {
	t.Helper()
	deploy := api.mustStored(t, deployKey)
	fingerprint, _, _ := unstructured.NestedString(deploy.Object, "spec", "template", "metadata", "annotations", tlsFingerprintAnnotation)
	return fingerprint
}

// createPodThrough drives one probe-shaped pod create straight through
// the fake's simulated admission path — the way the real apiserver
// evaluates the MutatingWebhookConfiguration's namespaceSelector — and
// returns the created object as the API answered it.
func createPodThrough(t *testing.T, api *fakeAPIServer, namespace, name string) *corev1.Pod {
	t.Helper()
	client, err := kube.NewClient(writeKubeconfig(t, api.server.URL))
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	pod, err := client.Clientset.CoreV1().Pods(namespace).Create(
		context.Background(), probePod(name, "test-attempt", imageRef(defaultSettings())), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("creating the pod in %s: %v", namespace, err)
	}
	return pod
}

// A probe pod the webhook does not mutate fails the converge with the
// cause named — the admission path is part of Apply's contract, not
// something Verify alone discovers.
func TestConvergeFailsWhenTheWebhookDoesNotMutate(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.mutatePods = false
	api.mu.Unlock()
	_, err := convergeLayer(t, api, defaultSettings())
	if err == nil || !strings.Contains(err.Error(), "admitted without a runAsUser") {
		t.Fatalf("converge = %v, want the unmutated probe pod to fail it", err)
	}
}

// laggingDeadlineCtx carries a deadline its cancellation never
// enforces: Done never closes and Err stays nil. This is the state
// the probe's context is in at the budget boundary — the deadline
// passed by the clock while the context's timer goroutine has not
// landed yet — held still, so the boundary behaves deterministically
// instead of racing.
type laggingDeadlineCtx struct{ deadline time.Time }

func (c laggingDeadlineCtx) Deadline() (time.Time, bool) { return c.deadline, true }
func (c laggingDeadlineCtx) Done() <-chan struct{}       { return nil }
func (c laggingDeadlineCtx) Err() error                  { return nil }
func (c laggingDeadlineCtx) Value(any) any               { return nil }

// The budget's boundary, made deterministic: the deadline passes by
// the clock while the context has not reported it, and the probe's
// next create fails there — client-go's rate limiter refuses a
// request that would wait past the deadline, as an ordinary request
// error, not a context one. That artifact must not displace the
// observation the probe recorded before the boundary (the live
// flake: "admitted without a runAsUser" replaced by the rate
// limiter's text). The first attempt lands inside the budget and
// records the observation; the retry pause is clamped to the
// remaining budget, so the second attempt starts exactly at the
// deadline, where the create fails while ctx.Err() is still nil.
func TestAwaitMutationKeepsTheObservationAtTheBudgetBoundary(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.mutatePods = false
	api.mu.Unlock()
	probe := fakeNoRedirectClientset(t, api)
	ctx := laggingDeadlineCtx{deadline: time.Now().Add(time.Second)}
	_, attempts, err := awaitMutation(ctx, probe, imageRef(defaultSettings()), "openshift-compliance")
	if err == nil {
		t.Fatal("awaitMutation succeeded against a non-mutating admission path, want failure")
	}
	if attempts < 2 {
		t.Errorf("attempts = %d, want at least the one that recorded the observation and the one that hit the boundary", attempts)
	}
	if !strings.Contains(err.Error(), "admitted without a runAsUser") {
		t.Fatalf("awaitMutation = %v\nwant the recorded observation to survive the boundary create failure", err)
	}
	assertNoPodsStored(t, api)
}

// The apiserver loads a fresh MutatingWebhookConfiguration on a delay
// of its own; the probe retries the admission path within its budget
// instead of failing on the first unmutated create. This is the race
// seen live: the rollout was complete, the configuration applied
// seconds earlier, and the first probe still passed through
// unmutated.
func TestConvergeRetriesUntilAdmissionIntercepts(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.mutateFromAttempt = 2 // the first create passes through unmutated
	api.mu.Unlock()
	if _, err := convergeLayer(t, api, defaultSettings()); err != nil {
		t.Fatalf("converge = %v, want the probe to keep trying until the webhook intercepts", err)
	}
	api.mu.Lock()
	creates := api.podCreates
	api.mu.Unlock()
	if creates < 2 {
		t.Errorf("probe attempts = %d, want at least the two the propagation delay costs", creates)
	}
}

// A stalled apiserver costs the probe its budget, not the run: the
// create attempt itself runs under the probe's deadline context, so it
// is cancelled at the budget instead of hanging the create. The
// cancelled create's outcome is unknown — it may still commit — so the
// probe fails resolving it, naming the pod, within the resolve budget.
func TestConvergeProbeIsBoundedAgainstAStalledAPI(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.stallPodCreates = true
	api.mu.Unlock()
	started := time.Now()
	_, err := convergeLayer(t, api, defaultSettings()) // a 3s probe budget
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("converge against a stalled apiserver succeeded, want the bounded probe to fail it")
	}
	for _, want := range []string{"could not be resolved", "kind-ocp-scc-probe-"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("converge = %v, want %q named as the cause", err, want)
		}
	}
	if elapsed > 10*time.Second {
		t.Fatalf("converge took %s against a stalled API, want it bounded near the probe plus resolve budgets", elapsed)
	}
}

// A probe that cannot delete its pod fails the check and names the
// leftover: a PASS that leaves a pod behind proves nothing it can be
// trusted on.
func TestConvergeFailsWhenTheProbePodCannotBeCleanedUp(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.failPodDeletes = true
	api.mu.Unlock()
	_, err := convergeLayer(t, api, defaultSettings())
	if err == nil || !strings.Contains(err.Error(), "cleaning up the probe pod") {
		t.Fatalf("converge = %v, want the cleanup failure surfaced", err)
	}
	if !strings.Contains(err.Error(), "kind-ocp-scc-probe-") {
		t.Errorf("converge = %v, want the leftover pod named in the error", err)
	}
}

// A create that fails ambiguously — the apiserver persisted the pod,
// but the client saw a proxy's 502 — must not leak the pod: the
// attempt is resolved by the name it retained, and the persisted pod,
// mutated, is adopted as the probe's own and cleaned up with the rest
// of the probe. One create, one delete, nothing left.
func TestConvergeAdoptsAPodFromAnAmbiguousCreate(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.persistThenFailCreates = 1
	api.mu.Unlock()
	if _, err := convergeLayer(t, api, defaultSettings()); err != nil {
		t.Fatalf("converge: %v", err)
	}
	api.mu.Lock()
	creates, deletes := api.podCreates, api.podDeletes
	api.mu.Unlock()
	if creates != 1 {
		t.Errorf("probe attempts = %d, want 1 (the ambiguous attempt is resolved, not dodged)", creates)
	}
	if deletes != 1 {
		t.Errorf("probe pod deletes = %d, want 1 (the adopted pod is cleaned up)", deletes)
	}
	assertNoPodsStored(t, api)
}

// The ambiguous create may also have persisted an unmutated pod (the
// webhook had not loaded yet): the attempt is resolved by its name,
// the unmutated pod is deleted — only after its ownership is
// confirmed — and only then is the next attempt made. The order of
// the probe's calls pins that: no second create before the first
// attempt's pod is read and gone.
func TestConvergeResolvesAndRetriesAnAmbiguousCreate(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.persistThenFailCreates = 1
	api.mutateFromAttempt = 2 // the first create passes through unmutated
	api.mu.Unlock()
	before, err := convergeLayer(t, api, defaultSettings())
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	api.mu.Lock()
	creates, deletes := api.podCreates, api.podDeletes
	api.mu.Unlock()
	if creates != 2 {
		t.Errorf("probe attempts = %d, want 2 (the ambiguous one resolved, then a fresh one)", creates)
	}
	if deletes != 2 {
		t.Errorf("probe pod deletes = %d, want 2 (both pods cleaned up)", deletes)
	}
	assertNoPodsStored(t, api)

	var podCalls []string
	for _, call := range api.callsFrom(before) {
		if strings.Contains(call, "/pods") {
			podCalls = append(podCalls, call)
		}
	}
	if len(podCalls) < 5 {
		t.Fatalf("pod calls = %v, want at least the five of the resolve-then-retry shape", podCalls)
	}
	if !strings.HasPrefix(podCalls[0], "POST ") || !strings.HasSuffix(podCalls[0], "/pods") {
		t.Fatalf("pod calls = %v, want a create first", podCalls)
	}
	if !strings.HasPrefix(podCalls[1], "GET ") {
		t.Fatalf("pod calls = %v, want the ambiguous attempt read by its name before anything else", podCalls)
	}
	attempt := strings.TrimPrefix(podCalls[1], "GET ")
	if podCalls[2] != "DELETE "+attempt {
		t.Errorf("pod calls = %v, want the resolved unmutated pod %s deleted before the next attempt", podCalls, attempt)
	}
	if podCalls[3] != "GET "+attempt {
		t.Errorf("pod calls = %v, want the deletion confirmed gone before the next attempt", podCalls)
	}
	if !strings.HasPrefix(podCalls[4], "POST ") {
		t.Errorf("pod calls = %v, want the next create only after the attempt was resolved and cleaned up", podCalls)
	}
}

// An ambiguous create whose attempt cannot be resolved — every read
// that would decide it fails — fails the probe naming the pod that may
// exist, instead of retrying under a fresh name and leaving the
// attempt behind. One attempt, ever.
func TestConvergeFailsAnUnresolvableCreate(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.persistThenFailCreates = 1
	api.failPodReads = true
	api.mu.Unlock()
	started := time.Now()
	_, err := convergeLayer(t, api, defaultSettings())
	if err == nil {
		t.Fatal("converge against unresolvable creates succeeded, want the probe to fail naming the pod")
	}
	if !strings.Contains(err.Error(), "could not be resolved") {
		t.Errorf("converge = %v, want the unresolved attempt named as the cause", err)
	}
	if !strings.Contains(err.Error(), "kind-ocp-scc-probe-") {
		t.Errorf("converge = %v, want the unresolved pod named in the error", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("converge took %s, want it bounded near the resolve budget", elapsed)
	}
	api.mu.Lock()
	creates := api.podCreates
	api.mu.Unlock()
	if creates != 1 {
		t.Errorf("probe attempts = %d, want 1 (an unresolved attempt is never retried away)", creates)
	}
}

// A create whose failure response precedes its commit — the read that
// would resolve the attempt sees the name free, and the pod lands
// behind it — must fail the probe naming the pod: absence does not
// prove the create did not persist, so nothing is retried under a
// fresh name and no PASS is returned with the leftover behind.
func TestConvergeFailsWhenTheCreateCommitsBehindTheResolvingRead(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.delayedCommitCreates = 1
	api.mu.Unlock()
	_, err := convergeLayer(t, api, defaultSettings())
	if err == nil {
		t.Fatal("converge succeeded while the ambiguous create's outcome was unknown, want the probe to fail naming the pod")
	}
	for _, want := range []string{"could not be resolved", "does not prove", "kind-ocp-scc-probe-"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("converge = %v, want %q in the error", err, want)
		}
	}
	api.mu.Lock()
	creates := api.podCreates
	leftovers := storedPodKeysLocked(api)
	api.mu.Unlock()
	if creates != 1 {
		t.Errorf("probe attempts = %d, want 1 (no fresh name while the attempt is unresolved)", creates)
	}
	if len(leftovers) != 1 {
		t.Fatalf("stored pods = %v, want the one pod the delayed create committed behind the read", leftovers)
	}
	if name := podNameFromPath(leftovers[0]); !strings.Contains(err.Error(), name) {
		t.Errorf("converge = %v, want the leftover pod %s named in the error", err, name)
	}
}

// A create answered 502 whose attempted name is held by a foreign,
// probe-shaped pod — and whose pod commits behind the read that
// observed the holder — must fail the probe naming the pod: a holder
// that goes away proves nothing about a create still in flight, so
// nothing is retried under a fresh name and no PASS is returned with
// the leftover behind. One create, no delete: the foreign pod is
// never touched, and the committed pod is not provably this
// attempt's to remove.
func TestConvergeFailsWhenTheCreateCommitsBehindAForeignHolder(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.foreignSwapCreates = 1
	api.mu.Unlock()
	before, err := convergeLayer(t, api, defaultSettings())
	if err == nil {
		t.Fatal("converge succeeded while the ambiguous create's outcome was unknown, want the probe to fail naming the pod")
	}
	for _, want := range []string{"could not be resolved", "does not settle", "kind-ocp-scc-probe-"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("converge = %v, want %q in the error", err, want)
		}
	}
	calls := api.callsFrom(before)
	if posts := countPodPosts(calls); posts != 1 {
		t.Errorf("probe creates = %d, want 1 (no fresh name while the attempt is unresolved)", posts)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "DELETE") {
			t.Errorf("the probe issued %s; nothing is deletable while the attempt is unresolved", call)
		}
	}
	api.mu.Lock()
	leftovers := storedPodKeysLocked(api)
	api.mu.Unlock()
	if len(leftovers) != 1 {
		t.Fatalf("stored pods = %v, want the one pod the create committed behind the read", leftovers)
	}
	if name := podNameFromPath(leftovers[0]); !strings.Contains(err.Error(), name) {
		t.Errorf("converge = %v, want the leftover pod %s named in the error", err, name)
	}
}

// A 502 answered with a Retry-After header is the response
// client-go's default retry policy replays — one typed Create call
// would be two wire POSTs under one name, and the second could
// succeed and be cleaned up while the first is still pending,
// committing after the deletion-confirming read. The probe's create
// disables those retries: the failure surfaces as exactly one POST,
// and the unresolved attempt — absence settles nothing — fails the
// probe naming the pod.
func TestConvergeSendsTheProbeCreateAsOneWirePOST(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.retryAfterCreates = 1
	api.mu.Unlock()
	before, err := convergeLayer(t, api, defaultSettings())
	if err == nil {
		t.Fatal("converge succeeded while the create's response was a retryable 502, want the probe to fail resolving the attempt")
	}
	for _, want := range []string{"could not be resolved", "kind-ocp-scc-probe-"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("converge = %v, want %q in the error", err, want)
		}
	}
	if posts := countPodPosts(api.callsFrom(before)); posts != 1 {
		t.Errorf("wire POSTs to the pods collection = %d, want exactly 1 (client-go's automatic retries are disabled for the probe's create)", posts)
	}
	assertNoPodsStored(t, api)
}

// The probe's create is one wire POST even when the answer invites a
// replay: a 502 with Retry-After is what client-go's default retry
// policy re-sends, and a second POST under the same name is a second
// create the probe did not ask for — the first can still commit
// behind the second's outcome. The probe's POST alone disables those
// retries; the failure is surfaced as-is.
func TestCreateProbePodIssuesOneWirePOST(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.retryAfterCreates = 1
	api.mu.Unlock()
	before := api.callCount()
	pod, err := createProbePod(context.Background(), fakeNoRedirectClientset(t, api), "openshift-compliance",
		probePod("one-wire-post", "this-attempts-token", imageRef(defaultSettings())))
	if err == nil {
		t.Fatalf("createProbePod = %+v, want the 502 surfaced as the create's answer", pod)
	}
	var statusErr *apierrors.StatusError
	if !errors.As(err, &statusErr) || statusErr.Status().Code != http.StatusBadGateway {
		t.Errorf("createProbePod = %v, want the 502 status surfaced as the create's answer", err)
	}
	if posts := countPodPosts(api.callsFrom(before)); posts != 1 {
		t.Errorf("wire POSTs = %d, want exactly 1 (client-go's automatic retries are disabled for the probe's create)", posts)
	}
}

// The probe's create refuses redirects: Go's http.Client follows a
// 307 by re-sending the original POST and body to the Location, so
// one create call would be two wire POSTs under one name — the second
// can succeed and be cleaned up while the first is still pending.
// The redirect is surfaced as the create's answer, one POST is sent,
// and nothing reaches the target.
func TestCreateProbePodDoesNotFollowARedirect(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.redirectPodCreates = 1
	api.mu.Unlock()
	before := api.callCount()
	pod, err := createProbePod(context.Background(), fakeNoRedirectClientset(t, api), "openshift-compliance",
		probePod("redirected-create", "this-attempts-token", imageRef(defaultSettings())))
	if err == nil {
		t.Fatalf("createProbePod = %+v, want the redirect refused as the create's answer", pod)
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("createProbePod = %v, want the redirect named in the create's answer", err)
	}
	if posts := countPodPosts(api.callsFrom(before)); posts != 1 {
		t.Errorf("wire POSTs = %d, want exactly 1 (a redirect must not re-send the probe's create)", posts)
	}
	api.mu.Lock()
	hits := api.redirectTargetHits
	api.mu.Unlock()
	if hits != 0 {
		t.Errorf("requests to the redirect target = %d, want 0 (the probe's create is not re-sent to a target it did not choose)", hits)
	}
}

// A create answered AlreadyExists against a pod that carries the
// probe's own static app label — another run's leftover under the
// attempted name — must not adopt or delete it: ownership is the
// per-attempt token, not the label every probe pod shares. The probe
// retries under a fresh name and passes; the same-label pod survives
// without receiving a delete.
func TestConvergeDoesNotTouchASameLabelPodUnderTheAttemptedName(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.conflictCreatesWithDecoy = 1
	api.mu.Unlock()
	before, err := convergeLayer(t, api, defaultSettings())
	if err != nil {
		t.Fatalf("converge: %v (a foreign pod under the attempted name is a name collision, not a probe failure)", err)
	}
	api.mu.Lock()
	creates, deletes := api.podCreates, api.podDeletes
	podKeys := storedPodKeysLocked(api)
	api.mu.Unlock()
	if creates != 2 {
		t.Errorf("probe attempts = %d, want 2 (the conflicted name, then a fresh one)", creates)
	}
	if deletes != 1 {
		t.Errorf("probe pod deletes = %d, want 1 (only the probe's own pod; the foreign pod is never touched)", deletes)
	}
	if len(podKeys) != 1 {
		t.Fatalf("stored pods = %v, want only the foreign pod", podKeys)
	}
	foreign := podKeys[0]
	for _, call := range api.callsFrom(before) {
		if call == "DELETE "+foreign {
			t.Errorf("the probe deleted the foreign pod %s; a pod that is not this attempt's must never be touched", foreign)
		}
	}
}

// decoyAttemptToken is the ownership token the planted decoy carries:
// a foreign token, so the decoy is probe-shaped without ever being
// this attempt's pod.
const decoyAttemptToken = "left-by-an-earlier-run"

// storedPodKeysLocked returns the object paths of every pod in the
// fake's store. The caller holds a.mu.
func storedPodKeysLocked(api *fakeAPIServer) []string {
	var keys []string
	for key := range api.store {
		if strings.Contains(key, "/pods/") {
			keys = append(keys, key)
		}
	}
	return keys
}

// countPodPosts counts wire POSTs to the watched namespace's pods
// collection — the probe's creates.
func countPodPosts(calls []string) int {
	n := 0
	for _, call := range calls {
		if call == "POST "+probePodNS {
			n++
		}
	}
	return n
}

// podNameFromPath extracts the object name from one object path.
func podNameFromPath(key string) string {
	return key[strings.LastIndex(key, "/")+1:]
}

// resolveCreate never treats a pod it cannot prove foreign as
// settling the attempt, and never touches it: the attempted name can
// be held by someone else's pod — a collision, an earlier run's
// leftover under the same probe-shaped label — whose holder can go
// away and let a create still in flight commit after the read, or by
// the pod this attempt did persist with its token stripped by
// admission. Either way the attempt is unresolved: an error naming
// the pod, no delete, no adoption.
func TestResolveCreateFailsOnAPodItCannotOwn(t *testing.T) {
	api := newFakeAPIServer(t)
	const foreignPath = "/api/v1/namespaces/openshift-compliance/pods/foreign-pod"
	api.replaceStored(foreignPath, mustUnstructured(&corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: "foreign-pod", Labels: map[string]string{"app": "someone-elses"}},
	}))
	pod, err := resolveCreate(context.Background(), fakeClientset(t, api), "openshift-compliance", "foreign-pod", "this-attempts-token")
	if err == nil || !strings.Contains(err.Error(), "does not settle") {
		t.Fatalf("resolveCreate = (%v, %v), want the foreign holder to leave the attempt unresolved", pod, err)
	}
	if !strings.Contains(err.Error(), "foreign-pod") {
		t.Errorf("resolveCreate = %v, want the pod named in the error", err)
	}
	// The hand-removal guidance must be conditional on ownership, not
	// on the name: the holder is not provably foreign (its token can
	// have been stripped by admission), so the error names the pod for
	// inspection with the token and the label that decides ownership —
	// advising an unconditional removal would delete a pod whose
	// owner is unknown.
	for _, want := range []string{
		"this-attempts-token",
		probeAttemptLabelKey,
		"remove it by hand only once you have confirmed",
		"is not proof",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("resolveCreate = %v, want %q in the guidance (the removal is conditional on the attempt's token)", err, want)
		}
	}

	// The adversarial shape: a pod carrying the probe's static app
	// label but another attempt's token — a leftover from an earlier
	// run under a colliding name.
	const decoyPath = "/api/v1/namespaces/openshift-compliance/pods/same-label-pod"
	api.replaceStored(decoyPath, mustUnstructured(&corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: "same-label-pod", Labels: map[string]string{
			probeLabelKey:        probeLabelValue,
			probeAttemptLabelKey: decoyAttemptToken,
		}},
	}))
	pod, err = resolveCreate(context.Background(), fakeClientset(t, api), "openshift-compliance", "same-label-pod", "this-attempts-token")
	if err == nil || !strings.Contains(err.Error(), "does not settle") {
		t.Fatalf("resolveCreate against the same-label pod = (%v, %v), want the attempt unresolved", pod, err)
	}

	// The admission shape: a pod this attempt could have persisted —
	// the static label is there — stripped of the ownership token, so
	// nothing distinguishes it from a foreign pod but a token it does
	// not carry.
	const strippedPath = "/api/v1/namespaces/openshift-compliance/pods/token-stripped-pod"
	api.replaceStored(strippedPath, mustUnstructured(&corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: "token-stripped-pod", Labels: map[string]string{probeLabelKey: probeLabelValue}},
	}))
	pod, err = resolveCreate(context.Background(), fakeClientset(t, api), "openshift-compliance", "token-stripped-pod", "this-attempts-token")
	if err == nil || !strings.Contains(err.Error(), "does not settle") {
		t.Fatalf("resolveCreate against the token-stripped pod = (%v, %v), want the attempt unresolved", pod, err)
	}

	for _, path := range []string{foreignPath, decoyPath, strippedPath} {
		if !api.storedOK(path) {
			t.Errorf("the pod at %s was deleted; a pod that is not provably this attempt's must never be touched", path)
		}
	}
	for _, call := range api.callsFrom(0) {
		if strings.HasPrefix(call, "DELETE") {
			t.Errorf("resolveCreate issued %s; resolving reads, it does not delete", call)
		}
	}
}

// deleteProbePod deletes only the object it observed: the UID
// precondition makes the apiserver refuse the delete when the name is
// held by a different object, so a cleanup can never remove what it
// did not read — the pod the probe created stays when the UID sent
// with the delete is not its own.
func TestDeleteProbePodIsUIDPreconditioned(t *testing.T) {
	api := newFakeAPIServer(t)
	pod := createPodThrough(t, api, "openshift-compliance", "precondition-check")
	err := deleteProbePod(context.Background(), fakeClientset(t, api), "openshift-compliance", pod.Name, "not-the-uid", time.Second)
	if err == nil || !strings.Contains(err.Error(), "cleaning up the probe pod") {
		t.Fatalf("deleteProbePod with a foreign UID = %v, want the precondition conflict surfaced", err)
	}
	if !api.storedOK("/api/v1/namespaces/openshift-compliance/pods/" + pod.Name) {
		t.Error("the pod was deleted despite the UID precondition failing; it must stay")
	}
}

// A delete the API accepts is not a deletion: with the object still
// present afterwards — a 202 while whatever holds the pod has not let
// go — the cleanup fails naming the leftover instead of passing. A
// PASS that leaves a pod behind proves nothing it can be trusted on.
func TestDeleteProbePodRequiresConfirmedDeletion(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.acceptWithoutDeleting = true
	api.mu.Unlock()
	pod := createPodThrough(t, api, "openshift-compliance", "accepted-delete")
	err := deleteProbePod(context.Background(), fakeClientset(t, api), "openshift-compliance", pod.Name, pod.UID, 300*time.Millisecond)
	if err == nil {
		t.Fatal("deleteProbePod succeeded against an accepted-but-incomplete delete, want the leftover named")
	}
	for _, want := range []string{"cleaning up the probe pod", "still present", pod.Name} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("deleteProbePod = %v, want %q in the error", err, want)
		}
	}
}

// fakeClientset returns a clientset pointed at the fake apiserver.
func fakeClientset(t *testing.T, api *fakeAPIServer) kubernetes.Interface {
	t.Helper()
	client, err := kube.NewClient(writeKubeconfig(t, api.server.URL))
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	return client.Clientset
}

// fakeNoRedirectClientset returns the layer's clientset pointed at the
// fake apiserver — the production shape: one load of the kubeconfig
// builds the one no-redirect client set whose typed clientset serves
// the probe's reads, deletes, and exactly-once create.
func fakeNoRedirectClientset(t *testing.T, api *fakeAPIServer) kubernetes.Interface {
	t.Helper()
	cfg, err := kube.RESTConfig(writeKubeconfig(t, api.server.URL))
	if err != nil {
		t.Fatalf("loading the REST config: %v", err)
	}
	client, err := kube.NewNoRedirectClientFromConfig(cfg)
	if err != nil {
		t.Fatalf("building the no-redirect client: %v", err)
	}
	return client.Clientset
}

// assertNoPodsStored fails when any pod remains in the fake's store —
// the no-leak assertion every probe regression ends with.
func assertNoPodsStored(t *testing.T, api *fakeAPIServer) {
	t.Helper()
	api.mu.Lock()
	defer api.mu.Unlock()
	for key := range api.store {
		if strings.Contains(key, "/pods/") {
			t.Errorf("a pod is left behind at %s", key)
		}
	}
}

// A probe pod the kubelet refuses to run fails the converge with the
// kubelet's reason in the error.
func TestConvergeFailsWhenTheProbePodCannotRun(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.podRuns = false
	api.mu.Unlock()
	_, err := convergeLayer(t, api, defaultSettings())
	if err == nil || !strings.Contains(err.Error(), "CreateContainerError") {
		t.Fatalf("converge = %v, want the kubelet's refusal in the error", err)
	}
}

// A probe pod running with the wrong UID fails the converge: the
// webhook must inject the configured UID, not just any.
func TestConvergeFailsOnTheWrongInjectedUID(t *testing.T) {
	api := newFakeAPIServer(t)
	api.mu.Lock()
	api.injectUID = 4242
	api.mu.Unlock()
	_, err := convergeLayer(t, api, defaultSettings())
	if err == nil || !strings.Contains(err.Error(), "4242") {
		t.Fatalf("converge = %v, want the wrong UID named in the error", err)
	}
}

func TestVerify(t *testing.T) {
	api := newFakeAPIServer(t)
	s := defaultSettings()
	if _, err := convergeLayer(t, api, s); err != nil {
		t.Fatalf("converge: %v", err)
	}
	env := testEnv(writeKubeconfig(t, api.server.URL))
	api.mu.Lock()
	createsBefore := api.podCreates
	api.mu.Unlock()
	if err := (Layer{}).Verify(context.Background(), env); err != nil {
		t.Fatalf("verify after converge: %v", err)
	}
	api.mu.Lock()
	creates := api.podCreates - createsBefore
	api.mu.Unlock()
	if creates == 0 {
		t.Error("the admission probe did not run against the layer's own registration; the registration-shape gate must not over-reject")
	}

	t.Run("a tampered caBundle fails with the desync named", func(t *testing.T) {
		mwc := api.mustStored(t, mwcKey).DeepCopy()
		// Walk the raw map: unstructured's Nested* helpers hand back
		// copies in this apimachinery, and the tamper must reach the
		// stored object.
		webhook := mwc.Object["webhooks"].([]any)[0].(map[string]any)
		clientConfig := webhook["clientConfig"].(map[string]any)
		original := clientConfig["caBundle"]
		clientConfig["caBundle"] = base64.StdEncoding.EncodeToString([]byte("not the CA"))
		api.replaceStored(mwcKey, mwc)
		defer func() { // keep the subtests that follow independent
			webhook["clientConfig"].(map[string]any)["caBundle"] = original
			api.replaceStored(mwcKey, mwc)
		}()
		err := (Layer{}).verify(context.Background(), env, 2*time.Second)
		if err == nil || !strings.Contains(err.Error(), "caBundle does not match") {
			t.Fatalf("verify = %v, want the caBundle desync to fail it", err)
		}
	})

	t.Run("an empty namespaceSelector fails without probing", func(t *testing.T) {
		// A removed namespaceSelector matches every namespace — the
		// widest admission-scope drift there is: pod creates anywhere
		// would receive the injected UID while every probe in the
		// watched namespaces still passes. The check must fail on the
		// registration's shape, before any probe pod is created.
		// (The raw map walk, like the caBundle tamper:
		// unstructured's Nested* helpers hand back copies, and the
		// tamper must reach the stored object.)
		mwc := api.mustStored(t, mwcKey).DeepCopy()
		webhook := mwc.Object["webhooks"].([]any)[0].(map[string]any)
		original, had := webhook["namespaceSelector"]
		delete(webhook, "namespaceSelector")
		api.replaceStored(mwcKey, mwc)
		defer func() {
			if had {
				webhook["namespaceSelector"] = original
			}
			api.replaceStored(mwcKey, mwc)
		}()
		api.mu.Lock()
		createsBefore := api.podCreates
		api.mu.Unlock()
		err := (Layer{}).verify(context.Background(), env, 2*time.Second)
		if err == nil || !strings.Contains(err.Error(), "matches every namespace") {
			t.Fatalf("verify = %v, want the empty selector to fail it", err)
		}
		if !strings.Contains(err.Error(), "the admission probe did not run") {
			t.Errorf("verify = %v, want the skipped probe named in the failure", err)
		}
		api.mu.Lock()
		creates := api.podCreates - createsBefore
		api.mu.Unlock()
		if creates != 0 {
			t.Errorf("probe attempts = %d, want 0 (a drifted registration is never probed)", creates)
		}
	})

	t.Run("an expanded allowlist fails without probing", func(t *testing.T) {
		// A selector broader than the configured watched namespaces
		// mutates pod creates outside the configured scope; the probe
		// pods, created inside the watched namespaces, cannot see it.
		mwc := api.mustStored(t, mwcKey).DeepCopy()
		webhook := mwc.Object["webhooks"].([]any)[0].(map[string]any)
		expr := nestedValue(t, webhook, "namespaceSelector", "matchExpressions").([]any)[0].(map[string]any)
		original := expr["values"]
		expr["values"] = []any{"openshift-compliance", "default"}
		api.replaceStored(mwcKey, mwc)
		defer func() {
			expr["values"] = original
			api.replaceStored(mwcKey, mwc)
		}()
		api.mu.Lock()
		createsBefore := api.podCreates
		api.mu.Unlock()
		err := (Layer{}).verify(context.Background(), env, 2*time.Second)
		if err == nil || !strings.Contains(err.Error(), "watched namespaces") {
			t.Fatalf("verify = %v, want the expanded allowlist to fail it", err)
		}
		if !strings.Contains(err.Error(), "the admission probe did not run") {
			t.Errorf("verify = %v, want the skipped probe named in the failure", err)
		}
		api.mu.Lock()
		creates := api.podCreates - createsBefore
		api.mu.Unlock()
		if creates != 0 {
			t.Errorf("probe attempts = %d, want 0 (a drifted registration is never probed)", creates)
		}
	})

	t.Run("an objectSelector that selects the probe fails without probing", func(t *testing.T) {
		// Kubernetes evaluates the objectSelector against the created
		// object, before the webhook is called. A selector matching
		// the probe's static app label mutates the probe's own
		// creates while ordinary pods in the same watched namespace
		// pass unmutated — every probe would pass, the false PASS
		// this gate exists to catch. The fake evaluates the stored
		// selector, so the hazard is demonstrated below, not assumed:
		// a probe-shaped pod is still mutated under the drifted
		// selector while an ordinary pod in the same namespace is
		// not. (The raw map walk, like the caBundle tamper:
		// unstructured's Nested* helpers hand back copies, and the
		// tamper must reach the stored object.)
		mwc := api.mustStored(t, mwcKey).DeepCopy()
		webhook := mwc.Object["webhooks"].([]any)[0].(map[string]any)
		original, had := webhook["objectSelector"]
		webhook["objectSelector"] = map[string]any{"matchLabels": map[string]any{probeLabelKey: probeLabelValue}}
		api.replaceStored(mwcKey, mwc)
		defer func() {
			if had {
				webhook["objectSelector"] = original
			} else {
				delete(webhook, "objectSelector")
			}
			api.replaceStored(mwcKey, mwc)
		}()
		// The hazard is real under the drifted selector: the probe's
		// own shape is selected and mutated…
		if pod := createPodThrough(t, api, "openshift-compliance", "object-selector-probe-shaped"); pod.Spec.SecurityContext == nil ||
			pod.Spec.SecurityContext.RunAsUser == nil || *pod.Spec.SecurityContext.RunAsUser != defaultRunAsUser {
			t.Fatal("the fixture lost the false-PASS shape: a probe-shaped pod under the drifted objectSelector must still be mutated")
		}
		// …while an ordinary pod in the same namespace is not.
		cs := fakeClientset(t, api)
		ordinary := &corev1.Pod{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
			ObjectMeta: metav1.ObjectMeta{Name: "object-selector-ordinary", Labels: map[string]string{"app": "compliance-operator"}},
		}
		created, err := cs.CoreV1().Pods("openshift-compliance").Create(context.Background(), ordinary, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("creating the ordinary pod: %v", err)
		}
		if created.Spec.SecurityContext != nil && created.Spec.SecurityContext.RunAsUser != nil {
			t.Fatal("the fixture lost the false-PASS shape: an ordinary pod under the drifted objectSelector must pass unmutated")
		}
		// Keep the subtests that follow independent of the two pods.
		api.mu.Lock()
		delete(api.store, "/api/v1/namespaces/openshift-compliance/pods/object-selector-probe-shaped")
		delete(api.store, "/api/v1/namespaces/openshift-compliance/pods/object-selector-ordinary")
		createsBefore := api.podCreates
		api.mu.Unlock()
		err = (Layer{}).verify(context.Background(), env, 2*time.Second)
		if err == nil || !strings.Contains(err.Error(), "objectSelector") {
			t.Fatalf("verify = %v, want the objectSelector drift to fail it", err)
		}
		if !strings.Contains(err.Error(), "the admission probe did not run") {
			t.Errorf("verify = %v, want the skipped probe named in the failure", err)
		}
		api.mu.Lock()
		creates := api.podCreates - createsBefore
		api.mu.Unlock()
		if creates != 0 {
			t.Errorf("probe attempts = %d, want 0 (a drifted registration is never probed)", creates)
		}
	})

	t.Run("an API-defaulted empty objectSelector passes and probes", func(t *testing.T) {
		// The API defaults a missing objectSelector to the empty
		// selector — present but empty — which matches every object,
		// exactly like nil. The gate accepts both forms; only a
		// nonempty selector is drift. The fake never defaults the
		// field (it stores what is sent), so this plants the stored
		// form a real apiserver answers with.
		mwc := api.mustStored(t, mwcKey).DeepCopy()
		webhook := mwc.Object["webhooks"].([]any)[0].(map[string]any)
		if _, had := webhook["objectSelector"]; had {
			t.Fatal("the stored registration already carries an objectSelector; the fixture changed")
		}
		webhook["objectSelector"] = map[string]any{}
		api.replaceStored(mwcKey, mwc)
		defer func() {
			delete(webhook, "objectSelector")
			api.replaceStored(mwcKey, mwc)
		}()
		api.mu.Lock()
		createsBefore := api.podCreates
		api.mu.Unlock()
		if err := (Layer{}).verify(context.Background(), env, 2*time.Second); err != nil {
			t.Fatalf("verify = %v, want the API-defaulted empty objectSelector to pass like nil", err)
		}
		api.mu.Lock()
		creates := api.podCreates - createsBefore
		api.mu.Unlock()
		if creates == 0 {
			t.Error("the admission probe did not run; the empty objectSelector must be accepted like nil, and the probe must run")
		}
	})

	t.Run("a matchCondition fails without probing", func(t *testing.T) {
		// matchConditions are CEL expressions the apiserver evaluates
		// before calling the webhook; the layer renders none, so any
		// condition is drift — one can admit the probe's creates
		// while refusing ordinary ones, the same false PASS an
		// objectSelector produces. The fake does not evaluate CEL;
		// the check must fail on the registration's shape, before
		// any probe pod is created.
		mwc := api.mustStored(t, mwcKey).DeepCopy()
		webhook := mwc.Object["webhooks"].([]any)[0].(map[string]any)
		original, had := webhook["matchConditions"]
		webhook["matchConditions"] = []any{map[string]any{"name": "drifted.example", "expression": "true"}}
		api.replaceStored(mwcKey, mwc)
		defer func() {
			if had {
				webhook["matchConditions"] = original
			} else {
				delete(webhook, "matchConditions")
			}
			api.replaceStored(mwcKey, mwc)
		}()
		api.mu.Lock()
		createsBefore := api.podCreates
		api.mu.Unlock()
		err := (Layer{}).verify(context.Background(), env, 2*time.Second)
		if err == nil || !strings.Contains(err.Error(), "matchCondition") {
			t.Fatalf("verify = %v, want the matchCondition drift to fail it", err)
		}
		if !strings.Contains(err.Error(), "the admission probe did not run") {
			t.Errorf("verify = %v, want the skipped probe named in the failure", err)
		}
		api.mu.Lock()
		creates := api.podCreates - createsBefore
		api.mu.Unlock()
		if creates != 0 {
			t.Errorf("probe attempts = %d, want 0 (a drifted registration is never probed)", creates)
		}
	})

	t.Run("an extra webhook entry fails without probing", func(t *testing.T) {
		// The apiserver evaluates every entry a registration carries;
		// an extra one mutates pod creates the layer never asked for,
		// and the probe says nothing about it.
		mwc := api.mustStored(t, mwcKey).DeepCopy()
		webhooks := mwc.Object["webhooks"].([]any)
		original := append([]any(nil), webhooks...)
		mwc.Object["webhooks"] = append(original, firstWebhook(t, mwc.Object))
		api.replaceStored(mwcKey, mwc)
		defer func() {
			mwc.Object["webhooks"] = original
			api.replaceStored(mwcKey, mwc)
		}()
		api.mu.Lock()
		createsBefore := api.podCreates
		api.mu.Unlock()
		err := (Layer{}).verify(context.Background(), env, 2*time.Second)
		if err == nil || !strings.Contains(err.Error(), "2 webhook entries") {
			t.Fatalf("verify = %v, want the extra entry to fail it", err)
		}
		if !strings.Contains(err.Error(), "the admission probe did not run") {
			t.Errorf("verify = %v, want the skipped probe named in the failure", err)
		}
		api.mu.Lock()
		creates := api.podCreates - createsBefore
		api.mu.Unlock()
		if creates != 0 {
			t.Errorf("probe attempts = %d, want 0 (a drifted registration is never probed)", creates)
		}
	})

	t.Run("a drifted rule set fails without probing", func(t *testing.T) {
		// The rules are the admission trigger; anything beyond pod
		// CREATE in the core group mutates operations or resources
		// the layer never asked for.
		mwc := api.mustStored(t, mwcKey).DeepCopy()
		webhook := mwc.Object["webhooks"].([]any)[0].(map[string]any)
		rule := nestedValue(t, webhook, "rules").([]any)[0].(map[string]any)
		original := rule["operations"]
		rule["operations"] = []any{"CREATE", "UPDATE"}
		api.replaceStored(mwcKey, mwc)
		defer func() {
			rule["operations"] = original
			api.replaceStored(mwcKey, mwc)
		}()
		api.mu.Lock()
		createsBefore := api.podCreates
		api.mu.Unlock()
		err := (Layer{}).verify(context.Background(), env, 2*time.Second)
		if err == nil || !strings.Contains(err.Error(), "pod CREATE") {
			t.Fatalf("verify = %v, want the drifted rules to fail it", err)
		}
		if !strings.Contains(err.Error(), "the admission probe did not run") {
			t.Errorf("verify = %v, want the skipped probe named in the failure", err)
		}
		api.mu.Lock()
		creates := api.podCreates - createsBefore
		api.mu.Unlock()
		if creates != 0 {
			t.Errorf("probe attempts = %d, want 0 (a drifted registration is never probed)", creates)
		}
	})

	t.Run("an ambiguous probe create leaves nothing behind", func(t *testing.T) {
		// The create's response is lost to a 502 while the pod was
		// persisted: Verify still passes — the attempt is resolved by
		// its name and the pod adopted — and no pod is left in the
		// watched namespace afterwards.
		api.mu.Lock()
		api.persistThenFailCreates = 1
		api.mu.Unlock()
		if err := (Layer{}).verify(context.Background(), env, 2*time.Second); err != nil {
			t.Fatalf("verify = %v, want the adopted pod to carry the probe", err)
		}
		assertNoPodsStored(t, api)
	})

	t.Run("a create that commits behind the resolving read fails naming the pod", func(t *testing.T) {
		// The create's response is lost to a 502 while the pod commits
		// only after the read that would resolve the attempt has seen
		// the name free: Verify must not treat that absence as proof
		// nothing was created — it fails naming the pod, and no fresh
		// name is tried while the attempt is unresolved.
		api.mu.Lock()
		createsBefore := api.podCreates
		api.delayedCommitCreates = 1
		api.mu.Unlock()
		err := (Layer{}).verify(context.Background(), env, 2*time.Second)
		if err == nil {
			t.Fatal("verify succeeded while the ambiguous create's outcome was unknown, want the failure naming the pod")
		}
		api.mu.Lock()
		creates := api.podCreates - createsBefore
		leftovers := storedPodKeysLocked(api)
		api.mu.Unlock()
		if creates != 1 {
			t.Errorf("probe attempts = %d, want 1 (no fresh name while the attempt is unresolved)", creates)
		}
		if len(leftovers) != 1 {
			t.Fatalf("stored pods = %v, want the one pod the delayed create committed", leftovers)
		}
		if name := podNameFromPath(leftovers[0]); !strings.Contains(err.Error(), name) {
			t.Errorf("verify = %v, want the leftover pod %s named in the error", err, name)
		}
		if !strings.Contains(err.Error(), "could not be resolved") {
			t.Errorf("verify = %v, want the unresolved attempt named as the cause", err)
		}
		// Keep the subtests that follow independent of this leftover.
		api.mu.Lock()
		delete(api.store, leftovers[0])
		api.mu.Unlock()
	})

	t.Run("a foreign holder under the attempted name does not settle the attempt", func(t *testing.T) {
		// The create's response is lost to a 502 while a foreign,
		// probe-shaped pod holds the attempted name; the resolving
		// read observes the holder, and the create commits behind
		// it. Verify must not treat the held name as proof the
		// create failed — the holder can go away and let a create
		// still in flight land after the read: it fails naming the
		// pod, issues no delete, and tries no fresh name.
		api.mu.Lock()
		createsBefore := api.podCreates
		api.foreignSwapCreates = 1
		api.mu.Unlock()
		before := api.callCount()
		err := (Layer{}).verify(context.Background(), env, 2*time.Second)
		if err == nil {
			t.Fatal("verify succeeded while the ambiguous create's outcome was unknown, want the failure naming the pod")
		}
		api.mu.Lock()
		creates := api.podCreates - createsBefore
		leftovers := storedPodKeysLocked(api)
		api.mu.Unlock()
		if creates != 1 {
			t.Errorf("probe attempts = %d, want 1 (no fresh name while the attempt is unresolved)", creates)
		}
		if len(leftovers) != 1 {
			t.Fatalf("stored pods = %v, want the one pod the create committed behind the read", leftovers)
		}
		if name := podNameFromPath(leftovers[0]); !strings.Contains(err.Error(), name) {
			t.Errorf("verify = %v, want the leftover pod %s named in the error", err, name)
		}
		for _, want := range []string{"could not be resolved", "does not settle"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("verify = %v, want %q in the error", err, want)
			}
		}
		for _, call := range api.callsFrom(before) {
			if strings.HasPrefix(call, "DELETE") {
				t.Errorf("verify issued %s; nothing is deletable while the attempt is unresolved", call)
			}
		}
		// Keep the subtests that follow independent of this leftover.
		api.mu.Lock()
		delete(api.store, leftovers[0])
		api.mu.Unlock()
	})

	t.Run("a retryable 502 costs exactly one wire POST", func(t *testing.T) {
		// A 502 answered with Retry-After is the response
		// client-go's default retry policy replays: one typed
		// Create call would be two wire POSTs under one name. The
		// probe's create disables those retries — the failure
		// surfaces as one POST, and the unresolved attempt fails
		// the check naming the pod.
		api.mu.Lock()
		api.retryAfterCreates = 1
		api.mu.Unlock()
		before := api.callCount()
		err := (Layer{}).verify(context.Background(), env, 2*time.Second)
		if err == nil {
			t.Fatal("verify succeeded against a create it never saw answered, want the unresolved attempt to fail it")
		}
		for _, want := range []string{"could not be resolved", "kind-ocp-scc-probe-"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("verify = %v, want %q in the error", err, want)
			}
		}
		if posts := countPodPosts(api.callsFrom(before)); posts != 1 {
			t.Errorf("wire POSTs to the pods collection = %d, want exactly 1 (client-go's automatic retries are disabled for the probe's create)", posts)
		}
		assertNoPodsStored(t, api)
	})

	t.Run("a redirected create is not re-sent to the target", func(t *testing.T) {
		// A 307 with a Location is followed by Go's http client —
		// the original POST and body re-sent to the target — under
		// no retry policy's control: one create call would be two
		// wire POSTs under one name. The probe's create refuses
		// redirects: the refusal surfaces as one POST, nothing
		// reaches the target, and the unresolved attempt fails the
		// check naming the pod.
		api.mu.Lock()
		api.redirectPodCreates = 1
		api.mu.Unlock()
		before := api.callCount()
		err := (Layer{}).verify(context.Background(), env, 2*time.Second)
		if err == nil {
			t.Fatal("verify succeeded against a redirected create, want the unresolved attempt to fail it")
		}
		for _, want := range []string{"could not be resolved", "kind-ocp-scc-probe-", "redirect"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("verify = %v, want %q in the error", err, want)
			}
		}
		if posts := countPodPosts(api.callsFrom(before)); posts != 1 {
			t.Errorf("wire POSTs to the pods collection = %d, want exactly 1 (a redirect must not re-send the probe's create)", posts)
		}
		api.mu.Lock()
		hits := api.redirectTargetHits
		api.mu.Unlock()
		if hits != 0 {
			t.Errorf("requests to the redirect target = %d, want 0 (the probe's create is not re-sent to a target it did not choose)", hits)
		}
		assertNoPodsStored(t, api)
	})

	t.Run("a same-label pod under the attempted name is never touched", func(t *testing.T) {
		// A create answered AlreadyExists against a pod carrying the
		// probe's static app label but not this attempt's token:
		// Verify passes on a fresh name, and the foreign pod survives
		// without receiving a delete.
		api.mu.Lock()
		createsBefore, deletesBefore := api.podCreates, api.podDeletes
		api.conflictCreatesWithDecoy = 1
		api.mu.Unlock()
		if err := (Layer{}).verify(context.Background(), env, 2*time.Second); err != nil {
			t.Fatalf("verify = %v, want the name collision retried under a fresh name", err)
		}
		api.mu.Lock()
		creates := api.podCreates - createsBefore
		deletes := api.podDeletes - deletesBefore
		podKeys := storedPodKeysLocked(api)
		api.mu.Unlock()
		if creates != 2 {
			t.Errorf("probe attempts = %d, want 2 (the conflicted name, then a fresh one)", creates)
		}
		if deletes != 1 {
			t.Errorf("probe pod deletes = %d, want 1 (only the probe's own pod)", deletes)
		}
		if len(podKeys) != 1 {
			t.Fatalf("stored pods = %v, want only the foreign pod", podKeys)
		}
		foreign := podKeys[0]
		for _, call := range api.callsFrom(0) {
			if call == "DELETE "+foreign {
				t.Errorf("the probe deleted the foreign pod %s; a pod that is not this attempt's must never be touched", foreign)
			}
		}
		// Keep the subtests that follow independent of the decoy.
		api.mu.Lock()
		delete(api.store, foreign)
		api.mu.Unlock()
	})

	t.Run("a secret the deployment never rolled onto fails the fingerprint check", func(t *testing.T) {
		// The interrupted-rollout shape: a valid replacement secret in
		// place of the one the deployment's pods were rolled from.
		replacement, err := generateTLS(time.Now())
		if err != nil {
			t.Fatal(err)
		}
		secret := api.mustStored(t, secretKey).DeepCopy()
		writeStoredSecretData(t, secret, map[string][]byte{
			secretCAKey: replacement.caCertPEM,
			"tls.crt":   replacement.tlsCertPEM,
			"tls.key":   replacement.tlsKeyPEM,
		})
		api.replaceStored(secretKey, secret)
		err = (Layer{}).verify(context.Background(), env, 2*time.Second)
		if err == nil || !strings.Contains(err.Error(), "TLS fingerprint") {
			t.Fatalf("verify = %v, want the fingerprint mismatch to fail it", err)
		}
	})

	t.Run("a webhook that stops mutating fails the live probe", func(t *testing.T) {
		api.mu.Lock()
		api.mutatePods = false
		api.mu.Unlock()
		err := (Layer{}).verify(context.Background(), env, 2*time.Second)
		if err == nil || !strings.Contains(err.Error(), "admitted without a runAsUser") {
			t.Fatalf("verify = %v, want the unmutated probe to fail it", err)
		}
	})

	t.Run("a probe failure and a cleanup failure are both reported", func(t *testing.T) {
		// The probe fails on its own terms (the wrong UID injected)
		// while its cleanup also fails (the delete refused): both
		// failures surface, and the leftover pod is named — a probe
		// error must not hide the pod it left behind.
		api.mu.Lock()
		api.mutatePods = true // the previous subtest stopped the webhook
		api.injectUID = 4242
		api.failPodDeletes = true
		api.mu.Unlock()
		defer func() {
			api.mu.Lock()
			api.injectUID = defaultRunAsUser
			api.failPodDeletes = false
			api.mu.Unlock()
		}()
		err := (Layer{}).verify(context.Background(), env, 2*time.Second)
		if err == nil {
			t.Fatal("verify succeeded with a failing probe and a failing cleanup")
		}
		for _, want := range []string{"4242", "cleaning up the probe pod", "kind-ocp-scc-probe-"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("verify = %v, want %q reported alongside the rest", err, want)
			}
		}
	})

	t.Run("a missing deployment fails naming the layer", func(t *testing.T) {
		api.mu.Lock()
		delete(api.store, deployKey)
		api.mu.Unlock()
		err := (Layer{}).verify(context.Background(), env, 2*time.Second)
		if err == nil || !strings.Contains(err.Error(), "was the scc-admission layer applied?") {
			t.Fatalf("verify = %v, want the missing deployment to fail it", err)
		}
	})

	t.Run("the reads are bounded", func(t *testing.T) {
		// The deployment read must not outlive the budget: a stalled
		// apiserver fails the check instead of hanging verify.
		err := (Layer{}).verify(context.Background(), env, time.Nanosecond)
		if err == nil {
			t.Fatal("verify with a zero budget succeeded, want the bounded read to fail")
		}
	})
}

// The mutating check is bound to the cluster instance its environment
// names: a kubeconfig replaced before the check runs — the same-name
// replacement — must fail the check before any pod is created, not
// probe the replacement. The binding is what kindocp's verify path
// hands the layer after the record's instance-id check passed.
func TestVerifyRefusesToProbeAReplacedCluster(t *testing.T) {
	api := newFakeAPIServer(t)
	s := defaultSettings()
	if _, err := convergeLayer(t, api, s); err != nil {
		t.Fatalf("converge: %v", err)
	}
	plantKubeSystem(t, api, "instance-recorded")

	replacement := newFakeAPIServer(t)
	plantKubeSystem(t, replacement, "instance-replacement")

	kubeconfig := writeKubeconfig(t, api.server.URL)
	env := testEnv(kubeconfig)
	env.ClusterInstanceID = "instance-recorded"
	// The replacement lands after the embedding validated the recorded
	// instance but before the layer loads the kubeconfig.
	rewriteKubeconfig(t, kubeconfig, replacement.server.URL)

	err := (Layer{}).Verify(context.Background(), env)
	if err == nil {
		t.Fatal("verify succeeded against a replaced cluster, want the instance binding to fail it")
	}
	for _, want := range []string{"instance-replacement", "instance-recorded", "replaced"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("verify = %v, want %q in the error", err, want)
		}
	}
	replacement.mu.Lock()
	creates := replacement.podCreates
	calls := append([]string(nil), replacement.calls...)
	replacement.mu.Unlock()
	if creates != 0 {
		t.Errorf("pod creates on the replacement = %d, want 0 (the check never mutates a cluster it was not bound to)", creates)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "POST ") {
			t.Errorf("the replacement cluster received %s; the binding read is the only request a replaced cluster may see", call)
		}
	}
	assertNoPodsStored(t, replacement)
}

// Every client the check builds comes from the one kubeconfig load:
// the file can be rewritten under the run — here between the reads and
// the probe, on the deployment read — and the probe still talks to the
// cluster that one load resolved. A second load (the pre-fix shape)
// would point the probe's POST at the replacement while its reads, its
// ambiguity resolution, and its UID-preconditioned cleanup stayed on
// the recorded cluster.
func TestVerifyKeepsEveryProbeClientOnTheOneLoadedKubeconfig(t *testing.T) {
	api := newFakeAPIServer(t)
	s := defaultSettings()
	if _, err := convergeLayer(t, api, s); err != nil {
		t.Fatalf("converge: %v", err)
	}
	plantKubeSystem(t, api, "instance-recorded")

	replacement := newFakeAPIServer(t)
	plantKubeSystem(t, replacement, "instance-replacement")

	kubeconfig := writeKubeconfig(t, api.server.URL)
	api.onDeploymentGet = func() { rewriteKubeconfig(t, kubeconfig, replacement.server.URL) }
	env := testEnv(kubeconfig)
	env.ClusterInstanceID = "instance-recorded"

	if err := (Layer{}).Verify(context.Background(), env); err != nil {
		t.Fatalf("verify = %v, want the probe to pass against the cluster the one load resolved", err)
	}
	replacement.mu.Lock()
	creates := replacement.podCreates
	calls := append([]string(nil), replacement.calls...)
	replacement.mu.Unlock()
	if creates != 0 {
		t.Errorf("pod creates on the replacement = %d, want 0 (the probe's POST never reaches a kubeconfig rewritten under the run)", creates)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "POST ") {
			t.Errorf("the replacement cluster received %s; every probe client must talk to the one loaded cluster", call)
		}
	}
	assertNoPodsStored(t, api)
}

// The apply path builds its probe from the same one load: the file
// rewritten during the rollout wait — the stage of converge between
// the applies and the probe — does not re-point the probe's POST at
// the replacement.
func TestConvergeKeepsTheProbeOnTheOneLoadedKubeconfig(t *testing.T) {
	api := newFakeAPIServer(t)
	s := defaultSettings()
	replacement := newFakeAPIServer(t)
	plantKubeSystem(t, replacement, "instance-replacement")

	kubeconfig := writeKubeconfig(t, api.server.URL)
	cfg, err := kube.RESTConfig(kubeconfig)
	if err != nil {
		t.Fatalf("loading the REST config: %v", err)
	}
	client, err := kube.NewNoRedirectClientFromConfig(cfg)
	if err != nil {
		t.Fatalf("building client: %v", err)
	}
	api.onDeploymentGet = func() { rewriteKubeconfig(t, kubeconfig, replacement.server.URL) }
	if err := (Layer{}).converge(context.Background(), testEnv(kubeconfig), client, s, imageRef(s), 3*time.Second); err != nil {
		t.Fatalf("converge = %v, want the probe to pass against the cluster the one load resolved", err)
	}
	replacement.mu.Lock()
	creates := replacement.podCreates
	calls := append([]string(nil), replacement.calls...)
	replacement.mu.Unlock()
	if creates != 0 {
		t.Errorf("pod creates on the replacement = %d, want 0 (the probe's POST never reaches a kubeconfig rewritten under the run)", creates)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "POST ") {
			t.Errorf("the replacement cluster received %s; every probe client must talk to the one loaded cluster", call)
		}
	}
	assertNoPodsStored(t, api)
}

// storedSecretData returns the secret's data as PEM bytes: the fake
// stores the wire representation, where the data values are base64 of
// what the typed client sent.
func storedSecretData(t *testing.T, api *fakeAPIServer) map[string][]byte {
	t.Helper()
	secret := api.mustStored(t, secretKey)
	raw, _, err := unstructured.NestedMap(secret.Object, "data")
	if err != nil {
		t.Fatalf("reading the stored secret's data: %v", err)
	}
	out := make(map[string][]byte, len(raw))
	for key, value := range raw {
		s, ok := value.(string)
		if !ok {
			t.Fatalf("secret data[%s] is not a string: %T", key, value)
		}
		decoded, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatalf("secret data[%s] is not base64: %v", key, err)
		}
		out[key] = decoded
	}
	return out
}

// writeStoredSecretData renders secret data back into the fake's store
// shape (base64 strings under data).
func writeStoredSecretData(t *testing.T, secret *unstructured.Unstructured, data map[string][]byte) {
	t.Helper()
	raw := make(map[string]any, len(data))
	for key, value := range data {
		raw[key] = base64.StdEncoding.EncodeToString(value)
	}
	_ = unstructured.SetNestedMap(secret.Object, raw, "data")
}

// firstContainer returns a pod template's first container.
func firstContainer(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()
	containers, _, err := unstructured.NestedSlice(obj, "spec", "template", "spec", "containers")
	if err != nil || len(containers) == 0 {
		t.Fatalf("reading the template's containers: %v (%v)", err, containers)
	}
	container, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("the first container is a %T, not an object", containers[0])
	}
	return container
}

// firstWebhook returns a MutatingWebhookConfiguration's first webhook.
func firstWebhook(t *testing.T, obj map[string]any) map[string]any {
	t.Helper()
	webhooks, _, err := unstructured.NestedSlice(obj, "webhooks")
	if err != nil || len(webhooks) == 0 {
		t.Fatalf("reading the webhooks: %v (%v)", err, webhooks)
	}
	webhook, ok := webhooks[0].(map[string]any)
	if !ok {
		t.Fatalf("the first webhook is a %T, not an object", webhooks[0])
	}
	return webhook
}

// containerEnvValue returns one env variable's value from a container.
func containerEnvValue(t *testing.T, container map[string]any, name string) string {
	t.Helper()
	variables, ok := container["env"].([]any)
	if !ok {
		t.Fatalf("the container carries no env: %T", container["env"])
	}
	for _, v := range variables {
		variable, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if variable["name"] == name {
			value, _ := variable["value"].(string)
			return value
		}
	}
	t.Fatalf("the container carries no %s env variable", name)
	return ""
}

// nestedValue walks maps by field name, failing the test on a type
// mismatch along the way.
func nestedValue(t *testing.T, obj map[string]any, fields ...string) any {
	t.Helper()
	var current any = obj
	for _, field := range fields {
		m, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("walking %v: hit a %T, not an object", fields, current)
		}
		current, ok = m[field]
		if !ok {
			t.Fatalf("walking %v: no field %q", fields, field)
		}
	}
	return current
}

// TestApplyRefusesWithoutImageServices: the layer builds and loads a
// local image, so an embedding that binds neither service is refused
// before anything else runs — no kubeconfig load, no cluster call.
// The build service is the provider capability the image build moved
// behind (no layer invokes a container-runtime CLI directly); the
// load service is the older one. Each is refused on its own.
func TestApplyRefusesWithoutImageServices(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	noopBuild := func(context.Context, string, string) error { return nil }
	noopLoad := func(context.Context, string) error { return nil }
	for _, tt := range []struct {
		name string
		env  layers.Env
		want string
	}{
		{
			name: "no builder",
			env:  layers.Env{ClusterName: "kind-ocp-1", KubeconfigPath: kubeconfig, Log: slog.New(slog.DiscardHandler), LoadImage: noopLoad},
			want: "no image-building service",
		},
		{
			name: "no loader",
			env:  layers.Env{ClusterName: "kind-ocp-1", KubeconfigPath: kubeconfig, Log: slog.New(slog.DiscardHandler), BuildImage: noopBuild},
			want: "no image-loading service",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := (Layer{}).Apply(context.Background(), tt.env)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Apply = %v, want the %q refusal", err, tt.want)
			}
		})
	}
}

package kube

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func node(name string, ready corev1.ConditionStatus) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
			Type:   corev1.NodeReady,
			Status: ready,
		}}},
	}
}

// kubeconfigYAML is a minimal but valid kubeconfig pointing at a fake
// server; RESTConfig only builds the client config, so the server never
// needs to answer.
const kubeconfigYAML = `apiVersion: v1
kind: Config
clusters:
- name: k
  cluster:
    server: http://kubeconfig.test
contexts:
- name: k
  context:
    cluster: k
    user: k
current-context: k
users:
- name: k
  user: {}
`

// RESTConfig must refuse a kubeconfig that is not a private,
// caller-owned regular file (a cluster-admin credential in a shared or
// restored state tree), and accept the private ones kind-ocp writes.
func TestRESTConfigRequiresPrivateKubeconfig(t *testing.T) {
	writeKubeconfig := func(mode os.FileMode) string {
		path := filepath.Join(t.TempDir(), "kubeconfig")
		if err := os.WriteFile(path, []byte(kubeconfigYAML), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}

	t.Run("private kubeconfig loads", func(t *testing.T) {
		cfg, err := RESTConfig(writeKubeconfig(0o600))
		if err != nil {
			t.Fatalf("RESTConfig(0600) = %v, want success", err)
		}
		if cfg.Host != "http://kubeconfig.test" {
			t.Errorf("Host = %q, want the kubeconfig's server", cfg.Host)
		}
	})

	t.Run("world-readable kubeconfig is refused", func(t *testing.T) {
		_, err := RESTConfig(writeKubeconfig(0o644))
		if err == nil || !strings.Contains(err.Error(), "mode") {
			t.Fatalf("RESTConfig(0644) = %v, want a mode rejection", err)
		}
	})

	t.Run("symlinked kubeconfig is refused", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte(kubeconfigYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		_, err := RESTConfig(link)
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("RESTConfig(symlink) = %v, want a symlink rejection", err)
		}
	})

	t.Run("missing kubeconfig is an error", func(t *testing.T) {
		if _, err := RESTConfig(filepath.Join(t.TempDir(), "absent")); err == nil {
			t.Fatal("RESTConfig(absent) must fail")
		}
	})
}

func TestNodesReady(t *testing.T) {
	t.Run("all ready", func(t *testing.T) {
		cs := fake.NewSimpleClientset(node("a", corev1.ConditionTrue), node("b", corev1.ConditionTrue))
		ready, notReady, err := NodesReady(context.Background(), cs)
		if err != nil || ready != 2 || len(notReady) != 0 {
			t.Errorf("NodesReady() = (%d, %v, %v), want (2, [], nil)", ready, notReady, err)
		}
	})

	t.Run("one not ready", func(t *testing.T) {
		cs := fake.NewSimpleClientset(node("a", corev1.ConditionTrue), node("b", corev1.ConditionFalse))
		ready, notReady, err := NodesReady(context.Background(), cs)
		if err != nil || ready != 1 || len(notReady) != 1 || notReady[0] != "b" {
			t.Errorf("NodesReady() = (%d, %v, %v), want (1, [b], nil)", ready, notReady, err)
		}
	})

	t.Run("zero nodes is not ready", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		ready, notReady, err := NodesReady(context.Background(), cs)
		if err != nil || ready != 0 || len(notReady) != 0 {
			t.Errorf("NodesReady() = (%d, %v, %v), want (0, [], nil)", ready, notReady, err)
		}
	})

	t.Run("missing condition counts as not ready", func(t *testing.T) {
		cs := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "a"}})
		_, notReady, err := NodesReady(context.Background(), cs)
		if err != nil || len(notReady) != 1 || notReady[0] != "a" {
			t.Errorf("NodesReady() = (_, %v, %v), want ([a], nil)", notReady, err)
		}
	})

	t.Run("list error", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		cs.PrependReactor("list", "nodes", func(_ k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errors.New(" apiserver down")
		})
		if _, _, err := NodesReady(context.Background(), cs); err == nil {
			t.Fatal("expected a list error")
		}
	})
}

func TestClusterInstanceID(t *testing.T) {
	ns := func(uid string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: "kube-system", UID: types.UID(uid),
		}}
	}

	t.Run("returns the kube-system UID", func(t *testing.T) {
		cs := fake.NewSimpleClientset(ns("instance-42"))
		id, err := ClusterInstanceID(context.Background(), cs)
		if err != nil || id != "instance-42" {
			t.Errorf("ClusterInstanceID() = (%q, %v), want (instance-42, nil)", id, err)
		}
	})

	t.Run("missing namespace is an error", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		if _, err := ClusterInstanceID(context.Background(), cs); err == nil {
			t.Error("expected an error when kube-system is missing")
		}
	})
}

func TestWaitForNodesReady(t *testing.T) {
	t.Run("ready immediately", func(t *testing.T) {
		cs := fake.NewSimpleClientset(node("a", corev1.ConditionTrue))
		if err := WaitForNodesReady(context.Background(), cs, time.Minute); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("becomes ready after polling", func(t *testing.T) {
		notReady := node("a", corev1.ConditionFalse)
		cs := fake.NewSimpleClientset(notReady)
		go func() {
			// The node flips to Ready shortly after the first poll.
			time.Sleep(1500 * time.Millisecond)
			if _, err := cs.CoreV1().Nodes().UpdateStatus(context.Background(), node("a", corev1.ConditionTrue), metav1.UpdateOptions{}); err != nil {
				t.Error(err)
			}
		}()
		if err := WaitForNodesReady(context.Background(), cs, 30*time.Second); err != nil {
			t.Fatalf("expected readiness after polling: %v", err)
		}
	})

	t.Run("timeout names the not-ready node", func(t *testing.T) {
		cs := fake.NewSimpleClientset(node("a", corev1.ConditionFalse))
		start := time.Now()
		err := WaitForNodesReady(context.Background(), cs, 50*time.Millisecond)
		if err == nil {
			t.Fatal("expected a timeout error")
		}
		for _, substr := range []string{"nodes not Ready", "a"} {
			if !strings.Contains(err.Error(), substr) {
				t.Errorf("error %q does not mention %q", err, substr)
			}
		}
		// The timeout must bound the wait: the poll wakes at the deadline,
		// not at the next poll interval (a prior revision waited a full
		// second past a 50 ms timeout).
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Errorf("50 ms timeout took %s; the wait is not bounded by its deadline", elapsed)
		}
	})

	t.Run("timeout with zero nodes", func(t *testing.T) {
		cs := fake.NewSimpleClientset()
		err := WaitForNodesReady(context.Background(), cs, 50*time.Millisecond)
		if err == nil || !strings.Contains(err.Error(), "no nodes found") {
			t.Fatalf("error = %v, want a zero-nodes timeout error", err)
		}
	})

	t.Run("context cancellation wins", func(t *testing.T) {
		cs := fake.NewSimpleClientset(node("a", corev1.ConditionFalse))
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := WaitForNodesReady(ctx, cs, time.Minute)
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})
}

// A stalled apiserver response must not outlive the timeout, and a late "all
// Ready" answer must not turn the wait into a success. The server below
// holds the node list until the client cancels the request, then answers
// with every node Ready anyway; the wait must fail within its deadline.
func TestWaitForNodesReadyStalledResponseIsBounded(t *testing.T) {
	lateResponse := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // hold the response until the client gives up
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"NodeList","apiVersion":"v1","metadata":{},"items":[` +
			`{"metadata":{"name":"stalled-control-plane"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`))
		close(lateResponse)
	}))
	t.Cleanup(srv.Close)

	cs, err := kubernetes.NewForConfig(&rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err = WaitForNodesReady(context.Background(), cs, 250*time.Millisecond)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("a stalled response must not produce success, even when the late answer says every node is Ready")
	}
	if !strings.Contains(err.Error(), "nodes not Ready") {
		t.Errorf("error = %v, want a readiness timeout error", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("250 ms timeout took %s; the stalled API call was not cancelled at the deadline", elapsed)
	}
	select {
	case <-lateResponse:
		// The server saw the cancellation and its late answer arrived
		// after the wait had already failed.
	case <-time.After(5 * time.Second):
		t.Error("the server never observed the request cancellation; the client did not bound the request")
	}
}

// The no-redirect client bundle refuses redirects: a 307 answered
// with a Location is surfaced as an error instead of being followed —
// Go's http.Client would re-send the original POST and body to the
// target — so a request that must be sent exactly once is never
// re-sent to a target it did not choose. The POST goes through the
// bundle's clientset CoreV1 REST client, the same path the probe's
// create takes.
func TestNewNoRedirectClientFromConfigRefusesRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		targetHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"kind":"Pod","apiVersion":"v1","metadata":{"name":"p","uid":"redirected"}}`))
	}))
	t.Cleanup(target.Close)

	var posts atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(api.Close)

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	content := strings.Replace(kubeconfigYAML, "http://kubeconfig.test", api.URL, 1)
	if err := os.WriteFile(kubeconfig, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := RESTConfig(kubeconfig)
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
	}
	client, err := NewNoRedirectClientFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewNoRedirectClientFromConfig: %v", err)
	}
	var pod corev1.Pod
	err = client.Clientset.CoreV1().RESTClient().Post().
		Namespace("default").
		Resource("pods").
		Body(&corev1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: "p"}}).
		MaxRetries(0).
		Do(context.Background()).
		Into(&pod)
	if err == nil {
		t.Fatal("the redirected POST succeeded, want the redirect refused")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("error = %v, want the redirect named in the refusal", err)
	}
	if n := posts.Load(); n != 1 {
		t.Errorf("wire POSTs = %d, want exactly 1 (the redirect must not re-send the request)", n)
	}
	if n := targetHits.Load(); n != 0 {
		t.Errorf("requests to the redirect target = %d, want 0 (the redirect must not be followed)", n)
	}
}

// The refusal discloses nothing about the redirect target. A Location
// can carry credentials in its userinfo or its query, and Go's own
// error paths would echo it: the url.Error http.Client returns for a
// followed redirect embeds the target URL, and the CheckRedirect error
// is its cause. Refusing inside the transport — before http.Client
// ever builds a target request — leaves the Location nowhere to
// surface: the refusal names the original request and the status, and
// neither the userinfo nor the query of the target appears anywhere in
// the reported error.
func TestNewNoRedirectClientFromConfigRefusalDisclosesNoTarget(t *testing.T) {
	// Distinctive secrets the assertion can look for verbatim.
	const (
		targetUser     = "token-mount-user"
		targetPassword = "hunter2-password"
		targetQuery    = "sa-token-value"
	)
	var posts atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.Header().Set("Location",
			"https://"+targetUser+":"+targetPassword+"@redirect-target.example/collect?serviceaccount_token="+targetQuery)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(api.Close)

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	content := strings.Replace(kubeconfigYAML, "http://kubeconfig.test", api.URL, 1)
	if err := os.WriteFile(kubeconfig, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := RESTConfig(kubeconfig)
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
	}
	client, err := NewNoRedirectClientFromConfig(cfg)
	if err != nil {
		t.Fatalf("NewNoRedirectClientFromConfig: %v", err)
	}
	var pod corev1.Pod
	err = client.Clientset.CoreV1().RESTClient().Post().
		Namespace("default").
		Resource("pods").
		Body(&corev1.Pod{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"}, ObjectMeta: metav1.ObjectMeta{Name: "p"}}).
		MaxRetries(0).
		Do(context.Background()).
		Into(&pod)
	if err == nil {
		t.Fatal("the redirected POST succeeded, want the redirect refused")
	}
	for _, want := range []string{"redirect", "307"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want %q named in the refusal (the status is not secret)", err, want)
		}
	}
	// The target's userinfo, query credential, host, and path: the
	// refusal names the original request's method and path only, so
	// none of these can appear. (/collect is the target's path; the
	// original request's path is /api/v1/namespaces/default/pods.)
	for _, secret := range []string{targetUser, targetPassword, targetQuery, "redirect-target.example", "serviceaccount_token", "/collect"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error = %v, want no trace of the redirect target (%q found)", err, secret)
		}
	}
	if n := posts.Load(); n != 1 {
		t.Errorf("wire POSTs = %d, want exactly 1 (the redirect must not re-send the request)", n)
	}
}

// The clientset kindocp's own paths call (New wires it as the Client's
// clientset field; the lifecycle waits, the identity reads, and the
// cross-cluster check's creates and UID-preconditioned cleanup deletes
// all go through it) refuses redirects: a 307 answered with a Location
// is surfaced as an error instead of being followed. Go's http.Client
// would re-send a replayable POST or DELETE body to the 307/308
// target, so a redirected UID-preconditioned delete could act on an
// endpoint the caller never chose, and a redirected read can report
// another endpoint's answer (an absence included) as this cluster's.
// The regression drives the two call shapes kindocp issues — the
// identity read and the UID-preconditioned cleanup delete — through
// the constructor against a local apiserver that answers with a
// redirect: the refusal must surface as an error, exactly one request
// may reach the original endpoint, and none may reach the target.
func TestNewClientsetRefusesRedirects(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(ctx context.Context, cs kubernetes.Interface) error
	}{
		{
			name: "the identity read (GET kube-system)",
			call: func(ctx context.Context, cs kubernetes.Interface) error {
				_, err := cs.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
				return err
			},
		},
		{
			name: "the UID-preconditioned cleanup delete",
			call: func(ctx context.Context, cs kubernetes.Interface) error {
				uid := types.UID("uid-probe-namespace")
				return cs.CoreV1().Namespaces().Delete(ctx, "kind-ocp-verify-deadbeefcafe", metav1.DeleteOptions{
					Preconditions: &metav1.Preconditions{UID: &uid},
				})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var targetHits atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				targetHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(target.Close)

			var apiRequests atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				apiRequests.Add(1)
				http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			}))
			t.Cleanup(api.Close)

			kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
			content := strings.Replace(kubeconfigYAML, "http://kubeconfig.test", api.URL, 1)
			if err := os.WriteFile(kubeconfig, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			cs, err := NewClientset(kubeconfig)
			if err != nil {
				t.Fatalf("NewClientset: %v", err)
			}
			err = tc.call(context.Background(), cs)
			if err == nil {
				t.Fatal("the redirected call succeeded, want the redirect refused")
			}
			if !strings.Contains(err.Error(), "redirect") {
				t.Errorf("error = %v, want the redirect named in the refusal", err)
			}
			if n := apiRequests.Load(); n != 1 {
				t.Errorf("requests reaching the original endpoint = %d, want exactly 1 (the redirect must not re-send the request)", n)
			}
			if n := targetHits.Load(); n != 0 {
				t.Errorf("requests reaching the redirect target = %d, want 0 (the redirect must not be followed)", n)
			}
		})
	}
}

// TestNewClientsetRefusalDisclosesNoTarget: the clientset
// constructor's refusal discloses nothing about the redirect target —
// the same guarantee the bundle variant pins, proven through the
// constructor kindocp actually calls. A Location can carry
// credentials in its userinfo or its query, and Go's own error paths
// would echo it: the url.Error http.Client returns for a followed
// redirect embeds the target URL. Refusing inside the transport —
// before http.Client ever builds a target request — leaves the
// Location nowhere to surface. The 308 is deliberate: it is the other
// status whose redirect re-sends a DELETE's body, so the refusal is
// proven on the body-carrying request shape.
func TestNewClientsetRefusalDisclosesNoTarget(t *testing.T) {
	// Distinctive secrets the assertion can look for verbatim.
	const (
		targetUser     = "token-mount-user"
		targetPassword = "hunter2-password"
		targetQuery    = "sa-token-value"
	)
	var deletes atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deletes.Add(1)
		w.Header().Set("Location",
			"https://"+targetUser+":"+targetPassword+"@redirect-target.example/collect?serviceaccount_token="+targetQuery)
		w.WriteHeader(http.StatusPermanentRedirect)
	}))
	t.Cleanup(api.Close)

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	content := strings.Replace(kubeconfigYAML, "http://kubeconfig.test", api.URL, 1)
	if err := os.WriteFile(kubeconfig, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := NewClientset(kubeconfig)
	if err != nil {
		t.Fatalf("NewClientset: %v", err)
	}
	uid := types.UID("uid-probe-namespace")
	err = cs.CoreV1().Namespaces().Delete(context.Background(), "kind-ocp-verify-deadbeefcafe", metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	})
	if err == nil {
		t.Fatal("the redirected DELETE succeeded, want the redirect refused")
	}
	for _, want := range []string{"redirect", "308"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want %q named in the refusal (the status is not secret)", err, want)
		}
	}
	// The target's userinfo, query credential, host, and path: the
	// refusal names the original request's method and path only, so
	// none of these can appear. (/collect is the target's path; the
	// original request's path is /api/v1/namespaces/<namespace>.)
	for _, secret := range []string{targetUser, targetPassword, targetQuery, "redirect-target.example", "serviceaccount_token", "/collect"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("error = %v, want no trace of the redirect target (%q found)", err, secret)
		}
	}
	if n := deletes.Load(); n != 1 {
		t.Errorf("wire DELETEs = %d, want exactly 1 (the redirect must not re-send the UID-preconditioned delete)", n)
	}
}

// The no-redirect bundle's clientset must issue the identical wire
// request a client-go clientset's Create does. The probe's create
// borrows that clientset's CoreV1 REST client — the same generated
// construction the typed create goes through, from a bundle whose
// http.Client is built separately — so the identity is pinned here:
// method, path, query, every header (User-Agent and Content-Length
// included), and the serialized body, against a baseline clientset
// constructed straight from the same config with
// kubernetes.NewForConfig (redirect-following, client-go's own
// construction — exactly the wire the bundle must not drift from).
// User-Agent is the header the construction order decides: the UA
// round-tripper installs only for a non-empty UserAgent at
// transport-build time, so the default must be applied before the
// transport is built, exactly as kubernetes.NewForConfig does —
// otherwise the raw POST carries Go's Go-http-client/1.1 where every
// other kind-ocp request carries the client-go User-Agent. The two
// pods differ the way two probe attempts differ — in name and attempt
// token, nothing else — with same-length values, so a Content-Length
// difference is a real defect, not a fixture-length confound.
func TestNewNoRedirectClientFromConfigWireMatchesTheTypedCreate(t *testing.T) {
	// wireReq is one recorded wire request.
	type wireReq struct {
		method        string
		path          string
		rawQuery      string
		headers       http.Header
		contentLength int64
		body          string
	}

	var mu sync.Mutex
	var reqs []wireReq
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading the recorded body: %v", err)
			return
		}
		mu.Lock()
		reqs = append(reqs, wireReq{
			method:        r.Method,
			path:          r.URL.Path,
			rawQuery:      r.URL.RawQuery,
			headers:       r.Header.Clone(),
			contentLength: r.ContentLength,
			body:          string(body),
		})
		mu.Unlock()
		var obj unstructured.Unstructured
		if err := json.Unmarshal(body, &obj); err != nil {
			t.Errorf("decoding the recorded body: %v", err)
			return
		}
		obj.SetUID(types.UID("wire-identity-uid"))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(obj.Object)
	}))
	t.Cleanup(api.Close)

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	content := strings.Replace(kubeconfigYAML, "http://kubeconfig.test", api.URL, 1)
	if err := os.WriteFile(kubeconfig, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := RESTConfig(kubeconfig)
	if err != nil {
		t.Fatalf("loading the REST config: %v", err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("building the client-go baseline clientset: %v", err)
	}
	bundle, err := NewNoRedirectClientFromConfig(cfg)
	if err != nil {
		t.Fatalf("building the no-redirect client: %v", err)
	}
	raw := bundle.Clientset.CoreV1().RESTClient()

	// wirePod builds the probe-shaped pod; a name and an attempt token
	// are the only fields two attempts differ in.
	wirePod := func(name, token string) *corev1.Pod {
		runAsNonRoot := true
		return &corev1.Pod{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
				"kind-ocp.io/wire-probe":   "scc-admission",
				"kind-ocp.io/wire-attempt": token,
			}},
			Spec: corev1.PodSpec{
				SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: &runAsNonRoot},
				RestartPolicy:   corev1.RestartPolicyNever,
				Containers: []corev1.Container{{
					Name:            "probe",
					Image:           "registry.test/kind-ocp/probe:v1",
					ImagePullPolicy: corev1.PullNever,
				}},
			},
		}
	}

	typedPod := wirePod("kind-ocp-wire-probe-1a2b3c4d", "a1b2c3d4e5f6a7b8a1b2c3d4e5f6a7b8")
	if _, err := cs.CoreV1().Pods("wire-identity").Create(
		context.Background(), typedPod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("the typed create: %v", err)
	}
	var created corev1.Pod
	rawPod := wirePod("kind-ocp-wire-probe-5e6f7a8b", "11223344556677881122334455667788")
	if err := raw.Post().
		Namespace("wire-identity").
		Resource("pods").
		Body(rawPod).
		MaxRetries(0).
		Do(context.Background()).
		Into(&created); err != nil {
		t.Fatalf("the no-redirect create: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(reqs) != 2 {
		t.Fatalf("recorded requests = %d, want 2", len(reqs))
	}
	typed, rawReq := reqs[0], reqs[1]

	wantLine := "POST /api/v1/namespaces/wire-identity/pods"
	for _, req := range []wireReq{typed, rawReq} {
		if line := req.method + " " + req.path; line != wantLine {
			t.Errorf("request line = %q, want %q", line, wantLine)
		}
	}
	if typed.rawQuery != rawReq.rawQuery {
		t.Errorf("query strings differ: typed %q, raw %q", typed.rawQuery, rawReq.rawQuery)
	}

	// Content-Length is tied to the recorded body on each request and
	// equal across the two: the same-length fixtures leave a real
	// difference observable.
	for _, req := range []wireReq{typed, rawReq} {
		if req.contentLength != int64(len(req.body)) {
			t.Errorf("Content-Length %d does not match the %d-byte body of %s %s",
				req.contentLength, len(req.body), req.method, req.path)
		}
	}
	if typed.contentLength != rawReq.contentLength {
		t.Errorf("Content-Length differs: typed %d, raw %d", typed.contentLength, rawReq.contentLength)
	}

	// The bodies must serialize identically modulo the two values that
	// identify an attempt.
	if normTyped, normRaw := normalizeWireBody(t, typed.body), normalizeWireBody(t, rawReq.body); normTyped != normRaw {
		t.Errorf("bodies differ beyond the attempt's own values:\ntyped: %s\nraw:   %s", normTyped, normRaw)
	}

	// User-Agent gets its own assertion with the exact expected value,
	// so a future construction change cannot pass by making both
	// requests wrong the same way.
	wantUA := rest.DefaultKubernetesUserAgent()
	if got := typed.headers.Get("User-Agent"); got != wantUA {
		t.Errorf("the typed create's User-Agent = %q, want the clientset default %q", got, wantUA)
	}
	if got := rawReq.headers.Get("User-Agent"); got != wantUA {
		t.Errorf("the no-redirect client's User-Agent = %q, want the clientset default %q", got, wantUA)
	}
	diffWireHeaders(t, typed.headers, rawReq.headers)
}

// normalizeWireBody blanks the pod's name and attempt token — the two
// values that differ between two probe attempts — so the two creates'
// bodies compare byte for byte.
func normalizeWireBody(t *testing.T, body string) string {
	t.Helper()
	var obj unstructured.Unstructured
	if err := json.Unmarshal([]byte(body), &obj); err != nil {
		t.Fatalf("normalizing the recorded body %q: %v", body, err)
	}
	obj.SetName("WIRE-NORMALIZED")
	if labels, _, _ := unstructured.NestedStringMap(obj.Object, "metadata", "labels"); labels != nil {
		labels["kind-ocp.io/wire-attempt"] = "WIRE-NORMALIZED"
		if err := unstructured.SetNestedStringMap(obj.Object, labels, "metadata", "labels"); err != nil {
			t.Fatalf("blanking the attempt token: %v", err)
		}
	}
	out, err := json.Marshal(obj.Object)
	if err != nil {
		t.Fatalf("re-encoding the recorded body: %v", err)
	}
	return string(out)
}

// diffWireHeaders reports every header the two recorded requests do not
// carry identically (values compared in order as slices — joining them
// into a string could make distinct repeated values compare equal; Go
// canonicalizes header names, so the maps are directly comparable key
// for key).
func diffWireHeaders(t *testing.T, a, b http.Header) {
	t.Helper()
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	for k := range keys {
		if !slices.Equal(a[k], b[k]) {
			t.Errorf("header %q differs:\ntyped: %v\nraw:   %v", k, a[k], b[k])
		}
	}
}

// roundTripperFunc adapts a function to http.RoundTripper.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// A successful List that races cancellation must not become success either:
// the poll helper accepts the callback's "done" even when the context ended
// during the callback, so the callback itself must refuse a post-deadline
// answer. The transport below ignores the request context, holds the node
// list until the deadline has fired, and then answers with every node Ready.
func TestWaitForNodesReadyLateReadyAnswerAfterDeadlineRejected(t *testing.T) {
	rt := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done() // the deadline fires while the List is in flight
		body := `{"kind":"NodeList","apiVersion":"v1","metadata":{},"items":[` +
			`{"metadata":{"name":"late-control-plane"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}]}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	cs, err := kubernetes.NewForConfig(&rest.Config{Host: "http://late.test", Transport: rt})
	if err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err = WaitForNodesReady(context.Background(), cs, 100*time.Millisecond)
	if err == nil {
		t.Fatal("an all-Ready answer that arrives after the deadline must not produce success")
	}
	for _, substr := range []string{"nodes not Ready", "not accepted"} {
		if !strings.Contains(err.Error(), substr) {
			t.Errorf("error %q does not mention %q", err, substr)
		}
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("100 ms timeout took %s; the wait is not bounded by its deadline", elapsed)
	}
}

// Package kube contains the Kubernetes client helpers shared by the
// provisioning and verification paths and (from the olm layer onward) by
// layers. It is the only package that builds client-go clients.
package kube

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/guzalv/kind-ocp/internal/fscheck"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// pollInterval is the node-readiness poll period. Node readiness takes
// seconds to minutes after kubeadm init; polling every second is cheap and
// keeps the reported timeout tight.
const pollInterval = time.Second

// clientQPS and clientBurst set the rate limit every client kind-ocp
// builds applies to the one local kind apiserver its kubeconfig points
// at. client-go's defaults (5 requests per second, burst 10) protect a
// shared master from a noisy tenant; here they only add latency — the
// platform-objects layer alone issues dozens of requests per run
// (object applies, status writes, SCC creates), each mapper rebuild
// adds a discovery round, and the waits poll once a second regardless.
// A local apiserver takes this without noticing.
const (
	clientQPS   = 50
	clientBurst = 100
)

// RESTConfig builds a client config from a kubeconfig file path. The
// file must first verify as a private, caller-owned regular file: the
// kubeconfigs kind-ocp loads are cluster-admin credentials, and one that
// is world-readable, foreign-owned, or a symlink (a shared or restored
// state tree) is refused instead of loaded.
func RESTConfig(kubeconfigPath string) (*rest.Config, error) {
	if err := fscheck.CheckPrivateFile(kubeconfigPath); err != nil {
		return nil, fmt.Errorf("using kubeconfig %s: %w", kubeconfigPath, err)
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig %s: %w", kubeconfigPath, err)
	}
	cfg.QPS = clientQPS
	cfg.Burst = clientBurst
	// JSON, not client-go's default protobuf negotiation: kind-ocp's
	// request volume is tiny (the rate limits above are the real
	// consideration), while a fixed, inspectable wire format keeps the
	// fake apiservers of the layer tests — and any operator watching
	// traffic — on one format.
	cfg.ContentType = "application/json"
	return cfg, nil
}

// NewClientset returns a typed clientset for a kubeconfig file path
// whose transport refuses redirects. It is the one typed constructor
// kind-ocp builds, and every request its callers issue is meant for
// exactly the endpoint the kubeconfig pins: a followed redirect would
// break that — Go's http.Client re-sends a replayable POST or DELETE
// body to the redirect target on 307/308, so a UID-preconditioned
// delete or an exactly-once create could land on an endpoint the
// caller never chose, and a redirected read can report another
// endpoint's answer (an absence included) as this cluster's. The
// apiservers kind-ocp talks to never redirect, so the refusal is loud,
// not lossy, and ordinary CRUD through this clientset is unaffected.
// See refuseRedirects for why the refusal belongs in the transport,
// and NewNoRedirectClientFromConfig for the bundle variant layers use.
func NewClientset(kubeconfigPath string) (kubernetes.Interface, error) {
	cfg, err := RESTConfig(kubeconfigPath)
	if err != nil {
		return nil, err
	}
	httpClient, err := noRedirectHTTPClient(cfg)
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfigAndClient(cfg, httpClient)
	if err != nil {
		return nil, fmt.Errorf("building clientset: %w", err)
	}
	return cs, nil
}

// noRedirectHTTPClient builds one http.Client for cfg whose transport
// refuses redirects: the single load-and-wrap step every no-redirect
// client shares. The UserAgent is defaulted before the transport is
// built, mirroring kubernetes.NewForConfig's order: the UA
// round-tripper installs only for a non-empty UserAgent at
// transport-build time, and the defaulting the generated constructors
// apply happens on copies that never reach an already-built transport —
// without this the requests would carry Go's Go-http-client/1.1 where
// every other kind-ocp request carries the client-go User-Agent (pinned
// by TestNewNoRedirectClientFromConfigWireMatchesTheTypedCreate). A
// shallow copy of the built client, then: every client constructed on
// it issues its requests through the one transport, so one config load
// means one endpoint and one redirect policy for everything the caller
// does.
func noRedirectHTTPClient(cfg *rest.Config) (*http.Client, error) {
	c := *cfg
	if c.UserAgent == "" {
		c.UserAgent = rest.DefaultKubernetesUserAgent()
	}
	base, err := rest.HTTPClientFor(&c)
	if err != nil {
		return nil, fmt.Errorf("building the HTTP client: %w", err)
	}
	noRedirect := *base
	noRedirect.Transport = refuseRedirects{next: base.Transport}
	return &noRedirect, nil
}

// refuseRedirects keeps a request exactly one on the wire. Go's
// http.Client answers a redirect status by issuing another request to
// the Location's target — re-sending a POST's method and body for
// 307/308, converting it to a GET for 301/302/303 — with no retry
// policy consulted. Refusing inside the transport means http.Client
// never sees the redirect answer: no target request is built, and no
// *url.Error can embed the target URL, which can carry credentials in
// its userinfo or query. The refusal error names the original request
// and the status, never the target. The five statuses below are
// exactly the ones Go's http.Client follows (net/http's
// redirectBehavior); every other 3xx answer passes through untouched
// and keeps the behavior it always had — a final response client-go
// reports as a status error.
type refuseRedirects struct{ next http.RoundTripper }

func (t refuseRedirects) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusMovedPermanently,
		http.StatusFound,
		http.StatusSeeOther,
		http.StatusTemporaryRedirect,
		http.StatusPermanentRedirect:
		// The body of the refused answer is closed, not drained: the
		// connection is dropped rather than reused, which is the
		// correct trade for a client whose requests must never be
		// re-sent.
		status := resp.StatusCode
		_ = resp.Body.Close()
		return nil, fmt.Errorf("the server answered %s %s with a redirect (status %d), which is refused: a request that must be sent exactly once is not re-sent to a target it did not choose, and the target is not reported here because a redirect Location can carry credentials in its userinfo or query",
			req.Method, req.URL.Path, status)
	}
	return resp, nil
}

// NodesReady checks the Ready condition of every node and returns the count
// of Ready nodes plus the names of nodes that are not Ready. Zero nodes is
// reported through the return values as "not ready" (ready == 0, no names):
// a usable cluster has at least one node, so callers treat it as failure
// rather than as a vacuous pass.
func NodesReady(ctx context.Context, cs kubernetes.Interface) (ready int, notReady []string, err error) {
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0, nil, fmt.Errorf("listing nodes: %w", err)
	}
	for i := range nodes.Items {
		if nodeReady(&nodes.Items[i]) {
			ready++
		} else {
			notReady = append(notReady, nodes.Items[i].Name)
		}
	}
	return ready, notReady, nil
}

// ClusterInstanceID returns a stable identifier for the cluster instance:
// the UID of the kube-system namespace, assigned at bootstrap and persisted
// for the life of the cluster. A cluster deleted and re-created under the
// same name gets a new UID, so records bound to the old UID can detect a
// same-named replacement.
func ClusterInstanceID(ctx context.Context, cs kubernetes.Interface) (string, error) {
	ns, err := cs.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("reading kube-system namespace: %w", err)
	}
	return string(ns.UID), nil
}

// WaitForNodesReady polls NodesReady until every node is Ready, the timeout
// elapses, or ctx is done. It is the one readiness wait in kind-ocp: kind's
// own wait logs a timeout and returns success, so this check — bounded,
// context-aware, and failing loudly — replaces it on both the create and the
// reuse path.
//
// The timeout bounds the whole wait, not just the polling: the poll runs
// under a deadline context that is passed to the node List call, so a
// stalled apiserver response is cancelled at the deadline. Success is also
// gated on the poll context: the poll helper returns as soon as the
// callback reports done — even when the context ended during the callback —
// so an all-Ready answer that arrives after the deadline (a List that raced
// cancellation) is recorded as a late observation and rejected, never
// accepted as success. The timeout error names the last useful observation.
func WaitForNodesReady(ctx context.Context, cs kubernetes.Interface, timeout time.Duration) error {
	return pollCondition(ctx, timeout, "node readiness", "nodes not Ready",
		func(ctx context.Context) (bool, string) {
			ready, notReady, err := NodesReady(ctx, cs)
			switch {
			case err == nil && len(notReady) == 0 && ready > 0:
				return true, "all nodes Ready"
			case err == nil && len(notReady) > 0:
				return false, fmt.Sprintf("nodes not Ready: %s", strings.Join(notReady, ", "))
			case err == nil:
				return false, "no nodes found"
			case ctx.Err() != nil:
				// The deadline or caller cancellation aborted this call;
				// the previous observation is more useful than the
				// cancellation text.
				return false, ""
			default:
				return false, err.Error()
			}
		})
}

func nodeReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

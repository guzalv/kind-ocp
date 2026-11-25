package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// centralPortForward is a localhost tunnel to the Central pod, used for the
// run's own API calls (generating the cluster registration secret, watching
// cluster health). The Sensors reach Central over the shared kind network's
// NodePort; that address is not reachable from the host on every platform
// (Docker Desktop's bridge is host-invisible on macOS), so the example
// talks to the pod the way `kubectl port-forward` does — through the same
// client-go machinery.
type centralPortForward struct {
	stop chan struct{}
}

// close tears the tunnel down. It is safe to call more than once.
func (p *centralPortForward) close() {
	select {
	case <-p.stop:
	default:
		close(p.stop)
	}
}

// startCentralPortForward locates the running Central pod on the Central
// cluster and forwards a local port to the pod's API port (the service's
// 443 behind it). It returns the forward — close it when done — and the
// https base URL to talk to.
func startCentralPortForward(ctx context.Context, kubeconfigPath string) (*centralPortForward, string, error) {
	cs, restConfig, err := clientsetFor(kubeconfigPath)
	if err != nil {
		return nil, "", err
	}
	podName, err := centralPodName(ctx, cs)
	if err != nil {
		return nil, "", err
	}
	localPort, err := freeLocalPort()
	if err != nil {
		return nil, "", err
	}

	serverURL, err := apiServerURL(restConfig)
	if err != nil {
		return nil, "", err
	}
	forwardURL, err := url.Parse(fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s/portforward",
		serverURL.String(), stackroxNamespace, podName))
	if err != nil {
		return nil, "", err
	}

	roundTripper, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return nil, "", fmt.Errorf("building the port-forward transport: %w", err)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, forwardURL)

	stop := make(chan struct{})
	ready := make(chan struct{})
	// The forwarder's own progress messages go to the run's stderr: they
	// are part of the evidence, and a tunnel that never comes up should
	// say so where the run can see it. Ports are "local:remote" pairs;
	// the forwarder's listen address is localhost.
	fw, err := portforward.New(dialer, []string{fmt.Sprintf("%d:%d", localPort, centralPodPort)},
		stop, ready, os.Stderr, os.Stderr)
	if err != nil {
		return nil, "", fmt.Errorf("building the port-forward: %w", err)
	}
	go func() {
		// ForwardPorts returns when the stop channel closes; its error
		// (a tunnel that died mid-run) lands on stderr from inside the
		// forwarder, and the API calls fail on their own deadlines.
		_ = fw.ForwardPorts()
	}()

	select {
	case <-ready:
		return &centralPortForward{stop: stop}, fmt.Sprintf("https://127.0.0.1:%d", localPort), nil
	case <-time.After(30 * time.Second):
		close(stop)
		return nil, "", errors.New("the Central port-forward did not become ready within 30s")
	case <-ctx.Done():
		close(stop)
		return nil, "", ctx.Err()
	}
}

// freeLocalPort asks the kernel for a free TCP port on the loopback
// interface. The listen-close-use window is a benign race for a recipe; a
// loser fails loudly when the forwarder cannot bind.
func freeLocalPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("reserving a local port: %w", err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// centralAPI is a minimal Central REST client: basic auth with the admin
// password, TLS verification skipped because the certificate is the
// chart-generated self-signed one and the connection is a loopback tunnel
// to the pod. The Sensors' own connection is different in both respects —
// mTLS with their registration identity over the shared network — and is
// verified by the readiness wait, not by this client.
type centralAPI struct {
	base     string
	password string
	client   *http.Client
}

func newCentralAPI(base, password string) *centralAPI {
	return &centralAPI{
		base:     base,
		password: password,
		client: &http.Client{
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // see the type comment
			Timeout:   30 * time.Second,
		},
	}
}

// do performs one authenticated JSON request. body may be nil. A non-200
// answer is returned as an *apiError so callers can branch on the status.
func (a *centralAPI) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, reader)
	if err != nil {
		return err
	}
	req.SetBasicAuth("admin", a.password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &apiError{
			method: method,
			path:   path,
			status: resp.StatusCode,
			body:   truncateForError(string(b)),
		}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// apiError is one failed Central API call, carrying its HTTP status.
type apiError struct {
	method string
	path   string
	status int
	body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("%s %s: %s: %s", e.method, e.path, http.StatusText(e.status), e.body)
}

func isUnauthorized(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.status == http.StatusUnauthorized
}

// isConflict reports whether err is Central refusing a name it already
// holds — its answer to a second CRS under a name that exists.
func isConflict(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.status == http.StatusConflict
}

func truncateForError(s string) string {
	const cap = 400
	if len(s) <= cap {
		return s
	}
	return s[:cap] + "…"
}

// pollInterval is the wait between Central API polls. A variable so the
// retry tests can drive it short.
var pollInterval = 5 * time.Second

// waitUntil sleeps for the poll interval or the remaining budget, whichever
// is shorter, so a wait whose deadline is near fires on time instead of one
// full interval late.
func waitUntil(ctx context.Context, deadline time.Time) error {
	wait := pollInterval
	if remaining := time.Until(deadline); remaining < wait {
		wait = remaining
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(wait):
		return nil
	}
}

// waitForCentral polls Central's unauthenticated ping endpoint until it
// answers. A rolled-out Central deployment is not yet a serving Central;
// this is the gate between them.
func waitForCentral(ctx context.Context, a *centralAPI, timeout time.Duration, log *slog.Logger) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if lastErr = a.ping(ctx); lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("central did not answer %s within %s (last error: %v)", a.base+"/v1/ping", timeout, lastErr)
		}
		log.Info("waiting for Central to answer", "last_error", lastErr.Error())
		if err := waitUntil(ctx, deadline); err != nil {
			return err
		}
	}
}

func (a *centralAPI) ping(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.base+"/v1/ping", nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/v1/ping: %s", resp.Status)
	}
	return nil
}

// generateCRS creates one cluster registration secret — the modern
// replacement for init bundles, which Central 5.0 deprecates — and returns
// its secret manifest. The manifest is the value the secured-cluster chart
// expects under crs.file; the chart validates it (the embedded CAs) and
// creates the secret in each sensor cluster.
//
// The name is deterministic, so a re-run collides with the secret the
// previous run generated: Central refuses a name held by a non-revoked
// init bundle or CRS. The collision converges — the existing secret is
// revoked and a fresh one generated. Revocation removes only the
// bootstrap credential (Central enforces it for the registration
// certificates only; registered Sensors authenticate with separately
// issued identities), and the helm upgrade that follows re-applies the
// fresh manifest to every Sensor cluster, so a cluster without an
// identity can register.
//
// A 401 is retried within a bounded window: an upgrade that changed the
// admin password rewrites central-htpasswd through a helm hook, and
// Central applies the new file on its own watcher's schedule — requests
// inside that window authenticate against the previous credentials.
func (a *centralAPI) generateCRS(ctx context.Context, name string, log *slog.Logger) (string, error) {
	deadline := time.Now().Add(credentialApplyTimeout)
	for {
		manifest, err := a.generateCRSConverging(ctx, name, log)
		if err == nil {
			return manifest, nil
		}
		if !isUnauthorized(err) || time.Now().After(deadline) {
			return "", err
		}
		log.Info("waiting for Central to apply the admin credentials", "last_error", err.Error())
		if err := waitUntil(ctx, deadline); err != nil {
			return "", err
		}
	}
}

// generateCRSConverging performs one generation attempt: generate, and
// when Central already holds a secret under the name — every re-run does —
// revoke it and generate a fresh one. A second conflict is returned, not
// looped: it means something outside this flow holds the name.
func (a *centralAPI) generateCRSConverging(ctx context.Context, name string, log *slog.Logger) (string, error) {
	manifest, err := a.generateCRSOnce(ctx, name)
	if err == nil {
		return manifest, nil
	}
	if !isConflict(err) {
		return "", err
	}
	log.Info("a cluster registration secret with this name already exists; revoking and regenerating it", "name", name)
	if err := a.revokeCRSsNamed(ctx, name); err != nil {
		return "", fmt.Errorf("revoking the cluster registration secret of the previous run: %w", err)
	}
	return a.generateCRSOnce(ctx, name)
}

// credentialApplyTimeout bounds how long a 401 is treated as the
// credential-reload window rather than a wrong password. A variable so
// the retry test can drive it short.
var credentialApplyTimeout = 2 * time.Minute

func (a *centralAPI) generateCRSOnce(ctx context.Context, name string) (string, error) {
	var resp struct {
		CRS string `json:"crs"`
	}
	if err := a.do(ctx, http.MethodPost, "/v1/cluster-init/crs", map[string]string{"name": name}, &resp); err != nil {
		return "", err
	}
	if resp.CRS == "" {
		return "", errors.New("central returned an empty cluster registration secret")
	}
	// The proto bytes field arrives base64-encoded in the JSON response.
	manifest, err := base64.StdEncoding.DecodeString(resp.CRS)
	if err != nil {
		return "", fmt.Errorf("decoding the cluster registration secret: %w", err)
	}
	if err := validateCRSManifest(string(manifest)); err != nil {
		return "", err
	}
	return string(manifest), nil
}

// validateCRSManifest checks that what Central returned is the secret
// manifest shape the chart consumes, so a malformed or truncated response
// fails here instead of inside helm's template rendering.
func validateCRSManifest(manifest string) error {
	var doc struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Data       struct {
			CRS string `yaml:"crs"`
		} `yaml:"data"`
	}
	if err := yaml.Unmarshal([]byte(manifest), &doc); err != nil {
		return fmt.Errorf("the cluster registration secret is not valid YAML: %w", err)
	}
	if doc.Kind != "Secret" || doc.Data.CRS == "" {
		return fmt.Errorf("the cluster registration secret is not a Secret carrying data.crs (kind %q)", doc.Kind)
	}
	return nil
}

// crsSummary is one cluster registration secret's metadata as Central
// lists it. The secret material itself is only ever issued, never listed.
type crsSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// listCRSs lists the cluster registration secrets Central holds (revoked
// ones are not listed).
func (a *centralAPI) listCRSs(ctx context.Context) ([]crsSummary, error) {
	var resp struct {
		Items []crsSummary `json:"items"`
	}
	if err := a.do(ctx, http.MethodGet, "/v1/cluster-init/crs", nil, &resp); err != nil {
		return nil, err
	}
	return resp.Items, nil
}

// revokeCRSsNamed revokes every cluster registration secret Central holds
// under the name, so the name is free for a fresh one (Central refuses a
// name held by a non-revoked init bundle or CRS). Central reports a
// failing revocation inside a 200 answer, not through the status code, so
// the reported errors and the coverage of the revoked ids are both
// checked.
func (a *centralAPI) revokeCRSsNamed(ctx context.Context, name string) error {
	summaries, err := a.listCRSs(ctx)
	if err != nil {
		return err
	}
	var ids []string
	for _, s := range summaries {
		if s.Name == name {
			ids = append(ids, s.ID)
		}
	}
	if len(ids) == 0 {
		return fmt.Errorf("central lists no cluster registration secret named %q, so the recipe cannot free the name — the conflicting object must be removed in Central, or the run needs a different prefix", name)
	}
	var resp struct {
		RevokedIDs []string `json:"revokedIds"`
		Errors     []struct {
			ID    string `json:"id"`
			Error string `json:"error"`
		} `json:"crsRevocationErrors"`
	}
	if err := a.do(ctx, http.MethodPatch, "/v1/cluster-init/crs/revoke", map[string][]string{"ids": ids}, &resp); err != nil {
		return err
	}
	if len(resp.Errors) > 0 {
		refusals := make([]string, 0, len(resp.Errors))
		for _, e := range resp.Errors {
			refusals = append(refusals, fmt.Sprintf("%s: %s", e.ID, e.Error))
		}
		return fmt.Errorf("central refused the revocation (%s)", strings.Join(refusals, "; "))
	}
	revoked := make(map[string]bool, len(resp.RevokedIDs))
	for _, id := range resp.RevokedIDs {
		revoked[id] = true
	}
	for _, id := range ids {
		if !revoked[id] {
			return fmt.Errorf("central's answer does not report %q as revoked", id)
		}
	}
	return nil
}

// clusterHealth is one cluster's registration state as Central reports it.
type clusterHealth struct {
	Name   string
	Status string // overall health status label: HEALTHY, DEGRADED, …
}

// clusters lists the clusters Central knows about and their overall health
// status (assigned by Central from the Sensors' connections).
func (a *centralAPI) clusters(ctx context.Context) ([]clusterHealth, error) {
	var resp struct {
		Clusters []struct {
			Name         string `json:"name"`
			HealthStatus struct {
				OverallHealthStatus string `json:"overallHealthStatus"`
			} `json:"healthStatus"`
		} `json:"clusters"`
	}
	if err := a.do(ctx, http.MethodGet, "/v1/clusters", nil, &resp); err != nil {
		return nil, err
	}
	out := make([]clusterHealth, 0, len(resp.Clusters))
	for _, c := range resp.Clusters {
		out = append(out, clusterHealth{Name: c.Name, Status: c.HealthStatus.OverallHealthStatus})
	}
	return out, nil
}

// waitForHealthyClusters polls Central until every named Sensor cluster is
// registered and reports HEALTHY. This is the example's proof: a HEALTHY
// Sensor is a workload pod on one cluster that reached Central's NodePort
// on another and stays connected.
func waitForHealthyClusters(ctx context.Context, a *centralAPI, names []string, timeout time.Duration, log *slog.Logger) error {
	deadline := time.Now().Add(timeout)
	last := ""
	for {
		clusters, err := a.clusters(ctx)
		if err == nil {
			missing, unhealthy := classify(clusters, names)
			if len(missing) == 0 && len(unhealthy) == 0 {
				for _, c := range clusters {
					log.Info("cluster healthy in Central", "cluster", c.Name, "status", c.Status)
				}
				return nil
			}
			if summary := fmt.Sprintf("missing: %v; unhealthy: %v", missing, unhealthy); summary != last {
				log.Info("waiting for Sensor clusters in Central", "summary", summary)
				last = summary
			}
		} else if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("central's cluster list failed within %s: %w", timeout, err)
			}
			missing, unhealthy := classify(clusters, names)
			return fmt.Errorf("not every Sensor cluster became healthy within %s (missing: %v; unhealthy: %v)",
				timeout, missing, unhealthy)
		}
		if err := waitUntil(ctx, deadline); err != nil {
			return err
		}
	}
}

// classify splits the expected cluster names into those Central does not
// know yet and those it knows but does not report HEALTHY.
func classify(clusters []clusterHealth, names []string) (missing, unhealthy []string) {
	byName := make(map[string]string, len(clusters))
	for _, c := range clusters {
		byName[c.Name] = c.Status
	}
	for _, name := range names {
		status, ok := byName[name]
		switch {
		case !ok:
			missing = append(missing, name)
		case status != "HEALTHY":
			unhealthy = append(unhealthy, fmt.Sprintf("%s (%s)", name, status))
		}
	}
	return missing, unhealthy
}

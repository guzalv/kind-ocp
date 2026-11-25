package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
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
	cancel context.CancelCauseFunc
}

// close tears the tunnel down and fails every call running under its
// context. It is safe to call more than once: context cancellation is
// idempotent. It cancels without a cause — a deliberate close is not a
// failure, so the context reports plain cancellation, not an error.
func (p *centralPortForward) close() {
	p.cancel(nil)
}

// startCentralPortForward locates the running Central pod on the Central
// cluster and forwards a local port to the pod's API port (the service's
// 443 behind it). It returns the forward — close it when done — the
// https base URL to talk to, and the context every authenticated call
// must use: one context is the forward's whole lifetime. ForwardPorts
// stops when it is cancelled, every API call runs under it, and the
// goroutine below cancels it the moment ForwardPorts returns — so the
// tunnel's death, its close, and the parent's cancellation all fail the
// calls instead of letting them dial a port the forward no longer owns.
//
// The listener is bound by client-go itself, on 127.0.0.1 only, with
// local port 0: the kernel picks the port and the forwarder reports the
// one it bound after readiness. There is no listen-close-use window (the
// example never picks a port on the forwarder's behalf), and a ready
// forward always means an IPv4 loopback listener — client-go's
// "localhost" default also binds ::1 and reports ready when only that
// side came up, while this client dials 127.0.0.1 and would reach
// whatever holds the IPv4 port instead.
//
// The context being the lifetime closes the intent, not the wire: inside
// client-go, ForwardPorts closes its listeners in a defer before it
// returns, so a few instructions can pass between the port's release and
// this side's cancellation. What actually keeps the admin password off
// whatever rebinds the port in that window is the client's own
// authentication of Central (see centralAPI): a request is only sent to
// a peer that presented the certificate the chart's CA issued.
func startCentralPortForward(ctx context.Context, kubeconfigPath string) (*centralPortForward, string, context.Context, error) {
	cs, restConfig, err := clientsetFor(kubeconfigPath)
	if err != nil {
		return nil, "", nil, err
	}
	podName, err := centralPodName(ctx, cs)
	if err != nil {
		return nil, "", nil, err
	}

	serverURL, err := apiServerURL(restConfig)
	if err != nil {
		return nil, "", nil, err
	}
	forwardURL, err := url.Parse(fmt.Sprintf("%s/api/v1/namespaces/%s/pods/%s/portforward",
		serverURL.String(), stackroxNamespace, podName))
	if err != nil {
		return nil, "", nil, err
	}

	roundTripper, upgrader, err := spdy.RoundTripperFor(restConfig)
	if err != nil {
		return nil, "", nil, fmt.Errorf("building the port-forward transport: %w", err)
	}
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: roundTripper}, http.MethodPost, forwardURL)

	ready := make(chan struct{})
	// The forwarder's own progress messages go to the run's stderr: they
	// are part of the evidence, and a tunnel that never comes up should
	// say so where the run can see it. Ports are "local:remote" pairs;
	// local 0 makes the forwarder bind and report the port itself.
	//
	// The lifetime is a cause-carrying context: the tunnel's own
	// failure is published as the context's cause in the same
	// synchronized step that closes it (context.WithCancelCause), so a
	// parent cancellation racing the startup reads either the fully
	// published cause or none — never a half-written shared variable.
	pfCtx, cancel := context.WithCancelCause(ctx)
	fw, err := portforward.NewOnAddressesWithContext(pfCtx, dialer, []string{"127.0.0.1"},
		[]string{fmt.Sprintf("0:%d", centralPodPort)}, ready, os.Stderr, os.Stderr)
	if err != nil {
		cancel(nil)
		return nil, "", nil, fmt.Errorf("building the port-forward: %w", err)
	}

	go func() {
		// The deferred cancel keeps the lifetime bounded even if
		// ForwardPorts panics; the first cause wins, so a failure
		// published in the body survives the deferred nil.
		defer cancel(nil)
		if err := fw.ForwardPorts(); err != nil {
			cancel(err)
		}
	}()

	select {
	case <-ready:
		ports, err := fw.GetPorts()
		if err != nil {
			cancel(nil)
			return nil, "", nil, fmt.Errorf("reading the forwarded local port: %w", err)
		}
		if len(ports) != 1 {
			cancel(nil)
			return nil, "", nil, fmt.Errorf("the port-forward reported %d local ports, want 1", len(ports))
		}
		return &centralPortForward{cancel: cancel},
			fmt.Sprintf("https://127.0.0.1:%d", ports[0].Local), pfCtx, nil
	case <-pfCtx.Done():
		// The cause says which side ended the lifetime: the tunnel's
		// goroutine published its failure as the cause, while a plain
		// Canceled means no failure was published — the parent
		// cancelled, or the forward stopped without reporting why.
		if cause := context.Cause(pfCtx); !errors.Is(cause, context.Canceled) {
			return nil, "", nil, fmt.Errorf("the Central port-forward exited before becoming ready: %w", cause)
		}
		if err := ctx.Err(); err != nil {
			return nil, "", nil, err
		}
		return nil, "", nil, errors.New("the Central port-forward stopped before becoming ready")
	case <-time.After(30 * time.Second):
		cancel(nil)
		return nil, "", nil, errors.New("the Central port-forward did not become ready within 30s")
	}
}

// centralAPI is a minimal Central REST client: basic auth with the admin
// password, over a TLS connection verified against the CA the chart's
// install hook wrote into the central-tls secret — read through the
// authenticated Kubernetes API, not through the tunnel. The client dials
// the forwarded loopback address but verifies the certificate as
// central.stackrox.svc, the DNS name the chart's generated certificate
// carries, so the admin password only ever travels to a peer that
// presented a certificate the chart's CA issued.
//
// That verification is what makes the tunnel's small teardown window
// harmless: inside client-go, ForwardPorts releases its listener before
// the lifetime context is cancelled, and a local process that rebinds
// the port in that window cannot present Central's certificate, so the
// handshake fails before any credential is sent. Redirects are refused
// for the same reason — Go's default client follows them and preserves
// the Authorization header across port changes on the same hostname, so
// a redirect answer would be a second, unverified destination for the
// password. The Sensors' own connection is different in both respects —
// mTLS with their registration identity over the shared network — and
// is verified by the readiness wait, not by this client.
type centralAPI struct {
	base     string
	password string
	client   *http.Client
}

// newCentralAPI builds the client for one Central base URL. caPEM is
// the CA that roots Central's serving certificate (see centralCAPEM); a
// base that is not https is refused outright — credentials do not
// travel in the clear.
func newCentralAPI(base, password string, caPEM []byte) (*centralAPI, error) {
	if u, err := url.Parse(base); err != nil || u.Scheme != "https" {
		return nil, fmt.Errorf("the Central API base %q is not an https URL", base)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("the Central CA from the central-tls secret is not a parseable certificate")
	}
	return &centralAPI{
		base:     base,
		password: password,
		client: &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					RootCAs:    roots,
					ServerName: centralTLSServerName,
					MinVersion: tls.VersionTLS12,
				},
			},
			// Credentials reach the peer that was verified, never a
			// location it named: a redirect answer fails the call
			// instead of being followed.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return fmt.Errorf("central answered with a redirect to %s; refusing to follow it", req.URL)
			},
			Timeout: 30 * time.Second,
		},
	}, nil
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
	if err := a.revokeCRSNamed(ctx, name); err != nil {
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

// revokeCRSNamed revokes the one cluster registration secret Central holds
// under the name, so the name is free for a fresh one (Central refuses a
// name held by a non-revoked init bundle or CRS). Central reports a
// failing revocation inside a 200 answer, not through the status code, so
// the reported errors and the coverage of the revoked id are both
// checked.
//
// The listing must hold exactly one entry under the name: Central's
// cluster-init store (central/clusterinit/store) rejects adding a second
// non-revoked init bundle or CRS under a name it already holds, and the
// listing omits revoked entries — so a consistent Central lists at most
// one active CRS per name. An ambiguous listing means a state this recipe
// does not understand; the ambiguity is refused instead of revoking on a
// guess.
func (a *centralAPI) revokeCRSNamed(ctx context.Context, name string) error {
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
	if len(ids) > 1 {
		return fmt.Errorf("central lists %d cluster registration secrets named %q (%s), so the recipe cannot pick which to revoke — remove the extras in Central or use a different prefix", len(ids), name, strings.Join(ids, ", "))
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
	if !slices.Contains(resp.RevokedIDs, ids[0]) {
		return fmt.Errorf("central's answer does not report %q as revoked", ids[0])
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

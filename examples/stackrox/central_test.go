package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// discardLogger keeps the wait functions' progress logging out of test
// output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// testCRSManifest is the secret-manifest shape Central's cluster-init API
// returns: a Kubernetes Secret whose data.crs carries the opaque
// registration secret.
const testCRSManifest = `# This is a StackRox Cluster Registration Secret (CRS).
apiVersion: v1
kind: Secret
metadata:
  name: cluster-registration-secret
data:
  crs: RklYTUU=
`

// mintCentralCA mints the CA half of the chart-shaped pair: the CA
// PEM is what a centralAPI trusts.
func mintCentralCA(t *testing.T) (caPEM []byte, ca *x509.Certificate, caKey *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), parsed, key
}

// mintCentralLeafUnder mints a serving certificate carrying the chart's
// SAN (central.stackrox.svc) under the given CA — the shape the chart's
// install hook writes into the central-tls secret. The certificate is
// what a test Central serves.
func mintCentralLeafUnder(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey) tls.Certificate {
	t.Helper()
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "CENTRAL_SERVICE: Central"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     []string{centralTLSServerName},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
}

// mintCentralCert mints a CA and a serving certificate under it. Minting
// a second, independent pair is how a test builds a peer the client
// must not trust: same name, different issuer.
func mintCentralCert(t *testing.T) (caPEM []byte, cert tls.Certificate) {
	t.Helper()
	caPEM, ca, caKey := mintCentralCA(t)
	return caPEM, mintCentralLeafUnder(t, ca, caKey)
}

// startCentralTLSServer serves h over TLS with a freshly minted
// chart-shaped certificate. It returns the https base URL and the CA a
// client must trust to verify the server — the pair the production flow
// gets from the central-tls secret and the forwarded pod.
func startCentralTLSServer(t *testing.T, h http.Handler) (baseURL string, caPEM []byte) {
	t.Helper()
	caPEM, ca, caKey := mintCentralCA(t)
	return startCentralTLSServerUnderCA(t, ca, caKey, h), caPEM
}

// startCentralTLSServerUnderCA serves h over TLS with a certificate the
// given CA issued — a peer a client trusting that CA fully verifies, so
// a test can prove the client never dialed it (rather than dialed it
// and failed the handshake).
func startCentralTLSServerUnderCA(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, h http.Handler) string {
	t.Helper()
	server := httptest.NewUnstartedServer(h)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{mintCentralLeafUnder(t, ca, caKey)}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server.URL
}

// newTestCentralAPI builds a centralAPI a test can drive: verified TLS
// against the CA the test server was minted with, exactly the production
// configuration.
func newTestCentralAPI(t *testing.T, baseURL, password string, caPEM []byte) *centralAPI {
	t.Helper()
	api, err := newCentralAPI(baseURL, password, caPEM)
	if err != nil {
		t.Fatalf("newCentralAPI = %v", err)
	}
	return api
}

// newCentralTestServer serves the Central REST surface the example uses,
// with the authentication it expects.
func newCentralTestServer(t *testing.T, clustersJSON string) (*centralAPI, *authRecorder) {
	t.Helper()
	rec := &authRecorder{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /v1/cluster-init/crs", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		resp := map[string]any{
			"meta": map[string]any{"name": req.Name},
			"crs":  base64.StdEncoding.EncodeToString([]byte(testCRSManifest)),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("GET /v1/clusters", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(clustersJSON))
	})
	base, ca := startCentralTLSServer(t, rec.wrap(mux))
	return newTestCentralAPI(t, base, "test-password", ca), rec
}

// authRecorder asserts the admin basic auth reached the server.
type authRecorder struct {
	user, password string
}

func (a *authRecorder) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.user, a.password, _ = r.BasicAuth()
		next.ServeHTTP(w, r)
	})
}

func TestGenerateCRSDecodesAndValidatesTheManifest(t *testing.T) {
	api, rec := newCentralTestServer(t, `{"clusters":[]}`)
	manifest, err := api.generateCRS(context.Background(), "example-crs", discardLogger())
	if err != nil {
		t.Fatalf("generateCRS = %v", err)
	}
	if manifest != testCRSManifest {
		t.Errorf("generateCRS returned %q, want the decoded manifest verbatim", manifest)
	}
	if rec.user != "admin" || rec.password != "test-password" {
		t.Errorf("the request authenticated as %q:%q, want admin with the configured password", rec.user, rec.password)
	}
}

func TestGenerateCRSRefusesAManifestThatIsNotASecret(t *testing.T) {
	manifest := base64.StdEncoding.EncodeToString([]byte("apiVersion: v1\nkind: ConfigMap\ndata: {}\n"))
	base, ca := startCentralTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"crs": "` + manifest + `"}`))
	}))
	api := newTestCentralAPI(t, base, "pw", ca)
	if _, err := api.generateCRS(context.Background(), "x", discardLogger()); err == nil || !strings.Contains(err.Error(), "not a Secret") {
		t.Fatalf("generateCRS on a ConfigMap = %v, want a shape refusal", err)
	}
}

func TestGenerateCRSRefusesAnEmptyOrUndecodableSecret(t *testing.T) {
	cases := map[string]string{
		"empty":            `{"crs": ""}`,
		"not base64":       `{"crs": "%%% not base64 %%%"}`,
		"trailing garbage": `{"crs": "YWJj"} Traditional`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			base, ca := startCentralTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			api := newTestCentralAPI(t, base, "pw", ca)
			if _, err := api.generateCRS(context.Background(), "x", discardLogger()); err == nil {
				t.Fatalf("generateCRS accepted %q", body)
			}
		})
	}
}

func TestClustersParsesHealthStatuses(t *testing.T) {
	api, _ := newCentralTestServer(t, `{"clusters":[
		{"id":"a","name":"kind-ocp-2","healthStatus":{"overallHealthStatus":"HEALTHY"}},
		{"id":"b","name":"kind-ocp-3","healthStatus":{"overallHealthStatus":"UNINITIALIZED"}}
	]}`)
	clusters, err := api.clusters(context.Background())
	if err != nil {
		t.Fatalf("clusters = %v", err)
	}
	want := map[string]string{"kind-ocp-2": "HEALTHY", "kind-ocp-3": "UNINITIALIZED"}
	for _, c := range clusters {
		if want[c.Name] != c.Status {
			t.Errorf("cluster %s reported %q, want %q", c.Name, c.Status, want[c.Name])
		}
	}
}

func TestClassifySplitsMissingFromUnhealthy(t *testing.T) {
	clusters := []clusterHealth{
		{Name: "c2", Status: "HEALTHY"},
		{Name: "c3", Status: "DEGRADED"},
	}
	missing, unhealthy := classify(clusters, []string{"c2", "c3", "c4"})
	if len(missing) != 1 || missing[0] != "c4" {
		t.Errorf("missing = %v, want [c4]", missing)
	}
	if len(unhealthy) != 1 || !strings.Contains(unhealthy[0], "c3 (DEGRADED)") {
		t.Errorf("unhealthy = %v, want [c3 (DEGRADED)]", unhealthy)
	}
}

func TestWaitForHealthyClustersReturnsWhenEveryClusterIsHealthy(t *testing.T) {
	api, _ := newCentralTestServer(t, `{"clusters":[
		{"name":"kind-ocp-2","healthStatus":{"overallHealthStatus":"HEALTHY"}}
	]}`)
	if err := waitForHealthyClusters(context.Background(), api, []string{"kind-ocp-2"}, 2*time.Second, discardLogger()); err != nil {
		t.Fatalf("waitForHealthyClusters = %v, want success", err)
	}
}

func TestWaitForHealthyClustersTimesOutNamingTheStragglers(t *testing.T) {
	api, _ := newCentralTestServer(t, `{"clusters":[
		{"name":"kind-ocp-2","healthStatus":{"overallHealthStatus":"UNHEALTHY"}}
	]}`)
	err := waitForHealthyClusters(context.Background(), api, []string{"kind-ocp-2", "kind-ocp-3"}, 150*time.Millisecond, discardLogger())
	if err == nil {
		t.Fatal("waitForHealthyClusters succeeded with a missing and an unhealthy cluster")
	}
	for _, want := range []string{"kind-ocp-3", "kind-ocp-2 (UNHEALTHY)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the timeout error %q does not name %q", err.Error(), want)
		}
	}
}

func TestWaitForCentralTimesOut(t *testing.T) {
	base, ca := startCentralTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	api := newTestCentralAPI(t, base, "pw", ca)
	err := waitForCentral(context.Background(), api, 150*time.Millisecond, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "/v1/ping") {
		t.Fatalf("waitForCentral on a failing Central = %v, want a timeout naming the endpoint", err)
	}
}

func TestGenerateCRSRetriesThroughTheCredentialReloadWindow(t *testing.T) {
	olds, oldPoll := credentialApplyTimeout, pollInterval
	credentialApplyTimeout, pollInterval = 2*time.Second, 100*time.Millisecond
	defer func() { credentialApplyTimeout, pollInterval = olds, oldPoll }()

	calls := 0
	base, ca := startCentralTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":16,"message":"credentials not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"crs": "` + base64.StdEncoding.EncodeToString([]byte(testCRSManifest)) + `"}`))
	}))

	api := newTestCentralAPI(t, base, "pw", ca)
	manifest, err := api.generateCRS(context.Background(), "retry-crs", discardLogger())
	if err != nil {
		t.Fatalf("generateCRS through a 401 window = %v, want success after retries", err)
	}
	if manifest != testCRSManifest {
		t.Errorf("generateCRS returned %q, want the manifest after the window cleared", manifest)
	}
	if calls < 3 {
		t.Errorf("the call count was %d, want at least three (two refused, one served)", calls)
	}
}

func TestGenerateCRSFailsAfterTheCredentialWindowWithTheLastError(t *testing.T) {
	olds, oldPoll := credentialApplyTimeout, pollInterval
	credentialApplyTimeout, pollInterval = 300*time.Millisecond, 100*time.Millisecond
	defer func() { credentialApplyTimeout, pollInterval = olds, oldPoll }()

	base, ca := startCentralTLSServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":16,"message":"credentials not found"}`))
	}))

	api := newTestCentralAPI(t, base, "wrong-password", ca)
	_, err := api.generateCRS(context.Background(), "x", discardLogger())
	if err == nil || !strings.Contains(err.Error(), "Unauthorized") {
		t.Fatalf("generateCRS with a persistently wrong password = %v, want the 401 surfaced", err)
	}
}

// crsCalls records what a re-run test drove through Central's CRS
// endpoints: the POST count and the bodies sent to the revoke endpoint.
type crsCalls struct {
	posts        int
	revokeBodies []string
}

// newCRSConflictServer serves Central's CRS surface as a re-run of the
// recipe meets it: a CRS with the requested name already exists, so
// POST /v1/cluster-init/crs answers 409 until that CRS is revoked through
// PATCH /v1/cluster-init/crs/revoke. The default listing also carries an
// unrelated CRS, so the revoke selection is pinned to the name, not to
// being the only listed entry. listBody and revokeBody override the
// default listing and revoke answers, so tests can break the flow.
func newCRSConflictServer(t *testing.T, listBody, revokeBody string) (*centralAPI, *crsCalls) {
	t.Helper()
	if listBody == "" {
		listBody = `{"items":[
			{"id":"crs-9","name":"someone-elses"},
			{"id":"crs-1","name":"kind-ocp-example-test"}
		]}`
	}
	if revokeBody == "" {
		revokeBody = `{"revokedIds":["crs-1"]}`
	}
	calls := &crsCalls{}
	revoked := false
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/cluster-init/crs", func(w http.ResponseWriter, r *http.Request) {
		calls.posts++
		if !revoked {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":6,"message":"init bundle or CRS already exists"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"crs": base64.StdEncoding.EncodeToString([]byte(testCRSManifest)),
		})
	})
	mux.HandleFunc("GET /v1/cluster-init/crs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(listBody))
	})
	mux.HandleFunc("PATCH /v1/cluster-init/crs/revoke", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		calls.revokeBodies = append(calls.revokeBodies, string(b))
		revoked = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(revokeBody))
	})
	base, ca := startCentralTLSServer(t, mux)
	return newTestCentralAPI(t, base, "pw", ca), calls
}

func TestGenerateCRSRevokesAndRegeneratesWhenTheNameIsTaken(t *testing.T) {
	api, calls := newCRSConflictServer(t, "", "")
	manifest, err := api.generateCRS(context.Background(), "kind-ocp-example-test", discardLogger())
	if err != nil {
		t.Fatalf("generateCRS on a re-run = %v, want convergence through revocation", err)
	}
	if manifest != testCRSManifest {
		t.Errorf("generateCRS returned %q, want the fresh manifest", manifest)
	}
	if calls.posts != 2 {
		t.Errorf("POST count = %d, want 2 (refused, then served after the revocation)", calls.posts)
	}
	if len(calls.revokeBodies) != 1 {
		t.Fatalf("revoke count = %d, want 1", len(calls.revokeBodies))
	}
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal([]byte(calls.revokeBodies[0]), &req); err != nil {
		t.Fatalf("the revoke request was not JSON: %v", err)
	}
	if len(req.IDs) != 1 || req.IDs[0] != "crs-1" {
		t.Errorf("the revoke request carried ids %v, want [crs-1] — the id the listing reported", req.IDs)
	}
}

func TestGenerateCRSFailsWhenTheConflictingNameIsNotARevocableCRS(t *testing.T) {
	api, calls := newCRSConflictServer(t, `{"items":[{"id":"other","name":"someone-elses"}]}`, "")
	_, err := api.generateCRS(context.Background(), "kind-ocp-example-test", discardLogger())
	if err == nil {
		t.Fatal("generateCRS succeeded although the name holder cannot be revoked")
	}
	if !strings.Contains(err.Error(), "kind-ocp-example-test") {
		t.Errorf("the error %q does not name the conflicting name", err.Error())
	}
	if len(calls.revokeBodies) != 0 {
		t.Errorf("revoke was called %d times, want 0 — there is no matching CRS to revoke", len(calls.revokeBodies))
	}
}

func TestGenerateCRSFailsWhenTheNameIsAmbiguous(t *testing.T) {
	api, calls := newCRSConflictServer(t, `{"items":[
		{"id":"crs-1","name":"kind-ocp-example-test"},
		{"id":"crs-2","name":"kind-ocp-example-test"}
	]}`, "")
	_, err := api.generateCRS(context.Background(), "kind-ocp-example-test", discardLogger())
	if err == nil {
		t.Fatal("generateCRS succeeded although the name is held by two listed CRSs")
	}
	for _, want := range []string{"kind-ocp-example-test", "crs-1", "crs-2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error %q does not name %q", err.Error(), want)
		}
	}
	if len(calls.revokeBodies) != 0 {
		t.Errorf("revoke was called %d times, want 0 — an ambiguous listing must not be revoked", len(calls.revokeBodies))
	}
}

func TestRevokeSurfacesACentralRefusal(t *testing.T) {
	cases := map[string]string{
		"refused":   `{"crsRevocationErrors":[{"id":"crs-1","error":"impacted by policy"}]}`,
		"uncovered": `{"revokedIds":[]}`,
	}
	for name, revokeBody := range cases {
		t.Run(name, func(t *testing.T) {
			api, _ := newCRSConflictServer(t, "", revokeBody)
			_, err := api.generateCRS(context.Background(), "kind-ocp-example-test", discardLogger())
			if err == nil {
				t.Fatalf("generateCRS succeeded although Central answered %q", revokeBody)
			}
			if !strings.Contains(err.Error(), "crs-1") {
				t.Errorf("the error %q does not name the CRS Central failed to revoke", err.Error())
			}
			// The per-ID refusal must be pinned by its own words: the
			// ID alone would not tell it apart from the missing
			// revoked-id coverage, which names the ID too.
			if name == "refused" && !strings.Contains(err.Error(), "impacted by policy") {
				t.Errorf("the error %q does not carry Central's refusal text", err.Error())
			}
		})
	}
}

// The port-forward's teardown releases its listener a few instructions
// before the lifetime context is cancelled, so a local process can hold
// the port when a call dials it. That peer must never see the admin
// password: the client authenticates Central before any credential is
// sent, and a peer whose certificate chains to a different CA fails the
// handshake. The attacking server here serves a perfectly formed
// certificate for the right name — only the issuer differs, which is
// everything a local squatter can forge.
func TestCentralClientSendsNoAuthenticatedRequestToAnUnexpectedTLSPeer(t *testing.T) {
	var requests atomic.Int64
	rec := &authRecorder{}
	_, attackerCert := mintCentralCert(t)
	server := httptest.NewUnstartedServer(rec.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	})))
	server.TLS = &tls.Config{Certificates: []tls.Certificate{attackerCert}}
	server.StartTLS()
	t.Cleanup(server.Close)

	trustedCA, _ := mintCentralCert(t)
	api := newTestCentralAPI(t, server.URL, "test-password", trustedCA)
	_, err := api.generateCRS(context.Background(), "squatted-crs", discardLogger())
	if err == nil {
		t.Fatal("generateCRS succeeded against a TLS peer the client does not trust")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("the error %q is not a certificate verification failure — the call must fail in the TLS handshake, before any credential is sent", err.Error())
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("the untrusted peer served %d requests, want 0 — no authenticated request may reach it", got)
	}
	if rec.user != "" || rec.password != "" {
		t.Errorf("the untrusted peer received basic auth %q:%q, want none", rec.user, rec.password)
	}
}

// Go's default redirect handling preserves the Authorization header
// across port changes on the same hostname — a redirect answer would be
// a second, unverified destination for the admin password. The client
// refuses to follow one, so the redirect target never sees a request.
// The target here is signed by the same trusted CA as the redirecting
// server, so its TLS would verify if dialed: zero requests on it prove
// the client never dialed it (the redirect refusal), not that a dial
// happened to fail the handshake.
func TestCentralClientRefusesToFollowARedirect(t *testing.T) {
	var redirectedTo atomic.Int64
	rec := &authRecorder{}
	caPEM, ca, caKey := mintCentralCA(t)
	targetBase := startCentralTLSServerUnderCA(t, ca, caKey, rec.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirectedTo.Add(1)
		w.WriteHeader(http.StatusOK)
	})))

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/cluster-init/crs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", targetBase+"/v1/cluster-init/crs")
		w.WriteHeader(http.StatusFound)
	})
	base := startCentralTLSServerUnderCA(t, ca, caKey, mux)

	api := newTestCentralAPI(t, base, "test-password", caPEM)
	_, err := api.generateCRS(context.Background(), "redirected-crs", discardLogger())
	if err == nil {
		t.Fatal("generateCRS followed a redirect answer")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("the error %q does not name the refused redirect", err.Error())
	}
	if got := redirectedTo.Load(); got != 0 {
		t.Errorf("the redirect target served %d requests, want 0 — credentials never travel to a location the verified peer named", got)
	}
	if rec.user != "" || rec.password != "" {
		t.Errorf("the redirect target received basic auth %q:%q, want none", rec.user, rec.password)
	}
}

func TestNewCentralAPIRefusesABaseThatIsNotHTTPS(t *testing.T) {
	if _, err := newCentralAPI("http://127.0.0.1:8443", "pw", nil); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("newCentralAPI on a plain-http base = %v, want a refusal — credentials do not travel in the clear", err)
	}
}

// forwardResult is what startCentralPortForward returns, delivered
// through a channel so a test can act on the forward while it starts.
type forwardResult struct {
	pf   *centralPortForward
	base string
	ctx  context.Context
	err  error
}

// startForward runs startCentralPortForward under parent and delivers
// its result on the returned channel.
func startForward(parent context.Context, kubeconfig string) <-chan forwardResult {
	ch := make(chan forwardResult, 1)
	go func() {
		pf, base, ctx, err := startCentralPortForward(parent, kubeconfig)
		ch <- forwardResult{pf: pf, base: base, ctx: ctx, err: err}
	}()
	return ch
}

// portForwardAPIServer serves the two requests startCentralPortForward
// makes around the tunnel: the Central pod list that finds the pod, and
// the pod's portforward endpoint the SPDY dialer POSTs to. The
// portforward request is held from the moment it arrives until the
// returned release function runs, and is then answered with a plain 404
// — never an upgrade — so ForwardPorts fails while starting. Holding
// the request is what makes the forward observably "starting": the
// dial is in flight, ForwardPorts has not returned, and the test can
// act in that window. The returned kubeconfig points at the server the
// way a kind kubeconfig points at a cluster.
func portForwardAPIServer(t *testing.T) (kubeconfigPath string, arrived <-chan struct{}, release func()) {
	t.Helper()
	var arrivedOnce sync.Once
	arrivedCh := make(chan struct{})
	releaseCh := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/namespaces/stackrox/pods", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		list := corev1.PodList{
			TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"},
			Items:    []corev1.Pod{*centralPod("central-7f9c", corev1.PodRunning)},
		}
		_ = json.NewEncoder(w).Encode(&list)
	})
	mux.HandleFunc("POST /api/v1/namespaces/stackrox/pods/central-7f9c/portforward", func(w http.ResponseWriter, r *http.Request) {
		arrivedOnce.Do(func() { close(arrivedCh) })
		<-releaseCh
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewUnstartedServer(mux)
	server.StartTLS()
	t.Cleanup(server.Close)

	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: %s
    insecure-skip-tls-verify: true
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user: {}
`, server.URL)
	if err := os.WriteFile(kubeconfig, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return kubeconfig, arrivedCh, func() { close(releaseCh) }
}

// A tunnel that dies on its own while starting reports its failure as
// the startup result: the failure is published as the lifetime
// context's cause, and the selector reads it back from there. Receiving
// the result proves the publication completed — the answer is ordered
// after the cause by the context's own synchronization.
func TestStartCentralPortForwardReportsATunnelThatDiesWhileStarting(t *testing.T) {
	kubeconfig, arrived, release := portForwardAPIServer(t)
	done := startForward(context.Background(), kubeconfig)
	<-arrived
	release()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatal("startCentralPortForward succeeded although the API server refused the upgrade")
		}
		for _, want := range []string{"before becoming ready", "upgrading connection"} {
			if !strings.Contains(r.err.Error(), want) {
				t.Errorf("the error %q does not name %q — the tunnel's own failure must be the startup result", r.err.Error(), want)
			}
		}
	case <-time.After(10 * time.Second):
		t.Fatal("startCentralPortForward did not return after the dial failed")
	}
}

// The parent's cancellation while the forward is starting must surface
// as the startup result with no synchronization on the tunnel's own
// outcome: the tunnel's goroutine publishes its error through the
// context cause (WithCancelCause), never through shared state the
// selector could read while it is still being written.
//
// The result is received only after the held dial has failed on
// purpose. The selector's answer races the goroutine's publication, and
// receiving the buffered result first would order the two through the
// result channel; the interleaving this test exists for is the one
// where the parent's cancellation is answered while ForwardPorts is
// still returning, and the publication lands afterwards. Under -race
// that pair is exactly the unsynchronized read-and-write the cause
// mechanism replaced.
func TestStartCentralPortForwardReportsParentCancellationWhileStarting(t *testing.T) {
	kubeconfig, arrived, release := portForwardAPIServer(t)
	parent, cancelParent := context.WithCancel(context.Background())
	done := startForward(parent, kubeconfig)
	<-arrived

	// The selector answers here: the parent's cancellation closes the
	// lifetime while the dial is still parked in the server.
	cancelParent()

	// Let the held dial fail and the forward's goroutine publish its
	// outcome while the selector's answer is already decided: release
	// the server's refusal and give the client the moment loopback
	// needs to finish the request. The result stays buffered in the
	// channel meanwhile.
	release()
	time.Sleep(150 * time.Millisecond)

	select {
	case r := <-done:
		if r.err == nil || !errors.Is(r.err, context.Canceled) {
			t.Fatalf("startCentralPortForward under parent cancellation = %v, want context.Canceled", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("startCentralPortForward did not return after the parent was cancelled")
	}
}

func TestNewCentralAPIRefusesUnusableTrust(t *testing.T) {
	cases := map[string][]byte{
		"no CA":       nil,
		"garbage":     []byte("not a certificate"),
		"empty block": []byte("-----BEGIN CERTIFICATE-----\n-----END CERTIFICATE-----\n"),
	}
	for name, caPEM := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := newCentralAPI("https://127.0.0.1:8443", "pw", caPEM); err == nil {
				t.Fatalf("newCentralAPI accepted trust material %q", name)
			}
		})
	}
}

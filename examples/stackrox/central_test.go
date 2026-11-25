package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	server := httptest.NewServer(rec.wrap(mux))
	t.Cleanup(server.Close)
	return newCentralAPI(server.URL, "test-password"), rec
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"crs": "` + manifest + `"}`))
	}))
	defer server.Close()
	api := newCentralAPI(server.URL, "pw")
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
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			api := newCentralAPI(server.URL, "pw")
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	api := newCentralAPI(server.URL, "pw")
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls <= 2 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":16,"message":"credentials not found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"crs": "` + base64.StdEncoding.EncodeToString([]byte(testCRSManifest)) + `"}`))
	}))
	defer server.Close()

	api := newCentralAPI(server.URL, "pw")
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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":16,"message":"credentials not found"}`))
	}))
	defer server.Close()

	api := newCentralAPI(server.URL, "wrong-password")
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
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return newCentralAPI(server.URL, "pw"), calls
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

//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/guzalv/kind-ocp/config"
	"github.com/guzalv/kind-ocp/internal/kube"
	"github.com/guzalv/kind-ocp/internal/layers/openshiftapicrds"
	"github.com/guzalv/kind-ocp/kindocp"
)

// fetchRegressionLayers is the two layers whose interaction the F-1
// regression covers: config-openshift's placeholder CRDs, and the
// openshift-api-crds layer that supersedes them. The regression needs
// neither OLM nor node-labels, so it runs without them — which makes the
// CRD fetch the only outbound HTTPS request the failing create can make.
// The four-layer happy path lives in openshift_api_crds_test.go.
var fetchRegressionLayers = []string{"config-openshift", "openshift-api-crds"}

// TestSecondCreateWithoutCacheAndNetwork is the F-1 regression: the
// increment-4 review's exact scenario. After a successful create, a
// second create runs with the CRD cache removed and the pinned fetch
// made unreachable. It must fail in openshift-api-crds with the fetch
// error — and the cluster must keep the real CRDs: real schema,
// api-approved.openshift.io present, no placeholder root schema. Before
// the fix, config-openshift's server-side apply overwrote the real CRDs
// with the placeholders before the fetch failed, downgrading a working
// cluster on a failed re-create.
//
// The second create runs as the kind-ocp CLI in a subprocess, because
// the unreachability is scoped with proxy environment variables: Go's
// http.DefaultTransport reads them once per process, and the test
// binary's transport has long since read (and ignored) them. A fresh
// process reads them fresh — the same way a user's shell would.
func TestSecondCreateWithoutCacheAndNetwork(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	prefix := randomPrefix(t)
	cluster := prefix + "-1"
	stateRoot := filepath.Join(t.TempDir(), "state")

	crdFetch := func(cfg *config.Config) {
		cfg.Profiles = map[string]config.Profile{"crd-fetch": {Layers: fetchRegressionLayers}}
		cfg.Profile = "crd-fetch"
	}
	client, _ := testClientAt(t, prefix, stateRoot, crdFetch, nil)
	if _, err := client.Delete(ctx); err != nil {
		t.Fatalf("pre-clean delete: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Delete(ctx) })

	// 1. A successful create: the cluster converges with the real CRDs
	// (fetched into a cold cache) and completes.
	records, err := client.Create(ctx)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(records) != 1 || records[0].Status != kindocp.RecordStatusComplete {
		t.Fatalf("records = %+v, want one complete record", records)
	}
	kcPath, err := client.Kubeconfig(ctx, cluster)
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	kubeClient, err := kube.NewClient(kcPath)
	if err != nil {
		t.Fatalf("building kube client: %v", err)
	}
	realCRD, err := kubeClient.Dynamic.Resource(crdGVR).Get(ctx, "infrastructures.config.openshift.io", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading the infrastructures CRD: %v", err)
	}
	requireRealCRD(t, realCRD, "infrastructures.config.openshift.io", "after the first create")

	// 2. Remove the layer's cache: the next create must fetch again.
	cacheDir := filepath.Join(client.StateRoot(), "cache", openshiftapicrds.Name)
	if err := os.RemoveAll(cacheDir); err != nil {
		t.Fatalf("removing the CRD cache: %v", err)
	}

	// 3. The second create, as the CLI in a subprocess with the pinned
	// fetch unreachable: a local proxy refuses every request — no
	// tunneling, no outbound dialing. The two-layer profile means the
	// CRD fetch is the run's only outbound HTTPS request (the kind
	// reuse and the apiserver on 127.0.0.1 are local, exempt through
	// NO_PROXY), so the failure lands where the review found it and
	// nowhere else — and the test depends on no upstream network
	// policy, because nothing is tunneled and nothing is dialed.
	bin := buildKindOcp(t)
	cfgFile := writeCreateConfig(t, prefix, stateRoot)
	proxy := newRefusingProxy(t)

	cmd := exec.Command(bin, "create", "--config", cfgFile)
	cmd.Env = append(os.Environ(),
		"HTTPS_PROXY="+proxy.URL,
		"NO_PROXY=localhost,127.0.0.1",
	)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the second create succeeded with the CRD cache removed and the fetch unreachable;\noutput:\n%s", out)
	}

	// 4. It failed in openshift-api-crds, with the fetch error.
	output := string(out)
	for _, want := range []string{
		"openshift-api-crds",
		"fetching manifest",
		"https://raw.githubusercontent.com/openshift/api/" + openshiftapicrds.ModuleCommit,
	} {
		if !strings.Contains(output, want) {
			t.Errorf("the second create's output does not mention %q:\n%s", want, output)
		}
	}

	// 5. The cluster kept the real CRDs — the whole point of the fix.
	// The real schema survived: the annotation, the root schema's
	// properties, no placeholder root marker.
	surviving, err := kubeClient.Dynamic.Resource(crdGVR).Get(ctx, "infrastructures.config.openshift.io", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-reading the infrastructures CRD after the failed create: %v", err)
	}
	requireRealCRD(t, surviving, "infrastructures.config.openshift.io", "after the failed second create")

	// The singletons and their status survived too: config-openshift's
	// second pass applied them through the real CRDs without touching
	// the CRDs themselves.
	infra, err := kubeClient.Dynamic.Resource(configGVR).Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading Infrastructure/cluster after the failed create: %v", err)
	}
	if platformStatus, found, _ := unstructured.NestedMap(infra.Object, "status", "platformStatus"); !found || len(platformStatus) == 0 {
		t.Errorf("Infrastructure/cluster status.platformStatus lost across the failed second create")
	}

	// 6. The record is honestly pending (the run died mid-layer), and
	// the layer's own check still passes against the surviving real
	// CRDs — the cluster is degraded only in that the record did not
	// complete, which a later successful create converges.
	list, err := client.List(ctx)
	if err != nil {
		t.Fatalf("list after the failed create: %v", err)
	}
	if len(list) != 1 || list[0].Status != kindocp.RecordStatusPending {
		t.Errorf("record after the failed create = %+v, want one pending record", list)
	}
	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify after the failed create: %v", err)
	}
	var layerCheck *kindocp.CheckResult
	for i := range results {
		if results[i].Check == "layer:openshift-api-crds" {
			layerCheck = &results[i]
		}
	}
	if layerCheck == nil {
		t.Fatalf("verify results carry no layer:openshift-api-crds check: %+v", results)
	}
	if layerCheck.Status != kindocp.StatusPass {
		t.Errorf("layer:openshift-api-crds = %s (%s); the real CRDs survived, so the layer's own check must pass",
			layerCheck.Status, layerCheck.Detail)
	}
}

// buildKindOcp builds the kind-ocp CLI into a scratch directory and
// returns its path. The F-1 regression runs the second create as the
// real CLI in a subprocess (see the test's comment for why), and the
// test binary already runs inside the module, so the build needs
// nothing but the Go toolchain the test itself needed.
func buildKindOcp(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "kind-ocp")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/guzalv/kind-ocp/cmd/kind-ocp")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building the kind-ocp CLI: %v:\n%s", err, out)
	}
	return bin
}

// writeCreateConfig writes the config file the F-1 regression's
// subprocess resolves: the same prefix, the same two-layer profile, and
// the same state root as the in-process first create, so the subprocess
// reuses (not replaces) the cluster.
func writeCreateConfig(t *testing.T, prefix, stateRoot string) string {
	t.Helper()
	content := fmt.Sprintf(`count: 1
prefix: %s
stateRoot: %s
profile: crd-fetch
profiles:
  crd-fetch:
    layers: [%s]
`, prefix, stateRoot, strings.Join(fetchRegressionLayers, ", "))
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// newRefusingProxy starts a local HTTP proxy that refuses every request
// it receives — CONNECT tunnels included — with an error, without
// dialing anywhere. The F-1 regression's failing create must fail its
// one outbound HTTPS request (the CRD fetch); refusing everything
// guarantees that failure regardless of the host's own upstream network
// policy, because nothing is tunneled and nothing is dialed. The run's
// local traffic (the kind reuse, the apiserver on 127.0.0.1) never
// reaches the proxy: NO_PROXY exempts it.
func newRefusingProxy(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, fmt.Sprintf("%s %s: no upstream for this test", r.Method, r.Host), http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	return srv
}

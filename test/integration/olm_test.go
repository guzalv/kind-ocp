//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/stackrox/kind-ocp/config"
	"github.com/stackrox/kind-ocp/internal/kube"
	"github.com/stackrox/kind-ocp/kindocp"
)

// TestCreateWithOLMLayer exercises increment 2's layer end to end on a
// real Docker daemon: create with an olm-only profile (not full — full
// grows as later increments register layers, and this test is only
// about olm), verify passes the layer check, and a re-run of create
// reconciles — server-side apply re-applying the same release under
// the same field manager must be a no-op against a live OLM, not a
// conflict.
func TestCreateWithOLMLayer(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	prefix := randomPrefix(t)
	cluster := prefix + "-1"
	stateRoot := filepath.Join(t.TempDir(), "state")

	// olmOnly selects exactly the olm layer, keeping the test stable
	// when the next increments register theirs.
	olmOnly := func(cfg *config.Config) {
		cfg.Profiles = map[string]config.Profile{"olm-only": {Layers: []string{"olm"}}}
		cfg.Profile = "olm-only"
	}
	client, _ := testClientAt(t, prefix, stateRoot, olmOnly, nil)
	if _, err := client.Delete(ctx); err != nil {
		t.Fatalf("pre-clean delete: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Delete(ctx) })

	// 1. Create with the olm layer applied.
	records, err := client.Create(ctx)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(records) != 1 || records[0].Name != cluster {
		t.Fatalf("records = %+v, want one record for %s", records, cluster)
	}
	if records[0].Status != kindocp.RecordStatusComplete {
		t.Fatalf("record status = %s, want complete", records[0].Status)
	}
	if !slices.Equal(records[0].Layers, []string{"olm"}) {
		t.Fatalf("record layers = %v, want [olm]", records[0].Layers)
	}

	// 2. Verify passes, including the layer check.
	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	requireAllPassAndOLMLayerCheck(t, results, "after create")

	// 3. The layer's postconditions hold independently of its own Verify:
	// the CRDs are established and the API is served, read through the
	// exported kubeconfig.
	kcPath, err := client.Kubeconfig(ctx, cluster)
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	kubeClient, err := kube.NewClient(kcPath)
	if err != nil {
		t.Fatalf("building kube client: %v", err)
	}
	crdNames := []string{
		"clusterserviceversions.operators.coreos.com",
		"catalogsources.operators.coreos.com",
		"operatorgroups.operators.coreos.com",
		"subscriptions.operators.coreos.com",
	}
	// A short bound: the CRDs are already established (Apply waited for
	// them), so this is an immediate read through the public helper.
	if err := kube.WaitForCRDsEstablished(ctx, kubeClient.Dynamic, 30*time.Second, crdNames); err != nil {
		t.Errorf("OLM CRDs not established: %v", err)
	}

	// 4. Re-running create reconciles: same cluster instance, layer
	// re-applied (server-side apply no-op), record complete.
	retryRecords, err := client.Create(ctx)
	if err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	if len(retryRecords) != 1 || retryRecords[0].Status != kindocp.RecordStatusComplete {
		t.Fatalf("reconcile records = %+v, want one complete record", retryRecords)
	}
	if retryRecords[0].InstanceID != records[0].InstanceID {
		t.Fatalf("reconcile changed the instance: %q -> %q",
			records[0].InstanceID, retryRecords[0].InstanceID)
	}

	// 5. Verify still passes after the reconcile, layer check included.
	results, err = client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify after reconcile: %v", err)
	}
	requireAllPassAndOLMLayerCheck(t, results, "after reconcile")

	// 6. A layer settings section flows through the real create path: a
	// floating version is rejected when the layer decodes its settings,
	// after the reuse checks pass (same state root, matching record) and
	// before any cluster work happens. The failed apply leaves the
	// record pending; delete recovers regardless of status.
	var layersCfg map[string]yaml.Node
	if err := yaml.Unmarshal([]byte("olm:\n  version: latest\n"), &layersCfg); err != nil {
		t.Fatal(err)
	}
	badClient, _ := testClientAt(t, prefix, stateRoot,
		func(cfg *config.Config) {
			olmOnly(cfg)
			cfg.Layers = layersCfg
		}, nil)
	_, err = badClient.Create(ctx)
	if err == nil || !strings.Contains(err.Error(), "invalid OLM version") {
		t.Fatalf("create with a floating version = %v, want a version rejection", err)
	}
}

// requireAllPassAndOLMLayerCheck asserts every check passed and the
// layer:olm check ran — presence, not just the absence of failures, so
// a verify that silently skipped the layer check fails the test.
func requireAllPassAndOLMLayerCheck(t *testing.T, results []kindocp.CheckResult, when string) {
	t.Helper()
	var layerCheck bool
	for _, r := range results {
		if r.Status != kindocp.StatusPass && !singleClusterSkipOK(r) {
			t.Errorf("check %s/%s = %s (%s), want PASS %s", r.Cluster, r.Check, r.Status, r.Detail, when)
		}
		if r.Check == "layer:olm" {
			layerCheck = true
		}
	}
	if !layerCheck {
		t.Fatalf("verify %s ran no layer:olm check (results: %v)", when, results)
	}
}

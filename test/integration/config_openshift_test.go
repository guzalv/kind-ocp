//go:build integration

package integration

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/guzalv/kind-ocp/config"
	"github.com/guzalv/kind-ocp/internal/kube"
	"github.com/guzalv/kind-ocp/kindocp"
)

// The layer names and API shapes this test asserts against; they mirror
// the config-openshift and node-labels layers' contracts.
var (
	increment3Layers = []string{"config-openshift", "node-labels"}

	// The 13 placeholder CRDs, in asset document order.
	configOpenshiftCRDs = []string{
		"apiservers.config.openshift.io",
		"infrastructures.config.openshift.io",
		"clusterversions.config.openshift.io",
		"clusteroperators.config.openshift.io",
		"networks.config.openshift.io",
		"oauths.config.openshift.io",
		"images.config.openshift.io",
		"proxies.config.openshift.io",
		"schedulers.config.openshift.io",
		"consoles.config.openshift.io",
		"ingresses.config.openshift.io",
		"authentications.config.openshift.io",
		"featuregates.config.openshift.io",
	}

	configGVR = schema.GroupVersionResource{
		Group: "config.openshift.io", Version: "v1", Resource: "infrastructures",
	}

	masterLabel       = "node-role.kubernetes.io/master"
	controlPlaneLabel = "node-role.kubernetes.io/control-plane"
)

// TestCreateWithConfigOpenshiftAndNodeLabels exercises increment 3's
// layers end to end on a real Docker daemon: create with a profile of
// exactly the two new layers (full grows as later increments register
// theirs), verify passes both layer checks, the layers' postconditions
// hold independently of their own Verify — the 13 CRDs established, the
// three singletons present, Infrastructure/cluster carrying the
// platformStatus status, every control-plane node master-labeled with
// the empty-string value — and a re-run of create reconciles: the
// server-side applies (labels and the status subresource included) are
// no-ops against a live cluster. A corrupted non-empty master label —
// the deviation the increment-3 review found live — FAILs verify naming
// the node and converges back to "" on the next create.
func TestCreateWithConfigOpenshiftAndNodeLabels(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	prefix := randomPrefix(t)
	cluster := prefix + "-1"
	stateRoot := filepath.Join(t.TempDir(), "state")

	increment3 := func(cfg *config.Config) {
		cfg.Profiles = map[string]config.Profile{"increment-3": {Layers: increment3Layers}}
		cfg.Profile = "increment-3"
	}
	client, _ := testClientAt(t, prefix, stateRoot, increment3, nil)
	if _, err := client.Delete(ctx); err != nil {
		t.Fatalf("pre-clean delete: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Delete(ctx) })

	// 1. Create with the two layers applied.
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
	if !slices.Equal(records[0].Layers, increment3Layers) {
		t.Fatalf("record layers = %v, want %v in registration order", records[0].Layers, increment3Layers)
	}

	// 2. Verify passes, including both layer checks.
	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	requireAllPassAndLayerChecks(t, results, increment3Layers, "after create")

	// 3. The layers' postconditions hold independently of their own
	// Verify, read through the exported kubeconfig.
	kcPath, err := client.Kubeconfig(ctx, cluster)
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	kubeClient, err := kube.NewClient(kcPath)
	if err != nil {
		t.Fatalf("building kube client: %v", err)
	}

	// A short bound: the CRDs are already established (Apply waited for
	// them), so this is an immediate read through the public helper.
	if err := kube.WaitForCRDsEstablished(ctx, kubeClient.Dynamic, 30*time.Second, configOpenshiftCRDs); err != nil {
		t.Errorf("config.openshift.io CRDs not established: %v", err)
	}

	// The three singletons, with Infrastructure/cluster carrying the
	// platformStatus Sensor's OpenShift detection reads.
	infra, err := kubeClient.Dynamic.Resource(configGVR).Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading Infrastructure/cluster: %v", err)
	}
	platformStatus, found, err := unstructured.NestedMap(infra.Object, "status", "platformStatus")
	if err != nil || !found || len(platformStatus) == 0 {
		t.Fatalf("Infrastructure/cluster status.platformStatus = (%v, %t, %v), want a populated map",
			platformStatus, found, err)
	}
	if typ, ok := platformStatus["type"].(string); !ok || typ != "None" {
		t.Errorf("Infrastructure/cluster platformStatus.type = %v, want None", platformStatus["type"])
	}
	for _, singleton := range []struct{ resource, name string }{
		{"apiservers", "cluster"},
		{"clusterversions", "version"},
	} {
		gvr := schema.GroupVersionResource{Group: "config.openshift.io", Version: "v1", Resource: singleton.resource}
		if _, err := kubeClient.Dynamic.Resource(gvr).Get(ctx, singleton.name, metav1.GetOptions{}); err != nil {
			t.Errorf("reading singleton %s/%s: %v", singleton.resource, singleton.name, err)
		}
	}

	// Every control-plane node carries the master role label with the
	// empty-string value — the exact pair the CSV nodeSelectors that
	// schedule OpenShift's operators match.
	nodes, err := kubeClient.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: controlPlaneLabel})
	if err != nil {
		t.Fatalf("listing control-plane nodes: %v", err)
	}
	if len(nodes.Items) == 0 {
		t.Fatal("no control-plane nodes found; a kind cluster always has one")
	}
	for i := range nodes.Items {
		value, ok := nodes.Items[i].Labels[masterLabel]
		if !ok {
			t.Errorf("control-plane node %s carries no %s label", nodes.Items[i].Name, masterLabel)
		} else if value != "" {
			t.Errorf("control-plane node %s carries %s=%q, want the empty string", nodes.Items[i].Name, masterLabel, value)
		}
	}

	// 4. Re-running create reconciles: same cluster instance, both
	// layers re-applied as server-side-apply no-ops.
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

	// 5. Verify still passes after the reconcile, layer checks included.
	results, err = client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify after reconcile: %v", err)
	}
	requireAllPassAndLayerChecks(t, results, increment3Layers, "after reconcile")

	// 6. The live regression the increment-3 review found: a master
	// label with a non-empty value — which nothing in kind sets, but a
	// stray `kubectl label node` produces — schedules master-selected
	// Deployments nowhere while a presence-only check would report
	// PASS. Verify must FAIL naming the node and its value, and a
	// re-run of create must converge the value back to the empty
	// string.
	corrupted := nodes.Items[0].Name
	patch := []byte(fmt.Sprintf(`{"metadata":{"labels":{%q:"weird"}}}`, masterLabel))
	if _, err := kubeClient.Clientset.CoreV1().Nodes().Patch(ctx, corrupted,
		types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		t.Fatalf("setting a non-empty master label on %s: %v", corrupted, err)
	}

	corruptResults, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify after label corruption: %v", err)
	}
	var nodeLabels *kindocp.CheckResult
	for i := range corruptResults {
		if corruptResults[i].Check == "layer:node-labels" {
			nodeLabels = &corruptResults[i]
		}
	}
	if nodeLabels == nil || nodeLabels.Status != kindocp.StatusFail {
		t.Fatalf("verify after label corruption = %+v, want a FAIL for layer:node-labels", corruptResults)
	}
	if !strings.Contains(nodeLabels.Detail, corrupted) || !strings.Contains(nodeLabels.Detail, "weird") {
		t.Errorf("layer:node-labels FAIL detail = %q, want it to name %s and the value weird", nodeLabels.Detail, corrupted)
	}

	// The reconcile converges the corrupted label back to "".
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("reconcile create after label corruption: %v", err)
	}
	refetched, err := kubeClient.Clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: controlPlaneLabel})
	if err != nil {
		t.Fatalf("re-listing control-plane nodes: %v", err)
	}
	for i := range refetched.Items {
		if got := refetched.Items[i].Labels[masterLabel]; got != "" {
			t.Errorf("control-plane node %s still carries %s=%q after reconcile, want the empty string",
				refetched.Items[i].Name, masterLabel, got)
		}
	}
	results, err = client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify after label recovery: %v", err)
	}
	requireAllPassAndLayerChecks(t, results, increment3Layers, "after label recovery")
}

// singleClusterSkipOK reports whether one check result is the
// cross-cluster NodePort check's legitimate SKIP on a single-cluster
// prefix: these tests create one cluster, so the check has no peer to
// reach — a reason, not a failure.
func singleClusterSkipOK(r kindocp.CheckResult) bool {
	return r.Check == "cross-cluster-nodeport" && r.Status == kindocp.StatusSkipped &&
		strings.Contains(r.Detail, "needs at least two clusters")
}

// requireAllPassAndLayerChecks asserts every check passed and every
// named layer check ran — presence, not just the absence of failures,
// so a verify that silently skipped a layer check fails the test.
func requireAllPassAndLayerChecks(t *testing.T, results []kindocp.CheckResult, wantLayers []string, when string) {
	t.Helper()
	ran := map[string]bool{}
	for _, r := range results {
		if r.Status != kindocp.StatusPass && !singleClusterSkipOK(r) {
			t.Errorf("check %s/%s = %s (%s), want PASS %s", r.Cluster, r.Check, r.Status, r.Detail, when)
		}
		ran[r.Check] = true
	}
	for _, layer := range wantLayers {
		if !ran["layer:"+layer] {
			t.Errorf("verify %s ran no layer:%s check (results: %v)", when, layer, results)
		}
	}
}

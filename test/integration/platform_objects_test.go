//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/stackrox/kind-ocp/config"
	"github.com/stackrox/kind-ocp/internal/kube"
	"github.com/stackrox/kind-ocp/internal/layers/platformobjects"
	"github.com/stackrox/kind-ocp/kindocp"
)

// increment5Layers is everything the platform-objects layer builds on,
// in registration order. The profile is not "full" — full grows as
// later increments register layers, and this test is only about
// increment 5.
var increment5Layers = []string{"config-openshift", "node-labels", "olm", "openshift-api-crds", "platform-objects"}

// The GVRs this test reads directly, independently of the layer's own
// table.
var (
	sccGVR = schema.GroupVersionResource{
		Group: "security.openshift.io", Version: "v1", Resource: "securitycontextconstraints",
	}
	configNetworkGVR = schema.GroupVersionResource{
		Group: "config.openshift.io", Version: "v1", Resource: "networks",
	}
	clusterOperatorGVR = schema.GroupVersionResource{
		Group: "config.openshift.io", Version: "v1", Resource: "clusteroperators",
	}
)

// The SCC required fields come from the layer itself
// (platformobjects.RequiredSCCFields, pinned against the pinned CRD by
// unit test): the live proof reads the same set the layer's Verify and
// the regeneration tool enforce.

// wantConfigMaps is the four control-plane ConfigMaps with the data key
// each carries.
var wantConfigMaps = []struct {
	namespace, name, dataKey string
}{
	{"openshift-kube-apiserver", "config", "config.yaml"},
	{"openshift-apiserver", "config", "config.yaml"},
	{"openshift-kube-controller-manager", "config", "config.yaml"},
	{"openshift-etcd", "etcd-pod", "pod.yaml"},
}

// TestCreateWithPlatformObjectsLayer exercises increment 5's layer end
// to end on a real Docker daemon: create with the five-layer profile,
// verify passes every layer check, and the layer's postconditions hold
// independently of its own Verify — both SCCs installed with every
// field the CRD requires, the nullable ones still null-valued (the
// create-semantics gotcha: an apply path would strip them), the four
// control-plane ConfigMaps with
// their data, and the status-bearing singletons carrying the statuses
// the PoC's inline applies silently lost (infrastructures,
// clusteroperators, and images serve a status subresource; networks
// carries its status in the main resource). A re-run of create
// reconciles — the SCC delete+recreate path included — and a deleted
// SCC plus a cleared Network status FAIL verify naming both, converging
// back on the next create.
func TestCreateWithPlatformObjectsLayer(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	prefix := randomPrefix(t)
	cluster := prefix + "-1"
	stateRoot := filepath.Join(t.TempDir(), "state")

	increment5 := func(cfg *config.Config) {
		cfg.Profiles = map[string]config.Profile{"increment-5": {Layers: increment5Layers}}
		cfg.Profile = "increment-5"
	}
	client, _ := testClientAt(t, prefix, stateRoot, increment5, nil)
	if _, err := client.Delete(ctx); err != nil {
		t.Fatalf("pre-clean delete: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Delete(ctx) })

	// 1. Create with the five layers applied.
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
	if !slices.Equal(records[0].Layers, increment5Layers) {
		t.Fatalf("record layers = %v, want %v in registration order", records[0].Layers, increment5Layers)
	}

	// 2. Verify passes, including every layer check.
	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	requireAllPassAndLayerChecks(t, results, increment5Layers, "after create")

	// 3. The layer's postconditions hold independently of its own
	// Verify, read through the exported kubeconfig.
	kcPath, err := client.Kubeconfig(ctx, cluster)
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	kubeClient, err := kube.NewClient(kcPath)
	if err != nil {
		t.Fatalf("building kube client: %v", err)
	}

	// Both SCCs, with every field the CRD requires still present —
	// the live proof of the create-semantics gotcha (an apply path
	// strips the explicit nulls and the CRD then rejects the object).
	for _, name := range []string{"restricted-v2", "privileged"} {
		scc, err := kubeClient.Dynamic.Resource(sccGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("reading the %s SCC: %v", name, err)
		}
		for _, field := range platformobjects.RequiredSCCFields {
			if _, present := scc.Object[field]; !present {
				t.Errorf("the live %s SCC carries no %q field; the create did not send the object as-is", name, field)
			}
		}
	}

	// The four control-plane ConfigMaps with the data the ocp4 checks
	// read.
	for _, cm := range wantConfigMaps {
		live, err := kubeClient.Dynamic.Resource(schema.GroupVersionResource{
			Group: "", Version: "v1", Resource: "configmaps",
		}).Namespace(cm.namespace).Get(ctx, cm.name, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("reading the %s/%s ConfigMap: %v", cm.namespace, cm.name, err)
		}
		value, found, _ := unstructured.NestedString(live.Object, "data", cm.dataKey)
		if !found || value == "" {
			t.Errorf("the %s/%s ConfigMap carries no non-empty %q data", cm.namespace, cm.name, cm.dataKey)
		}
	}

	// The status-bearing singletons: the three the PoC's inline applies
	// lost (status subresource) and the one whose status travels in the
	// main resource (networks carries no subresource).
	infra, err := kubeClient.Dynamic.Resource(configGVR).Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading Infrastructure/cluster: %v", err)
	}
	if name, _, _ := unstructured.NestedString(infra.Object, "status", "infrastructureName"); name != "kind-platform" {
		t.Errorf("Infrastructure status.infrastructureName = %q, want kind-platform (the platform-objects status write)", name)
	}
	clusterOperator, err := kubeClient.Dynamic.Resource(clusterOperatorGVR).Get(ctx, "kube-apiserver", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading ClusterOperator/kube-apiserver: %v", err)
	}
	if conditions, _, _ := unstructured.NestedSlice(clusterOperator.Object, "status", "conditions"); len(conditions) == 0 {
		t.Error("ClusterOperator/kube-apiserver carries no status conditions; the status subresource write did not land")
	}
	network, err := kubeClient.Dynamic.Resource(configNetworkGVR).Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading Network/cluster: %v", err)
	}
	if mtu, _, _ := unstructured.NestedInt64(network.Object, "status", "clusterNetworkMTU"); mtu != 1400 {
		t.Errorf("Network status.clusterNetworkMTU = %d, want 1400 (the status the main apply carries for the subresource-less kind)", mtu)
	}

	// 4. Re-running create reconciles. This is the run that exercises
	// the SCC delete+recreate path live: both SCCs exist, the creates
	// report AlreadyExists, and the layer deletes and re-creates them.
	retryRecords, err := client.Create(ctx)
	if err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	if len(retryRecords) != 1 || retryRecords[0].Status != kindocp.RecordStatusComplete {
		t.Fatalf("reconcile records = %+v, want one complete record", retryRecords)
	}
	if retryRecords[0].InstanceID != records[0].InstanceID {
		t.Fatalf("reconcile changed the instance: %q -> %q", records[0].InstanceID, retryRecords[0].InstanceID)
	}
	results, err = client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify after reconcile: %v", err)
	}
	requireAllPassAndLayerChecks(t, results, increment5Layers, "after reconcile")
	// The re-created SCC still carries its nulls.
	scc, err := kubeClient.Dynamic.Resource(sccGVR).Get(ctx, "restricted-v2", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("re-reading the restricted-v2 SCC: %v", err)
	}
	if _, present := scc.Object["priority"]; !present {
		t.Error("the re-created restricted-v2 SCC carries no priority field; the delete+recreate did not preserve the nulls")
	}

	// 5. The tamper regressions: a deleted SCC and a cleared Network
	// status FAIL verify naming both, and the next create converges
	// them back.
	if err := kubeClient.Dynamic.Resource(sccGVR).Delete(ctx, "privileged", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deleting the privileged SCC for the tamper: %v", err)
	}
	if _, err := kubeClient.Dynamic.Resource(configNetworkGVR).Patch(ctx, "cluster", types.MergePatchType,
		[]byte(`{"status": null}`), metav1.PatchOptions{}); err != nil {
		t.Fatalf("clearing the Network status for the tamper: %v", err)
	}
	corruptResults, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify after the tamper: %v", err)
	}
	var layerCheck *kindocp.CheckResult
	for i := range corruptResults {
		if corruptResults[i].Check == "layer:platform-objects" {
			layerCheck = &corruptResults[i]
		}
	}
	if layerCheck == nil || layerCheck.Status != kindocp.StatusFail {
		t.Fatalf("verify after the tamper = %+v, want a FAIL for layer:platform-objects", corruptResults)
	}
	for _, want := range []string{"privileged", "Network cluster carries no populated status"} {
		if !strings.Contains(layerCheck.Detail, want) {
			t.Errorf("layer:platform-objects FAIL detail = %q, want it to name %q", layerCheck.Detail, want)
		}
	}

	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("converge create after the tamper: %v", err)
	}
	results, err = client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify after the tamper recovery: %v", err)
	}
	requireAllPassAndLayerChecks(t, results, increment5Layers, "after the tamper recovery")
}

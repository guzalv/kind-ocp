//go:build integration

package integration

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/guzalv/kind-ocp/config"
	"github.com/guzalv/kind-ocp/internal/kube"
	kindprov "github.com/guzalv/kind-ocp/internal/provider/kind"
	"github.com/guzalv/kind-ocp/kindocp"
)

// testLogger is the integration suite's logger: kind's progress and
// the layers' logs on stderr, like the CLI's default.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// defaultProfileClient resolves the configuration with no overrides —
// the same defaults the bare `kind-ocp create` uses (count 2, profile
// full) — repointed at an isolated prefix and state root.
func defaultProfileClient(t *testing.T, prefix string, count int) (*kindocp.Client, *config.Config) {
	t.Helper()
	cfg, err := config.Resolve(config.Flags{}, config.Env{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Prefix = prefix
	cfg.Count = count
	cfg.StateRoot = filepath.Join(t.TempDir(), "state")
	prov, err := kindprov.New(testLogger())
	if err != nil {
		t.Fatalf("Docker required for integration tests: %v", err)
	}
	client, err := kindocp.New(kindocp.Options{Config: cfg, Provisioner: prov, Log: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	return client, cfg
}

// podLevelNodePortImage is the one tool the pod-level reachability
// assertion needs that no kind-preloaded image carries: a connect
// binary. busybox is digest-pinned (AC-4.1's verification-path-only
// pin pattern, like the CO catalog), pulled by the kubelet once.
const podLevelNodePortImage = "docker.io/library/busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"

// assertPodLevelNodePortReachable proves AC-1.4's literal property on
// the live clusters: a pod running on the source cluster reaches a
// NodePort service on the target cluster by node IP. It builds the
// same service shape the product's check builds — a selectorless
// NodePort Service whose hand-written EndpointSlice points at the
// target's own apiserver endpoint, so kube-proxy's real NodePort path
// carries the traffic — and runs a digest-pinned busybox client pod on
// the source. The Central-to-Sensor pattern is exactly this shape with
// real workloads instead of the probe pod.
func assertPodLevelNodePortReachable(ctx context.Context, t *testing.T, source, target string, sourceKC, targetKC string) {
	t.Helper()
	const ns = "kind-ocp-it-nodeport"

	targetCS, err := kube.NewClientset(targetKC)
	if err != nil {
		t.Fatal(err)
	}
	sourceCS, err := kube.NewClientset(sourceKC)
	if err != nil {
		t.Fatal(err)
	}

	// The target's apiserver endpoint: the address its own kubernetes
	// service routes to.
	esList, err := targetCS.DiscoveryV1().EndpointSlices("default").List(ctx, metav1.ListOptions{
		LabelSelector: discoveryv1.LabelServiceName + "=kubernetes",
	})
	if err != nil || len(esList.Items) == 0 || len(esList.Items[0].Endpoints) == 0 {
		t.Fatalf("reading the target's apiserver endpoint: (%v, %d items)", err, len(esList.Items))
	}
	apiIP := esList.Items[0].Endpoints[0].Addresses[0]
	apiPort := int32(6443)
	if len(esList.Items[0].Ports) > 0 && esList.Items[0].Ports[0].Port != nil {
		apiPort = *esList.Items[0].Ports[0].Port
	}

	_ = targetCS.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	_ = sourceCS.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	if _, err := targetCS.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = targetCS.CoreV1().Namespaces().Delete(delCtx, ns, metav1.DeleteOptions{})
		_ = sourceCS.CoreV1().Namespaces().Delete(delCtx, ns, metav1.DeleteOptions{})
	})

	svc, err := targetCS.CoreV1().Services(ns).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "nodeport-check"},
		Spec: corev1.ServiceSpec{
			Type:  corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{{Name: "check", Protocol: corev1.ProtocolTCP, Port: 443}},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(svc.Spec.Ports) == 0 || svc.Spec.Ports[0].NodePort == 0 {
		t.Fatalf("service = %+v, want an allocated node port", svc.Spec)
	}
	nodePort := svc.Spec.Ports[0].NodePort
	portName, protocol := "check", corev1.ProtocolTCP
	if _, err := targetCS.DiscoveryV1().EndpointSlices(ns).Create(ctx, &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nodeport-check",
			Namespace: ns,
			Labels:    map[string]string{discoveryv1.LabelServiceName: "nodeport-check"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{{Addresses: []string{apiIP}}},
		Ports:       []discoveryv1.EndpointPort{{Name: &portName, Protocol: &protocol, Port: &apiPort}},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	if _, err := sourceCS.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	podName := "nodeport-check-" + target
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ns},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:  "connect",
				Image: podLevelNodePortImage,
				Command: []string{"sh", "-c", fmt.Sprintf(
					`for i in $(seq 1 30); do if nc -w 3 %s %d </dev/null; then echo connected; exit 0; fi; sleep 1; done; echo could-not-connect; exit 1`,
					apiIP, nodePort)},
			}},
		},
	}
	if _, err := sourceCS.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		live, err := sourceCS.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if live.Status.Phase == corev1.PodSucceeded {
			t.Logf("pod on %s reached NodePort %s:%d on %s", source, apiIP, nodePort, target)
			return
		}
		if live.Status.Phase == corev1.PodFailed {
			t.Fatalf("pod on %s could not reach NodePort %s:%d on %s (AC-1.4: cross-cluster NodePort reachability)", source, apiIP, nodePort, target)
		}
		if time.Now().After(deadline) {
			t.Fatalf("pod on %s did not finish within 2m (phase %s)", source, live.Status.Phase)
		}
		time.Sleep(time.Second)
	}
}

// TestCreateDefaultCountFullProfileAndVerify is the increment's
// canonical run: the default count (2) with the default profile (full,
// all six layers) on a real Docker daemon. Both clusters come up
// (cluster 1 first, cluster 2 concurrently — the create path's
// fan-out), every layer's check passes on both, the cross-cluster
// NodePort check passes in both directions, and the pod-level property
// AC-1.4 names — a pod on cluster 1 reaching a NodePort service on
// cluster 2 by node IP — is proven directly with a client pod.
func TestCreateDefaultCountFullProfileAndVerify(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	prefix := randomPrefix(t)
	client, cfg := defaultProfileClient(t, prefix, config.DefaultCount)

	if _, err := client.Delete(ctx); err != nil {
		t.Fatalf("pre-clean delete: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Delete(ctx) })

	records, err := client.Create(ctx)
	if err != nil {
		t.Fatalf("create (default count %d, profile %s): %v", cfg.Count, cfg.Profile, err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want the default count 2", len(records))
	}
	for i, rec := range records {
		want := fmt.Sprintf("%s-%d", prefix, i+1)
		if rec.Name != want || rec.Status != kindocp.RecordStatusComplete {
			t.Errorf("records[%d] = %+v, want complete %s", i, rec, want)
		}
		if len(rec.Layers) != 6 {
			t.Errorf("records[%d] layers = %v, want all six of the full profile", i, rec.Layers)
		}
	}

	// Per-cluster state: each kubeconfig exists, is private, and works
	// against its own cluster.
	var instanceIDs []string
	for _, rec := range records {
		kc, err := client.Kubeconfig(ctx, rec.Name)
		if err != nil {
			t.Fatalf("kubeconfig for %s: %v", rec.Name, err)
		}
		cs, err := kube.NewClientset(kc)
		if err != nil {
			t.Fatalf("clientset for %s: %v", rec.Name, err)
		}
		id, err := kube.ClusterInstanceID(ctx, cs)
		if err != nil {
			t.Fatalf("instance ID for %s: %v", rec.Name, err)
		}
		instanceIDs = append(instanceIDs, id)
		if records[0].InstanceID != id && rec.Name == records[0].Name {
			t.Errorf("record instance %q does not match live %q", records[0].InstanceID, id)
		}
	}
	if instanceIDs[0] == instanceIDs[1] {
		t.Errorf("both clusters report instance %q; two clusters must be two instances", instanceIDs[0])
	}

	// Verify: every check passes on both clusters, including the
	// cross-cluster NodePort check in both directions.
	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	cross := 0
	for _, r := range results {
		if r.Status != kindocp.StatusPass {
			t.Errorf("check %s/%s = %s (%s), want PASS", r.Cluster, r.Check, r.Status, r.Detail)
		}
		if r.Check == "cross-cluster-nodeport" {
			cross++
		}
	}
	if cross != 2 {
		t.Errorf("cross-cluster lines = %d, want one per cluster (2)", cross)
	}

	// The AC-1.4 property, asserted at pod level: a pod on cluster 1
	// reaches a NodePort service on cluster 2 by node IP.
	kc1, err := client.Kubeconfig(ctx, prefix+"-1")
	if err != nil {
		t.Fatal(err)
	}
	kc2, err := client.Kubeconfig(ctx, prefix+"-2")
	if err != nil {
		t.Fatal(err)
	}
	assertPodLevelNodePortReachable(ctx, t, prefix+"-1", prefix+"-2", kc1, kc2)

	// Delete removes both clusters and all state; idempotent.
	deleted, err := client.Delete(ctx)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !slices.Equal(deleted, []string{prefix + "-1", prefix + "-2"}) {
		t.Errorf("deleted = %v, want both clusters", deleted)
	}
	if _, err := os.Stat(client.StateRoot()); !os.IsNotExist(err) {
		t.Error("state dir must be gone after delete")
	}
	assertNoLeftoverContainers(t, prefix)
}

// TestCreateCountThreeConcurrent is the N=3 acceptance (AC-1.2):
// clusters 2 and 3 are created concurrently with the full profile —
// concurrent layer application included (the PoC's parallel-deploy
// lesson) — and no per-cluster state corrupts: three complete records,
// three distinct live instances, all checks green, and cleanup whole.
func TestCreateCountThreeConcurrent(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	prefix := randomPrefix(t)
	client, _ := defaultProfileClient(t, prefix, 3)

	if _, err := client.Delete(ctx); err != nil {
		t.Fatalf("pre-clean delete: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Delete(ctx) })

	started := time.Now()
	records, err := client.Create(ctx)
	if err != nil {
		t.Fatalf("create (count 3, profile full): %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("records = %d, want 3", len(records))
	}
	seen := map[string]bool{}
	for i, rec := range records {
		want := fmt.Sprintf("%s-%d", prefix, i+1)
		if rec.Name != want || rec.Status != kindocp.RecordStatusComplete || len(rec.Layers) != 6 {
			t.Errorf("records[%d] = %+v, want complete %s with all six layers", i, rec, want)
		}
		if rec.InstanceID == "" || seen[rec.InstanceID] {
			t.Errorf("records[%d] instance %q is empty or repeated; three clusters must be three distinct instances", i, rec.InstanceID)
		}
		seen[rec.InstanceID] = true
	}
	t.Logf("three full-profile clusters in %s (cluster 1 sequential-first, 2 and 3 concurrent)", time.Since(started).Round(time.Second))

	// Each record's kubeconfig still fronts exactly its own live
	// instance before Verify re-exports the files: the concurrent wave
	// must leave every cluster's state pointing at the cluster it
	// provisioned, not a sibling's.
	for _, rec := range records {
		kc, err := client.Kubeconfig(ctx, rec.Name)
		if err != nil {
			t.Fatalf("kubeconfig for %s: %v", rec.Name, err)
		}
		cs, err := kube.NewClientset(kc)
		if err != nil {
			t.Fatalf("clientset for %s: %v", rec.Name, err)
		}
		live, err := kube.ClusterInstanceID(ctx, cs)
		if err != nil {
			t.Fatalf("instance ID for %s: %v", rec.Name, err)
		}
		if live != rec.InstanceID {
			t.Errorf("%s: kubeconfig fronts instance %q, its record says %q", rec.Name, live, rec.InstanceID)
		}
	}

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	cross := 0
	for _, r := range results {
		if r.Status != kindocp.StatusPass {
			t.Errorf("check %s/%s = %s (%s), want PASS", r.Cluster, r.Check, r.Status, r.Detail)
		}
		if r.Check == "cross-cluster-nodeport" {
			cross++
		}
	}
	if cross != 3 {
		t.Errorf("cross-cluster lines = %d, want one per cluster (3)", cross)
	}

	deleted, err := client.Delete(ctx)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !slices.Equal(deleted, []string{prefix + "-1", prefix + "-2", prefix + "-3"}) {
		t.Errorf("deleted = %v, want all three clusters", deleted)
	}
	assertNoLeftoverContainers(t, prefix)
}

// assertNoLeftoverContainers fails when Docker still carries containers
// named after the prefix — the residue discipline every integration
// run is held to.
func assertNoLeftoverContainers(t *testing.T, prefix string) {
	t.Helper()
	out, err := exec.Command("docker", "ps", "-a", "--format", "{{.Names}}", "--filter", "name="+prefix).Output()
	if err != nil {
		t.Fatalf("listing containers for %s: %v", prefix, err)
	}
	if names := strings.Fields(string(out)); len(names) != 0 {
		t.Errorf("containers left behind for prefix %s: %v", prefix, names)
	}
}

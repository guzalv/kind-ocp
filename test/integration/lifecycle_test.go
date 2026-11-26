//go:build integration

// Package integration holds kind-ocp's Docker-gated integration tests. They
// are excluded from `make test` (which must run without Docker) and run via
// `make integration`. Every test skips with a clear message when Docker is
// unavailable (AC-6.3).
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/guzalv/kind-ocp/config"
	"github.com/guzalv/kind-ocp/internal/builtin"
	"github.com/guzalv/kind-ocp/internal/kube"
	kindprov "github.com/guzalv/kind-ocp/internal/provider/kind"
	"github.com/guzalv/kind-ocp/kindocp"
	"github.com/guzalv/kind-ocp/layers"
)

func dockerAvailable() bool {
	cmd := exec.Command("docker", "info")
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run() == nil
}

// requireDocker is the suite's Docker gate, and the split is the
// point. Locally, an unavailable daemon skips the test with a clear
// message (AC-6.3). In CI — every GitHub Actions runner sets CI=true —
// an unavailable daemon fails it instead: this suite, and the CO
// fitness proof inside it, is what the integration job exists to run
// (AC-5.2), so a green run that skipped every test would be a false
// pass, indistinguishable in the job log from a real one. A skipped
// CI run is a red run.
func requireDocker(t *testing.T) {
	t.Helper()
	if dockerAvailable() {
		return
	}
	if os.Getenv("CI") != "" {
		t.Fatalf("Docker is not available in CI (CI=%q): the integration suite fails rather than skip — a green run that proved nothing is worse than a red one", os.Getenv("CI"))
	}
	t.Skip("Docker is not available; skipping integration test (local only — under CI this gate is fatal)")
}

// randomPrefix returns a fresh per-run prefix. A fixed, pre-deleted prefix
// hides failures behind leftovers from earlier runs; a random one starts
// every run from provably empty state.
func randomPrefix(t *testing.T) string {
	t.Helper()
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("kindocp-it-%s", hex.EncodeToString(b[:]))
}

// testClient builds a kindocp.Client against a real kind provisioner with an
// isolated prefix and state root.
func testClient(t *testing.T, prefix string) (*kindocp.Client, *config.Config) {
	t.Helper()
	return testClientAt(t, prefix, filepath.Join(t.TempDir(), "state"), nil, nil)
}

// testClientAt is testClient with a caller-chosen state root (for tests
// that need two clients over the same root), an optional config tweak
// applied before the client snapshots the config, and an optional layer
// registry (nil: the builtin one).
func testClientAt(t *testing.T, prefix, stateRoot string, tweak func(*config.Config), reg *layers.Registry) (*kindocp.Client, *config.Config) {
	t.Helper()

	count := 1
	prefixCopy := prefix
	profileCopy := "minimal"
	cfg, err := config.Resolve(config.Flags{
		Prefix:  &prefixCopy,
		Count:   &count,
		Profile: &profileCopy,
	}, config.Env{})
	if err != nil {
		t.Fatal(err)
	}
	// Keep test state out of the user's state root.
	cfg.StateRoot = stateRoot
	if tweak != nil {
		tweak(cfg)
	}
	if reg == nil {
		reg = builtin.NewRegistry()
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	prov, err := kindprov.New(log)
	if err != nil {
		t.Fatalf("Docker required for integration tests: %v", err)
	}
	client, err := kindocp.New(kindocp.Options{
		Config:      cfg,
		Registry:    reg,
		Provisioner: prov,
		Log:         log,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, cfg
}

// TestCreateDeleteLifecycle exercises the minimal-profile lifecycle end to
// end on a real Docker daemon: create, list, kubeconfig, verify, delete, and
// — the PoC's stale-state bug — a create after delete starting clean.
func TestCreateDeleteLifecycleMinimal(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	prefix := randomPrefix(t)
	client, _ := testClient(t, prefix)
	cluster := prefix + "-1"

	// Clean slate for this run's random prefix (a no-op unless a previous
	// run crashed mid-test with the same random name).
	if _, err := client.Delete(ctx); err != nil {
		t.Fatalf("pre-clean delete: %v", err)
	}
	t.Cleanup(func() {
		_, _ = client.Delete(ctx)
	})

	// 1. Create one minimal-profile cluster.
	records, err := client.Create(ctx)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(records) != 1 || records[0].Name != cluster {
		t.Fatalf("records = %+v, want one record for %s", records, cluster)
	}
	if records[0].Context != "kind-"+cluster {
		t.Errorf("context = %q, want %q (kind context rule)", records[0].Context, "kind-"+cluster)
	}
	if records[0].Profile != "minimal" || len(records[0].Layers) != 0 {
		t.Errorf("record = %+v, want minimal profile with no layers", records[0])
	}

	// 2. Kubeconfig exists in state with restrictive permissions.
	kcPath, err := client.Kubeconfig(ctx, cluster)
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	info, err := os.Stat(kcPath)
	if err != nil {
		t.Fatalf("kubeconfig stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("kubeconfig perms = %o, want 0600", perm)
	}

	// 3. List shows the cluster as running and managed.
	infos, err := client.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(infos) != 1 || infos[0].Name != cluster ||
		infos[0].State != "running" || !infos[0].Managed || infos[0].Profile != "minimal" {
		t.Fatalf("list = %+v, want one running managed %s", infos, cluster)
	}

	// 4. Verify passes (nodes Ready; no layers to verify with minimal;
	// the cross-cluster check skips on a single-cluster prefix).
	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, r := range results {
		if r.Status != kindocp.StatusPass && !singleClusterSkipOK(r) {
			t.Errorf("check %s/%s = %s (%s), want PASS", r.Cluster, r.Check, r.Status, r.Detail)
		}
	}
	if len(results) == 0 {
		t.Fatal("verify produced no results")
	}

	// 5. Delete removes the cluster and all state; idempotent.
	deleted, err := client.Delete(ctx)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !slices.Equal(deleted, []string{cluster}) {
		t.Errorf("deleted = %v, want [%s]", deleted, cluster)
	}
	if _, err := os.Stat(client.StateRoot()); !os.IsNotExist(err) {
		t.Error("state dir must be gone after delete")
	}
	if deleted, err = client.Delete(ctx); err != nil || len(deleted) != 0 {
		t.Errorf("second delete = (%v, %v), want (none, nil)", deleted, err)
	}

	// 6. A create after delete succeeds from zero state (stale-state guard).
	records, err = client.Create(ctx)
	if err != nil {
		t.Fatalf("re-create after delete: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("re-create records = %+v, want one", records)
	}

	// 7. Re-running create reconciles: the matching record is reused, the
	// kubeconfig re-exported, no new cluster.
	records, err = client.Create(ctx)
	if err != nil {
		t.Fatalf("reconcile create: %v", err)
	}
	if len(records) != 1 || records[0].Name != cluster {
		t.Fatalf("reconcile records = %+v, want one %s", records, cluster)
	}
	if records[0].CreatedAt.IsZero() {
		t.Error("reconcile must carry the original CreatedAt, not zero it")
	}

	// The kubeconfig must actually work against the live cluster.
	cs, err := kube.NewClientset(kcPath)
	if err != nil {
		t.Fatalf("building clientset from %s: %v", kcPath, err)
	}
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing nodes through exported kubeconfig: %v", err)
	}
	if len(nodes.Items) == 0 {
		t.Error("no nodes visible through the exported kubeconfig")
	}

	// 8. A cluster that exists without a matching state record is rejected:
	// a second client over the same prefix but a different state root must
	// refuse to reuse it.
	otherClient, otherCfg := testClient(t, prefix)
	if _, err := otherClient.Create(ctx); err == nil ||
		!strings.Contains(err.Error(), "not managed by kind-ocp") {
		t.Fatalf("create through a foreign state root = %v, want an unmanaged-reuse rejection", err)
	}
	if err := os.MkdirAll(otherCfg.StateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := otherClient.Delete(ctx); err != nil {
		t.Fatalf("foreign client must still be able to delete by prefix: %v", err)
	}
}

// TestSameNameReplacementRejected reproduces the review finding on a real
// daemon: a cluster deleted through one state root and re-created under the
// same name through another must not be trusted by the first root's record.
// The record is bound to the cluster instance (kube-system UID); verify
// fails it explicitly and create refuses to reuse it.
func TestSameNameReplacementRejected(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	prefix := randomPrefix(t)
	cluster := prefix + "-1"

	// Root A: create the original cluster and complete its record.
	clientA, _ := testClient(t, prefix)
	if _, err := clientA.Delete(ctx); err != nil {
		t.Fatalf("pre-clean delete: %v", err)
	}
	t.Cleanup(func() {
		_, _ = clientA.Delete(ctx)
	})
	if _, err := clientA.Create(ctx); err != nil {
		t.Fatalf("create under root A: %v", err)
	}

	// The reviewer's path: delete the cluster through a foreign root B,
	// then re-create the same name under B. Root A's state is untouched.
	clientB, _ := testClient(t, prefix)
	if _, err := clientB.Delete(ctx); err != nil {
		t.Fatalf("delete through root B: %v", err)
	}
	if _, err := clientB.Create(ctx); err != nil {
		t.Fatalf("re-create under root B: %v", err)
	}
	t.Cleanup(func() {
		_, _ = clientB.Delete(ctx)
	})

	// Verify through root A must fail the replacement explicitly, not pass
	// the original record's checks against it.
	results, err := clientA.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	var instanceFail int
	for _, r := range results {
		if r.Check == "instance-id" && r.Status == kindocp.StatusFail {
			instanceFail++
		}
		// The replacement cluster also skips the cross-cluster check:
		// its record failed verification, and this prefix holds one
		// cluster — both are the check's designed SKIP reasons.
		if r.Status != kindocp.StatusPass && r.Check != "instance-id" &&
			!(r.Check == "cross-cluster-nodeport" && r.Status == kindocp.StatusSkipped) {
			t.Errorf("unexpected FAIL/PASS-less check %s = %s (%s)", r.Check, r.Status, r.Detail)
		}
	}
	if instanceFail != 1 {
		t.Fatalf("instance-id FAIL count = %d, want 1 (results: %v)", instanceFail, results)
	}

	// Create through root A must refuse to reuse the replacement.
	if _, err := clientA.Create(ctx); err == nil ||
		!strings.Contains(err.Error(), "different instance") {
		t.Fatalf("create through root A = %v, want a different-instance rejection", err)
	}

	// Delete through A still recovers everything by generated name.
	if deleted, err := clientA.Delete(ctx); err != nil ||
		!slices.Equal(deleted, []string{cluster}) {
		t.Fatalf("delete through root A = (%v, %v), want ([%s], nil)", deleted, err, cluster)
	}
}

// TestInterruptedCreateReplacementRejected reproduces the review finding on
// a real daemon: a create that dies before the cluster identity is recorded
// (here: a readiness timeout that fires before the first poll can answer)
// leaves an unbound pending record. A same-named replacement cluster —
// deleted and re-created through another state root — must not be adopted
// by the retry, verify must fail the record explicitly, and delete recovers.
func TestInterruptedCreateReplacementRejected(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	prefix := randomPrefix(t)
	cluster := prefix + "-1"
	stateRootA := filepath.Join(t.TempDir(), "state-a")

	// Root A: the interrupted create. The readiness timeout is already
	// expired when the wait starts, so the run dies after the pending
	// record is written but before the identity is bound.
	nanoTimeout := func(cfg *config.Config) { cfg.Timeouts.NodeReady = time.Nanosecond }
	clientA, _ := testClientAt(t, prefix, stateRootA, nanoTimeout, nil)
	if _, err := clientA.Delete(ctx); err != nil {
		t.Fatalf("pre-clean delete: %v", err)
	}
	t.Cleanup(func() { _, _ = clientA.Delete(ctx) })
	if _, err := clientA.Create(ctx); err == nil ||
		!strings.Contains(err.Error(), "waiting for nodes Ready") {
		t.Fatalf("create under root A = %v, want a readiness timeout", err)
	}

	// The reviewer's path: delete the cluster through a foreign root B and
	// re-create the same name under it. Root A's state is untouched.
	clientB, _ := testClient(t, prefix)
	if _, err := clientB.Delete(ctx); err != nil {
		t.Fatalf("delete through root B: %v", err)
	}
	if _, err := clientB.Create(ctx); err != nil {
		t.Fatalf("re-create under root B: %v", err)
	}
	t.Cleanup(func() { _, _ = clientB.Delete(ctx) })

	// Retry through root A, now with a sane timeout: the unbound record
	// must not adopt the replacement — nor anything else carrying the name.
	retryA, _ := testClientAt(t, prefix, stateRootA, nil, nil)
	_, err := retryA.Create(ctx)
	if err == nil || !strings.Contains(err.Error(), "not bound to a cluster instance") {
		t.Fatalf("retry through root A = %v, want an unbound-record rejection", err)
	}
	if !strings.Contains(err.Error(), "delete and re-create") {
		t.Errorf("error %q must point at the recovery path", err)
	}

	// Verify through root A fails the record explicitly and stops there.
	results, err := retryA.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	var instanceFail, layerChecks int
	for _, r := range results {
		if r.Check == "instance-id" && r.Status == kindocp.StatusFail {
			instanceFail++
		}
		if strings.HasPrefix(r.Check, "layer:") {
			layerChecks++
		}
	}
	if instanceFail != 1 {
		t.Fatalf("instance-id FAIL count = %d, want 1 (results: %v)", instanceFail, results)
	}
	if layerChecks != 0 {
		t.Errorf("layer checks ran against an unverified cluster: %v", results)
	}

	// Delete through A still recovers everything by generated name.
	if deleted, err := retryA.Delete(ctx); err != nil ||
		!slices.Equal(deleted, []string{cluster}) {
		t.Fatalf("delete through root A = (%v, %v), want ([%s], nil)", deleted, err, cluster)
	}
}

// flakyLayer fails its first Apply and succeeds afterwards, so a create can
// be interrupted mid-layer on a real daemon (the builtin registry has no
// layers to fail yet).
type flakyLayer struct {
	failFirst bool
	applied   int
}

func (l *flakyLayer) Name() string { return "it-flaky" }

func (l *flakyLayer) Apply(_ context.Context, _ layers.Env) error {
	l.applied++
	if l.failFirst {
		l.failFirst = false
		return errors.New("flaky layer: first apply fails by design")
	}
	return nil
}

func (l *flakyLayer) Verify(_ context.Context, _ layers.Env) error { return nil }

// TestLayerFailureRetryConvergesSameInstance is the interrupted-create
// companion the review asked for: a create interrupted mid-layer (after the
// identity is bound, before completion) must converge on a retry against
// the same instance, with the binding preserved.
func TestLayerFailureRetryConvergesSameInstance(t *testing.T) {
	requireDocker(t)
	ctx := context.Background()
	prefix := randomPrefix(t)
	cluster := prefix + "-1"

	flaky := &flakyLayer{failFirst: true}
	reg := layers.NewRegistry()
	if err := reg.Register(flaky); err != nil {
		t.Fatal(err)
	}
	fullProfile := func(cfg *config.Config) { cfg.Profile = "full" }
	client, _ := testClientAt(t, prefix, filepath.Join(t.TempDir(), "state"), fullProfile, reg)
	if _, err := client.Delete(ctx); err != nil {
		t.Fatalf("pre-clean delete: %v", err)
	}
	t.Cleanup(func() { _, _ = client.Delete(ctx) })

	// First create: the layer fails, the run stops, the cluster is kept.
	if _, err := client.Create(ctx); err == nil ||
		!strings.Contains(err.Error(), "layer it-flaky") {
		t.Fatalf("create = %v, want the layer failure", err)
	}

	// Capture the live instance before the retry, through the public API.
	kc, err := client.Kubeconfig(ctx, cluster)
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	cs, err := kube.NewClientset(kc)
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	instanceBefore, err := kube.ClusterInstanceID(ctx, cs)
	if err != nil {
		t.Fatalf("reading the live instance: %v", err)
	}

	// Same instance, failure spent: the retry converges.
	records, err := client.Create(ctx)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(records) != 1 || records[0].Status != kindocp.RecordStatusComplete {
		t.Fatalf("retry records = %+v, want one complete record", records)
	}
	if records[0].InstanceID == "" || records[0].InstanceID != instanceBefore {
		t.Fatalf("retry record instanceID = %q, want the pre-retry instance %q", records[0].InstanceID, instanceBefore)
	}
	if flaky.applied != 2 {
		t.Errorf("layer applied %d times, want 2 (failed apply + retry)", flaky.applied)
	}

	// Verify passes end to end after the retry.
	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, r := range results {
		if r.Status != kindocp.StatusPass && !singleClusterSkipOK(r) {
			t.Errorf("check %s/%s = %s (%s), want PASS", r.Cluster, r.Check, r.Status, r.Detail)
		}
	}
}

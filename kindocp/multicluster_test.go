package kindocp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/guzalv/kind-ocp/internal/state"
	"github.com/guzalv/kind-ocp/layers"
)

// concurrentLayer is a recording layer safe for the concurrent wave:
// clusters 2..N apply their layers at the same time, so everything the
// layer touches is mutex-guarded.
type concurrentLayer struct {
	name string

	mu      sync.Mutex
	applied []string
	failAt  map[string]error // cluster name → Apply failure
}

func (l *concurrentLayer) Name() string { return l.name }

func (l *concurrentLayer) Apply(_ context.Context, env layers.Env) error {
	l.mu.Lock()
	l.applied = append(l.applied, env.ClusterName)
	err := l.failAt[env.ClusterName]
	l.mu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

func (l *concurrentLayer) Verify(_ context.Context, _ layers.Env) error { return nil }

func (l *concurrentLayer) appliedClusters() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.applied)
}

// rendezvousLayer proves the wave concurrent: the first Apply passes
// straight through (cluster 1 runs alone), and every later Apply blocks
// until two of them are inside Apply at the same time. A sequential
// wave deadlocks the second Apply against its timeout and fails the
// create — the regression the layer exists to catch.
type rendezvousLayer struct {
	name string

	mu       sync.Mutex
	passed   int // applies that already ran to completion
	inflight map[string]bool
	release  chan struct{}
	once     sync.Once
}

func newRendezvousLayer() *rendezvousLayer {
	return &rendezvousLayer{
		name:     "rendezvous",
		inflight: map[string]bool{},
		release:  make(chan struct{}),
	}
}

func (l *rendezvousLayer) Name() string { return l.name }

func (l *rendezvousLayer) Apply(_ context.Context, env layers.Env) error {
	l.mu.Lock()
	first := l.passed == 0
	if !first {
		l.inflight[env.ClusterName] = true
	}
	two := len(l.inflight) >= 2
	l.mu.Unlock()
	if two {
		l.once.Do(func() { close(l.release) })
	}
	if first {
		l.mu.Lock()
		l.passed++
		l.mu.Unlock()
		return nil
	}
	select {
	case <-l.release:
		return nil
	case <-time.After(10 * time.Second):
		return fmt.Errorf("cluster %s: no second concurrent apply within 10s — the wave ran sequentially", env.ClusterName)
	}
}

func (l *rendezvousLayer) Verify(_ context.Context, _ layers.Env) error { return nil }

// newConcurrentClient builds a client whose count clusters all apply
// one layer under profile full.
func newConcurrentClient(t *testing.T, count int, layer layers.Layer) *Client {
	t.Helper()
	cfg := testConfig(t, count, "full")
	reg := layers.NewRegistry()
	if err := reg.Register(layer); err != nil {
		t.Fatal(err)
	}
	client := newTestClient(t, cfg, reg, newFakeProvisioner())
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
	return client
}

// TestCreateConcurrentClustersCompleteStateIsolated is the N=3
// acceptance: cluster 1 runs alone, clusters 2 and 3 run concurrently,
// and no per-cluster state corrupts — every record completes, every
// kubeconfig lands under its own cluster directory, and the records
// come back in name order regardless of completion order.
func TestCreateConcurrentClustersCompleteStateIsolated(t *testing.T) {
	layer := &concurrentLayer{name: "one"}
	client := newConcurrentClient(t, 3, layer)

	records, err := client.Create(context.Background())
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(records) != 3 {
		t.Fatalf("records = %+v, want three", records)
	}
	for i, want := range []string{"p-1", "p-2", "p-3"} {
		if records[i].Name != want || records[i].Status != state.StatusComplete {
			t.Errorf("records[%d] = %+v, want complete %s", i, records[i], want)
		}
		if records[i].InstanceID != testInstanceID {
			t.Errorf("records[%d].InstanceID = %q, want the live identity", i, records[i].InstanceID)
		}
	}
	applied := layer.appliedClusters()
	slices.Sort(applied)
	if !slices.Equal(applied, []string{"p-1", "p-2", "p-3"}) {
		t.Errorf("layer applied to %v, want all three clusters", applied)
	}

	// Per-cluster state isolation: each cluster's kubeconfig is its own
	// file under its own directory, and each record is complete.
	// (CheckKubeconfig returns the walk-resolved path — on macOS the
	// state root's /var ancestor is a symlink the walk resolves away.)
	for _, name := range []string{"p-1", "p-2", "p-3"} {
		kc, err := client.store.CheckKubeconfig(name)
		if err != nil {
			t.Errorf("kubeconfig for %s: %v (per-cluster paths must not corrupt under concurrency)", name, err)
			continue
		}
		if _, err := os.Stat(kc); err != nil {
			t.Errorf("kubeconfig for %s at %s: %v", name, kc, err)
		}
		rec, found, err := client.store.Record(name)
		if err != nil || !found || rec.Status != state.StatusComplete || rec.InstanceID != testInstanceID {
			t.Errorf("state record for %s = (%v, %v, %v), want complete and bound", name, rec, found, err)
		}
	}
}

// TestCreateClustersTwoThroughNRunConcurrently pins the concurrency
// itself: the Applies for clusters 2 and 3 must overlap in time. The
// rendezvous layer fails the create when the second wave cluster finds
// no partner within its timeout, so a sequential wave (a regression to
// the increment-1 loop) fails this test.
func TestCreateClustersTwoThroughNRunConcurrently(t *testing.T) {
	client := newConcurrentClient(t, 3, newRendezvousLayer())

	if _, err := client.Create(context.Background()); err != nil {
		t.Fatalf("create: %v (the wave clusters never applied concurrently)", err)
	}
}

// TestCreateConcurrentFailuresAreAggregated: failures in the concurrent
// wave neither cancel the other clusters mid-layer nor hide behind the
// first failure — the error names every failing cluster, the successful
// cluster's record completes, and the failing ones stay pending.
func TestCreateConcurrentFailuresAreAggregated(t *testing.T) {
	layer := &concurrentLayer{
		name: "one",
		failAt: map[string]error{
			"p-2": errors.New("first failure"),
			"p-3": errors.New("second failure"),
		},
	}
	client := newConcurrentClient(t, 3, layer)

	records, err := client.Create(context.Background())
	if err == nil {
		t.Fatal("create succeeded over two failing clusters, want an aggregate error")
	}
	for _, substr := range []string{"cluster p-2", "first failure", "cluster p-3", "second failure"} {
		if !strings.Contains(err.Error(), substr) {
			t.Errorf("error %q does not mention %q (both wave failures must be reported)", err, substr)
		}
	}
	// The successful cluster is still reported alongside the error.
	if len(records) != 1 || records[0].Name != "p-1" {
		t.Errorf("records = %+v, want only the successful p-1", records)
	}
	if rec, found, _ := client.store.Record("p-1"); !found || rec.Status != state.StatusComplete {
		t.Errorf("p-1 record = (%v, %v), want complete", rec, found)
	}
	for _, name := range []string{"p-2", "p-3"} {
		if rec, found, _ := client.store.Record(name); !found || rec.Status != state.StatusPending {
			t.Errorf("%s record = (%v, %v), want pending (kept for debugging, retry converges)", name, rec, found)
		}
	}
}

// TestCreateClusterOneFailureStopsTheWave: cluster 1's create is the
// gate — when it fails, clusters 2..N are never started, which is the
// sequential contract every earlier increment ran under.
func TestCreateClusterOneFailureStopsTheWave(t *testing.T) {
	cfg := testConfig(t, 3, "full")
	prov := newFakeProvisioner()
	prov.mu.Lock()
	prov.createErr["p-1"] = errors.New("docker exploded")
	prov.mu.Unlock()
	reg := layers.NewRegistry()
	if err := reg.Register(&concurrentLayer{name: "one"}); err != nil {
		t.Fatal(err)
	}
	client := newTestClient(t, cfg, reg, prov)

	_, err := client.Create(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cluster p-1") || !strings.Contains(err.Error(), "docker exploded") {
		t.Fatalf("create = %v, want the cluster-1 failure naming the cluster", err)
	}
	prov.mu.Lock()
	created := slices.Clone(prov.created)
	prov.mu.Unlock()
	if len(created) != 0 {
		t.Errorf("provisioned = %v, want nothing after the cluster-1 failure", created)
	}
}

// TestCreateRaisesHostLimitsOncePerRun: create asks a provisioner
// implementing the host-limit capability to raise the host's limits
// exactly once per run (the inotify raise kind's node containers need);
// a provisioner without the capability — a remote runtime, a test
// double — is never asked, so nothing but a real container runtime
// ever changes host settings. What the kind implementation does with
// the call is unit-tested in its own package.
func TestCreateRaisesHostLimitsOncePerRun(t *testing.T) {
	prov := &hostRaisingProvisioner{fakeProvisioner: newFakeProvisioner()}
	cfg := testConfig(t, 3, "full")
	reg := layers.NewRegistry()
	if err := reg.Register(&concurrentLayer{name: "one"}); err != nil {
		t.Fatal(err)
	}
	client := newTestClient(t, cfg, reg, prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }

	if _, err := client.Create(context.Background()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if prov.raised != 1 {
		t.Errorf("RaiseHostLimits called %d times, want exactly once per run (not once per cluster)", prov.raised)
	}
}

// hostRaisingProvisioner records the host-limit capability's calls.
type hostRaisingProvisioner struct {
	*fakeProvisioner
	raised int
}

func (p *hostRaisingProvisioner) RaiseHostLimits(context.Context) { p.raised++ }

// TestCreateSkipsHostLimitsOnCancelledContext: the host tune changes
// real host state, so a run that never started — its context already
// cancelled — must not tune anything on its way out.
func TestCreateSkipsHostLimitsOnCancelledContext(t *testing.T) {
	prov := &hostRaisingProvisioner{fakeProvisioner: newFakeProvisioner()}
	client := newTestClient(t, testConfig(t, 2, "minimal"), layers.NewRegistry(), prov)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Create(ctx); err == nil {
		t.Fatal("create with a cancelled context = nil, want the context error")
	}
	if prov.raised != 0 {
		t.Errorf("RaiseHostLimits called %d times under a cancelled context, want zero", prov.raised)
	}
}

// TestCreateSkipsHostLimitsOnUnsafeStateRoot: the state boundary is
// verified before the tune runs, so a create whose state root is
// refused — here an unmarked, non-empty directory at the prefix path,
// which is never adopted — fails without having touched host settings.
func TestCreateSkipsHostLimitsOnUnsafeStateRoot(t *testing.T) {
	prov := &hostRaisingProvisioner{fakeProvisioner: newFakeProvisioner()}
	cfg := testConfig(t, 2, "minimal")
	prefixDir := filepath.Join(cfg.StateRoot, cfg.Prefix)
	if err := os.MkdirAll(prefixDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefixDir, "stray"), []byte("not kind-ocp state"), 0o600); err != nil {
		t.Fatal(err)
	}
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	if _, err := client.Create(context.Background()); err == nil {
		t.Fatal("create over an unmanaged state root = nil, want the boundary refusal")
	}
	if prov.raised != 0 {
		t.Errorf("RaiseHostLimits called %d times on a refused state root, want zero", prov.raised)
	}
}

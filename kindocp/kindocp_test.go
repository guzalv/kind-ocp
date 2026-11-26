package kindocp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/guzalv/kind-ocp/config"
	"github.com/guzalv/kind-ocp/internal/state"
	"github.com/guzalv/kind-ocp/layers"
	"github.com/guzalv/kind-ocp/provider"
)

// fakeProvisioner is an in-memory provider.Provisioner. Its maps and
// slices are mutex-guarded: create's concurrent wave calls into one
// instance from several goroutines.
type fakeProvisioner struct {
	mu         sync.Mutex
	clusters   map[string]bool // existing clusters
	created    []string
	deleted    []string
	exported   []string
	createErr  map[string]error
	kubeconfig map[string][]byte // content written per path
	loaded     []loadedImage     // LoadImage calls, in order
	built      []builtImage      // BuildImage calls, in order
}

// loadedImage records one LoadImage call.
type loadedImage struct {
	cluster string
	ref     string
}

// builtImage records one BuildImage call.
type builtImage struct {
	ref string
	dir string
}

func newFakeProvisioner(existing ...string) *fakeProvisioner {
	fp := &fakeProvisioner{
		clusters:   map[string]bool{},
		createErr:  map[string]error{},
		kubeconfig: map[string][]byte{},
	}
	for _, n := range existing {
		fp.clusters[n] = true
	}
	return fp
}

func (f *fakeProvisioner) Create(_ context.Context, opts provider.CreateOptions) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.createErr[opts.Name]; err != nil {
		return false, err
	}
	if f.clusters[opts.Name] {
		// Reconcile path: re-export.
		f.exported = append(f.exported, opts.Name)
		f.kubeconfig[opts.KubeconfigPath] = []byte("existing")
		if err := f.exportTo(opts.KubeconfigPath, "existing"); err != nil {
			return false, err
		}
		return false, nil
	}
	f.clusters[opts.Name] = true
	f.created = append(f.created, opts.Name)
	f.kubeconfig[opts.KubeconfigPath] = []byte("new")
	// kind writes the kubeconfig during create; the fake mirrors that.
	if err := f.exportTo(opts.KubeconfigPath, "new"); err != nil {
		return true, err
	}
	return true, nil
}

// exportTo writes kubeconfig content to path the way kind does,
// creating the parent directory first.
func (f *fakeProvisioner) exportTo(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o600)
}

func (f *fakeProvisioner) Delete(_ context.Context, name string, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.clusters, name)
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakeProvisioner) List(_ context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for n := range f.clusters {
		out = append(out, n)
	}
	slices.Sort(out)
	return out, nil
}

func (f *fakeProvisioner) ExportKubeconfig(_ context.Context, name string, path string) error {
	f.mu.Lock()
	exists := f.clusters[name]
	content := f.kubeconfig[path]
	f.mu.Unlock()
	if !exists {
		return fmt.Errorf("cluster %s does not exist", name)
	}
	f.mu.Lock()
	f.exported = append(f.exported, name)
	f.mu.Unlock()
	return f.exportTo(path, string(content))
}

func (f *fakeProvisioner) LoadImage(_ context.Context, cluster string, imageRef string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.clusters[cluster] {
		return fmt.Errorf("cluster %s does not exist", cluster)
	}
	f.loaded = append(f.loaded, loadedImage{cluster: cluster, ref: imageRef})
	return nil
}

// BuildImage implements provider.ImageBuilder: the build is
// cluster-independent, so the call is recorded without one.
func (f *fakeProvisioner) BuildImage(_ context.Context, ref, contextDir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.built = append(f.built, builtImage{ref: ref, dir: contextDir})
	return nil
}

// loaderOnlyProvisioner delegates the lifecycle to inner and
// implements only the image-loading capability — the shape of a
// runtime that can import images but not build them locally.
type loaderOnlyProvisioner struct {
	inner *fakeProvisioner
}

func (l *loaderOnlyProvisioner) Create(ctx context.Context, opts provider.CreateOptions) (bool, error) {
	return l.inner.Create(ctx, opts)
}

func (l *loaderOnlyProvisioner) Delete(ctx context.Context, name string, kubeconfigPath string) error {
	return l.inner.Delete(ctx, name, kubeconfigPath)
}

func (l *loaderOnlyProvisioner) List(ctx context.Context) ([]string, error) {
	return l.inner.List(ctx)
}

func (l *loaderOnlyProvisioner) ExportKubeconfig(ctx context.Context, name string, path string) error {
	return l.inner.ExportKubeconfig(ctx, name, path)
}

func (l *loaderOnlyProvisioner) LoadImage(ctx context.Context, cluster string, imageRef string) error {
	return l.inner.LoadImage(ctx, cluster, imageRef)
}

// lifecycleOnlyProvisioner delegates exactly the required Provisioner
// lifecycle and deliberately implements nothing else — a provider with
// no optional capabilities, the shape an external runtime plugs in
// when it cannot import images.
type lifecycleOnlyProvisioner struct {
	inner *fakeProvisioner
}

func (l *lifecycleOnlyProvisioner) Create(ctx context.Context, opts provider.CreateOptions) (bool, error) {
	return l.inner.Create(ctx, opts)
}

func (l *lifecycleOnlyProvisioner) Delete(ctx context.Context, name string, kubeconfigPath string) error {
	return l.inner.Delete(ctx, name, kubeconfigPath)
}

func (l *lifecycleOnlyProvisioner) List(ctx context.Context) ([]string, error) {
	return l.inner.List(ctx)
}

func (l *lifecycleOnlyProvisioner) ExportKubeconfig(ctx context.Context, name string, path string) error {
	return l.inner.ExportKubeconfig(ctx, name, path)
}

// imageNeedingRef is the reference the image-needing test layer loads.
const imageNeedingRef = "localhost/kind-ocp/needs-image:test"

// imageNeedingLayer models a layer like scc-admission that builds a
// local image: it refuses to run without the image-building and
// image-loading services and exercises both when it has them.
type imageNeedingLayer struct {
	name   string
	loaded bool
}

func (l *imageNeedingLayer) Name() string { return l.name }

func (l *imageNeedingLayer) Apply(ctx context.Context, env layers.Env) error {
	if env.BuildImage == nil {
		return errors.New("no image-building service: cannot build " + imageNeedingRef)
	}
	if env.LoadImage == nil {
		return errors.New("no image-loading service: cannot load " + imageNeedingRef)
	}
	if err := env.BuildImage(ctx, imageNeedingRef, "/fake/context"); err != nil {
		return err
	}
	if err := env.LoadImage(ctx, imageNeedingRef); err != nil {
		return err
	}
	l.loaded = true
	return nil
}

func (l *imageNeedingLayer) Verify(_ context.Context, env layers.Env) error {
	if env.BuildImage != nil {
		return errors.New("Verify's environment must not carry an image builder")
	}
	if env.LoadImage != nil {
		return errors.New("Verify's environment must not carry an image loader")
	}
	return nil
}

// recordingLayer records apply/verify calls and can fail on demand.
type recordingLayer struct {
	name       string
	applied    []string
	verified   []layers.Env
	verifyErr  error
	applyErrAt string // cluster name that fails on Apply
}

func (l *recordingLayer) Name() string { return l.name }

func (l *recordingLayer) Apply(_ context.Context, env layers.Env) error {
	l.applied = append(l.applied, env.ClusterName)
	if l.applyErrAt == env.ClusterName {
		return errors.New("apply exploded")
	}
	return nil
}

func (l *recordingLayer) Verify(_ context.Context, env layers.Env) error {
	l.verified = append(l.verified, env)
	return l.verifyErr
}

// exportHookProvisioner wraps a provisioner and runs hook during
// ExportKubeconfig — the seam for interference between two of verify's
// reads (cluster selection by list, then the record read). exported
// records every path the wrapper saw, so tests can assert how many
// exports actually happened.
type exportHookProvisioner struct {
	provider.Provisioner
	hook     func(name string)
	exported []string
}

func (e *exportHookProvisioner) ExportKubeconfig(ctx context.Context, name string, path string) error {
	if e.hook != nil {
		e.hook(name)
	}
	e.exported = append(e.exported, path)
	return e.Provisioner.ExportKubeconfig(ctx, name, path)
}

// deleteHookProvisioner wraps a provisioner and runs hook during
// Delete — the seam for filesystem changes between delete's up-front
// state verification and its cleanup.
type deleteHookProvisioner struct {
	provider.Provisioner
	hook func(name string)
}

func (d *deleteHookProvisioner) Delete(ctx context.Context, name string, kubeconfigPath string) error {
	if d.hook != nil {
		d.hook(name)
	}
	return d.Provisioner.Delete(ctx, name, kubeconfigPath)
}

func testConfig(t *testing.T, count int, profile string) *config.Config {
	t.Helper()
	return &config.Config{
		Prefix:        "p",
		Count:         count,
		Profile:       profile,
		KindNodeImage: "kindest/node:test",
		StateRoot:     filepath.Join(t.TempDir(), "state"),
		Timeouts:      config.TimeoutsConfig{NodeReady: time.Minute},
		Clusters:      map[string]config.ClusterOverride{},
		Profiles:      map[string]config.Profile{},
		Layers:        map[string]yaml.Node{},
	}
}

func newTestClient(t *testing.T, cfg *config.Config, reg *layers.Registry, prov provider.Provisioner) *Client {
	t.Helper()
	c, err := New(Options{Config: cfg, Registry: reg, Provisioner: prov, Log: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// testInstanceID is the kube-system UID served by the test clientsets; it
// stands in for the identity kind assigns at bootstrap.
const testInstanceID = "uid-test-instance"

// readyClientset returns a fake clientset whose nodes are all Ready and
// whose kube-system namespace carries testInstanceID.
func readyClientset() kubernetes.Interface {
	return clientsetFor(testInstanceID, corev1.ConditionTrue)
}

// notReadyClientset returns a fake clientset with one not-Ready node.
func notReadyClientset() kubernetes.Interface {
	return clientsetFor(testInstanceID, corev1.ConditionFalse)
}

// clientsetFor builds a fake clientset serving one control-plane node with
// the given readiness and a kube-system namespace with the given instance
// ID. It returns the concrete fake type so tests can assert which actions
// were really issued against it.
func clientsetFor(instanceID string, ready corev1.ConditionStatus) *fake.Clientset {
	return fake.NewSimpleClientset(
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "p-1-control-plane"},
			Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{
				Type: corev1.NodeReady, Status: ready,
			}}},
		},
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: "kube-system", UID: types.UID(instanceID),
		}},
	)
}

func TestNewValidatesConfig(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	cfg.Prefix = "../escape"
	if _, err := New(Options{Config: cfg, Provisioner: newFakeProvisioner()}); err == nil {
		t.Fatal("expected New to reject an invalid prefix before opening state")
	}
	cfg = testConfig(t, 1, "minimal")
	cfg.StateRoot = ""
	if _, err := New(Options{Config: cfg, Provisioner: newFakeProvisioner()}); err == nil {
		t.Fatal("expected New to reject an empty state root")
	}
}

func TestNewDefaultsRegistry(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	// No Registry passed: the builtin registry is the default. The
	// provisioner is overridden so the test needs no Docker.
	c, err := New(Options{Config: cfg, Provisioner: newFakeProvisioner(), Log: slog.Default()})
	if err != nil {
		t.Fatal(err)
	}
	// The builtin registry grows one layer per increment; assert its
	// current shape so a new registration extends it deliberately.
	want := []string{"config-openshift", "node-labels", "olm", "openshift-api-crds", "platform-objects", "scc-admission"}
	if names := c.reg.Names(); !slices.Equal(names, want) {
		t.Fatalf("default registry = %v, want the builtin registry %v", names, want)
	}
}

func TestCreateAppliesLayersInOrderAndWritesRecords(t *testing.T) {
	cfg := testConfig(t, 2, "full")
	prov := newFakeProvisioner()
	reg := layers.NewRegistry()
	l1 := &recordingLayer{name: "one"}
	l2 := &recordingLayer{name: "two"}
	for _, l := range []layers.Layer{l1, l2} {
		if err := reg.Register(l); err != nil {
			t.Fatal(err)
		}
	}
	client := newTestClient(t, cfg, reg, prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }

	records, err := client.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	// Layer apply order: cluster 1 fully, then cluster 2, each layer in
	// registration order.
	if !slices.Equal(l1.applied, []string{"p-1", "p-2"}) || !slices.Equal(l2.applied, []string{"p-1", "p-2"}) {
		t.Errorf("apply order: l1=%v l2=%v, want p-1,p-2 each", l1.applied, l2.applied)
	}
	if records[0].Name != "p-1" || records[0].Context != "kind-p-1" {
		t.Errorf("record[0] = %+v", records[0])
	}
	if !slices.Equal(records[0].Layers, []string{"one", "two"}) {
		t.Errorf("record layers = %v, want [one two]", records[0].Layers)
	}
	if records[0].Status != state.StatusComplete {
		t.Errorf("record status = %q, want %q", records[0].Status, state.StatusComplete)
	}
	if records[0].InstanceID != testInstanceID {
		t.Errorf("record instanceID = %q, want the live cluster identity %q", records[0].InstanceID, testInstanceID)
	}
	// The result Create returns is the public contract; probe the fields
	// the other assertions do not cover.
	if records[0].KindNodeImage != "kindest/node:test" || records[0].CreatedAt.IsZero() {
		t.Errorf("record = %+v, want the configured node image and a creation time", records[0])
	}
	// Records in state.
	rec, found, err := client.store.Record("p-2")
	if err != nil || !found || rec.Profile != "full" || rec.Status != state.StatusComplete {
		t.Errorf("state record for p-2 = (%v, %v, %v)", rec, found, err)
	}
}

// findResult returns one cluster's check result, or nil when absent.
func findResult(results []CheckResult, cluster, check string) *CheckResult {
	for i := range results {
		if results[i].Cluster == cluster && results[i].Check == check {
			return &results[i]
		}
	}
	return nil
}

// The image-loading service follows the provisioner's optional
// capability (provider.ImageLoader), not the required interface: a
// lifecycle-only provider leaves the service unset and a layer that
// needs it refuses to run — create fails naming the service, not a
// stubbed method — while a provider that implements the capability
// gets the per-cluster binding and the layer's load reaches exactly
// that cluster. Verify's environments carry no loader either way.
// TestCreateBindsImageServicesByProvisionerCapability: the
// image-building and image-loading services ride the same binding
// rule — present exactly when the provisioner implements the matching
// optional capability, never in Verify's environments — because a
// layer that builds a local image needs both halves (the build on the
// daemon, the load into the cluster) and a runtime that offers one
// but not the other must fail the layer, not half-run it.
func TestCreateBindsImageServicesByProvisionerCapability(t *testing.T) {
	ctx := context.Background()

	newClient := func(t *testing.T, prov provider.Provisioner, l layers.Layer) *Client {
		t.Helper()
		reg := layers.NewRegistry()
		if err := reg.Register(l); err != nil {
			t.Fatal(err)
		}
		client := newTestClient(t, testConfig(t, 2, "full"), reg, prov)
		client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
		return client
	}

	t.Run("a lifecycle-only provisioner leaves both services unset", func(t *testing.T) {
		prov := &lifecycleOnlyProvisioner{inner: newFakeProvisioner()}
		client := newClient(t, prov, &imageNeedingLayer{name: "needs-image"})
		_, err := client.Create(ctx)
		if err == nil || !strings.Contains(err.Error(), "no image-building service") {
			t.Fatalf("create = %v, want the image-needing layer's refusal", err)
		}
	})

	t.Run("a loader without a builder leaves the build service unset", func(t *testing.T) {
		prov := &loaderOnlyProvisioner{inner: newFakeProvisioner()}
		client := newClient(t, prov, &imageNeedingLayer{name: "needs-image"})
		_, err := client.Create(ctx)
		if err == nil || !strings.Contains(err.Error(), "no image-building service") {
			t.Fatalf("create = %v, want the build-service refusal when the runtime cannot build images", err)
		}
	})

	t.Run("a provider with the capabilities gets the per-cluster bindings", func(t *testing.T) {
		prov := newFakeProvisioner()
		needsImage := &imageNeedingLayer{name: "needs-image"}
		client := newClient(t, prov, needsImage)
		if _, err := client.Create(ctx); err != nil {
			t.Fatal(err)
		}
		if !needsImage.loaded {
			t.Error("the layer saw no image services; Apply gets both when the provisioner implements the capabilities")
		}
		wantLoads := []loadedImage{
			{cluster: "p-1", ref: imageNeedingRef},
			{cluster: "p-2", ref: imageNeedingRef},
		}
		if !reflect.DeepEqual(prov.loaded, wantLoads) {
			t.Errorf("provisioner loads = %v, want the layer's image loaded into each cluster %v", prov.loaded, wantLoads)
		}
		wantBuilds := []builtImage{
			{ref: imageNeedingRef, dir: "/fake/context"},
			{ref: imageNeedingRef, dir: "/fake/context"},
		}
		if !reflect.DeepEqual(prov.built, wantBuilds) {
			t.Errorf("provisioner builds = %v, want the layer's image built once per cluster env %v", prov.built, wantBuilds)
		}
		// Verify's environments carry neither service. A layer refusal
		// there is a FAIL result, not a returned error (Verify reports
		// per check), so assert the layer's check passed on both
		// clusters — discarding the results would let a refusal hide.
		results, err := client.Verify(ctx, nil)
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		for _, cluster := range []string{"p-1", "p-2"} {
			r := findResult(results, cluster, "layer:"+needsImage.Name())
			if r == nil {
				t.Fatalf("no layer:%s result for %s in %v", needsImage.Name(), cluster, results)
			}
			if r.Status != StatusPass {
				t.Errorf("%s layer:%s = %s (%s), want PASS: Verify's environment must not carry an image service", cluster, needsImage.Name(), r.Status, r.Detail)
			}
		}
	})
}

func TestCreatePerClusterProfileOverride(t *testing.T) {
	cfg := testConfig(t, 2, "full")
	cfg.Clusters["p-1"] = config.ClusterOverride{Profile: "minimal"}
	prov := newFakeProvisioner()
	reg := layers.NewRegistry()
	l1 := &recordingLayer{name: "one"}
	if err := reg.Register(l1); err != nil {
		t.Fatal(err)
	}
	client := newTestClient(t, cfg, reg, prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }

	if _, err := client.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	// minimal = no layers: p-1 untouched, p-2 applied.
	if !slices.Equal(l1.applied, []string{"p-2"}) {
		t.Errorf("applied = %v, want [p-2] only (p-1 is minimal)", l1.applied)
	}
	rec, _, _ := client.store.Record("p-1")
	if rec.Profile != "minimal" || len(rec.Layers) != 0 {
		t.Errorf("p-1 record = %+v, want minimal profile, no layers", rec)
	}
}

func TestCreateFailureNamesClusterAndLayer(t *testing.T) {
	cfg := testConfig(t, 2, "full")
	prov := newFakeProvisioner()
	reg := layers.NewRegistry()
	l1 := &recordingLayer{name: "one", applyErrAt: "p-2"}
	if err := reg.Register(l1); err != nil {
		t.Fatal(err)
	}
	client := newTestClient(t, cfg, reg, prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }

	_, err := client.Create(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, substr := range []string{"cluster p-2", "layer one"} {
		if !strings.Contains(err.Error(), substr) {
			t.Errorf("error %q does not mention %q", err, substr)
		}
	}
	// Cluster 1 succeeded and is complete; cluster 2 is marked pending.
	if rec, found, _ := client.store.Record("p-1"); !found || rec.Status != state.StatusComplete {
		t.Errorf("p-1 record = (%v, %v), want complete", rec, found)
	}
	if rec, found, _ := client.store.Record("p-2"); !found || rec.Status != state.StatusPending {
		t.Errorf("p-2 record = (%v, %v), want pending (create did not finish)", rec, found)
	}
}

func TestCreateProvisionFailureIncludesStep(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	prov := newFakeProvisioner()
	prov.createErr["p-1"] = errors.New("docker exploded")
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)

	_, err := client.Create(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, substr := range []string{"cluster p-1", "provisioning kind cluster", "docker exploded"} {
		if !strings.Contains(err.Error(), substr) {
			t.Errorf("error %q does not mention %q", err, substr)
		}
	}
}

// A bad profile on a later cluster must fail before the first cluster is
// provisioned, not after clusters already exist.
func TestCreateEagerValidation(t *testing.T) {
	t.Run("unknown per-cluster profile", func(t *testing.T) {
		cfg := testConfig(t, 3, "full")
		cfg.Clusters["p-3"] = config.ClusterOverride{Profile: "nope"}
		prov := newFakeProvisioner()
		reg := layers.NewRegistry()
		if err := reg.Register(&recordingLayer{name: "one"}); err != nil {
			t.Fatal(err)
		}
		client := newTestClient(t, cfg, reg, prov)

		_, err := client.Create(context.Background())
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "p-3") || !strings.Contains(err.Error(), "nope") {
			t.Errorf("error %q must name the cluster and profile", err)
		}
		if len(prov.created) != 0 || len(prov.clusters) != 0 {
			t.Errorf("provisioned = %v, want nothing (validation precedes provisioning)", prov.created)
		}
	})

	t.Run("override of an ungenerated cluster name", func(t *testing.T) {
		cfg := testConfig(t, 2, "minimal")
		cfg.Clusters["p-9"] = config.ClusterOverride{Profile: "minimal"}
		prov := newFakeProvisioner()
		client := newTestClient(t, cfg, layers.NewRegistry(), prov)

		if _, err := client.Create(context.Background()); err == nil {
			t.Fatal("expected ValidateCreate to reject the override")
		}
		if len(prov.created) != 0 {
			t.Errorf("provisioned = %v, want nothing", prov.created)
		}
	})

	// A profile's layer list naming an unknown layer fails create up
	// front, selected (the long-standing behavior) or not (§5.3's
	// cold-machine finding: a typo in an unselected profile previously
	// created fine and surfaced only on the run that selected it).
	t.Run("unknown layer in a selected profile", func(t *testing.T) {
		cfg := testConfig(t, 1, "typoed")
		cfg.Profiles["typoed"] = config.Profile{Layers: []string{"one", "nope"}}
		prov := newFakeProvisioner()
		reg := layers.NewRegistry()
		if err := reg.Register(&recordingLayer{name: "one"}); err != nil {
			t.Fatal(err)
		}
		client := newTestClient(t, cfg, reg, prov)

		_, err := client.Create(context.Background())
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "typoed") || !strings.Contains(err.Error(), "nope") {
			t.Errorf("error %q must name the profile and the unknown layer", err)
		}
		if len(prov.created) != 0 {
			t.Errorf("provisioned = %v, want nothing (validation precedes provisioning)", prov.created)
		}
	})

	t.Run("unknown layer in an unselected profile", func(t *testing.T) {
		cfg := testConfig(t, 1, "minimal")
		cfg.Profiles["unselected"] = config.Profile{Layers: []string{"one", "nope"}}
		cfg.Profiles["also-fine"] = config.Profile{Layers: []string{"one"}}
		prov := newFakeProvisioner()
		reg := layers.NewRegistry()
		if err := reg.Register(&recordingLayer{name: "one"}); err != nil {
			t.Fatal(err)
		}
		client := newTestClient(t, cfg, reg, prov)

		_, err := client.Create(context.Background())
		if err == nil {
			t.Fatal("expected the unselected profile's typo to fail create")
		}
		if !strings.Contains(err.Error(), "unselected") || !strings.Contains(err.Error(), "nope") {
			t.Errorf("error %q must name the profile and the unknown layer", err)
		}
		if len(prov.created) != 0 {
			t.Errorf("provisioned = %v, want nothing (validation precedes provisioning)", prov.created)
		}
	})
}

func TestCreateReusesMatchingRecord(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	prov := newFakeProvisioner("p-1") // already exists
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
	createdAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	if err := client.store.WriteRecord(state.Record{
		Name:          "p-1",
		Context:       "kind-p-1",
		Profile:       "minimal",
		KindNodeImage: "kindest/node:test",
		Status:        state.StatusComplete,
		CreatedAt:     createdAt,
		InstanceID:    testInstanceID, // bound to the live cluster
	}); err != nil {
		t.Fatal(err)
	}

	records, err := client.Create(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(prov.created) != 0 {
		t.Errorf("created = %v, want none (cluster exists)", prov.created)
	}
	if !slices.Equal(prov.exported, []string{"p-1"}) {
		t.Errorf("exported = %v, want kubeconfig re-export for p-1", prov.exported)
	}
	if records[0].Name != "p-1" || records[0].Status != state.StatusComplete {
		t.Errorf("record = %+v, want complete p-1", records[0])
	}
	if !records[0].CreatedAt.Equal(createdAt) {
		t.Errorf("createdAt = %v, want the original %v preserved on reuse", records[0].CreatedAt, createdAt)
	}
	// Retrying a complete record preserves its binding.
	if records[0].InstanceID != testInstanceID {
		t.Errorf("instanceID = %q, want the preserved binding %q", records[0].InstanceID, testInstanceID)
	}
}

func TestCreateRejectsIncompatibleReuse(t *testing.T) {
	seed := func(t *testing.T, client *Client, rec state.Record) {
		t.Helper()
		if err := client.store.WriteRecord(rec); err != nil {
			t.Fatal(err)
		}
	}
	baseRecord := state.Record{
		Name:          "p-1",
		Context:       "kind-p-1",
		Profile:       "full",
		Layers:        []string{"one"},
		KindNodeImage: "kindest/node:test",
		Status:        state.StatusComplete,
	}

	t.Run("no record", func(t *testing.T) {
		cfg := testConfig(t, 1, "minimal")
		prov := newFakeProvisioner("p-1")
		client := newTestClient(t, cfg, layers.NewRegistry(), prov)
		_, err := client.Create(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not managed by kind-ocp") {
			t.Fatalf("error = %v, want an unmanaged-reuse rejection", err)
		}
		if len(prov.deleted) != 0 {
			t.Errorf("delete = %v, want nothing (cluster kept for debugging)", prov.deleted)
		}
	})

	t.Run("node image mismatch", func(t *testing.T) {
		cfg := testConfig(t, 1, "full")
		prov := newFakeProvisioner("p-1")
		reg := layers.NewRegistry()
		if err := reg.Register(&recordingLayer{name: "one"}); err != nil {
			t.Fatal(err)
		}
		client := newTestClient(t, cfg, reg, prov)
		rec := baseRecord
		rec.KindNodeImage = "kindest/node:other"
		seed(t, client, rec)
		_, err := client.Create(context.Background())
		if err == nil || !strings.Contains(err.Error(), "delete and re-create") {
			t.Fatalf("error = %v, want an image-mismatch rejection", err)
		}
	})

	t.Run("profile mismatch", func(t *testing.T) {
		cfg := testConfig(t, 1, "minimal") // record says full
		prov := newFakeProvisioner("p-1")
		reg := layers.NewRegistry()
		if err := reg.Register(&recordingLayer{name: "one"}); err != nil {
			t.Fatal(err)
		}
		client := newTestClient(t, cfg, reg, prov)
		seed(t, client, baseRecord)
		_, err := client.Create(context.Background())
		if err == nil || !strings.Contains(err.Error(), "delete and re-create") {
			t.Fatalf("error = %v, want a profile-mismatch rejection", err)
		}
		if len(prov.deleted) != 0 {
			t.Errorf("delete = %v, want nothing (mismatch never removes layers)", prov.deleted)
		}
	})
}

// A readiness timeout must leave the cluster in place for debugging and
// mark the record pending. Because the run died before the cluster identity
// was recorded, the pending record is unbound — and an unbound record is
// never adopted: the name alone proves nothing, so the retry is rejected
// with the delete-and-re-create recovery, and delete does recover.
func TestCreateReadinessTimeoutKeepsCluster(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	cfg.Timeouts.NodeReady = 100 * time.Millisecond
	prov := newFakeProvisioner()
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return notReadyClientset(), nil }

	_, err := client.Create(context.Background())
	if err == nil || !strings.Contains(err.Error(), "waiting for nodes Ready") {
		t.Fatalf("error = %v, want a readiness timeout error", err)
	}
	if !strings.Contains(err.Error(), "cluster p-1") {
		t.Errorf("error %q must name the cluster", err)
	}
	if !prov.clusters["p-1"] {
		t.Error("cluster must be kept for debugging after a readiness timeout")
	}
	rec, found, _ := client.store.Record("p-1")
	if !found || rec.Status != state.StatusPending {
		t.Errorf("record = (%v, %v), want pending — no success record on timeout", rec, found)
	}
	if rec.InstanceID != "" {
		t.Errorf("record instanceID = %q, want empty (identity is bound only once the API answers)", rec.InstanceID)
	}

	// Retry with a healthy API: rejected, because the record cannot prove
	// the live cluster is the one it describes.
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
	_, err = client.Create(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not bound to a cluster instance") {
		t.Fatalf("error = %v, want an unbound-record rejection", err)
	}
	if !strings.Contains(err.Error(), "delete and re-create") {
		t.Errorf("error %q must point at the recovery path", err)
	}
	if !prov.clusters["p-1"] || len(prov.deleted) != 0 {
		t.Error("the cluster must be kept, nothing deleted by the rejection")
	}
	// The rejection must not rewrite the record.
	if rec, found, _ := client.store.Record("p-1"); !found || rec.Status != state.StatusPending || rec.InstanceID != "" {
		t.Errorf("record = (%v, %v); the rejection must not rewrite it", rec, found)
	}

	// Delete recovers, and the next create starts from zero state.
	if _, err := client.Delete(context.Background()); err != nil {
		t.Fatalf("delete after the rejection: %v", err)
	}
	records, err := client.Create(context.Background())
	if err != nil {
		t.Fatalf("create after delete: %v", err)
	}
	if len(prov.created) != 2 || records[0].Status != state.StatusComplete || records[0].InstanceID != testInstanceID {
		t.Errorf("create after delete = %+v (created %v), want a fresh complete bound record", records, prov.created)
	}
}

// A create that dies mid-layer leaves a pending record bound to the live
// instance; retrying against the same instance converges to complete. This
// is the legitimate retry path the unbound-record rejection must not break.
func TestCreateRetryAfterLayerFailureConvergesSameInstance(t *testing.T) {
	cfg := testConfig(t, 1, "full")
	prov := newFakeProvisioner()
	reg := layers.NewRegistry()
	flaky := &recordingLayer{name: "one", applyErrAt: "p-1"}
	if err := reg.Register(flaky); err != nil {
		t.Fatal(err)
	}
	client := newTestClient(t, cfg, reg, prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }

	if _, err := client.Create(context.Background()); err == nil {
		t.Fatal("expected the first create to fail at the layer")
	}
	// The pending record is already bound: the identity was recorded
	// before any layer ran.
	rec, found, _ := client.store.Record("p-1")
	if !found || rec.Status != state.StatusPending || rec.InstanceID != testInstanceID {
		t.Fatalf("record = (%v, %v), want pending and bound to %s", rec, found, testInstanceID)
	}
	if !prov.clusters["p-1"] || len(prov.deleted) != 0 {
		t.Error("the cluster must be kept, nothing deleted by the failure")
	}

	// Same instance, failure cleared: the retry converges.
	flaky.applyErrAt = ""
	records, err := client.Create(context.Background())
	if err != nil {
		t.Fatalf("retry against the same instance: %v", err)
	}
	if len(prov.created) != 1 {
		t.Errorf("created = %v, want the cluster reused, not re-created", prov.created)
	}
	if records[0].Status != state.StatusComplete || records[0].InstanceID != testInstanceID {
		t.Errorf("retry record = %+v, want complete with the preserved binding %q", records[0], testInstanceID)
	}
	if rec, _, _ := client.store.Record("p-1"); rec.Status != state.StatusComplete || rec.InstanceID != testInstanceID {
		t.Errorf("state record after retry = %+v, want complete with the preserved binding", rec)
	}
}

// The interrupted-create attack: a record whose run died before the
// identity was bound must not let a later create adopt whatever cluster
// carries the name — not the original, and not a replacement.
func TestCreateRejectsReplacementAfterInterruptedCreate(t *testing.T) {
	t.Run("unbound pending record is not adopted", func(t *testing.T) {
		cfg := testConfig(t, 1, "minimal")
		cfg.Timeouts.NodeReady = 100 * time.Millisecond
		prov := newFakeProvisioner()
		client := newTestClient(t, cfg, layers.NewRegistry(), prov)
		// Interrupted create: dies at readiness, before the identity is
		// recorded.
		client.clientset = func(string) (kubernetes.Interface, error) { return notReadyClientset(), nil }
		if _, err := client.Create(context.Background()); err == nil {
			t.Fatal("expected the interrupted create to fail")
		}
		rec, _, _ := client.store.Record("p-1")
		if rec.InstanceID != "" {
			t.Fatalf("precondition: record = %+v, want unbound", rec)
		}

		// The name now fronts a different cluster (deleted and re-created
		// out of band). The retry must reject regardless: even the
		// original instance cannot be proven from an unbound record.
		client.clientset = func(string) (kubernetes.Interface, error) {
			return clientsetFor("uid-replacement", corev1.ConditionTrue), nil
		}
		_, err := client.Create(context.Background())
		if err == nil || !strings.Contains(err.Error(), "not bound to a cluster instance") {
			t.Fatalf("error = %v, want an unbound-record rejection", err)
		}
		if !prov.clusters["p-1"] || len(prov.deleted) != 0 {
			t.Error("the cluster must be kept, nothing deleted by the rejection")
		}
		if rec, found, _ := client.store.Record("p-1"); !found || rec.InstanceID != "" || rec.Status != state.StatusPending {
			t.Errorf("record = (%v, %v); the rejection must not rewrite it", rec, found)
		}
	})

	t.Run("bound pending record rejects a replacement", func(t *testing.T) {
		cfg := testConfig(t, 1, "full")
		prov := newFakeProvisioner()
		reg := layers.NewRegistry()
		flaky := &recordingLayer{name: "one", applyErrAt: "p-1"}
		if err := reg.Register(flaky); err != nil {
			t.Fatal(err)
		}
		client := newTestClient(t, cfg, reg, prov)
		client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
		if _, err := client.Create(context.Background()); err == nil {
			t.Fatal("expected the first create to fail at the layer")
		}
		rec, _, _ := client.store.Record("p-1")
		if rec.InstanceID != testInstanceID || rec.Status != state.StatusPending {
			t.Fatalf("precondition: record = %+v, want pending and bound", rec)
		}

		// The name now fronts a replacement instance; the retry must
		// reject it instead of converging the recorded profile onto it.
		client.clientset = func(string) (kubernetes.Interface, error) {
			return clientsetFor("uid-replacement", corev1.ConditionTrue), nil
		}
		_, err := client.Create(context.Background())
		if err == nil || !strings.Contains(err.Error(), "different instance") {
			t.Fatalf("error = %v, want a different-instance rejection", err)
		}
		if !strings.Contains(err.Error(), "delete and re-create") {
			t.Errorf("error %q must point at the recovery path", err)
		}
		if len(flaky.applied) != 1 {
			t.Errorf("applied = %v, want only the first run's apply (no apply against the replacement)", flaky.applied)
		}
		if rec, found, _ := client.store.Record("p-1"); !found || rec.InstanceID != testInstanceID || rec.Status != state.StatusPending {
			t.Errorf("record = (%v, %v); the rejection must not rewrite it", rec, found)
		}
	})

	t.Run("replacement during the run is caught at bind time", func(t *testing.T) {
		cfg := testConfig(t, 1, "minimal")
		prov := newFakeProvisioner("p-1")
		client := newTestClient(t, cfg, layers.NewRegistry(), prov)
		if err := client.store.WriteRecord(state.Record{
			Name: "p-1", Context: "kind-p-1", Profile: "minimal",
			KindNodeImage: "kindest/node:test", Status: state.StatusComplete,
			InstanceID: "uid-recorded",
		}); err != nil {
			t.Fatal(err)
		}
		// Clientset calls in order: the reuse identity check reads the
		// recorded instance; the readiness wait and the bind then see the
		// replacement's identity.
		calls := 0
		client.clientset = func(string) (kubernetes.Interface, error) {
			calls++
			if calls == 1 {
				return clientsetFor("uid-recorded", corev1.ConditionTrue), nil
			}
			return clientsetFor("uid-replacement", corev1.ConditionTrue), nil
		}

		_, err := client.Create(context.Background())
		if err == nil || !strings.Contains(err.Error(), "replaced while this run was converging") {
			t.Fatalf("error = %v, want the mid-run replacement rejection", err)
		}
		if rec, found, _ := client.store.Record("p-1"); !found || rec.InstanceID != "uid-recorded" {
			t.Errorf("record = (%v, %v); the rejection must not rebind it to the replacement", rec, found)
		}
	})
}

func TestDeleteExactGeneratedShapeOnly(t *testing.T) {
	cfg := testConfig(t, 2, "minimal")
	// p-3 and p-10 are leftovers from a run with a larger count; p-01, p-1a,
	// and p-dev-1 are not generated names for this prefix.
	prov := newFakeProvisioner("p-1", "p-2", "p-3", "p-10", "p-01", "p-1a", "p-dev-1", "px-1", "other-1")
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	if err := client.store.WriteRecord(state.Record{Name: "p-1"}); err != nil {
		t.Fatal(err)
	}

	deleted, err := client.Delete(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(deleted)
	if want := []string{"p-1", "p-10", "p-2", "p-3"}; !slices.Equal(deleted, want) {
		t.Errorf("deleted = %v, want %v (exact generated shape, any count)", deleted, want)
	}
	for _, kept := range []string{"p-01", "p-1a", "p-dev-1", "px-1", "other-1"} {
		if !prov.clusters[kept] {
			t.Errorf("cluster %s must not be touched (not a generated name for prefix p)", kept)
		}
	}
	if _, err := os.Stat(client.StateRoot()); !os.IsNotExist(err) {
		t.Error("state dir must be removed")
	}

	// Second delete: nothing left, no error.
	deleted, err = client.Delete(context.Background())
	if err != nil || len(deleted) != 0 {
		t.Errorf("second Delete() = (%v, %v), want (empty, nil)", deleted, err)
	}
}

func TestListMergesStateAndProvider(t *testing.T) {
	cfg := testConfig(t, 2, "minimal")
	prov := newFakeProvisioner("p-1", "other-1")
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	if err := client.store.WriteRecord(state.Record{
		Name: "p-2", Context: "kind-p-2", Profile: "full", Layers: []string{"olm"}, Status: state.StatusComplete,
	}); err != nil {
		t.Fatal(err)
	}

	infos, err := client.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("got %d infos, want 2", len(infos))
	}
	// p-1: running, unmanaged (no record).
	if infos[0].Name != "p-1" || infos[0].State != "running" || infos[0].Managed || infos[0].Status != "" {
		t.Errorf("infos[0] = %+v", infos[0])
	}
	// p-2: recorded but cluster gone.
	if infos[1].Name != "p-2" || infos[1].State != "missing" || !infos[1].Managed ||
		infos[1].Profile != "full" || infos[1].Status != state.StatusComplete {
		t.Errorf("infos[1] = %+v", infos[1])
	}
}

func TestKubeconfigRejectsNonGeneratedNames(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	prov := newFakeProvisioner("p-1")
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)

	for _, bad := range []string{"../../etc/passwd", "p", "p-0", "p-1a", "other-1"} {
		if _, err := client.Kubeconfig(context.Background(), bad); err == nil {
			t.Errorf("Kubeconfig(%q) = nil error, want rejection before any state path is built", bad)
		}
	}
	// Unknown but well-formed name: still an error, naming the prefix.
	if _, err := client.Kubeconfig(context.Background(), "p-2"); err == nil ||
		!strings.Contains(err.Error(), "unknown cluster") {
		t.Errorf("Kubeconfig(p-2) error = %v, want unknown-cluster", err)
	}
	// A live cluster exports fine even without a record. The returned
	// path is the symlink-resolved verified one, so compare against the
	// resolved state root (t.TempDir paths sit under macOS's /var
	// symlink).
	path, err := client.Kubeconfig(context.Background(), "p-1")
	if err != nil {
		t.Fatalf("Kubeconfig(p-1) = %v, want success", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(client.StateRoot())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(path, resolvedRoot+string(filepath.Separator)) {
		t.Errorf("path %s is not under the resolved state root %s", path, resolvedRoot)
	}
}

func TestVerifyUnknownClusterName(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	prov := newFakeProvisioner("p-1")
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)

	if _, err := client.Verify(context.Background(), []string{"nope"}); err == nil {
		t.Fatal("expected unknown-cluster error")
	}
}

// A repeated name in the selection would reach the cross-cluster check
// as two peers that are one cluster: the two-peer requirement would be
// satisfied with nothing to connect to, and the check would report a
// vacuous PASS. Verify rejects the repetition up front.
func TestVerifyRejectsRepeatedNames(t *testing.T) {
	cfg := testConfig(t, 2, "minimal")
	prov := newFakeProvisioner("p-1", "p-2")
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }

	_, err := client.Verify(context.Background(), []string{"p-1", "p-1"})
	if err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("Verify(p-1, p-1) = %v, want the repeated-name rejection", err)
	}
	if !strings.Contains(err.Error(), "p-1") {
		t.Errorf("error %q must name the repeated cluster", err)
	}
}

func TestVerifyNoClustersIsError(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	prov := newFakeProvisioner()
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)

	if _, err := client.Verify(context.Background(), nil); err == nil {
		t.Fatal("expected an error when no clusters exist")
	}
}

func TestVerifyReportsMissingAndUnmanagedClusters(t *testing.T) {
	cfg := testConfig(t, 2, "minimal")
	prov := newFakeProvisioner("p-1") // p-2 recorded but gone
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	if err := client.store.WriteRecord(state.Record{Name: "p-2", Context: "kind-p-2"}); err != nil {
		t.Fatal(err)
	}

	results, err := client.Verify(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var missing, unmanaged int
	for _, r := range results {
		switch {
		case r.Cluster == "p-2" && r.Check == "cluster-running" && r.Status == StatusFail:
			missing++
		case r.Cluster == "p-1" && r.Check == "managed-state" && r.Status == StatusFail:
			unmanaged++
		}
	}
	if missing != 1 {
		t.Errorf("missing-cluster FAIL count = %d, want 1 (results: %v)", missing, results)
	}
	if unmanaged != 1 {
		t.Errorf("unmanaged-cluster FAIL count = %d, want 1 (results: %v)", unmanaged, results)
	}
}

// vanishCluster removes a cluster the way the increment-10 finding
// describes: the container and the state record both go, so the cluster
// is invisible to list and to every per-cluster check — exactly the
// shape that let a lone survivor report all-PASS before the count
// assertion existed.
func vanishCluster(t *testing.T, client *Client, prov *fakeProvisioner, name string) {
	t.Helper()
	prov.mu.Lock()
	delete(prov.clusters, name)
	prov.mu.Unlock()
	kubeconfigPath, err := client.store.KubeconfigPath(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(kubeconfigPath)); err != nil {
		t.Fatal(err)
	}
}

// The increment-10 finding: a cluster whose container and state record
// both vanished produced no verify line at all, and the survivors alone
// reported all-PASS. The whole-prefix count assertion must FAIL naming
// the vanished cluster and the remedy, while the survivor's checks
// still run.
func TestVerifyFailsWhenAVanishedClusterLeavesNoTrace(t *testing.T) {
	cfg := testConfig(t, 2, "minimal")
	prov := newFakeProvisioner()
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
	if _, err := client.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	vanishCluster(t, client, prov, "p-2")

	results, err := client.Verify(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	count := findResult(results, cfg.Prefix, expectedClustersCheck)
	if count == nil {
		t.Fatalf("no cluster-count result in %v", results)
	}
	if count.Status != StatusFail {
		t.Fatalf("cluster-count = %s (%s), want FAIL", count.Status, count.Detail)
	}
	for _, want := range []string{"p-2", "re-run the original create command", "--count 2",
		"excludes every missing cluster", "no count create can write excludes p-1"} {
		if !strings.Contains(count.Detail, want) {
			t.Errorf("cluster-count detail %q does not name %q", count.Detail, want)
		}
	}
	// The survivor is still checked.
	if r := findResult(results, "p-1", "nodes-ready"); r == nil || r.Status != StatusPass {
		t.Errorf("p-1 nodes-ready = %+v, want PASS — the vanished sibling must not stop the survivor's checks", r)
	}
	// The vanished cluster produces no per-cluster lines of its own (it
	// is invisible to list); the count line is its only mention.
	for _, r := range results {
		if r.Cluster == "p-2" {
			t.Errorf("result %+v names p-2; a vanished cluster has no per-cluster checks", r)
		}
	}
}

// The count assertion reads what create persisted, never a count a
// verify-time client would resolve: a prefix created with --count 3 and
// verified through a client that resolves count 2 (the default) must
// not FAIL. This is §3.3's paste-safety property standing — the CLI
// reads no create-only values outside create, and the façade's verify
// path reads none either.
func TestVerifyCountAssertionUsesThePersistedExpectationNotTheConfig(t *testing.T) {
	createCfg := testConfig(t, 3, "minimal")
	prov := newFakeProvisioner()
	client := newTestClient(t, createCfg, layers.NewRegistry(), prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
	if _, err := client.Create(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A plain verify resolves a different count (2, the default); the
	// assertion must still expect all three clusters.
	verifyCfg := testConfig(t, 2, "minimal")
	verifyCfg.StateRoot = createCfg.StateRoot
	verifyClient := newTestClient(t, verifyCfg, layers.NewRegistry(), prov)
	verifyClient.clientset = client.clientset
	results, err := verifyClient.Verify(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	count := findResult(results, verifyCfg.Prefix, expectedClustersCheck)
	if count == nil {
		t.Fatalf("no cluster-count result in %v", results)
	}
	if count.Status != StatusPass {
		t.Fatalf("cluster-count = %s (%s), want PASS — the expectation is the persisted one (3), not the resolved count (2)",
			count.Status, count.Detail)
	}
	if !strings.Contains(count.Detail, "3") {
		t.Errorf("cluster-count detail %q does not report the persisted expectation", count.Detail)
	}
}

// Old and corrupt state degrade to a SKIP, never a false FAIL and never
// a blocked verify: a state root written by an older binary carries no
// expectation at all, an expectation carrying a value no create could
// have written is corrupt — below 1, above create's safety cap
// (including a huge value whose names would exhaust memory), or not an
// integer at all (a string, a fraction, an overflow) — and so is a
// marker that does not parse. In every case the clusters that exist
// are still checked. The marker that does not parse is the one
// genuinely invalid ownership marker: it fails the boundary walk
// behind the kubeconfig check (the tree is refused, never silently
// skipped over), while every corrupt-expectation shape leaves the
// tree's ownership intact.
func TestVerifyCountAssertionDegradesOnOldOrCorruptState(t *testing.T) {
	// The first count no create could write, derived from the
	// classifier create validates with, so a cap bump moves this
	// probe with it.
	firstNonCreatable := 1
	for config.IsCreatableCount(firstNonCreatable) {
		firstNonCreatable++
	}
	cases := []struct {
		name     string
		expected string // the sidecar's content; "" writes no sidecar
		// readyCheck is the check that must have run for p-1: the
		// first shapes leave a verifiable tree (nodes-ready runs and
		// passes); a marker that does not parse fails the boundary
		// walk behind the kubeconfig check — the cluster is still
		// checked, with that failure, never dropped.
		readyCheck string
		readyWant  CheckStatus
	}{
		{
			name:       "older binary, no expectation",
			expected:   "",
			readyCheck: "nodes-ready",
			readyWant:  StatusPass,
		},
		{
			name:       "count no create could write",
			expected:   "0\n",
			readyCheck: "nodes-ready",
			readyWant:  StatusPass,
		},
		{
			name:       "count above create's cap",
			expected:   fmt.Sprintf("%d\n", firstNonCreatable),
			readyCheck: "nodes-ready",
			readyWant:  StatusPass,
		},
		{
			// The corruption the cap classification exists for:
			// before it, this value reached name generation and its
			// sized allocation (observed past 3.5 GB of RSS before a
			// kill) — the row must return promptly.
			name:       "huge count never reaches name generation",
			expected:   "999999999\n",
			readyCheck: "nodes-ready",
			readyWant:  StatusPass,
		},
		{
			// The shapes the reviewer wrote live: a non-integer
			// expectation must classify as corrupt state (SKIP with
			// the remedy) while the tree stays fully usable — the
			// marker's ownership never depends on the count's shape.
			name:       "count is a string",
			expected:   "two\n",
			readyCheck: "nodes-ready",
			readyWant:  StatusPass,
		},
		{
			name:       "count is a fraction",
			expected:   "2.5\n",
			readyCheck: "nodes-ready",
			readyWant:  StatusPass,
		},
		{
			name:       "count overflows int",
			expected:   "99999999999999999999999999999\n",
			readyCheck: "nodes-ready",
			readyWant:  StatusPass,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, 1, "minimal")
			prov := newFakeProvisioner("p-1")
			client := newTestClient(t, cfg, layers.NewRegistry(), prov)
			client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
			if err := client.store.WriteRecord(state.Record{
				Name: "p-1", Context: "kind-p-1", Profile: "minimal",
				Status: state.StatusComplete, InstanceID: testInstanceID,
			}); err != nil {
				t.Fatal(err)
			}
			if tc.expected != "" {
				if err := os.WriteFile(filepath.Join(client.StateRoot(), "expected-clusters"), []byte(tc.expected), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			results, err := client.Verify(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			count := findResult(results, cfg.Prefix, expectedClustersCheck)
			if count == nil {
				t.Fatalf("no cluster-count result in %v", results)
			}
			if count.Status != StatusSkipped {
				t.Fatalf("cluster-count = %s (%s), want SKIP", count.Status, count.Detail)
			}
			// The cluster that exists is still checked.
			r := findResult(results, "p-1", tc.readyCheck)
			if r == nil || r.Status != tc.readyWant {
				t.Errorf("p-1 %s = %+v, want %s — a degraded expectation must not stop the cluster's own checks",
					tc.readyCheck, r, tc.readyWant)
			}
		})
	}
	// A marker that does not parse is refused as ownership-invalid —
	// the tree fails its walks (kubeconfig FAILs), and the count
	// assertion SKIPs with the read error instead of a phantom FAIL.
	t.Run("marker does not parse", func(t *testing.T) {
		cfg := testConfig(t, 1, "minimal")
		prov := newFakeProvisioner("p-1")
		client := newTestClient(t, cfg, layers.NewRegistry(), prov)
		client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
		if err := client.store.WriteRecord(state.Record{
			Name: "p-1", Context: "kind-p-1", Profile: "minimal",
			Status: state.StatusComplete, InstanceID: testInstanceID,
		}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(client.StateRoot(), "kind-ocp-state.json"), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		results, err := client.Verify(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		count := findResult(results, cfg.Prefix, expectedClustersCheck)
		if count == nil || count.Status != StatusSkipped {
			t.Fatalf("cluster-count = %+v, want SKIP with the read error", count)
		}
		if !strings.Contains(count.Detail, "reading the persisted cluster expectation") {
			t.Errorf("cluster-count detail %q does not report the read failure — an unparseable marker must not be mistaken for old state", count.Detail)
		}
		if r := findResult(results, "p-1", "kubeconfig"); r == nil || r.Status != StatusFail {
			t.Errorf("p-1 kubeconfig = %+v, want FAIL — an unparseable marker must keep refusing the tree", r)
		}
	})
}

// The empty-inventory case the reviewer found live: with a valid
// expectation of 2 and every cluster vanished (no containers, no
// records), verify must return the count FAIL naming what is missing —
// not the bare "no clusters with prefix" structural error, which is
// reserved for a prefix create never wrote an expectation for.
func TestVerifyFailsWhenEveryClusterVanished(t *testing.T) {
	cfg := testConfig(t, 2, "minimal")
	prov := newFakeProvisioner()
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
	if _, err := client.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	vanishCluster(t, client, prov, "p-1")
	vanishCluster(t, client, prov, "p-2")

	results, err := client.Verify(context.Background(), nil)
	if err != nil {
		t.Fatalf("verify with every cluster vanished = %v, want the count FAIL as the result", err)
	}
	count := findResult(results, cfg.Prefix, expectedClustersCheck)
	if count == nil {
		t.Fatalf("no cluster-count result in %v", results)
	}
	if count.Status != StatusFail {
		t.Fatalf("cluster-count = %s (%s), want FAIL", count.Status, count.Detail)
	}
	for _, want := range []string{"p-1", "p-2", "re-run the original create command", "--count 2",
		"excludes every missing cluster", "no count create can write excludes p-1"} {
		if !strings.Contains(count.Detail, want) {
			t.Errorf("cluster-count detail %q does not name %q", count.Detail, want)
		}
	}
}

// Extra prefix-scoped clusters beyond the expectation are not a count
// FAIL: leftovers from a larger count are supported state (create never
// removes them), so the count line PASSes and the leftover is reported
// by its own per-cluster lines.
func TestVerifyCountAssertionPassesOnLeftoversBeyondTheExpectation(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	prov := newFakeProvisioner()
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
	if _, err := client.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A leftover from a larger count: still in the provisioner with a
	// complete, bound record — the state an earlier create --count 2
	// leaves behind when a later create runs with a smaller count.
	prov.mu.Lock()
	prov.clusters["p-2"] = true
	prov.mu.Unlock()
	if err := client.store.WriteRecord(state.Record{
		Name: "p-2", Context: "kind-p-2", Profile: "minimal",
		Status: state.StatusComplete, InstanceID: testInstanceID,
	}); err != nil {
		t.Fatal(err)
	}

	results, err := client.Verify(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	count := findResult(results, cfg.Prefix, expectedClustersCheck)
	if count == nil {
		t.Fatalf("no cluster-count result in %v", results)
	}
	if count.Status != StatusPass {
		t.Fatalf("cluster-count = %s (%s), want PASS — a leftover beyond the expectation is supported state, not a missing cluster",
			count.Status, count.Detail)
	}
	if strings.Contains(count.Detail, "p-2") {
		t.Errorf("cluster-count detail %q names the leftover; the count line reports the expectation only", count.Detail)
	}
	// The leftover is reported by its own lines, never by the count's.
	if r := findResult(results, "p-2", "nodes-ready"); r == nil || r.Status != StatusPass {
		t.Errorf("p-2 nodes-ready = %+v, want PASS — a leftover gets its own per-cluster lines (results: %v)", r, results)
	}
	for _, r := range results {
		if r.Cluster == "p-2" && r.Status == StatusFail {
			t.Errorf("p-2 result %+v is a FAIL; a leftover with a bound record verifies like any other cluster", r)
		}
	}
}

// The named form keeps its targeted semantics: verifying one cluster
// during recovery must not fail on a vanished sibling — the count
// assertion is a property of the whole-prefix audit form only.
func TestVerifyNamedSelectionRunsNoCountAssertion(t *testing.T) {
	cfg := testConfig(t, 2, "minimal")
	prov := newFakeProvisioner()
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
	if _, err := client.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	vanishCluster(t, client, prov, "p-2")

	results, err := client.Verify(context.Background(), []string{"p-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if r.Check == expectedClustersCheck {
			t.Errorf("cluster-count ran in the named form: %+v", r)
		}
	}
	if r := findResult(results, "p-1", "nodes-ready"); r == nil || r.Status != StatusPass {
		t.Errorf("p-1 nodes-ready = %+v, want PASS", r)
	}
}

// A pending record fails create-status but its layer checks still run (the
// run may be a legitimate same-instance retry away from complete) — as long
// as the record is bound to the live instance.
func TestVerifyPendingRecordFails(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	prov := newFakeProvisioner("p-1")
	reg := layers.NewRegistry()
	l1 := &recordingLayer{name: "one"}
	if err := reg.Register(l1); err != nil {
		t.Fatal(err)
	}
	client := newTestClient(t, cfg, reg, prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
	if err := client.store.WriteRecord(state.Record{
		Name: "p-1", Context: "kind-p-1", Profile: "full",
		Layers: []string{"one"}, Status: state.StatusPending, InstanceID: testInstanceID,
	}); err != nil {
		t.Fatal(err)
	}

	results, err := client.Verify(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var pending, layerPass, nodesPass int
	for _, r := range results {
		switch {
		case r.Check == "create-status" && r.Status == StatusFail:
			pending++
		case r.Check == "layer:one" && r.Status == StatusPass:
			layerPass++
		case r.Check == "nodes-ready" && r.Status == StatusPass:
			nodesPass++
		}
	}
	if pending != 1 {
		t.Errorf("create-status FAIL count = %d, want 1 (results: %v)", pending, results)
	}
	if layerPass != 1 || nodesPass != 1 {
		t.Errorf("layer:one PASS = %d, nodes-ready PASS = %d, want 1 each (results: %v)", layerPass, nodesPass, results)
	}
}

// An unbound record (its create run was interrupted before the identity was
// recorded) cannot be verified against the live cluster: instance-id FAILs
// and stops the layer checks, with delete and re-create as the recovery.
func TestVerifyUnboundRecordFails(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	prov := newFakeProvisioner("p-1")
	reg := layers.NewRegistry()
	l1 := &recordingLayer{name: "one"}
	if err := reg.Register(l1); err != nil {
		t.Fatal(err)
	}
	client := newTestClient(t, cfg, reg, prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
	if err := client.store.WriteRecord(state.Record{
		Name: "p-1", Context: "kind-p-1", Profile: "full",
		Layers: []string{"one"}, Status: state.StatusPending, // no InstanceID
	}); err != nil {
		t.Fatal(err)
	}

	results, err := client.Verify(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var instanceFail, layerChecks int
	for _, r := range results {
		if r.Check == "instance-id" && r.Status == StatusFail {
			instanceFail++
			if !strings.Contains(r.Detail, "delete and re-create") {
				t.Errorf("detail %q must point at the recovery path", r.Detail)
			}
		}
		if strings.HasPrefix(r.Check, "layer:") {
			layerChecks++
		}
	}
	if instanceFail != 1 {
		t.Errorf("instance-id FAIL count = %d, want 1 (results: %v)", instanceFail, results)
	}
	if layerChecks != 0 {
		t.Errorf("layer checks ran against an unverified cluster: %v", results)
	}
}

// A state record removed after verify selected the cluster (here: during
// the kubeconfig export, between selection and the record read) must FAIL
// managed-state and stop there. Selection alone must never stand in for
// the record: without the fresh read, a record that vanishes mid-verify
// left only the global checks, all PASS, and no identity question at all.
func TestVerifyRecordRemovedAfterSelectionFails(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	prov := newFakeProvisioner("p-1")
	reg := layers.NewRegistry()
	l1 := &recordingLayer{name: "one"}
	if err := reg.Register(l1); err != nil {
		t.Fatal(err)
	}
	client := newTestClient(t, cfg, reg, prov)
	cs := clientsetFor(testInstanceID, corev1.ConditionTrue)
	client.clientset = func(string) (kubernetes.Interface, error) { return cs, nil }
	if err := client.store.WriteRecord(state.Record{
		Name: "p-1", Context: "kind-p-1", Profile: "full",
		Layers: []string{"one"}, Status: state.StatusComplete, InstanceID: testInstanceID,
	}); err != nil {
		t.Fatal(err)
	}
	// The record file's path is the state package's documented layout
	// (<cluster dir>/record.json next to the kubeconfig).
	kubeconfigPath, err := client.store.KubeconfigPath("p-1")
	if err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(filepath.Dir(kubeconfigPath), "record.json")
	var removeErr error
	client.prov = &exportHookProvisioner{
		Provisioner: prov,
		hook:        func(string) { removeErr = os.Remove(recordPath) },
	}

	results, err := client.Verify(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if removeErr != nil {
		t.Fatalf("removing the record during the kubeconfig export: %v", removeErr)
	}
	var managedFail, layerChecks int
	for _, r := range results {
		switch {
		case r.Check == "managed-state" && r.Status == StatusFail:
			managedFail++
		case strings.HasPrefix(r.Check, "layer:"):
			layerChecks++
		}
	}
	if managedFail != 1 {
		t.Errorf("managed-state FAIL count = %d, want 1 (results: %v)", managedFail, results)
	}
	if layerChecks != 0 {
		t.Errorf("layer checks = %d; a cluster with no current record must stop at managed-state (results: %v)",
			layerChecks, results)
	}
	// The identity query succeeds silently — it emits no check result — so
	// "no instance-id result" proves nothing. Assert the fake client's
	// actions instead: verify must touch the API only for nodes-ready, and
	// must never read the kube-system namespace (the identity query) for a
	// cluster whose record is gone.
	var nodeLists, namespaceGets int
	for _, a := range cs.Actions() {
		switch {
		case a.GetVerb() == "list" && a.GetResource().Resource == "nodes":
			nodeLists++
		case a.GetVerb() == "get" && a.GetResource().Resource == "namespaces":
			namespaceGets++
		}
	}
	if namespaceGets != 0 {
		t.Errorf("identity queries after the record vanished = %d, want 0; verify must stop at managed-state (client actions: %v)",
			namespaceGets, cs.Actions())
	}
	if nodeLists != 1 {
		t.Errorf("node lists = %d, want exactly the one nodes-ready check (client actions: %v)",
			nodeLists, cs.Actions())
	}
}

// A record replaced after selection is verified as it is now: the
// replacement's identity is checked and its layer list and context run —
// not the ones list captured from the record the replacement overwrote.
func TestVerifyRecordReplacedAfterSelectionUsesCurrentRecord(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	prov := newFakeProvisioner("p-1")
	reg := layers.NewRegistry()
	l1 := &recordingLayer{name: "one"}
	l2 := &recordingLayer{name: "two"}
	for _, l := range []layers.Layer{l1, l2} {
		if err := reg.Register(l); err != nil {
			t.Fatal(err)
		}
	}
	client := newTestClient(t, cfg, reg, prov)
	client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
	if err := client.store.WriteRecord(state.Record{
		Name: "p-1", Context: "kind-p-1", Profile: "full",
		Layers: []string{"one"}, Status: state.StatusComplete, InstanceID: testInstanceID,
	}); err != nil {
		t.Fatal(err)
	}
	var replaceErr error
	client.prov = &exportHookProvisioner{
		Provisioner: prov,
		hook: func(string) {
			// Same instance, different context and layers: the
			// replacement must drive every record-derived check.
			replaceErr = client.store.WriteRecord(state.Record{
				Name: "p-1", Context: "kind-p-1-replacement", Profile: "full",
				Layers: []string{"two"}, Status: state.StatusComplete, InstanceID: testInstanceID,
			})
		},
	}

	results, err := client.Verify(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if replaceErr != nil {
		t.Fatalf("writing the replacement record during the kubeconfig export: %v", replaceErr)
	}
	var layerTwo, layerOne, instanceFail int
	for _, r := range results {
		switch {
		case r.Check == "layer:two" && r.Status == StatusPass:
			layerTwo++
		case r.Check == "layer:one":
			layerOne++
		case r.Check == "instance-id" && r.Status == StatusFail:
			instanceFail++
		}
	}
	if layerTwo != 1 {
		t.Errorf("layer:two PASS count = %d, want 1: the current record's layers must run (results: %v)", layerTwo, results)
	}
	if layerOne != 0 {
		t.Errorf("layer:one ran from the replaced record's layer list: %v", results)
	}
	if instanceFail != 0 {
		t.Errorf("the replacement is bound to the live instance; instance-id FAIL count = %d (results: %v)", instanceFail, results)
	}
	if len(l2.verified) != 1 || l2.verified[0].ContextName != "kind-p-1-replacement" {
		t.Errorf("layer two saw %d verify env(s); the current record's context must reach the layers", len(l2.verified))
	}
}

// A cluster deleted and re-created under the same name is a different
// instance. A complete record bound to the old instance must not be reused
// (the replacement's layers say nothing about the record), must not be
// rewritten by the rejection, and verify must fail it explicitly.
func TestSameNameReplacementRejected(t *testing.T) {
	seedRecord := state.Record{
		Name:          "p-1",
		Context:       "kind-p-1",
		Profile:       "minimal",
		KindNodeImage: "kindest/node:test",
		Status:        state.StatusComplete,
		InstanceID:    "uid-previous-instance",
	}

	t.Run("create rejects the replacement", func(t *testing.T) {
		cfg := testConfig(t, 1, "minimal")
		prov := newFakeProvisioner("p-1")
		client := newTestClient(t, cfg, layers.NewRegistry(), prov)
		// The live cluster reports a different instance than the record.
		client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
		if err := client.store.WriteRecord(seedRecord); err != nil {
			t.Fatal(err)
		}

		_, err := client.Create(context.Background())
		if err == nil || !strings.Contains(err.Error(), "different instance") {
			t.Fatalf("error = %v, want a different-instance rejection", err)
		}
		if !strings.Contains(err.Error(), "delete and re-create") {
			t.Errorf("error %q must point at the recovery path", err)
		}
		if !prov.clusters["p-1"] || len(prov.deleted) != 0 {
			t.Error("the replacement cluster must be kept, nothing deleted")
		}
		// The rejection must precede any state write: the record still
		// describes the original instance.
		rec, found, _ := client.store.Record("p-1")
		if !found || rec.InstanceID != "uid-previous-instance" || rec.Status != state.StatusComplete {
			t.Errorf("record = (%v, %v); the rejection must not rewrite it", rec, found)
		}
	})

	t.Run("create still reuses the same instance", func(t *testing.T) {
		cfg := testConfig(t, 1, "minimal")
		prov := newFakeProvisioner("p-1")
		client := newTestClient(t, cfg, layers.NewRegistry(), prov)
		client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
		rec := seedRecord
		rec.InstanceID = testInstanceID // record matches the live cluster
		if err := client.store.WriteRecord(rec); err != nil {
			t.Fatal(err)
		}

		records, err := client.Create(context.Background())
		if err != nil {
			t.Fatalf("reuse of the same instance must succeed: %v", err)
		}
		if len(prov.created) != 0 || records[0].Status != state.StatusComplete {
			t.Errorf("records = %+v, created %v, want reuse and completion", records, prov.created)
		}
	})

	t.Run("verify fails the replacement explicitly", func(t *testing.T) {
		cfg := testConfig(t, 1, "minimal")
		prov := newFakeProvisioner("p-1")
		reg := layers.NewRegistry()
		if err := reg.Register(&recordingLayer{name: "one"}); err != nil {
			t.Fatal(err)
		}
		client := newTestClient(t, cfg, reg, prov)
		client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
		rec := seedRecord
		rec.Profile = "full"
		rec.Layers = []string{"one"}
		if err := client.store.WriteRecord(rec); err != nil {
			t.Fatal(err)
		}

		results, err := client.Verify(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var instanceFail, layerChecks int
		for _, r := range results {
			if r.Check == "instance-id" && r.Status == StatusFail {
				instanceFail++
			}
			if strings.HasPrefix(r.Check, "layer:") {
				layerChecks++
			}
		}
		if instanceFail != 1 {
			t.Errorf("instance-id FAIL count = %d, want 1 (results: %v)", instanceFail, results)
		}
		if layerChecks != 0 {
			t.Errorf("layer checks ran against the replacement cluster: %v", results)
		}
	})

	t.Run("verify passes the matching instance", func(t *testing.T) {
		cfg := testConfig(t, 1, "minimal")
		prov := newFakeProvisioner("p-1")
		reg := layers.NewRegistry()
		layer := &recordingLayer{name: "one"}
		if err := reg.Register(layer); err != nil {
			t.Fatal(err)
		}
		client := newTestClient(t, cfg, reg, prov)
		client.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
		if err := client.store.WriteRecord(state.Record{
			Name: "p-1", Context: "kind-p-1", Profile: "full",
			Layers: []string{"one"}, Status: state.StatusComplete, InstanceID: testInstanceID,
		}); err != nil {
			t.Fatal(err)
		}

		results, err := client.Verify(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range results {
			if r.Check == crossClusterCheck {
				// Single-cluster prefix: the cross-cluster check
				// SKIPs with that reason, asserted below the loop.
				continue
			}
			if r.Check == expectedClustersCheck {
				// Hand-seeded state carries no persisted
				// expectation (an older binary's shape): the
				// count assertion SKIPs with that reason,
				// asserted below the loop.
				continue
			}
			if r.Status != StatusPass {
				t.Errorf("check %s = %s (%s), want PASS for the matching instance", r.Check, r.Status, r.Detail)
			}
		}
		if r := findResult(results, "p-1", crossClusterCheck); r == nil || r.Status != StatusSkipped {
			t.Errorf("cross-cluster check = %+v, want SKIP (single cluster)", r)
		}
		// The count assertion's degradation is part of the contract
		// under test here: state without an expectation is the
		// older-binary shape, and verify must SKIP it, never FAIL.
		if r := findResult(results, cfg.Prefix, expectedClustersCheck); r == nil ||
			r.Status != StatusSkipped || !strings.Contains(r.Detail, "older kind-ocp") {
			t.Errorf("cluster-count = %+v, want SKIP with the old-state reason", r)
		}
		// The layer checks are bound to the instance the identity
		// check just confirmed: a layer whose Verify mutates the
		// cluster refuses any other instance, so a kubeconfig
		// replaced between the two cannot turn its mutation onto an
		// unverified replacement.
		if len(layer.verified) != 1 || layer.verified[0].ClusterInstanceID != testInstanceID {
			t.Errorf("layer verify environments = %+v, want one carrying the validated instance %s", layer.verified, testInstanceID)
		}
	})
}

// A Client is scoped at construction: mutating the caller's Config
// afterwards must not re-scope it (a changed prefix would select other
// clusters while clearing the original prefix's state).
func TestClientSnapshotsConfig(t *testing.T) {
	cfg := testConfig(t, 2, "minimal")
	prov := newFakeProvisioner("p-1", "p-2", "other-1")
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	originalRoot := client.StateRoot()
	if err := client.store.WriteRecord(state.Record{Name: "p-1"}); err != nil {
		t.Fatal(err)
	}

	cfg.Prefix = "other"
	cfg.StateRoot = filepath.Join(t.TempDir(), "elsewhere")

	deleted, err := client.Delete(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deleted, []string{"p-1", "p-2"}) {
		t.Errorf("deleted = %v, want the original prefix's clusters [p-1 p-2]", deleted)
	}
	if !prov.clusters["other-1"] {
		t.Error("other-1 must not be touched (not a cluster of the snapshotted prefix)")
	}
	if _, err := os.Stat(originalRoot); !os.IsNotExist(err) {
		t.Error("the original prefix's state must be cleared, not the mutated copy's")
	}
}

// The public Record is the storage record's mirror: every field Create
// reports must survive the conversion, statuses included.
func TestRecordMirrorsStateRecord(t *testing.T) {
	if RecordStatusPending != state.StatusPending || RecordStatusComplete != state.StatusComplete {
		t.Fatalf("public statuses (%q, %q) must mirror the persisted ones (%q, %q)",
			RecordStatusPending, RecordStatusComplete, state.StatusPending, state.StatusComplete)
	}
	in := state.Record{
		Name:          "p-1",
		Context:       "kind-p-1",
		Profile:       "full",
		Layers:        []string{"one", "two"},
		KindNodeImage: "kindest/node:test",
		Status:        state.StatusComplete,
		CreatedAt:     time.Now().UTC().Truncate(time.Second),
		InstanceID:    "uid-x",
	}
	want := Record{
		Name:          "p-1",
		Context:       "kind-p-1",
		Profile:       "full",
		Layers:        []string{"one", "two"},
		KindNodeImage: "kindest/node:test",
		Status:        RecordStatusComplete,
		CreatedAt:     in.CreatedAt,
		InstanceID:    "uid-x",
	}
	if out := fromStateRecord(in); !reflect.DeepEqual(out, want) {
		t.Errorf("fromStateRecord mismatch:\nin  = %+v\nout = %+v", in, out)
	}
}

// kindLockProvisioner mimics the part of kind's cluster deletion that
// bit the boundary checks: kind strips the cluster's context from the
// kubeconfig at the given path, and its file locking creates a missing
// parent directory 0755 (sigs.k8s.io/kind lockFile). Delete must hand
// the provisioner a path whose directory already exists.
type kindLockProvisioner struct {
	provider.Provisioner
}

func (k *kindLockProvisioner) Delete(ctx context.Context, name string, kubeconfigPath string) error {
	if kubeconfigPath != "" {
		if err := os.MkdirAll(filepath.Dir(kubeconfigPath), 0o755); err != nil {
			return err
		}
		// kind's transient lock artifact, created mode 0 next to the
		// kubeconfig and removed again.
		lock := kubeconfigPath + ".lock"
		if err := os.WriteFile(lock, nil, 0); err != nil {
			return err
		}
		if err := os.Remove(lock); err != nil {
			return err
		}
	}
	return k.Provisioner.Delete(ctx, name, kubeconfigPath)
}

// Deleting clusters by generated name through a state root that does not
// exist yet (the foreign-root delete) must still succeed: the state dir
// is ensured 0700 before the kubeconfig path reaches the provisioner, so
// kind's 0755 parent creation never fires, and RemoveAll clears what the
// ensure created. Before the fix, kind's 0755 directory made RemoveAll
// refuse and delete failed outright.
func TestDeleteEnsuresStateDirBeforeHandingKubeconfigPath(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	prov := &kindLockProvisioner{Provisioner: newFakeProvisioner("p-1")}
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	if _, err := os.Stat(client.StateRoot()); !os.IsNotExist(err) {
		t.Fatalf("precondition: no state may exist, got %v", err)
	}

	deleted, err := client.Delete(context.Background())
	if err != nil {
		t.Fatalf("Delete with absent state and a live cluster = %v, want success", err)
	}
	if !slices.Equal(deleted, []string{"p-1"}) {
		t.Errorf("deleted = %v, want [p-1]", deleted)
	}
	// Everything the delete had to create is gone again.
	if _, err := os.Stat(client.StateRoot()); !os.IsNotExist(err) {
		t.Errorf("state dir must not linger after delete: %v", err)
	}
	// Second delete stays a no-op.
	if deleted, err := client.Delete(context.Background()); err != nil || len(deleted) != 0 {
		t.Errorf("second Delete() = (%v, %v), want (none, nil)", deleted, err)
	}
}

// An unrelated pre-existing directory under a custom state root must
// survive delete: the configurable root plus a valid prefix is enough to
// name the directory, but only a verified, marked kind-ocp state tree is
// ever removed — and the check happens before any cluster is deleted, so
// a refusal leaves everything in place.
func TestDeletePreservesUnrecognizedStateDirectory(t *testing.T) {
	cfg := testConfig(t, 2, "minimal")
	prov := newFakeProvisioner("p-1")
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)

	// A pre-existing directory exactly where kind-ocp would put its
	// state, holding someone else's files.
	foreign := filepath.Join(client.StateRoot(), "precious.txt")
	if err := os.MkdirAll(client.StateRoot(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("not kind-ocp's"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := client.Delete(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing to delete") {
		t.Fatalf("Delete() = %v, want a refusal for the unrecognized state directory", err)
	}
	if !strings.Contains(err.Error(), "kind-ocp state") {
		t.Errorf("error %q must say the directory is not recognized as kind-ocp state", err)
	}
	// The refusal gates every side effect: the directory and its
	// contents survive, and the cluster is still there.
	if b, rerr := os.ReadFile(foreign); rerr != nil || string(b) != "not kind-ocp's" {
		t.Errorf("foreign file = (%q, %v), want it preserved", b, rerr)
	}
	if !prov.clusters["p-1"] {
		t.Error("the cluster must not be deleted while the state directory is unrecognized")
	}

	// Once the foreign directory is gone, delete recovers fully and
	// stays idempotent.
	if err := os.RemoveAll(client.StateRoot()); err != nil {
		t.Fatal(err)
	}
	deleted, err := client.Delete(context.Background())
	if err != nil || !slices.Equal(deleted, []string{"p-1"}) {
		t.Fatalf("Delete after removing the foreign directory = (%v, %v), want ([p-1], nil)", deleted, err)
	}
	if deleted, err := client.Delete(context.Background()); err != nil || len(deleted) != 0 {
		t.Errorf("second Delete() = (%v, %v), want (none, nil)", deleted, err)
	}
	if _, err := os.Stat(client.StateRoot()); !os.IsNotExist(err) {
		t.Error("state dir must be gone after the legitimate delete")
	}
}

// Create must refuse to provision anything when the state root it would
// write credentials into already exists with permissive permissions —
// MkdirAll's 0700 only applies to directories it creates. The refusal
// happens while the store root is being ensured, before the first
// cluster exists, so the error names the state root, not a cluster.
func TestCreateRefusesPermissiveStateRoot(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	prov := newFakeProvisioner()
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	if err := os.MkdirAll(client.StateRoot(), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := client.Create(context.Background())
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("Create() = %v, want a mode rejection for the permissive state root", err)
	}
	if !strings.Contains(err.Error(), client.StateRoot()) {
		t.Errorf("error %q must name the state root", err)
	}
	if len(prov.created) != 0 {
		t.Errorf("provisioned = %v, want nothing (the boundary is checked before provisioning)", prov.created)
	}
}

// The cached kubeconfig is returned only after it verifies as a private,
// caller-owned regular file — a restored or tampered cache is refused,
// not handed to kubectl.
func TestKubeconfigRefusesUnsafeCachedKubeconfig(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	prov := newFakeProvisioner("p-1")
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)

	// The helper writes the on-disk state directly: the store's own
	// KubeconfigPath would preflight (and refuse) exactly the unsafe
	// files these subtests are about to plant. The state root it builds
	// is a legitimate one (marked, 0700), as a real create would leave.
	stateRoot := client.StateRoot()
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, "kind-ocp-state.json"), []byte(`{"tool":"kind-ocp"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	kubeconfigPath := filepath.Join(stateRoot, "clusters", "p-1", "kubeconfig")
	seedKubeconfig := func(mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(kubeconfigPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(kubeconfigPath, []byte("cached"), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(kubeconfigPath, mode); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("exposed cached kubeconfig is refused", func(t *testing.T) {
		seedKubeconfig(0o644)
		_, err := client.Kubeconfig(context.Background(), "p-1")
		if err == nil || !strings.Contains(err.Error(), "mode") {
			t.Fatalf("Kubeconfig(0644 cache) = %v, want a mode rejection", err)
		}
	})

	t.Run("private cached kubeconfig is returned", func(t *testing.T) {
		seedKubeconfig(0o600)
		path, err := client.Kubeconfig(context.Background(), "p-1")
		if err != nil {
			t.Fatalf("Kubeconfig(0600 cache) = %v, want success", err)
		}
		want, err := client.store.KubeconfigPath("p-1")
		if err != nil {
			t.Fatal(err)
		}
		if path != want {
			t.Errorf("path = %q, want the state path", path)
		}
		// The export path was not taken: nothing was re-exported.
		if slices.Contains(prov.exported, "p-1") {
			t.Error("a verified cache must be returned without re-exporting")
		}
	})

	t.Run("symlinked cached kubeconfig is refused", func(t *testing.T) {
		if err := os.Remove(kubeconfigPath); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.WriteFile(target, []byte("cfg"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, kubeconfigPath); err != nil {
			t.Fatal(err)
		}
		_, kcerr := client.Kubeconfig(context.Background(), "p-1")
		if kcerr == nil || !strings.Contains(kcerr.Error(), "symlink") {
			t.Fatalf("Kubeconfig(symlink cache) = %v, want a symlink rejection", kcerr)
		}
	})
}

// plantKubeconfig builds a legitimate marked state root (as a real create
// would leave) and puts an arbitrary kubeconfig entry into it. The store's
// own KubeconfigPath would preflight — and refuse — exactly the unsafe
// entries these tests are about to plant, so the seeding bypasses it.
func plantKubeconfig(t *testing.T, client *Client, name string, plant func(path string)) {
	t.Helper()
	stateRoot := client.StateRoot()
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateRoot, "kind-ocp-state.json"), []byte(`{"tool":"kind-ocp"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(stateRoot, "clusters", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "kubeconfig")
	plant(path)
}

// Create hands the provisioner a kubeconfig path kind already verified
// (the store's preflight). An unsafe cached kubeconfig must refuse the
// create before any provider call — kind merges into an existing
// destination, so provisioning would rewrite the unsafe file — and must
// not touch anything outside state on the way out.
func TestCreateRefusesUnsafeKubeconfigBeforeProvisioning(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	prov := newFakeProvisioner()
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	target := filepath.Join(t.TempDir(), "outside-state")
	if err := os.WriteFile(target, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	plantKubeconfig(t, client, "p-1", func(path string) {
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	})

	_, err := client.Create(context.Background())
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("Create() with a symlinked kubeconfig = %v, want a symlink rejection", err)
	}
	if !strings.Contains(err.Error(), "cluster p-1") {
		t.Errorf("error %q must name the cluster", err)
	}
	if len(prov.created) != 0 || len(prov.exported) != 0 {
		t.Errorf("provisioner was called (created=%v exported=%v); the refusal must gate every provider call", prov.created, prov.exported)
	}
	if b, rerr := os.ReadFile(target); rerr != nil || string(b) != "precious" {
		t.Errorf("symlink target = (%q, %v), want it preserved", b, rerr)
	}
}

// Delete hands kind the kubeconfig path so it can strip the cluster's
// context from the file; an unsafe cached kubeconfig must refuse the
// delete before kind reads and rewrites it. The refusal keeps the
// cluster and the state tree in place.
func TestDeleteRefusesUnsafeKubeconfigBeforeProviderCall(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	prov := newFakeProvisioner("p-1")
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	plantKubeconfig(t, client, "p-1", func(path string) {
		if err := os.WriteFile(path, []byte("cached"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	})

	_, err := client.Delete(context.Background())
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("Delete() with an exposed kubeconfig = %v, want a mode rejection", err)
	}
	if !prov.clusters["p-1"] {
		t.Error("the cluster must not be deleted behind the refusal")
	}
	if len(prov.deleted) != 0 {
		t.Errorf("provisioner deleted %v; the refusal must gate every provider call", prov.deleted)
	}
	// The refusal is recoverable: fixing the file lets delete proceed.
	path := filepath.Join(client.StateRoot(), "clusters", "p-1", "kubeconfig")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	deleted, err := client.Delete(context.Background())
	if err != nil || !slices.Equal(deleted, []string{"p-1"}) {
		t.Fatalf("Delete after fixing the kubeconfig = (%v, %v), want ([p-1], nil)", deleted, err)
	}
}

// Verify re-exports the kubeconfig for running clusters; an unsafe
// cached kubeconfig must fail the check without invoking the provider.
func TestVerifyReportsUnsafeKubeconfigWithoutExport(t *testing.T) {
	cfg := testConfig(t, 1, "minimal")
	prov := newFakeProvisioner("p-1")
	client := newTestClient(t, cfg, layers.NewRegistry(), prov)
	target := filepath.Join(t.TempDir(), "outside-state")
	if err := os.WriteFile(target, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	plantKubeconfig(t, client, "p-1", func(path string) {
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	})

	results, err := client.Verify(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var kubeconfigCheck *CheckResult
	for i := range results {
		if results[i].Check == "kubeconfig" {
			kubeconfigCheck = &results[i]
		}
	}
	if kubeconfigCheck == nil || kubeconfigCheck.Status != StatusFail || !strings.Contains(kubeconfigCheck.Detail, "symlink") {
		t.Fatalf("verify results = %+v, want a failing kubeconfig check naming the symlink", results)
	}
	if len(prov.exported) != 0 {
		t.Errorf("provisioner exported %v; the refusal must gate every provider call", prov.exported)
	}
	if b, rerr := os.ReadFile(target); rerr != nil || string(b) != "precious" {
		t.Errorf("symlink target = (%q, %v), want it preserved", b, rerr)
	}
}

// Delete resolves the removable state root once, before any lifecycle
// side effect, and removes exactly that tree afterwards: an ancestor
// alias re-pointed while the clusters are being deleted must not
// redirect cleanup onto a different tree — it is a refusal that leaves
// both trees alone, and pointing the alias back lets delete finish.
func TestDeleteRefusesRootSwitchMidDelete(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}

	// A legitimate state tree behind the alias.
	cfg := testConfig(t, 1, "minimal")
	cfg.StateRoot = filepath.Join(alias, "state")
	createClient := newTestClient(t, cfg, layers.NewRegistry(), newFakeProvisioner())
	createClient.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
	if _, err := createClient.Create(context.Background()); err != nil {
		t.Fatal(err)
	}

	// A second, equally legitimate tree the alias could be re-pointed
	// at: only the root-switch refusal tells them apart.
	decoyRoot := filepath.Join(dir, "decoy", "state", "p")
	if err := os.MkdirAll(decoyRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoyRoot, "kind-ocp-state.json"), []byte(`{"tool":"kind-ocp"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	hooked := &deleteHookProvisioner{
		Provisioner: newFakeProvisioner("p-1"),
		hook: func(string) {
			// Mid-delete: the alias now resolves elsewhere.
			if err := os.Remove(alias); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(filepath.Join(dir, "decoy"), alias); err != nil {
				t.Error(err)
			}
		},
	}
	deleteClient := newTestClient(t, cfg, layers.NewRegistry(), hooked)

	_, err := deleteClient.Delete(context.Background())
	if err == nil || !strings.Contains(err.Error(), "will not act on a tree it did not verify") {
		t.Fatalf("Delete() with the state root re-pointed mid-delete = %v, want the root-switch refusal", err)
	}
	// Neither tree was removed.
	if _, serr := os.Stat(createClient.StateRoot()); serr != nil {
		t.Errorf("the verified state tree must survive the refusal: %v", serr)
	}
	if _, serr := os.Stat(decoyRoot); serr != nil {
		t.Errorf("the decoy tree must survive the refusal: %v", serr)
	}

	// Pointing the alias back lets delete finish — on the tree it
	// verified, never the decoy.
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	retry := newTestClient(t, cfg, layers.NewRegistry(), newFakeProvisioner("p-1"))
	deleted, err := retry.Delete(context.Background())
	if err != nil || !slices.Equal(deleted, []string{"p-1"}) {
		t.Fatalf("Delete after pointing the alias back = (%v, %v), want ([p-1], nil)", deleted, err)
	}
	if _, serr := os.Stat(retry.StateRoot()); !os.IsNotExist(serr) {
		t.Errorf("state tree must be gone after the clean delete: %v", serr)
	}
	if _, serr := os.Stat(decoyRoot); serr != nil {
		t.Errorf("the decoy tree must never be touched: %v", serr)
	}
}

// The façade-level companion of the state-level remove-and-recreate
// regression: while delete is deleting the clusters, the verified state
// tree is renamed away and a fresh, marked tree is put in its place.
// Cleanup must refuse the replacement — reporting success would erase a
// tree this delete never verified — and both trees must survive.
func TestDeleteRefusesReplacedStateRootMidDelete(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, 1, "minimal")
	cfg.StateRoot = filepath.Join(dir, "state")
	createClient := newTestClient(t, cfg, layers.NewRegistry(), newFakeProvisioner())
	createClient.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
	if _, err := createClient.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	pinnedRoot := createClient.StateRoot()
	moved := filepath.Join(dir, "moved")

	hooked := &deleteHookProvisioner{
		Provisioner: newFakeProvisioner("p-1"),
		hook: func(string) {
			// Mid-delete: the pinned tree moves away, and an equally
			// marked tree appears at the same path.
			if err := os.Rename(pinnedRoot, moved); err != nil {
				t.Error(err)
			}
			if err := state.Open(pinnedRoot).WriteRecord(state.Record{Name: "decoy", Status: state.StatusComplete}); err != nil {
				t.Error(err)
			}
		},
	}
	deleteClient := newTestClient(t, cfg, layers.NewRegistry(), hooked)

	_, err := deleteClient.Delete(context.Background())
	if err == nil || !strings.Contains(err.Error(), "it was replaced") {
		t.Fatalf("Delete() with the state tree replaced mid-delete = %v, want the replacement refusal", err)
	}
	// The replacement at the pinned path survives the refusal, with the
	// decoy record intact, and so does the renamed original.
	if _, serr := os.Stat(filepath.Join(pinnedRoot, "clusters", "decoy", "record.json")); serr != nil {
		t.Errorf("the replacement tree must survive the refusal: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(moved, "clusters", "p-1", "record.json")); serr != nil {
		t.Errorf("the renamed original must survive the refusal: %v", serr)
	}
}

// A finished delete retires its removal (the handle is released on
// return), so state that appears at the same path afterwards is judged
// by a fresh verification, never by the retired removal: a marked tree
// is removable as new state, and a foreign one is refused on its own
// lack of a marker — not trusted for sharing the path, not rejected for
// a removal that no longer exists.
func TestDeleteJudgesStateAfterARetiredRemovalAnew(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(t, 1, "minimal")
	cfg.StateRoot = filepath.Join(dir, "state")
	createClient := newTestClient(t, cfg, layers.NewRegistry(), newFakeProvisioner())
	createClient.clientset = func(string) (kubernetes.Interface, error) { return readyClientset(), nil }
	if _, err := createClient.Create(context.Background()); err != nil {
		t.Fatal(err)
	}
	pinnedRoot := createClient.StateRoot()

	// The first delete finishes: the tree is gone and the removal is
	// retired when Delete returns.
	first := newTestClient(t, cfg, layers.NewRegistry(), newFakeProvisioner("p-1"))
	if deleted, err := first.Delete(context.Background()); err != nil || !slices.Equal(deleted, []string{"p-1"}) {
		t.Fatalf("first Delete = (%v, %v), want ([p-1], nil)", deleted, err)
	}
	if _, serr := os.Stat(pinnedRoot); !os.IsNotExist(serr) {
		t.Fatalf("the state tree must be gone: %v", serr)
	}

	t.Run("marked interloper is removed as new state", func(t *testing.T) {
		if err := state.Open(pinnedRoot).WriteRecord(state.Record{Name: "interloper", Status: state.StatusComplete}); err != nil {
			t.Fatal(err)
		}
		second := newTestClient(t, cfg, layers.NewRegistry(), newFakeProvisioner())
		if _, err := second.Delete(context.Background()); err != nil {
			t.Fatalf("Delete against a marked interloper = %v, want it removed as new state", err)
		}
		if _, serr := os.Stat(pinnedRoot); !os.IsNotExist(serr) {
			t.Errorf("the marked interloper must be gone: %v", serr)
		}
	})

	t.Run("foreign interloper is refused on its own", func(t *testing.T) {
		if err := os.MkdirAll(filepath.Join(pinnedRoot, "clusters"), 0o700); err != nil {
			t.Fatal(err)
		}
		foreign := filepath.Join(pinnedRoot, "important.txt")
		if err := os.WriteFile(foreign, []byte("keep me"), 0o600); err != nil {
			t.Fatal(err)
		}
		second := newTestClient(t, cfg, layers.NewRegistry(), newFakeProvisioner())
		_, err := second.Delete(context.Background())
		if err == nil || !strings.Contains(err.Error(), "refusing to delete") {
			t.Fatalf("Delete against a foreign interloper = %v, want the unproven-tree refusal", err)
		}
		if b, rerr := os.ReadFile(foreign); rerr != nil || string(b) != "keep me" {
			t.Errorf("foreign file = (%q, %v), want it preserved", b, rerr)
		}
	})
}

// The re-export must return the kubeconfig it exported: when the state
// root moves underneath the export (an ancestor alias re-pointed), the
// post-check verifies a different tree's pre-existing cache — not what
// was just written — and the answer is a refusal naming both trees,
// never the other tree's file. There is no repair: re-exporting into
// the new tree would follow the moved root.
func TestKubeconfigExportRefusesStateRootMovedMidExport(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, 1, "minimal")
	cfg.StateRoot = filepath.Join(alias, "state")

	// The decoy the alias is re-pointed at mid-export: a legitimate
	// state tree with its own valid cached kubeconfig.
	decoyCluster := filepath.Join(dir, "decoy", "state", "p", "clusters", "p-1")
	if err := os.MkdirAll(decoyCluster, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "decoy", "state", "p", "kind-ocp-state.json"), []byte(`{"tool":"kind-ocp"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(decoyCluster, "kubeconfig"), []byte("decoy"), 0o600); err != nil {
		t.Fatal(err)
	}

	hooked := &exportHookProvisioner{
		Provisioner: newFakeProvisioner("p-1"),
		hook: func(string) {
			if err := os.Remove(alias); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(filepath.Join(dir, "decoy"), alias); err != nil {
				t.Error(err)
			}
		},
	}
	client := newTestClient(t, cfg, layers.NewRegistry(), hooked)

	path, err := client.Kubeconfig(context.Background(), "p-1")
	if err == nil || !strings.Contains(err.Error(), "moved during the export") {
		t.Fatalf("Kubeconfig with the state root re-pointed mid-export = %v, want the moved-root refusal", err)
	}
	if path != "" {
		t.Errorf("returned path = %s, want none (the refusal returns no kubeconfig)", path)
	}
	// The decoy's cache was never returned — or touched.
	decoyBytes, rerr := os.ReadFile(filepath.Join(decoyCluster, "kubeconfig"))
	if rerr != nil || string(decoyBytes) != "decoy" {
		t.Errorf("decoy kubeconfig = (%q, %v), want it untouched", decoyBytes, rerr)
	}
	// The export itself did happen (into the tree that was verified
	// when it started), and there was exactly one — no repair.
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	exported := filepath.Join(resolvedReal, "state", "p", "clusters", "p-1", "kubeconfig")
	if _, serr := os.Stat(exported); serr != nil {
		t.Errorf("the exported kubeconfig must exist at %s: %v", exported, serr)
	}
	if len(hooked.exported) != 1 {
		t.Errorf("exports = %v, want exactly one (no re-export after the refusal)", hooked.exported)
	}

	// Pointing the alias back and re-running returns the exported
	// kubeconfig — the refusal is recoverable, not a dead end.
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	again, err := client.Kubeconfig(context.Background(), "p-1")
	if err != nil {
		t.Fatalf("Kubeconfig after pointing the alias back = %v, want success", err)
	}
	if again != exported {
		t.Errorf("returned path = %s, want the verified export %s", again, exported)
	}
}

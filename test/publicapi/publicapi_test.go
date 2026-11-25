// Package publicapi holds a smoke test of kind-ocp's public API surface,
// written the way an external Go program would use it: only the public
// packages (config, kindocp, layers, provider), a custom layer, and a
// custom provisioner. It needs no Docker.
package publicapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stackrox/kind-ocp/config"
	"github.com/stackrox/kind-ocp/kindocp"
	"github.com/stackrox/kind-ocp/layers"
	"github.com/stackrox/kind-ocp/provider"
)

// smokeProvisioner is a caller-supplied provisioner: an in-memory cluster
// provider whose kubeconfig points at a fake API server (a real HTTP server
// speaking enough of the nodes API for client-go).
type smokeProvisioner struct {
	clusters map[string]bool
	api      *fakeAPIServer
	loaded   []string // "cluster ref" per LoadImage call, in order
}

func (p *smokeProvisioner) Create(_ context.Context, opts provider.CreateOptions) (bool, error) {
	if p.clusters[opts.Name] {
		return false, nil
	}
	p.clusters[opts.Name] = true
	if err := os.MkdirAll(filepath.Dir(opts.KubeconfigPath), 0o700); err != nil {
		return false, err
	}
	return true, os.WriteFile(opts.KubeconfigPath, []byte(p.api.kubeconfig()), 0o600)
}

func (p *smokeProvisioner) Delete(_ context.Context, name string, _ string) error {
	delete(p.clusters, name)
	return nil
}

func (p *smokeProvisioner) List(_ context.Context) ([]string, error) {
	var out []string
	for n := range p.clusters {
		out = append(out, n)
	}
	slices.Sort(out)
	return out, nil
}

func (p *smokeProvisioner) ExportKubeconfig(_ context.Context, name string, path string) error {
	if !p.clusters[name] {
		return errors.New("cluster does not exist")
	}
	return os.WriteFile(path, []byte(p.api.kubeconfig()), 0o600)
}

func (p *smokeProvisioner) LoadImage(_ context.Context, cluster string, imageRef string) error {
	if !p.clusters[cluster] {
		return fmt.Errorf("cluster %s does not exist", cluster)
	}
	p.loaded = append(p.loaded, cluster+" "+imageRef)
	return nil
}

// labeledLayer is a caller-supplied layer: it counts Apply and Verify
// calls, records the services the framework hands it — the cache
// directory bound to the layer, the image loader bound to the cluster —
// the same services the builtin layers get.
type labeledLayer struct {
	name          string
	applied       []string
	cacheDir      string
	loadedImage   string
	verified      []string
	verifyCacheFn bool // whether Verify's environment carried a cache service
	verifyLoadFn  bool // whether Verify's environment carried an image loader
}

func (l *labeledLayer) Name() string { return l.name }

func (l *labeledLayer) Apply(ctx context.Context, env layers.Env) error {
	l.applied = append(l.applied, env.ClusterName)
	// No layer name and no path components: the callback is bound to
	// this layer, and the empty pin is this layer's plain cache
	// directory.
	dir, err := env.CacheDir("")
	if err != nil {
		return err
	}
	l.cacheDir = dir
	// The loader is bound to the cluster being applied to: loading the
	// same reference through it must reach that cluster.
	if err := env.LoadImage(ctx, "smoke.example/image:v1"); err != nil {
		return err
	}
	l.loadedImage = "smoke.example/image:v1"
	return nil
}

func (l *labeledLayer) Verify(_ context.Context, env layers.Env) error {
	l.verified = append(l.verified, env.ClusterName)
	l.verifyCacheFn = env.CacheDir != nil
	l.verifyLoadFn = env.LoadImage != nil
	return nil
}

// lifecycleOnlySmoke implements exactly the required Provisioner
// lifecycle around a capable inner provider — the embedding whose
// runtime cannot import images, plugging in without stubbing anything.
type lifecycleOnlySmoke struct {
	inner *smokeProvisioner
}

func (l *lifecycleOnlySmoke) Create(ctx context.Context, opts provider.CreateOptions) (bool, error) {
	return l.inner.Create(ctx, opts)
}

func (l *lifecycleOnlySmoke) Delete(ctx context.Context, name string, kubeconfigPath string) error {
	return l.inner.Delete(ctx, name, kubeconfigPath)
}

func (l *lifecycleOnlySmoke) List(ctx context.Context) ([]string, error) {
	return l.inner.List(ctx)
}

func (l *lifecycleOnlySmoke) ExportKubeconfig(ctx context.Context, name string, path string) error {
	return l.inner.ExportKubeconfig(ctx, name, path)
}

// imageObserverLayer records whether the framework handed it an
// image-loading service, without needing one.
type imageObserverLayer struct {
	name            string
	applySawLoader  bool
	verifySawLoader bool
}

func (l *imageObserverLayer) Name() string { return l.name }

func (l *imageObserverLayer) Apply(_ context.Context, env layers.Env) error {
	l.applySawLoader = env.LoadImage != nil
	return nil
}

func (l *imageObserverLayer) Verify(_ context.Context, env layers.Env) error {
	l.verifySawLoader = env.LoadImage != nil
	return nil
}

// TestPublicAPILifecycleOnlyProvisioner pins the pluggability contract:
// an embedding whose provisioner implements only the required lifecycle
// still creates, verifies, and deletes clusters. Layers that need no
// images run unchanged — they simply see no image-loading service, in
// Apply and Verify alike, and a layer that needs one refuses to run.
func TestPublicAPILifecycleOnlyProvisioner(t *testing.T) {
	ctx := context.Background()
	prov := &smokeProvisioner{clusters: map[string]bool{}, api: newFakeAPIServer(t)}
	observer := &imageObserverLayer{name: "acme-observe"}

	reg := layers.NewRegistry()
	if err := reg.Register(observer); err != nil {
		t.Fatal(err)
	}

	client, err := kindocp.New(kindocp.Options{
		Config:      smokeConfig(t),
		Registry:    reg,
		Provisioner: &lifecycleOnlySmoke{inner: prov},
		Log:         slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	records, err := client.Create(ctx)
	if err != nil {
		t.Fatalf("create with a lifecycle-only provisioner: %v", err)
	}
	if len(records) != 1 || records[0].Status != kindocp.RecordStatusComplete {
		t.Fatalf("records = %+v, want one complete cluster", records)
	}
	if observer.applySawLoader {
		t.Error("Apply's environment carried an image loader; a lifecycle-only provisioner provides none")
	}
	if len(prov.loaded) != 0 {
		t.Errorf("provisioner loads = %v, want none (the capability is absent)", prov.loaded)
	}

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, r := range results {
		if r.Check == "cross-cluster-nodeport" {
			continue // asserted explicitly below: single cluster → SKIP
		}
		if r.Status != kindocp.StatusPass {
			t.Errorf("check %s/%s = %s (%s), want PASS", r.Cluster, r.Check, r.Status, r.Detail)
		}
	}
	// The cross-cluster check skips on a single-cluster prefix: there
	// is no second cluster to reach, which is a reason, not a failure.
	skip := findCheck(results, "kind-ocp-1", "cross-cluster-nodeport")
	if skip == nil || skip.Status != kindocp.StatusSkipped ||
		!strings.Contains(skip.Detail, "needs at least two clusters") {
		t.Errorf("cross-cluster-nodeport = %+v, want SKIP with the single-cluster reason", skip)
	}
	if observer.verifySawLoader {
		t.Error("Verify's environment carried an image loader; no environment may load images in Verify")
	}
}

// findCheck returns one cluster's check result, or nil when absent.
func findCheck(results []kindocp.CheckResult, cluster, check string) *kindocp.CheckResult {
	for i := range results {
		if results[i].Cluster == cluster && results[i].Check == check {
			return &results[i]
		}
	}
	return nil
}

// smokeConfig resolves configuration exactly as the CLI does, then points
// the state root at a scratch directory.
func smokeConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Resolve(config.Flags{}, config.Env{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Count = 1
	cfg.StateRoot = filepath.Join(t.TempDir(), "state")
	return cfg
}

// fakeAPIServer serves the endpoints the create path needs. The kubeconfig
// it hands out points client-go at it over plain HTTP, so the smoke test
// exercises the real client wiring without Docker.
type fakeAPIServer struct {
	server *httptest.Server
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	api := &fakeAPIServer{}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/nodes":
			_, _ = w.Write([]byte(`{
  "kind": "NodeList", "apiVersion": "v1",
  "items": [{"metadata": {"name": "smoke-control-plane"},
             "status": {"conditions": [{"type": "Ready", "status": "True"}]}}]
}`))
		case "/api/v1/namespaces/kube-system":
			_, _ = w.Write([]byte(`{
  "kind": "Namespace", "apiVersion": "v1",
  "metadata": {"name": "kube-system", "uid": "smoke-instance-1"}
}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.server.Close)
	return api
}

func (a *fakeAPIServer) kubeconfig() string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: smoke
  cluster:
    server: %s
contexts:
- name: smoke
  context:
    cluster: smoke
    user: smoke
current-context: smoke
users:
- name: smoke
  user: {}
`, a.server.URL)
}

// TestPublicAPILifecycle drives create, list, verify, and delete through the
// public API with caller-supplied extensions, the way an embedding program
// would.
func TestPublicAPILifecycle(t *testing.T) {
	ctx := context.Background()
	prov := &smokeProvisioner{clusters: map[string]bool{}, api: newFakeAPIServer(t)}
	labeler := &labeledLayer{name: "acme-labels"}

	reg := layers.NewRegistry()
	if err := reg.Register(labeler); err != nil {
		t.Fatal(err)
	}

	client, err := kindocp.New(kindocp.Options{
		Config:      smokeConfig(t),
		Registry:    reg,
		Provisioner: prov,
		Log:         slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}

	records, err := client.Create(ctx)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(records) != 1 || records[0].Name != "kind-ocp-1" ||
		records[0].Status != kindocp.RecordStatusComplete ||
		records[0].InstanceID != "smoke-instance-1" ||
		!slices.Equal(records[0].Layers, []string{"acme-labels"}) {
		t.Fatalf("records = %+v, want one complete kind-ocp-1 with the custom layer and the cluster's instance ID", records)
	}
	if !slices.Equal(labeler.applied, []string{"kind-ocp-1"}) {
		t.Errorf("layer applied to %v, want [kind-ocp-1]", labeler.applied)
	}
	// The framework supplied the custom layer's cache directory — bound
	// to the layer, not named by it — inside the state root's verified
	// tree (the resolved path may differ from the configured state root
	// on platforms with symlinked temp dirs).
	if !strings.HasSuffix(labeler.cacheDir, filepath.Join("cache", "acme-labels")) {
		t.Errorf("layer cache dir = %q, want it under cache/acme-labels (framework-bound)", labeler.cacheDir)
	}
	if info, err := os.Stat(labeler.cacheDir); err != nil || !info.IsDir() {
		t.Errorf("layer cache dir %s: %v", labeler.cacheDir, err)
	}
	// The framework supplied the image-loading service too, bound to the
	// cluster being applied to: the layer's load reached the
	// provisioner for that cluster and that reference.
	if labeler.loadedImage == "" {
		t.Error("the layer could not load its image; Apply gets an image-loading service")
	}
	if !slices.Equal(prov.loaded, []string{"kind-ocp-1 smoke.example/image:v1"}) {
		t.Errorf("provisioner loads = %v, want the layer's image loaded into kind-ocp-1", prov.loaded)
	}

	infos, err := client.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(infos) != 1 || !infos[0].Managed || infos[0].State != "running" {
		t.Fatalf("list = %+v, want one running managed cluster", infos)
	}

	results, err := client.Verify(ctx, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, r := range results {
		if r.Check == "cross-cluster-nodeport" {
			continue // asserted explicitly below: single cluster → SKIP
		}
		if r.Status != kindocp.StatusPass {
			t.Errorf("check %s/%s = %s (%s), want PASS", r.Cluster, r.Check, r.Status, r.Detail)
		}
	}
	// The cross-cluster check skips on a single-cluster prefix: there
	// is no second cluster to reach, which is a reason, not a failure.
	skip := findCheck(results, "kind-ocp-1", "cross-cluster-nodeport")
	if skip == nil || skip.Status != kindocp.StatusSkipped ||
		!strings.Contains(skip.Detail, "needs at least two clusters") {
		t.Errorf("cross-cluster-nodeport = %+v, want SKIP with the single-cluster reason", skip)
	}
	// The custom layer's Verify ran — the framework runs every
	// enabled layer's check, caller-supplied ones included — and its
	// environment carried no cache-directory service: only Apply gets
	// one, because calling it creates a directory and Verify leaves no
	// state behind.
	if !slices.Equal(labeler.verified, []string{"kind-ocp-1"}) {
		t.Errorf("layer verified %v, want [kind-ocp-1]", labeler.verified)
	}
	if labeler.verifyCacheFn {
		t.Error("Verify's environment carried a cache-directory service; only Apply gets one (Verify leaves no state behind)")
	}
	if labeler.verifyLoadFn {
		t.Error("Verify's environment carried an image-loading service; only Apply gets one (Verify leaves no state behind)")
	}

	kcPath, err := client.Kubeconfig(ctx, "kind-ocp-1")
	if err != nil {
		t.Fatalf("kubeconfig: %v", err)
	}
	if info, err := os.Stat(kcPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("kubeconfig %s perms: (%v, %v), want 0600", kcPath, info, err)
	}

	deleted, err := client.Delete(ctx)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !slices.Equal(deleted, []string{"kind-ocp-1"}) {
		t.Errorf("deleted = %v, want [kind-ocp-1]", deleted)
	}
	if _, err := os.Stat(client.StateRoot()); !os.IsNotExist(err) {
		t.Errorf("state root still exists after delete: %v", err)
	}
}

// The façade must reject library-constructed configurations that bypass
// config.Resolve with an unsafe prefix.
func TestPublicAPIRejectsUnsafePrefix(t *testing.T) {
	cfg, err := config.Resolve(config.Flags{}, config.Env{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Prefix = "../escape"
	cfg.StateRoot = filepath.Join(t.TempDir(), "state")
	_, err = kindocp.New(kindocp.Options{
		Config:      cfg,
		Provisioner: &smokeProvisioner{clusters: map[string]bool{}, api: newFakeAPIServer(t)},
	})
	if err == nil {
		t.Fatal("expected New to reject an unsafe prefix")
	}
}

// The client is scoped at construction: mutating the caller's Config after
// New must not re-scope a live client (a changed prefix would select other
// clusters while clearing the original prefix's state).
func TestPublicAPIClientSnapshotsConfig(t *testing.T) {
	ctx := context.Background()
	cfg := smokeConfig(t)
	cfg.Prefix = "snap"
	prov := &smokeProvisioner{clusters: map[string]bool{}, api: newFakeAPIServer(t)}
	// A caller-supplied registry keeps this test about snapshot
	// semantics: the builtin registry's real layers (olm) would try to
	// reach the network and the fake API server's discovery.
	reg := layers.NewRegistry()
	if err := reg.Register(&labeledLayer{name: "snap-labels"}); err != nil {
		t.Fatal(err)
	}
	client, err := kindocp.New(kindocp.Options{
		Config:      cfg,
		Registry:    reg,
		Provisioner: prov,
		Log:         slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Create(ctx); err != nil {
		t.Fatalf("create: %v", err)
	}

	// The caller mutates its Config after construction.
	cfg.Prefix = "hijack"
	cfg.StateRoot = filepath.Join(t.TempDir(), "elsewhere")

	infos, err := client.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(infos) != 1 || infos[0].Name != "snap-1" {
		t.Fatalf("list = %+v, want the snapshotted prefix's snap-1", infos)
	}
	deleted, err := client.Delete(ctx)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !slices.Equal(deleted, []string{"snap-1"}) {
		t.Errorf("deleted = %v, want [snap-1] (the snapshotted prefix)", deleted)
	}
	if len(prov.clusters) != 0 {
		t.Errorf("clusters left behind: %v", prov.clusters)
	}
}

// A config file with per-layer settings must reach the layer through
// layers.Env.DecodeSettings.
func TestPublicAPILayerSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	content := "count: 1\nlayers:\n  acme-labels:\n    region: eu\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	count := 1
	cfg, err := config.Resolve(config.Flags{ConfigPath: &path, Count: &count}, config.Env{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.StateRoot = filepath.Join(dir, "state")

	consumer := &settingsLayer{name: "acme-labels"}
	reg := layers.NewRegistry()
	if err := reg.Register(consumer); err != nil {
		t.Fatal(err)
	}

	client, err := kindocp.New(kindocp.Options{
		Config:      cfg,
		Registry:    reg,
		Provisioner: &smokeProvisioner{clusters: map[string]bool{}, api: newFakeAPIServer(t)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Create(context.Background()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if consumer.region != "eu" {
		t.Errorf("layer settings region = %q, want the file value eu", consumer.region)
	}
}

// settingsLayer decodes its own settings section, defaults first, the way
// real layers consume configuration.
type settingsLayer struct {
	name   string
	region string
}

func (l *settingsLayer) Name() string { return l.name }

func (l *settingsLayer) Apply(_ context.Context, env layers.Env) error {
	type settings struct {
		Region string `yaml:"region"`
	}
	s := settings{Region: "us"} // the layer's default
	if err := env.DecodeSettings(l.name, &s); err != nil {
		return err
	}
	l.region = s.Region
	return nil
}

func (l *settingsLayer) Verify(_ context.Context, _ layers.Env) error { return nil }

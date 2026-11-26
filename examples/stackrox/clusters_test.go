package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stackrox/kind-ocp/config"
	"github.com/stackrox/kind-ocp/kindocp"
	"github.com/stackrox/kind-ocp/provider"
)

func mustConfig(t *testing.T, prefix, stateRoot string) *config.Config {
	t.Helper()
	flags := config.Flags{}
	if prefix != "" {
		flags.Prefix = &prefix
	}
	if stateRoot != "" {
		flags.StateRoot = &stateRoot
	}
	cfg, err := config.Resolve(flags, config.Env{})
	if err != nil {
		t.Fatalf("resolving the test config: %v", err)
	}
	return cfg
}

func TestResolveConfigUsesKindOCPDefaults(t *testing.T) {
	cfg, err := resolveConfig(new(string), new(string), new(int))
	if err != nil {
		t.Fatalf("resolveConfig with no flags = %v", err)
	}
	if cfg.Prefix != config.DefaultPrefix || cfg.Count != config.DefaultCount || cfg.Profile != config.DefaultProfile {
		t.Errorf("defaults = %q/%d/%q, want kind-ocp's own", cfg.Prefix, cfg.Count, cfg.Profile)
	}
	if cfg.KindNodeImage != config.DefaultKindNodeImage {
		t.Error("the example does not run on the pinned node image")
	}
	if cfg.StateRoot == "" {
		t.Error("the state root did not resolve")
	}
}

func TestResolveConfigAppliesOverrides(t *testing.T) {
	prefix, stateRoot, count := "stackrox-demo", "/tmp/example-state", intPtr(3)
	cfg, err := resolveConfig(&prefix, &stateRoot, count)
	if err != nil {
		t.Fatalf("resolveConfig = %v", err)
	}
	if cfg.Prefix != "stackrox-demo" || cfg.Count != 3 || cfg.StateRoot != "/tmp/example-state" {
		t.Errorf("overrides not applied: %q/%d/%q", cfg.Prefix, cfg.Count, cfg.StateRoot)
	}
}

func intPtr(i int) *int { return &i }

// mustWorkConfig resolves a config whose state root sits under dir, so
// the workspace openWorkDir derives from it lands under dir too and the
// test cleans up with the temp dir. The state root is created the way
// ensureClusters leaves it — openWorkDir resolves it before deriving the
// workspace, and a path that does not exist cannot be resolved.
func mustWorkConfig(t *testing.T, dir, prefix string) *config.Config {
	t.Helper()
	cfg := mustConfig(t, prefix, filepath.Join(dir, "kind-ocp"))
	if err := os.MkdirAll(cfg.StateRoot, 0o700); err != nil {
		t.Fatalf("creating the state root %s: %v", cfg.StateRoot, err)
	}
	return cfg
}

// workDirPath is the workspace path a test expects for a state root
// named kind-ocp under dir: the resolved state base's sibling, named
// after it with the work directory suffix.
func workDirPath(dir, prefix string) string {
	return filepath.Join(dir, "kind-ocp"+workDirSuffix, prefix)
}

func TestOpenWorkDirCreatesAPrivatePerPrefixDirectory(t *testing.T) {
	base := t.TempDir()
	cfg := mustWorkConfig(t, base, "quote-test")
	dir, err := openWorkDir(cfg)
	if err != nil {
		t.Fatalf("openWorkDir = %v", err)
	}
	if want := workDirPath(base, "quote-test"); dir != want {
		// The walk resolves ancestor aliases (macOS temp dirs live
		// under the /var symlink), so compare against the resolved
		// form of the expected path — the run writes through exactly
		// what the walk verified.
		resolved, err := filepath.EvalSymlinks(want)
		if err != nil || dir != resolved {
			t.Errorf("the work dir %q is not the per-prefix sibling of the state root, want %q (resolved %q)", dir, want, resolved)
		}
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("the work dir mode = (%v, %v), want 0700", info, err)
	}
	// The second open adopts the directory the first run left behind —
	// the admin password is reused across runs, so this is the path a
	// re-run takes.
	again, err := openWorkDir(cfg)
	if err != nil || again != dir {
		t.Errorf("re-open = (%q, %v), want (%q, nil)", again, err, dir)
	}
}

func TestOpenWorkDirRefusesAPermissiveExistingDirectory(t *testing.T) {
	base := t.TempDir()
	cfg := mustWorkConfig(t, base, "permissive-test")
	dir := workDirPath(base, "permissive-test")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := openWorkDir(cfg); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("openWorkDir = %v, want a mode rejection (the directory holds the admin password)", err)
	}
}

func TestOpenWorkDirRefusesASymlinkedWorkspace(t *testing.T) {
	base := t.TempDir()
	cfg := mustWorkConfig(t, base, "symlinked-test")
	target := filepath.Join(base, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Join(base, "kind-ocp"+workDirSuffix)
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	// Even a private, caller-owned target: the managed region refuses
	// the symlink itself, so nothing can redirect the workspace.
	if err := os.Symlink(target, filepath.Join(parent, "symlinked-test")); err != nil {
		t.Fatal(err)
	}
	if _, err := openWorkDir(cfg); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("openWorkDir = %v, want a symlink rejection", err)
	}
}

// The workspace's ancestors are the state root's ancestors — the
// workspace is derived from the *resolved* state base, so the chain is
// verified during that resolution, before the workspace is derived at
// all. A directory another local user can write (group- or
// world-writable, no sticky bit) can substitute everything below it,
// so a substitutable ancestor refuses the run at the resolution step.
func TestOpenWorkDirRefusesASubstitutableAncestor(t *testing.T) {
	base := t.TempDir()
	cfg := mustWorkConfig(t, base, "substituted-test")
	if err := os.Chmod(base, 0o777); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(base, 0o700) }()
	if _, err := openWorkDir(cfg); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("openWorkDir = %v, want a substitutable-ancestor rejection", err)
	}
}

// The colliding basename: a state root that ends in the workspace's own
// name used to make the workspace exactly the per-prefix state
// directory, and `kind-ocp delete` would have removed the credentials
// with the state. The workspace is derived from the resolved state base,
// so it is a distinct sibling whatever the state root is called.
func TestOpenWorkDirStaysOutsideAStateRootNamedLikeTheWorkspace(t *testing.T) {
	base := t.TempDir()
	// A state root whose basename is the workspace name the default
	// state root derives (kind-ocp-stackrox-example).
	stateRoot := filepath.Join(base, "kind-ocp"+workDirSuffix)
	cfg := mustConfig(t, "collide-test", stateRoot)
	if err := os.MkdirAll(stateRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	dir, err := openWorkDir(cfg)
	if err != nil {
		t.Fatalf("openWorkDir with a colliding state-root basename = %v", err)
	}
	resolvedState, err := filepath.EvalSymlinks(stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	if dir == resolvedState || strings.HasPrefix(dir, resolvedState+string(filepath.Separator)) {
		t.Fatalf("the work dir %q resolves inside the state root %q — kind-ocp delete would remove the credentials with the state", dir, resolvedState)
	}
	want := filepath.Join(base, "kind-ocp"+workDirSuffix+workDirSuffix, "collide-test")
	if resolved, err := filepath.EvalSymlinks(want); err != nil || dir != resolved {
		t.Errorf("the work dir %q is not the distinct sibling %q (resolved %q, %v)", dir, want, resolved, err)
	}
}

// A workspace base that aliases into the state tree: the derivation
// itself cannot nest the trees (the workspace is a sibling of the
// resolved state base), but a symlink at the workspace's name could
// point anywhere. The derived chain follows no aliases, so the symlink
// is refused at the base whatever its target — and the refusal leaves
// no directory behind inside the state tree.
func TestOpenWorkDirRefusesAWorkspaceAliasedIntoTheStateTree(t *testing.T) {
	base := t.TempDir()
	cfg := mustWorkConfig(t, base, "alias-test")
	resolvedState, err := filepath.EvalSymlinks(filepath.Join(base, "kind-ocp"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(resolvedState, filepath.Join(base, "kind-ocp"+workDirSuffix)); err != nil {
		t.Fatal(err)
	}

	if _, err := openWorkDir(cfg); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("openWorkDir through a workspace aliased into the state tree = %v, want a symlink rejection", err)
	}
	// The refusal happened before any write: nothing was created inside
	// the state tree.
	if entries, err := os.ReadDir(resolvedState); err != nil || len(entries) != 0 {
		t.Errorf("the state root holds %v (%v) after the refusal, want nothing — refusals do not write", entries, err)
	}
}

// The other direction: an alias at the workspace base that points above
// the state root would make the per-prefix workspace contain the state
// tree — kind-ocp's removals would then sweep through the credential
// directory's own tree. The derivation alone cannot produce this shape
// (the workspace base is a sibling of the resolved state root), so what
// produces it is an alias, and the derived chain refuses the alias
// itself rather than comparing where it resolves.
func TestOpenWorkDirRefusesAStateRootInsideTheWorkspace(t *testing.T) {
	base := t.TempDir()
	resolved, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	// A state root at <base>/nested/kind-ocp …
	if err := os.MkdirAll(filepath.Join(base, "nested", "kind-ocp"), 0o700); err != nil {
		t.Fatal(err)
	}
	// … whose derived workspace base is an alias to the temp dir itself:
	// the per-prefix workspace would then resolve to <base>/nested, which
	// contains the state root.
	if err := os.Symlink(resolved, filepath.Join(base, "nested", "kind-ocp"+workDirSuffix)); err != nil {
		t.Fatal(err)
	}
	cfg := mustConfig(t, "nested", filepath.Join(base, "nested", "kind-ocp"))

	if _, err := openWorkDir(cfg); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("openWorkDir with the state root inside the workspace = %v, want a symlink rejection", err)
	}
}

// caseInsensitiveVolume reports whether dir's filesystem resolves names
// that differ only by case to the same entry — macOS's default APFS and
// HFS+ do. On such a volume a state base named State and a path naming
// state are one directory while remaining two strings, which is exactly
// the shape no lexical comparison can tell apart.
func caseInsensitiveVolume(t *testing.T, dir string) bool {
	t.Helper()
	if err := os.Mkdir(filepath.Join(dir, "CaseProbe"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := os.Lstat(filepath.Join(dir, "caseprobe"))
	return err == nil
}

// The spelling collision on a case-insensitive volume: an alias planted
// at the derived workspace base that names the state root in the other
// case points the credentials at the very tree `kind-ocp delete`
// removes, while every lexical comparison of the two spellings says
// they differ. The derived chain never follows the alias — it is
// refused at the base before anything is created — so the case
// collision never gets the chance to matter. The fixture only denotes
// one directory on a case-insensitive volume; elsewhere it is a
// dangling alias, and the same refusal is already pinned by the
// exact-spelling tests above.
func TestOpenWorkDirRefusesAWorkspaceAliasedByCase(t *testing.T) {
	base := t.TempDir()
	if !caseInsensitiveVolume(t, base) {
		t.Skip("the volume resolves names case-sensitively; a case-colliding alias cannot denote one directory here")
	}
	cfg := mustConfig(t, "case-test", filepath.Join(base, "State"))
	if err := os.MkdirAll(cfg.StateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	// The workspace base names the state root in the other case: on
	// this volume the alias resolves into the state tree itself.
	if err := os.Symlink(filepath.Join(base, "state"), filepath.Join(base, "State"+workDirSuffix)); err != nil {
		t.Fatal(err)
	}

	if _, err := openWorkDir(cfg); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("openWorkDir through a case-colliding workspace alias = %v, want a symlink rejection", err)
	}
	// The refusal wrote nothing into the state tree the alias named.
	if entries, err := os.ReadDir(cfg.StateRoot); err != nil || len(entries) != 0 {
		t.Errorf("the state root holds %v (%v) after the refusal, want nothing — refusals do not write", entries, err)
	}
}

// The pre-check's candidate must be the workspace the run actually
// uses: workspaceBase, without creating anything, names (by directory
// identity) the base openWorkDir derives and rewrites. The override
// refusal rides on this agreement.
func TestWorkspaceBaseNamesTheDerivedWorkspace(t *testing.T) {
	base := t.TempDir()
	cfg := mustWorkConfig(t, base, "base-test")
	work, err := openWorkDir(cfg)
	if err != nil {
		t.Fatal(err)
	}
	candidate := workspaceBase(cfg.StateRoot)
	got, err := os.Stat(candidate)
	if err != nil {
		t.Fatalf("workspaceBase(%q) = %q, which does not exist: %v", cfg.StateRoot, candidate, err)
	}
	want, err := os.Stat(filepath.Dir(work))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(got, want) {
		t.Errorf("workspaceBase(%q) = %q, want the base of the derived workspace %q", cfg.StateRoot, candidate, filepath.Dir(work))
	}
}

// stubProvisioner keeps kindocp.New Docker-free: nothing is
// provisioned, the client is only asked where its state tree is.
type stubProvisioner struct{}

func (stubProvisioner) Create(context.Context, provider.CreateOptions) (bool, error) {
	return false, nil
}
func (stubProvisioner) Delete(context.Context, string, string) error           { return nil }
func (stubProvisioner) List(context.Context) ([]string, error)                 { return nil, nil }
func (stubProvisioner) ExportKubeconfig(context.Context, string, string) error { return nil }

// The state-tree candidate must name the directory kind-ocp itself
// opens for this prefix: the façade derives its store from
// cfg.StateRoot and cfg.Prefix (kindocp.New), and the startup override
// refusal rides on that agreement.
func TestStateTreePathNamesTheKindOCPStateDirectory(t *testing.T) {
	base := t.TempDir()
	cfg := mustWorkConfig(t, base, "agree")
	client, err := kindocp.New(kindocp.Options{Config: cfg, Provisioner: stubProvisioner{}})
	if err != nil {
		t.Fatalf("kindocp.New = %v", err)
	}
	// The tree kind-ocp owns for this prefix, created the way the
	// store's first write does: 0700 under the state base.
	root := client.StateRoot()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	candidate := stateTreePath(cfg.StateRoot, cfg.Prefix)
	got, err := os.Stat(candidate)
	if err != nil {
		t.Fatalf("stateTreePath(%q, %q) = %q, which does not exist: %v", cfg.StateRoot, cfg.Prefix, candidate, err)
	}
	want, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(got, want) {
		t.Errorf("stateTreePath(%q, %q) = %q, want the kind-ocp state directory %q", cfg.StateRoot, cfg.Prefix, candidate, root)
	}
}

func TestCentralCAPEMReadsTheChartSecret(t *testing.T) {
	caPEM := []byte("-----BEGIN CERTIFICATE-----\ntest-ca\n-----END CERTIFICATE-----\n")
	cs := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: centralTLSSecretName, Namespace: stackroxNamespace},
		Data:       map[string][]byte{centralTLSCAKey: caPEM},
	})
	got, err := centralCAPEM(context.Background(), cs)
	if err != nil {
		t.Fatalf("centralCAPEM = %v", err)
	}
	if string(got) != string(caPEM) {
		t.Errorf("centralCAPEM returned %q, want the secret's CA entry verbatim", got)
	}
}

func TestCentralCAPEMNamesWhatItCannotRead(t *testing.T) {
	t.Run("missing secret", func(t *testing.T) {
		_, err := centralCAPEM(context.Background(), fake.NewSimpleClientset())
		if err == nil || !strings.Contains(err.Error(), centralTLSSecretName) {
			t.Fatalf("centralCAPEM without the secret = %v, want the secret named", err)
		}
	})
	t.Run("secret without the CA entry", func(t *testing.T) {
		cs := fake.NewSimpleClientset(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: centralTLSSecretName, Namespace: stackroxNamespace},
			Data:       map[string][]byte{"cert.pem": []byte("leaf")},
		})
		_, err := centralCAPEM(context.Background(), cs)
		if err == nil || !strings.Contains(err.Error(), centralTLSCAKey) {
			t.Fatalf("centralCAPEM without the CA entry = %v, want the key named", err)
		}
	})
}

func node(name string, labels map[string]string, addresses ...corev1.NodeAddress) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status:     corev1.NodeStatus{Addresses: addresses},
	}
}

func TestControlPlaneNodeAddressPrefersIPv4OnADualStackNode(t *testing.T) {
	cs := fake.NewSimpleClientset(
		node("worker", map[string]string{"node-role.kubernetes.io/worker": ""}, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "10.0.0.9"}),
		node("control-plane", map[string]string{"node-role.kubernetes.io/control-plane": ""},
			corev1.NodeAddress{Type: corev1.NodeHostName, Address: "control-plane"},
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "fc00:f853:ccd:e793::2"},
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "172.18.0.2"}),
	)
	addr, err := controlPlaneNodeAddress(context.Background(), cs)
	if err != nil {
		t.Fatalf("controlPlaneNodeAddress = %v", err)
	}
	if addr.String() != "172.18.0.2" {
		t.Errorf("controlPlaneNodeAddress = %q, want the IPv4 address (the composed URL then needs no brackets)", addr)
	}
}

func TestControlPlaneNodeAddressFallsBackToIPv6WithBracketsInEndpoint(t *testing.T) {
	cs := fake.NewSimpleClientset(
		node("control-plane", map[string]string{"node-role.kubernetes.io/control-plane": ""},
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "fc00:f853:ccd:e793::2"}),
		centralExposureService(31443),
	)
	endpoint, err := centralEndpoint(context.Background(), cs)
	if err != nil {
		t.Fatalf("centralEndpoint = %v", err)
	}
	if endpoint != "https://[fc00:f853:ccd:e793::2]:31443" {
		t.Errorf("centralEndpoint = %q, want the IPv6 address bracketed", endpoint)
	}
}

func TestControlPlaneNodeAddressFailsWithoutAnAddress(t *testing.T) {
	cs := fake.NewSimpleClientset(
		node("control-plane", map[string]string{"node-role.kubernetes.io/control-plane": ""}),
	)
	if _, err := controlPlaneNodeAddress(context.Background(), cs); err == nil {
		t.Fatal("controlPlaneNodeAddress succeeded with no address to report")
	}
}

func centralExposureService(nodePort int32) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: centralExposureName, Namespace: stackroxNamespace},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Ports: []corev1.ServicePort{
				{Name: "https", Port: 443, NodePort: nodePort},
			},
		},
	}
}

func TestCentralNodePortReadsTheExposureService(t *testing.T) {
	cs := fake.NewSimpleClientset(centralExposureService(31443))
	port, err := centralNodePort(context.Background(), cs)
	if err != nil {
		t.Fatalf("centralNodePort = %v", err)
	}
	if port != 31443 {
		t.Errorf("centralNodePort = %d, want the NodePort behind port 443", port)
	}
}

func TestCentralNodePortNamesTheServiceWhenItIsMissing(t *testing.T) {
	cs := fake.NewSimpleClientset()
	_, err := centralNodePort(context.Background(), cs)
	if err == nil || !strings.Contains(err.Error(), centralExposureName) {
		t.Fatalf("centralNodePort on a missing service = %v, want the service named for debugging", err)
	}
}

func centralPod(name string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: stackroxNamespace, Labels: map[string]string{"app": "central"}},
		Status:     corev1.PodStatus{Phase: phase},
	}
}

func TestCentralPodNameSkipsNonRunningPods(t *testing.T) {
	var objects []runtime.Object
	objects = append(objects, centralPod("central-pending", corev1.PodPending), centralPod("central-7f9c", corev1.PodRunning))
	cs := fake.NewSimpleClientset(objects...)
	name, err := centralPodName(context.Background(), cs)
	if err != nil {
		t.Fatalf("centralPodName = %v", err)
	}
	if name != "central-7f9c" {
		t.Errorf("centralPodName = %q, want the running pod", name)
	}
}

func TestCentralEndpointComposesTheNodeAddressAndNodePort(t *testing.T) {
	cs := fake.NewSimpleClientset(
		node("control-plane", map[string]string{"node-role.kubernetes.io/control-plane": ""},
			corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: "172.18.0.2"}),
		centralExposureService(31443),
	)
	endpoint, err := centralEndpoint(context.Background(), cs)
	if err != nil {
		t.Fatalf("centralEndpoint = %v", err)
	}
	if endpoint != "https://172.18.0.2:31443" {
		t.Errorf("centralEndpoint = %q, want the node address and NodePort composed", endpoint)
	}
}

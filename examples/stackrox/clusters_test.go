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
// test cleans up with the temp dir.
func mustWorkConfig(t *testing.T, dir, prefix string) *config.Config {
	t.Helper()
	return mustConfig(t, prefix, filepath.Join(dir, "kind-ocp"))
}

func TestOpenWorkDirCreatesAPrivatePerPrefixDirectory(t *testing.T) {
	base := t.TempDir()
	cfg := mustWorkConfig(t, base, "quote-test")
	dir, err := openWorkDir(cfg)
	if err != nil {
		t.Fatalf("openWorkDir = %v", err)
	}
	if want := filepath.Join(base, "kind-ocp-stackrox-example", "quote-test"); dir != want {
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
	dir := filepath.Join(base, workDirName, "permissive-test")
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
	parent := filepath.Join(base, workDirName)
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

// The workspace's ancestors are verified, not assumed: a directory
// another local user can write (group- or world-writable, no sticky bit)
// can substitute the workspace entries below it, so the parent of the
// per-prefix directory is held to the ancestor policy too.
func TestOpenWorkDirRefusesASubstitutableAncestor(t *testing.T) {
	base := t.TempDir()
	cfg := mustWorkConfig(t, base, "substituted-test")
	if err := os.MkdirAll(filepath.Join(base, workDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(base, workDirName), 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := openWorkDir(cfg); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("openWorkDir = %v, want a substitutable-ancestor rejection", err)
	}
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

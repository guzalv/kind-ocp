package cli

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/guzalv/kind-ocp/config"
	"github.com/guzalv/kind-ocp/internal/shell"
	"github.com/guzalv/kind-ocp/kindocp"
	"github.com/guzalv/kind-ocp/layers"
	"github.com/guzalv/kind-ocp/provider"
)

// cliFakeProvisioner is the in-memory provisioner behind CLI tests. When
// api is set, created clusters get a kubeconfig pointing at it, so the full
// create path (readiness wait included) runs without Docker.
type cliFakeProvisioner struct {
	clusters  map[string]bool
	createErr string // non-empty: every Create fails with this message
	api       *fakeAPIServer
}

func (f *cliFakeProvisioner) Create(_ context.Context, opts provider.CreateOptions) (bool, error) {
	if f.createErr != "" {
		return false, errString(f.createErr)
	}
	if f.clusters[opts.Name] {
		return false, nil
	}
	f.clusters[opts.Name] = true
	if f.api != nil {
		if err := os.MkdirAll(filepath.Dir(opts.KubeconfigPath), 0o700); err != nil {
			return false, err
		}
		if err := os.WriteFile(opts.KubeconfigPath, []byte(f.api.kubeconfig()), 0o600); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (f *cliFakeProvisioner) Delete(_ context.Context, name string, _ string) error {
	delete(f.clusters, name)
	return nil
}

func (f *cliFakeProvisioner) List(_ context.Context) ([]string, error) {
	var out []string
	for n := range f.clusters {
		out = append(out, n)
	}
	return out, nil
}

func (f *cliFakeProvisioner) ExportKubeconfig(_ context.Context, _ string, _ string) error {
	return nil
}

func (f *cliFakeProvisioner) LoadImage(_ context.Context, _ string, _ string) error {
	return nil
}

type errString string

func (e errString) Error() string { return string(e) }

// fakeAPIServer serves the two endpoints the create path needs (node
// readiness and cluster identity) over plain HTTP, the way publicapi's
// smoke server does.
type fakeAPIServer struct {
	server *httptest.Server
}

func newFakeAPIServer(t *testing.T, nodesReady bool) *fakeAPIServer {
	t.Helper()
	nodeStatus := "False"
	if nodesReady {
		nodeStatus = "True"
	}
	api := &fakeAPIServer{}
	api.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/nodes":
			_, _ = w.Write([]byte(`{"kind":"NodeList","apiVersion":"v1","metadata":{},"items":[` +
				`{"metadata":{"name":"t-1-control-plane"},"status":{"conditions":[{"type":"Ready","status":"` + nodeStatus + `"}]}}]}`))
		case "/api/v1/namespaces/kube-system":
			_, _ = w.Write([]byte(`{"kind":"Namespace","apiVersion":"v1","metadata":{"name":"kube-system","uid":"cli-test-instance"}}`))
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
- name: cli-test
  cluster:
    server: %s
contexts:
- name: cli-test
  context:
    cluster: cli-test
    user: cli-test
current-context: cli-test
users:
- name: cli-test
  user: {}
`, a.server.URL)
}

// runCommand executes the root command against a fake provisioner and
// returns the captured stdout, stderr, and execution error. Extra
// environment variables (cleared first) let tests exercise the KIND_OCP_*
// precedence.
func runCommand(t *testing.T, prov *cliFakeProvisioner, args ...string) (string, string, error) {
	t.Helper()
	return runCommandEnv(t, prov, nil, args...)
}

func runCommandEnv(t *testing.T, prov *cliFakeProvisioner, env map[string]string, args ...string) (string, string, error) {
	t.Helper()
	for _, k := range []string{"KIND_OCP_CONFIG", "KIND_OCP_PREFIX", "KIND_OCP_COUNT", "KIND_OCP_PROFILE"} {
		t.Setenv(k, "")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}

	var out, errOut bytes.Buffer
	build := func(cfg *config.Config, log *slog.Logger) (*kindocp.Client, error) {
		return kindocp.New(kindocp.Options{
			Config:      cfg,
			Registry:    layers.NewRegistry(),
			Provisioner: prov,
			Log:         log,
		})
	}
	root := newRootCmd(build)
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

// Regression tests for the review finding that scope-only commands rejected
// create-only selections: `delete --count 3` was an unknown flag, a config
// file with an override for cluster 3 failed count validation during
// delete, `delete --count 0` and a malformed KIND_OCP_COUNT failed cleanup.
// All must succeed: delete is prefix-scoped and ignores create inputs.
func TestDeleteAcceptsCreateOnlySelections(t *testing.T) {
	t.Run("count flag beyond the current count", func(t *testing.T) {
		prov := &cliFakeProvisioner{clusters: map[string]bool{}}
		out, _, err := runCommand(t, prov, "delete", "--prefix", "t", "--count", "3",
			"--state-root", filepath.Join(t.TempDir(), "state"))
		if err != nil {
			t.Fatalf("delete --count 3 failed: %v", err)
		}
		if !strings.Contains(out, "no clusters to delete") {
			t.Errorf("stdout = %q, want the empty-delete message", out)
		}
	})

	t.Run("count flag zero", func(t *testing.T) {
		prov := &cliFakeProvisioner{clusters: map[string]bool{}}
		if _, _, err := runCommand(t, prov, "delete", "--prefix", "t", "--count", "0",
			"--state-root", filepath.Join(t.TempDir(), "state")); err != nil {
			t.Fatalf("delete --count 0 failed: %v", err)
		}
	})

	t.Run("malformed count environment", func(t *testing.T) {
		prov := &cliFakeProvisioner{clusters: map[string]bool{}}
		if _, _, err := runCommandEnv(t, prov, map[string]string{"KIND_OCP_COUNT": "bad"},
			"delete", "--prefix", "t", "--state-root", filepath.Join(t.TempDir(), "state")); err != nil {
			t.Fatalf("KIND_OCP_COUNT=bad kind-ocp delete failed: %v", err)
		}
	})

	t.Run("config with a count and override beyond it", func(t *testing.T) {
		cfgFile := filepath.Join(t.TempDir(), "cfg.yaml")
		content := `
count: 2
clusters:
  t-3:
    profile: minimal
`
		if err := os.WriteFile(cfgFile, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		prov := &cliFakeProvisioner{clusters: map[string]bool{}}
		out, _, err := runCommand(t, prov, "delete", "--config", cfgFile, "--prefix", "t",
			"--state-root", filepath.Join(t.TempDir(), "state"))
		if err != nil {
			t.Fatalf("delete with a cluster-3 override failed: %v", err)
		}
		if !strings.Contains(out, "no clusters to delete") {
			t.Errorf("stdout = %q, want the empty-delete message", out)
		}
	})

	// The same selections must still be rejected where they matter: create.
	t.Run("create still rejects count zero", func(t *testing.T) {
		prov := &cliFakeProvisioner{clusters: map[string]bool{}}
		_, _, err := runCommand(t, prov, "create", "--prefix", "t", "--count", "0",
			"--state-root", filepath.Join(t.TempDir(), "state"))
		if err == nil {
			t.Fatal("create --count 0 must fail")
		}
		if len(prov.clusters) != 0 {
			t.Errorf("provisioned = %v, want nothing (validation precedes provisioning)", prov.clusters)
		}
	})

	t.Run("create still rejects a malformed count environment", func(t *testing.T) {
		prov := &cliFakeProvisioner{clusters: map[string]bool{}}
		if _, _, err := runCommandEnv(t, prov, map[string]string{"KIND_OCP_COUNT": "bad"},
			"create", "--prefix", "t", "--state-root", filepath.Join(t.TempDir(), "state")); err == nil {
			t.Fatal("KIND_OCP_COUNT=bad kind-ocp create must fail")
		}
	})
}

// A typo'd layer name in an unselected profile is a create-only
// failure (§3.3, §5.3): create must fail fast on it, and the scope
// commands — delete, list, verify, kubeconfig — must run their normal
// course with the same config file, never the profile validation.
func TestUnselectedProfileTypoIsCreateOnly(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	cfgFile := filepath.Join(t.TempDir(), "cfg.yaml")
	content := `
prefix: t
count: 1
profile: minimal
stateRoot: ` + stateRoot + `
profiles:
  typoed:
    layers: [nope]
`
	if err := os.WriteFile(cfgFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	prov := &cliFakeProvisioner{clusters: map[string]bool{}, api: newFakeAPIServer(t, true)}

	// Create fails on the unselected profile's typo, before anything
	// is provisioned, naming the profile and the unknown layer.
	_, errOut, err := runCommand(t, prov, "create", "--config", cfgFile)
	if err == nil {
		t.Fatal("create must fail on the unselected profile's unknown layer")
	}
	if !strings.Contains(errOut, "typoed") || !strings.Contains(errOut, "nope") {
		t.Errorf("stderr = %q, want the profile and unknown layer named", errOut)
	}
	if len(prov.clusters) != 0 {
		t.Errorf("provisioned = %v, want nothing (validation precedes provisioning)", prov.clusters)
	}

	// A clean create (no config file) builds the cluster the scope
	// commands below operate on, under the same prefix and state root.
	if _, _, err := runCommand(t, prov, "create", "--prefix", "t", "--count", "1",
		"--profile", "minimal", "--state-root", stateRoot); err != nil {
		t.Fatalf("clean create failed: %v", err)
	}

	// The same config file must not block any scope command.
	out, _, err := runCommand(t, prov, "list", "--config", cfgFile)
	if err != nil {
		t.Fatalf("list with the typo'd config failed: %v", err)
	}
	if !strings.Contains(out, "t-1") {
		t.Errorf("stdout = %q, want cluster t-1 listed", out)
	}
	if _, _, err := runCommand(t, prov, "kubeconfig", "t-1", "--config", cfgFile); err != nil {
		t.Fatalf("kubeconfig with the typo'd config failed: %v", err)
	}
	out, _, err = runCommand(t, prov, "verify", "--config", cfgFile)
	if err != nil {
		t.Fatalf("verify with the typo'd config failed: %v", err)
	}
	if !strings.Contains(out, "PASS") {
		t.Errorf("stdout = %q, want verify's normal check output", out)
	}
	if _, _, err := runCommand(t, prov, "delete", "--config", cfgFile); err != nil {
		t.Fatalf("delete with the typo'd config failed: %v", err)
	}
}

// The cleanup hint must appear after every failed create, including
// failures that happen before any record is written (the old code gated the
// hint on having records, which hid live clusters).
func TestCreateFailureAlwaysPrintsCleanupCommand(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "state")
	prov := &cliFakeProvisioner{clusters: map[string]bool{}, createErr: "docker exploded"}

	_, errOut, err := runCommand(t, prov, "create", "--prefix", "t", "--state-root", stateRoot)
	if err == nil {
		t.Fatal("expected create to fail")
	}
	want := "kind-ocp delete --config '' --prefix t --state-root " + stateRoot
	if !strings.Contains(errOut, want) {
		t.Errorf("stderr = %q, want it to contain the executable cleanup command %q", errOut, want)
	}
	if !strings.Contains(errOut, "docker exploded") {
		t.Errorf("stderr = %q, want the failure cause", errOut)
	}
}

// The printed cleanup command must survive a copy-paste into a real shell
// when the state root contains spaces and a single quote: the test extracts
// the rendered line from a failed create's stderr and executes it through
// sh with a stub kind-ocp that records its argv.
func TestCreateFailureCleanupCommandIsShellSafe(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no POSIX sh available")
	}
	stateRoot := filepath.Join(t.TempDir(), "my state'root")
	prov := &cliFakeProvisioner{clusters: map[string]bool{}, createErr: "docker exploded"}

	_, errOut, err := runCommand(t, prov, "create", "--prefix", "t", "--state-root", stateRoot)
	if err == nil {
		t.Fatal("expected create to fail")
	}
	var line string
	for _, l := range strings.Split(errOut, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "kind-ocp delete ") {
			line = strings.TrimSpace(l)
		}
	}
	if line == "" {
		t.Fatalf("stderr = %q, want a kind-ocp delete line", errOut)
	}

	// A stub kind-ocp that records its arguments, reachable through PATH.
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "argv")
	stub := filepath.Join(dir, "kind-ocp")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shell.Quote(argsFile) + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run := exec.Command("sh", "-c", line)
	run.Env = append(os.Environ(), "PATH="+dir+":/usr/bin:/bin")
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("the printed command does not run verbatim: %v (%s)", err, out)
	}

	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	want := []string{"delete", "--config", "", "--prefix", "t", "--state-root", stateRoot}
	if !slices.Equal(got, want) {
		t.Errorf("the pasted command delivered %q, want %q", got, want)
	}
}

// The printed cleanup command must not depend on the config-file lookup it
// does not carry: a create whose --config flag beat a losing KIND_OCP_CONFIG
// (here: a nonexistent path) prints a delete line that still has to work
// when pasted into that same shell. The test extracts the line, splits it
// with a real sh under the original environment, and runs the split words
// through the real command tree in that same environment.
func TestCreateFailureCleanupCommandIgnoresLosingConfigEnv(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no POSIX sh available")
	}
	stateRoot := filepath.Join(t.TempDir(), "state")
	// The config file create used: readable, but its count fails create
	// validation after resolution (the flag still beats the env path).
	cfgFile := filepath.Join(t.TempDir(), "cfg.yaml")
	if err := os.WriteFile(cfgFile, []byte("count: 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	losingEnv := "/nonexistent/kind-ocp-config.yaml"

	prov := &cliFakeProvisioner{clusters: map[string]bool{}}
	_, errOut, err := runCommandEnv(t, prov,
		map[string]string{"KIND_OCP_CONFIG": losingEnv},
		"create", "--config", cfgFile, "--prefix", "t", "--state-root", stateRoot)
	if err == nil {
		t.Fatal("expected create to fail on the config file's count")
	}
	var line string
	for _, l := range strings.Split(errOut, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "kind-ocp delete ") {
			line = strings.TrimSpace(l)
		}
	}
	if line == "" {
		t.Fatalf("stderr = %q, want a kind-ocp delete line", errOut)
	}

	// Split the pasted line the way a real shell would, under the original
	// environment (KIND_OCP_CONFIG still pointing at the losing path).
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "argv")
	stub := filepath.Join(dir, "kind-ocp")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + shell.Quote(argsFile) + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	run := exec.Command("sh", "-c", line)
	run.Env = pasteEnv(dir, losingEnv)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("the printed command does not run verbatim: %v (%s)", err, out)
	}
	b, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	want := []string{"delete", "--config", "", "--prefix", "t", "--state-root", stateRoot}
	if !slices.Equal(args, want) {
		t.Fatalf("the pasted command delivered %q, want %q", args, want)
	}

	// The words the shell delivered, run through the real command tree in
	// the same losing environment: the delete must succeed and stay scoped
	// to the printed prefix and state root.
	scoped := &cliFakeProvisioner{clusters: map[string]bool{"t-1": true, "t-2": true, "other-1": true}}
	out, _, err := runCommandEnv(t, scoped, map[string]string{"KIND_OCP_CONFIG": losingEnv}, args...)
	if err != nil {
		t.Fatalf("the pasted delete failed under the original environment: %v", err)
	}
	for _, want := range []string{"deleted t-1", "deleted t-2"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout = %q, want %q", out, want)
		}
	}
	if strings.Contains(out, "other-1") || !scoped.clusters["other-1"] {
		t.Error("the pasted delete must not touch clusters of other prefixes")
	}
}

// pasteEnv builds the environment a pasted command runs in: the test's own
// environment minus the KIND_OCP_* variables and PATH, the stub directory
// first on PATH, and the given (losing) KIND_OCP_CONFIG.
func pasteEnv(stubDir, configEnv string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "KIND_OCP_") || strings.HasPrefix(kv, "PATH=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "PATH="+stubDir+":/usr/bin:/bin", "KIND_OCP_CONFIG="+configEnv)
}

// The readiness gate must also bite through the CLI: a provisioned cluster
// whose nodes never report Ready fails create (it used to exit 0 because
// kind's internal wait reports timeouts as success).
func TestCreateFailsWhenNodesNeverReady(t *testing.T) {
	prov := &cliFakeProvisioner{clusters: map[string]bool{}, api: newFakeAPIServer(t, false)}
	cfgFile := filepath.Join(t.TempDir(), "cfg.yaml")
	// A short nodeReady timeout keeps the test fast; the API server keeps
	// reporting the node not Ready.
	content := "timeouts:\n  nodeReady: 10ms\n"
	if err := os.WriteFile(cfgFile, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	_, errOut, err := runCommand(t, prov, "create", "--prefix", "t", "--config", cfgFile,
		"--state-root", filepath.Join(t.TempDir(), "state"))
	if err == nil {
		t.Fatal("expected create to fail when nodes never become Ready")
	}
	if !strings.Contains(errOut, "waiting for nodes Ready") {
		t.Errorf("stderr = %q, want the readiness failure", errOut)
	}
	if !strings.Contains(errOut, "kind-ocp delete --config '' --prefix t") {
		t.Errorf("stderr = %q, want the cleanup command after a readiness failure", errOut)
	}
}

// The successful create hints at the kubeconfig command including the
// effective prefix and state root, shell-quoted.
func TestCreateSuccessPrintsQuotedKubeconfigHint(t *testing.T) {
	stateRoot := filepath.Join(t.TempDir(), "my state")
	prov := &cliFakeProvisioner{clusters: map[string]bool{}, api: newFakeAPIServer(t, true)}

	out, _, err := runCommand(t, prov, "create", "--prefix", "t", "--state-root", stateRoot)
	if err != nil {
		t.Fatal(err)
	}
	want := "kubeconfig: kind-ocp kubeconfig <name> --config '' --prefix t --state-root " + shell.Quote(stateRoot)
	if !strings.Contains(out, want) {
		t.Errorf("stdout = %q, want the kubeconfig hint %q", out, want)
	}
}

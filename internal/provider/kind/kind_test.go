package kind

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/guzalv/kind-ocp/internal/fscheck"
	kindlog "sigs.k8s.io/kind/pkg/log"
)

// The verbosity boundary: user-facing progress (level 0 and below) must be
// visible, while kind's verbose detail (V(1)+: the raw kubeadm config with
// the bootstrap token at V(2), kubeadm init/join output at V(3)) must never
// reach the log.
func TestKindLoggerVerbosityBoundary(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	adapter := kindLogger{log: logger}

	if !adapter.V(kindLevelInfo).Enabled() {
		t.Error("V(info) must be enabled: kind's ordinary progress is user-facing")
	}
	adapter.V(kindLevelInfo).Info("creating cluster")
	adapter.V(kindLevelInfo).Infof("node %s ready", "cp")
	if !strings.Contains(buf.String(), "creating cluster") || !strings.Contains(buf.String(), "node cp ready") {
		t.Errorf("progress output missing from the log: %q", buf.String())
	}

	// Warnings and errors are level-independent and always visible.
	adapter.Warn("a warning")
	adapter.Errorf("an error %d", 42)
	if !strings.Contains(buf.String(), "a warning") || !strings.Contains(buf.String(), "an error 42") {
		t.Errorf("warn/error output missing from the log: %q", buf.String())
	}

	for _, level := range []kindlog.Level{1, 2, 3, 7} {
		if adapter.V(level).Enabled() {
			t.Errorf("V(%d) must be disabled: verbose kind output can contain credential material", level)
		}
	}
	buf.Reset()
	adapter.V(2).Infof("kubeadm config %s", "with-bootstrap-token")
	adapter.V(3).Info("kubeadm init output")
	if buf.Len() != 0 {
		t.Errorf("verbose output leaked into the log: %q", buf.String())
	}
}

// privateDir returns a private (0700) directory, the shape of a cluster
// state directory the store creates. t.TempDir() itself is 0755 on some
// platforms, which is fine for ancestors but not for a credential's
// containing directory.
func privateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// verifyKubeconfig must verify what kind actually wrote — kind merges
// into an existing destination and would create a missing containing
// directory 0755, so a non-private result is an error, not a debug log
// line. It must not repair anything: a corrective chmod is itself a
// write to a file that has not verified.
func TestVerifyKubeconfig(t *testing.T) {
	t.Run("refuses a permissive file instead of repairing it", func(t *testing.T) {
		dir := privateDir(t)
		path := filepath.Join(dir, "kubeconfig")
		if err := os.WriteFile(path, []byte("cfg"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		err := verifyKubeconfig(path)
		if err == nil || !strings.Contains(err.Error(), "mode") {
			t.Fatalf("verifyKubeconfig(0644 in a 0700 dir) = %v, want a mode rejection", err)
		}
		// The refusal must not have touched the file: repair is a write
		// to an unverified destination.
		info, statErr := os.Stat(path)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if perm := info.Mode().Perm(); perm != 0o644 {
			t.Errorf("kubeconfig perms = %o, want 0644 unchanged (no repair)", perm)
		}
	})

	t.Run("rejects a permissive containing directory", func(t *testing.T) {
		// kind would create a missing directory 0755; whatever wrote
		// the file, a non-private directory around a credential is
		// refused.
		dir := t.TempDir()
		shared := filepath.Join(dir, "shared")
		if err := os.Mkdir(shared, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(shared, "kubeconfig")
		if err := os.WriteFile(path, []byte("cfg"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := verifyKubeconfig(path)
		if err == nil || !strings.Contains(err.Error(), "kubeconfig directory") {
			t.Fatalf("verifyKubeconfig(0600 in a 0755 dir) = %v, want a directory rejection", err)
		}
	})

	t.Run("rejects a symlinked kubeconfig without touching the target", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.WriteFile(target, []byte("cfg"), 0o600); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "kubeconfig")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		err := verifyKubeconfig(link)
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("verifyKubeconfig(symlink) = %v, want a symlink rejection", err)
		}
		// The refusal must not have modified the symlink's target —
		// not its mode, not its content. (An earlier revision chmod'ed
		// the destination before rejecting it.)
		info, statErr := os.Stat(target)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("target perms = %o, want 0600 unchanged (no external mutation)", perm)
		}
		if b, rerr := os.ReadFile(target); rerr != nil || string(b) != "cfg" {
			t.Errorf("target content = (%q, %v), want it preserved", b, rerr)
		}
	})

	t.Run("rejects a missing file", func(t *testing.T) {
		// After kind reports success the file must exist; a missing
		// one means something is wrong and the caller must hear it.
		err := verifyKubeconfig(filepath.Join(t.TempDir(), "absent"))
		if err == nil {
			t.Fatal("verifyKubeconfig(absent) must fail")
		}
	})

	t.Run("empty path is a no-op", func(t *testing.T) {
		if err := verifyKubeconfig(""); err != nil {
			t.Errorf("verifyKubeconfig(\"\") = %v, want nil", err)
		}
	})

	t.Run("private file in a private directory passes", func(t *testing.T) {
		path := filepath.Join(privateDir(t), "kubeconfig")
		if err := os.WriteFile(path, []byte("cfg"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := verifyKubeconfig(path); err != nil {
			t.Errorf("verifyKubeconfig(0600 in 0700) = %v, want nil", err)
		}
	})
}

// dockerRecorder stands in for the Docker CLI: it records every
// invocation the adapter makes, and answers from an injected responder
// (nil output by default). The regressions assert the exact argv —
// what the adapter would ask Docker to execute — without a daemon and
// without executing anything.
type dockerRecorder struct {
	mu      sync.Mutex
	calls   [][]string
	respond func(ctx context.Context, args []string) ([]byte, error)
}

func (r *dockerRecorder) run(ctx context.Context, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string(nil), args...))
	respond := r.respond
	r.mu.Unlock()
	if respond != nil {
		return respond(ctx, append([]string(nil), args...))
	}
	return nil, nil
}

func (r *dockerRecorder) recorded() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.calls...)
}

// TestProbeTCPFromNodePassesEndpointAsArgv: the probe's Docker
// boundary is one label-filtered container lookup plus one exec
// against the returned ID — non-privileged, with the static script and
// the endpoint as separate argv elements. The endpoint is the one
// thing here an untrusted cluster controls (it is read from the
// cluster's own EndpointSlices), so no byte of it may be parsed as
// shell language: the script text is a constant, and the address and
// port arrive as bash's quoted positional parameters.
func TestProbeTCPFromNodePassesEndpointAsArgv(t *testing.T) {
	rec := &dockerRecorder{respond: func(_ context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "ps":
			return []byte("container-id-123\n"), nil
		case "exec":
			return []byte("connected\n"), nil
		}
		return nil, fmt.Errorf("unexpected docker call %v", args)
	}}
	p := &Provisioner{log: slog.Default(), docker: rec.run}
	addr := netip.MustParseAddr("10.42.0.7")
	if err := p.ProbeTCPFromNode(context.Background(), "c1", addr, 31005); err != nil {
		t.Fatalf("ProbeTCPFromNode = %v, want success", err)
	}
	calls := rec.recorded()
	if len(calls) != 2 {
		t.Fatalf("docker calls = %v, want exactly the lookup and the exec", calls)
	}

	// The lookup: one docker ps, by kind's cluster and role labels.
	wantPS := []string{
		"ps",
		"--filter", "label=io.x-k8s.kind.cluster=c1",
		"--filter", "label=io.x-k8s.kind.role=control-plane",
		"--format", "{{.ID}}",
	}
	if !reflect.DeepEqual(calls[0], wantPS) {
		t.Errorf("lookup argv = %v, want %v", calls[0], wantPS)
	}

	// The exec: non-privileged, against the looked-up container, with
	// the static script and the endpoint behind it.
	exec := calls[1]
	if exec[0] != "exec" || exec[1] != "container-id-123" {
		t.Errorf("exec argv = %v, want it against the looked-up container ID", exec)
	}
	for _, arg := range exec {
		if arg == "--privileged" {
			t.Errorf("exec argv %v requests --privileged; a TCP connect needs no privileges", exec)
		}
	}
	scriptAt := -1
	for i, arg := range exec {
		if arg == connectScript {
			scriptAt = i
			break
		}
	}
	if scriptAt < 0 {
		t.Fatalf("exec argv = %v, want the static script verbatim", exec)
	}
	wantTail := []string{"connect", "10.42.0.7", "31005"}
	if tail := exec[scriptAt+1:]; !reflect.DeepEqual(tail, wantTail) {
		t.Errorf("argv after the script = %v, want the endpoint as separate elements %v", tail, wantTail)
	}

	// The script itself carries no endpoint bytes and reaches the
	// endpoint only through quoted positional parameters.
	if strings.Contains(connectScript, "10.42.0.7") || strings.Contains(connectScript, "31005") {
		t.Errorf("the script text interpolates the endpoint: %q", connectScript)
	}
	for _, quoted := range []string{`"$1"`, `"$2"`} {
		if !strings.Contains(connectScript, quoted) {
			t.Errorf("the script must reach the endpoint through %s: %q", quoted, connectScript)
		}
	}
	// Each attempt is its own bash under coreutils timeout, so the
	// bound reaches the blocked connect itself.
	if !strings.Contains(connectScript, "timeout 2 bash -c") {
		t.Errorf("the script must bound each attempt: %q", connectScript)
	}
}

// TestProbeTCPFromNodeRefusesUnsafeEndpoints: a scoped address (whose
// zone text is arbitrary — the injectable part of a netip.Addr) and an
// out-of-range port are refused before Docker is invoked at all. The
// zone here is a live command substitution; under the old design it
// was interpolated into executable script text.
func TestProbeTCPFromNodeRefusesUnsafeEndpoints(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr netip.Addr
		port int
	}{
		{"scoped address", netip.MustParseAddr("fe80::1%eth0"), 6443},
		{"hostile zone", netip.MustParseAddr("fe80::1%$(touch /tmp/pwned)"), 6443},
		{"port zero", netip.MustParseAddr("10.42.0.7"), 0},
		{"port above the range", netip.MustParseAddr("10.42.0.7"), 65536},
		{"negative port", netip.MustParseAddr("10.42.0.7"), -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &dockerRecorder{}
			p := &Provisioner{log: slog.Default(), docker: rec.run}
			err := p.ProbeTCPFromNode(context.Background(), "c1", tc.addr, tc.port)
			if err == nil {
				t.Fatalf("ProbeTCPFromNode(%s, %d) = nil, want a refusal", tc.addr, tc.port)
			}
			if calls := rec.recorded(); len(calls) != 0 {
				t.Errorf("docker calls = %v, want none before the endpoint validated", calls)
			}
		})
	}
}

// TestProbeTCPFromNodeCancelledLookupStartsNoExec: a Docker lookup
// that ends cancelled — the context firing mid-lookup, as an
// interrupted run delivers it — fails the probe without starting the
// exec, and a context already cancelled at entry runs nothing at all.
// Both invocations run under the context (exec.CommandContext), so a
// stalled Docker CLI is killed rather than merely out-waited.
func TestProbeTCPFromNodeCancelledLookupStartsNoExec(t *testing.T) {
	t.Run("cancelled mid-lookup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		rec := &dockerRecorder{respond: func(c context.Context, args []string) ([]byte, error) {
			if args[0] != "ps" {
				t.Errorf("docker %v ran after the lookup was cancelled; the exec must not start", args)
				return nil, nil
			}
			cancel()   // the cancellation lands mid-lookup
			<-c.Done() // the stalled CLI is ended by the context
			return nil, c.Err()
		}}
		p := &Provisioner{log: slog.Default(), docker: rec.run}
		err := p.ProbeTCPFromNode(ctx, "c1", netip.MustParseAddr("10.42.0.7"), 6443)
		if err == nil || !strings.Contains(err.Error(), "control-plane container") {
			t.Fatalf("ProbeTCPFromNode = %v, want the failed lookup named", err)
		}
		calls := rec.recorded()
		if len(calls) != 1 || calls[0][0] != "ps" {
			t.Errorf("docker calls = %v, want exactly the cancelled lookup", calls)
		}
	})
	t.Run("cancelled at entry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rec := &dockerRecorder{}
		p := &Provisioner{log: slog.Default(), docker: rec.run}
		if err := p.ProbeTCPFromNode(ctx, "c1", netip.MustParseAddr("10.42.0.7"), 6443); err == nil {
			t.Fatal("ProbeTCPFromNode with a cancelled context = nil, want the context error")
		}
		if calls := rec.recorded(); len(calls) != 0 {
			t.Errorf("docker calls = %v, want none under a cancelled context", calls)
		}
	})
}

// TestControlPlaneAPIEndpointsReadsTheNodesEndpoints: the
// corroboration's Docker boundary is the same label-filtered node
// lookup the probe connects through, followed by one inspect that
// emits the network the node was created on and its per-network
// attachment map as JSON — so the endpoints the check corroborates
// against and the node the connect runs on are one container,
// decided by one lookup, and every address is attributable to the
// network it was read from. Each address carries kind's internal API
// port (the kubeadm bind port every kind cluster's apiserver listens
// on): the two calls are the whole boundary, with no per-cluster
// port lookup beside them.
func TestControlPlaneAPIEndpointsReadsTheNodesEndpoints(t *testing.T) {
	rec := &dockerRecorder{respond: func(_ context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "ps":
			return []byte("container-id-123\n"), nil
		case "inspect":
			return []byte(`{"mode":"kind","networks":{"kind":{"IPAddress":"172.18.0.2","GlobalIPv6Address":""}}}` + "\n"), nil
		}
		return nil, fmt.Errorf("unexpected docker call %v", args)
	}}
	p := &Provisioner{log: slog.Default(), docker: rec.run}
	endpoints, err := p.ControlPlaneAPIEndpoints(context.Background(), "c1")
	if err != nil {
		t.Fatalf("ControlPlaneAPIEndpoints = %v, want success", err)
	}
	if want := []netip.AddrPort{netip.MustParseAddrPort("172.18.0.2:6443")}; !reflect.DeepEqual(endpoints, want) {
		t.Errorf("endpoints = %v, want %v (the recorded network's address with kind's internal API port)", endpoints, want)
	}
	calls := rec.recorded()
	if len(calls) != 2 {
		t.Fatalf("docker calls = %v, want exactly the lookup and the inspect", calls)
	}
	// The lookup is the probe's own — same labels, so the same node
	// the connect would run on.
	wantPS := []string{
		"ps",
		"--filter", "label=io.x-k8s.kind.cluster=c1",
		"--filter", "label=io.x-k8s.kind.role=control-plane",
		"--format", "{{.ID}}",
	}
	if !reflect.DeepEqual(calls[0], wantPS) {
		t.Errorf("lookup argv = %v, want %v (the probe's own lookup, so one node serves both)", calls[0], wantPS)
	}
	wantInspect := []string{"inspect", "--format", nodeNetworksFormat, "container-id-123"}
	if !reflect.DeepEqual(calls[1], wantInspect) {
		t.Errorf("inspect argv = %v, want %v", calls[1], wantInspect)
	}
}

// TestControlPlaneAPIEndpointsReadsOnlyTheSharedNetwork: the
// corroboration's identity is the node's endpoints on the network
// kind's clusters share — nothing else Docker reports about the
// container's other attachments. That network is the one Docker
// recorded the node as created on, so when the record names a custom
// network (kind created the node under a create-time
// KIND_EXPERIMENTAL_DOCKER_NETWORK override), the custom network's
// entry is what corroborates — not a "kind"-named attachment beside
// it. The node's identity on a side network does not identify it from
// a peer on the shared network, so an extra attachment's addresses
// never corroborate an endpoint; the recorded network's entry is the
// only one read, from the attachment map. The fixtures are
// docker-shaped: the per-network objects carry the fields real docker
// inspect emits, and the ones the corroboration does not read
// (NetworkID, Gateway, MacAddress, the prefix lengths) are ignored
// rather than rejected. Every returned endpoint carries kind's
// internal API port (6443) beside the address.
func TestControlPlaneAPIEndpointsReadsOnlyTheSharedNetwork(t *testing.T) {
	// withAPIPort builds the expected endpoint set: each address on
	// the recorded network, carrying kind's internal API port. The
	// AddrPort is built from the parsed address (not a joined string,
	// which an IPv6 literal's own colons would break).
	withAPIPort := func(addrs ...string) []netip.AddrPort {
		eps := make([]netip.AddrPort, 0, len(addrs))
		for _, a := range addrs {
			eps = append(eps, netip.AddrPortFrom(netip.MustParseAddr(a), 6443))
		}
		return eps
	}
	// dualStackExtra is a dual-stack shared network plus an extra
	// attachment: the exact boundary the review asked to hold. The
	// extra network's IPv6 (fd00::2) followed by the shared network's
	// IPv4 (172.18.0.2) is the pair the old comma-joined format merged
	// into the unparseable single field "fd00::2172.18.0.2"; as
	// separate JSON strings each field is attributable to its own
	// network and only the recorded network's are read.
	const dualStackExtra = `{"mode":"kind","networks":{` +
		`"extra":{"IPAddress":"172.19.0.2","GlobalIPv6Address":"fd00::2",` +
		`"NetworkID":"net-extra","Gateway":"172.19.0.1","MacAddress":"02:42:ac:13:00:02"},` +
		`"kind":{"IPAddress":"172.18.0.2","GlobalIPv6Address":"fc00:f853:ccd:e793::2",` +
		`"NetworkID":"net-kind","Gateway":"172.18.0.1","MacAddress":"02:42:ac:12:00:02"}}}`
	for _, tc := range []struct {
		name   string
		output string
		want   []netip.AddrPort
		errSub string
	}{
		{
			name:   "a dual-stack shared network with an extra attachment yields only the shared endpoints",
			output: dualStackExtra,
			want:   withAPIPort("172.18.0.2", "fc00:f853:ccd:e793::2"),
		},
		{
			name:   "a dual-stack shared network alone yields both its endpoints",
			output: `{"mode":"kind","networks":{"kind":{"IPAddress":"172.18.0.2","GlobalIPv6Address":"fc00:f853:ccd:e793::2"}}}`,
			want:   withAPIPort("172.18.0.2", "fc00:f853:ccd:e793::2"),
		},
		{
			name:   "an IPv6-only shared network yields the IPv6 endpoint",
			output: `{"mode":"kind","networks":{"kind":{"IPAddress":"","GlobalIPv6Address":"fc00:f853:ccd:e793::2"}}}`,
			want:   withAPIPort("fc00:f853:ccd:e793::2"),
		},
		{
			name: "a custom recorded network is read even with a kind-named attachment beside it",
			output: `{"mode":"custom","networks":{"custom":{"IPAddress":"10.10.0.2","GlobalIPv6Address":""},` +
				`"kind":{"IPAddress":"172.18.0.2","GlobalIPv6Address":""}}}`,
			want: withAPIPort("10.10.0.2"),
		},
		{
			name:   "the recorded network missing entirely is an error naming it and the attachments",
			output: `{"mode":"kind","networks":{"extra":{"IPAddress":"172.19.0.2","GlobalIPv6Address":""}}}`,
			errSub: `was created on network "kind" but is not attached to it (it is attached to: extra)`,
		},
		{
			name: "a recorded network none of the attachments matches is an error naming it",
			output: `{"mode":"custom","networks":{"extra":{"IPAddress":"172.19.0.2","GlobalIPv6Address":""},` +
				`"kind":{"IPAddress":"172.18.0.2","GlobalIPv6Address":""}}}`,
			errSub: `was created on network "custom" but is not attached to it (it is attached to: extra, kind)`,
		},
		{
			name:   "no recorded network at all is an error naming the attachments",
			output: `{"networks":{"kind":{"IPAddress":"172.18.0.2","GlobalIPv6Address":""}}}`,
			errSub: `docker reports no network it was created on (it is attached to: kind)`,
		},
		{
			name:   "the recorded network attached with empty addresses is an error naming it",
			output: `{"mode":"kind","networks":{"kind":{"IPAddress":"","GlobalIPv6Address":""},"extra":{"IPAddress":"172.19.0.2","GlobalIPv6Address":""}}}`,
			errSub: `reports no address on the network it was created on ("kind")`,
		},
		{
			name:   "an unparseable field on the recorded network is an error naming it",
			output: `{"mode":"kind","networks":{"kind":{"IPAddress":"not-an-ip","GlobalIPv6Address":""}}}`,
			errSub: `"not-an-ip" is not an IP`,
		},
		{
			name:   "output that does not decode is an error",
			output: `172.18.0.2,`,
			errSub: "do not decode",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &dockerRecorder{respond: func(_ context.Context, args []string) ([]byte, error) {
				switch args[0] {
				case "ps":
					return []byte("container-id-123\n"), nil
				case "inspect":
					return []byte(tc.output + "\n"), nil
				}
				return nil, fmt.Errorf("unexpected docker call %v", args)
			}}
			p := &Provisioner{log: slog.Default(), docker: rec.run}
			endpoints, err := p.ControlPlaneAPIEndpoints(context.Background(), "c1")
			switch {
			case tc.errSub != "":
				if err == nil || !strings.Contains(err.Error(), tc.errSub) {
					t.Fatalf("ControlPlaneAPIEndpoints(%q) = (%v, %v), want an error naming %q", tc.output, endpoints, err, tc.errSub)
				}
			case err != nil:
				t.Fatalf("ControlPlaneAPIEndpoints(%q) = %v, want success", tc.output, err)
			default:
				if !reflect.DeepEqual(endpoints, tc.want) {
					t.Errorf("endpoints = %v, want %v", endpoints, tc.want)
				}
			}
		})
	}

	// The extra attachment cannot corroborate an endpoint: with the
	// dual-stack-plus-extra attachment map above, neither of the
	// extra network's addresses is in the returned set, so an
	// endpoint address that names one is refused by the caller's
	// membership check exactly like any other foreign address.
	t.Run("the extra attachment's addresses are not in the set", func(t *testing.T) {
		rec := &dockerRecorder{respond: func(_ context.Context, args []string) ([]byte, error) {
			switch args[0] {
			case "ps":
				return []byte("container-id-123\n"), nil
			case "inspect":
				return []byte(dualStackExtra + "\n"), nil
			}
			return nil, fmt.Errorf("unexpected docker call %v", args)
		}}
		p := &Provisioner{log: slog.Default(), docker: rec.run}
		endpoints, err := p.ControlPlaneAPIEndpoints(context.Background(), "c1")
		if err != nil {
			t.Fatalf("ControlPlaneAPIEndpoints = %v, want success", err)
		}
		for _, foreign := range []netip.Addr{
			netip.MustParseAddr("172.19.0.2"),
			netip.MustParseAddr("fd00::2"),
		} {
			if slices.ContainsFunc(endpoints, func(ep netip.AddrPort) bool { return ep.Addr() == foreign }) {
				t.Errorf("the returned set %v contains %s, the extra attachment's address; a side network's address must not corroborate an endpoint", endpoints, foreign)
			}
		}
	})
}

// unsetenv removes name for the duration of the test and restores it
// afterwards: t.Setenv can only set a value, and the fresh-shell
// regression below needs the variable genuinely absent, not empty.
func unsetenv(t *testing.T, name string) {
	t.Helper()
	old, had := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(name, old)
			return
		}
		_ = os.Unsetenv(name)
	})
}

// TestControlPlaneAPIEndpointsUsesTheRecordedNetworkNotTheEnvironment:
// which network the corroboration reads is decided by the network
// Docker recorded when kind created the node (the --net of the
// create), never by this process's environment. Kind itself reads
// KIND_EXPERIMENTAL_DOCKER_NETWORK at provisioning time only;
// re-deriving the name in the verify shell can only guess, and both
// ways of guessing are pinned here as regressions: the variable set
// to a network the node is merely attached to as a side attachment
// (a selector that would choose that side attachment by name), and
// the variable absent altogether (the live failure the review
// demonstrated: clusters created with the override set, verified from
// a fresh shell without it — rejected as not attached to "kind").
func TestControlPlaneAPIEndpointsUsesTheRecordedNetworkNotTheEnvironment(t *testing.T) {
	t.Run("reads the recorded network's entry when the environment names a network the node is merely attached to", func(t *testing.T) {
		t.Setenv("KIND_EXPERIMENTAL_DOCKER_NETWORK", "kind")
		rec := &dockerRecorder{respond: func(_ context.Context, args []string) ([]byte, error) {
			switch args[0] {
			case "ps":
				return []byte("container-id-123\n"), nil
			case "inspect":
				return []byte(`{"mode":"custom","networks":{"custom":{"IPAddress":"10.10.0.2","GlobalIPv6Address":""},` +
					`"kind":{"IPAddress":"172.18.0.2","GlobalIPv6Address":""}}}` + "\n"), nil
			}
			return nil, fmt.Errorf("unexpected docker call %v", args)
		}}
		p := &Provisioner{log: slog.Default(), docker: rec.run}
		endpoints, err := p.ControlPlaneAPIEndpoints(context.Background(), "c1")
		if err != nil {
			t.Fatalf("ControlPlaneAPIEndpoints = %v, want success", err)
		}
		if want := []netip.AddrPort{netip.MustParseAddrPort("10.10.0.2:6443")}; !reflect.DeepEqual(endpoints, want) {
			t.Errorf("endpoints = %v, want %v (the recorded create network's entry, not the environment's pick)", endpoints, want)
		}
	})

	t.Run("reads the recorded network's entry from a shell without the variable", func(t *testing.T) {
		// A fresh verify shell: the variable kind read at create time
		// is absent here, and the node carries only its create-time
		// network — the exact input the environment-derived design
		// rejected a healthy cluster for.
		unsetenv(t, "KIND_EXPERIMENTAL_DOCKER_NETWORK")
		rec := &dockerRecorder{respond: func(_ context.Context, args []string) ([]byte, error) {
			switch args[0] {
			case "ps":
				return []byte("container-id-123\n"), nil
			case "inspect":
				return []byte(`{"mode":"custom","networks":{"custom":{"IPAddress":"10.10.0.2","GlobalIPv6Address":""}}}` + "\n"), nil
			}
			return nil, fmt.Errorf("unexpected docker call %v", args)
		}}
		p := &Provisioner{log: slog.Default(), docker: rec.run}
		endpoints, err := p.ControlPlaneAPIEndpoints(context.Background(), "c1")
		if err != nil {
			t.Fatalf("ControlPlaneAPIEndpoints = %v, want success", err)
		}
		if want := []netip.AddrPort{netip.MustParseAddrPort("10.10.0.2:6443")}; !reflect.DeepEqual(endpoints, want) {
			t.Errorf("endpoints = %v, want %v (the recorded create network's entry, with no environment to consult)", endpoints, want)
		}
	})

	t.Run("a node whose recorded network carries no address fails naming it, whatever the environment says", func(t *testing.T) {
		t.Setenv("KIND_EXPERIMENTAL_DOCKER_NETWORK", "kind")
		rec := &dockerRecorder{respond: func(_ context.Context, args []string) ([]byte, error) {
			switch args[0] {
			case "ps":
				return []byte("container-id-123\n"), nil
			case "inspect":
				return []byte(`{"mode":"custom","networks":{"custom":{"IPAddress":"","GlobalIPv6Address":""}}}` + "\n"), nil
			}
			return nil, fmt.Errorf("unexpected docker call %v", args)
		}}
		p := &Provisioner{log: slog.Default(), docker: rec.run}
		_, err := p.ControlPlaneAPIEndpoints(context.Background(), "c1")
		if err == nil || !strings.Contains(err.Error(), `reports no address on the network it was created on ("custom")`) {
			t.Fatalf("ControlPlaneAPIEndpoints = %v, want a refusal naming the recorded network", err)
		}
	})
}

// TestControlPlaneAPIEndpointsCancelledContextRunsNothing: like the
// probe, the resolution runs under its caller's context — a run that
// has given up resolves nothing, so no Docker invocation starts.
func TestControlPlaneAPIEndpointsCancelledContextRunsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := &dockerRecorder{}
	p := &Provisioner{log: slog.Default(), docker: rec.run}
	if _, err := p.ControlPlaneAPIEndpoints(ctx, "c1"); err == nil {
		t.Fatal("ControlPlaneAPIEndpoints with a cancelled context = nil, want the context error")
	}
	if calls := rec.recorded(); len(calls) != 0 {
		t.Errorf("docker calls = %v, want none under a cancelled context", calls)
	}
}

// The state walk's ancestor policy is what lets real temp-dir state
// roots work: t.TempDir() chains are caller-owned shared directories
// (0755 here), which pass the ancestor policy; the managed region below
// the store root is where the strict 0700 policy applies.
func TestTempDirChainPassesAncestorChecks(t *testing.T) {
	if err := fscheck.CheckAncestorDir(t.TempDir()); err != nil {
		t.Errorf("t.TempDir() failed the ancestor-dir check: %v", err)
	}
}

// TestRunDockerParsesStdoutOnly: the adapter's Docker boundary parses
// stdout alone. `docker ps` can exit 0 while printing a config warning
// to stderr — under a combined stream that warning shared the buffer
// the container ID was parsed from. The stub here prints both, on the
// streams they belong to; the runner must hand back stdout as the
// data and keep stderr out of it.
func TestRunDockerParsesStdoutOnly(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "docker")
	script := "#!/bin/sh\necho 'warning: a Docker config advice' >&2\necho container-id-123\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	out, err := runDocker(context.Background(), "ps", "--format", "{{.ID}}")
	if err != nil {
		t.Fatalf("runDocker = %v, want success (the stderr warning is not a failure)", err)
	}
	if id := firstLine(out); id != "container-id-123" {
		t.Errorf("parsed ID = %q, want the stdout line alone (container-id-123)", id)
	}
}

// TestRunDockerKeepsStderrForErrorContext: a failed invocation's
// stderr is the cause a human needs, so it rides along in the error
// instead of being mixed into the parsed data.
func TestRunDockerKeepsStderrForErrorContext(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "docker")
	script := "#!/bin/sh\necho 'cannot connect to the Docker daemon at unix:///var/run/docker.sock' >&2\nexit 1\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	out, err := runDocker(context.Background(), "ps")
	if err == nil || !strings.Contains(err.Error(), "cannot connect to the Docker daemon") {
		t.Fatalf("runDocker error = %v, want the stderr tail carried in it", err)
	}
	if len(out) != 0 {
		t.Errorf("stdout = %q, want none on a failed call", out)
	}
}

// TestBuildImageRunsOneDockerBuild: the image-building capability is
// exactly one docker build — the reference and the caller's context
// directory as separate argv elements, no shell, no cluster
// knowledge, no privileged flags. The reference and the directory are
// both caller-controlled and never parsed: they arrive as argv, the
// same rule the probe's endpoint follows.
func TestBuildImageRunsOneDockerBuild(t *testing.T) {
	rec := &dockerRecorder{}
	p := &Provisioner{log: slog.Default(), docker: rec.run}
	ctx := context.Background()
	if err := p.BuildImage(ctx, "localhost/kind-ocp/test:sha256-abcd", "/tmp/context-dir"); err != nil {
		t.Fatalf("BuildImage = %v, want success", err)
	}
	calls := rec.recorded()
	if len(calls) != 1 {
		t.Fatalf("docker calls = %v, want exactly one build", calls)
	}
	want := []string{"build", "--provenance=false", "-t", "localhost/kind-ocp/test:sha256-abcd", "/tmp/context-dir"}
	if !reflect.DeepEqual(calls[0], want) {
		t.Errorf("build argv = %v, want %v", calls[0], want)
	}
}

// TestBuildImageRefusesMissingInputs: an empty reference or context
// directory is refused before Docker is invoked at all — the caller's
// programming error, not a daemon invocation.
func TestBuildImageRefusesMissingInputs(t *testing.T) {
	rec := &dockerRecorder{}
	p := &Provisioner{log: slog.Default(), docker: rec.run}
	for _, tt := range []struct{ name, ref, dir string }{
		{"no reference", "", "/tmp/context-dir"},
		{"no context directory", "localhost/kind-ocp/test:sha256-abcd", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := p.BuildImage(context.Background(), tt.ref, tt.dir); err == nil {
				t.Fatalf("BuildImage(%q, %q) = nil, want the refusal", tt.ref, tt.dir)
			}
		})
	}
	if calls := rec.recorded(); len(calls) != 0 {
		t.Errorf("docker calls = %v, want none for refused inputs", calls)
	}
}

// TestBuildImageHonorsContextCancellation: the build runs under the
// caller's context — a cancelled run must not invoke Docker at all,
// and the cancellation surfaces as the error.
func TestBuildImageHonorsContextCancellation(t *testing.T) {
	rec := &dockerRecorder{}
	p := &Provisioner{log: slog.Default(), docker: rec.run}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.BuildImage(ctx, "localhost/kind-ocp/test:sha256-abcd", "/tmp/context-dir"); err == nil {
		t.Fatal("BuildImage on a cancelled context = nil, want the cancellation error")
	}
	if calls := rec.recorded(); len(calls) != 0 {
		t.Errorf("docker calls = %v, want none on a cancelled context", calls)
	}
}

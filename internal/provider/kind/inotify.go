package kind

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// The host inotify raise, ported from the PoC's raise_inotify. Running
// several kind clusters on one host multiplies the processes that hold
// inotify watches (every node's containerd, kubelet, and apiserver
// watch files), and the default fs.inotify.max_user_instances (often
// 128) can run out. The PoC raised it to 512 before creating anything;
// kind-ocp does the same, best-effort: a host kind-ocp cannot tune (no
// root, no sudo) must still get its clusters — the symptom of a short
// limit is a kube-apiserver that fails to start, which surfaces on its
// own — so the raise only ever logs.
const (
	// inotifyInstancesPath is the procfs file behind
	// fs.inotify.max_user_instances.
	inotifyInstancesPath = "/proc/sys/fs/inotify/max_user_instances"

	// inotifyInstancesKey is the same knob as a sysctl key, for the
	// sudo fallback (CI runners are non-root with passwordless sudo,
	// where the direct procfs write cannot work).
	inotifyInstancesKey = "fs.inotify.max_user_instances"

	// wantedInotifyInstances is the value the PoC settled on for a
	// Central cluster plus one secured cluster; it is a floor, never a
	// reduction: a host already above it is left alone.
	wantedInotifyInstances = 512
)

// sudoTimeout bounds the privileged fallback. sudo -n never prompts, but
// a slow or wedged sudo must not stall a create run on a hidden hang;
// the bound is the whole attempt's deadline. A variable, not a constant,
// so a regression test can observe it as the production bound.
var sudoTimeout = 10 * time.Second

// RaiseHostLimits implements provider.HostLimitRaiser: the inotify raise
// kind's node containers need before several clusters run on one host.
// On non-Linux platforms (macOS, a supported platform) the knob does not
// exist and the raise is skipped with a note; on Linux a host that
// cannot be raised (the direct procfs write and the sudo fallback both
// refused) is a warning, never a failure. create calls this once per
// run, before the first cluster is provisioned; a provisioner without
// the capability is never asked, so nothing but this real runtime ever
// changes host settings.
//
// The host it changes is the machine kind-ocp itself runs on — the CLI
// host — not necessarily the host the Docker daemon runs on: the
// platform check reads the CLI's own runtime.GOOS and the writes are
// local (procfs, sudo). With a remote Docker context (DOCKER_HOST
// pointing elsewhere) the node containers read the daemon host's
// fs.inotify.max_user_instances, while this raise tunes the CLI host's
// — harmless there, useless for the nodes, and the honest symptom (a
// kube-apiserver failing to start) still surfaces on its own.
func (p *Provisioner) RaiseHostLimits(ctx context.Context) {
	if runtime.GOOS != "linux" {
		p.log.Info("inotify raise skipped: fs.inotify.max_user_instances is a Linux knob and this host runs " + runtime.GOOS)
		return
	}
	err := raiseSysctl(ctx, inotifyInstancesPath, inotifyInstancesKey, writeProcfs, sudoSysctl)
	switch {
	case err == nil:
	case errors.Is(err, errSysctlSufficient):
		p.log.Info("inotify limit already sufficient", "fs.inotify.max_user_instances", wantedInotifyInstances)
	default:
		p.log.Warn("could not raise fs.inotify.max_user_instances (best-effort; multi-cluster hosts may need it)",
			"want", wantedInotifyInstances, "error", err)
	}
}

// errSysctlSufficient reports a value that already meets the floor, so
// the caller can report the raise as a no-op instead of silence.
var errSysctlSufficient = errors.New("current value already meets the wanted floor")

// raiseSysctl raises the integer knob at path to at least
// wantedInotifyInstances. It reads the current value first (never
// lowering a higher setting), then tries the direct write (the caller
// is root), then the privileged fallback (the caller is a sudoer on a
// CI runner). path, key, the direct write, and the fallback are
// parameters so the logic is unit-testable against an ordinary file
// with injected failures: whether the direct write works depends on the
// caller's UID and the file's permissions, which a portable test can
// neither arrange nor assume (a root runner writes through 0444).
func raiseSysctl(ctx context.Context, path, key string,
	write func(path string, value int) error,
	trySudo func(ctx context.Context, key string, value int) error,
) error {
	want := wantedInotifyInstances
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	cur, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	if cur >= want {
		return errSysctlSufficient
	}
	// Cancellation before any write: the tune changes host state, and
	// a run that has given up — or whose deadline passed while the
	// current value was being read — must not change host settings on
	// its way out. This covers the direct write, which cannot carry
	// the context itself; the sudo fallback runs under it too.
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("raising %s: %w", key, err)
	}
	// The direct write works when kind-ocp already runs as root. The
	// mode is irrelevant on procfs, which ignores it.
	if err := write(path, want); err == nil {
		return nil
	}
	if err := trySudo(ctx, key, want); err != nil {
		return fmt.Errorf("raising %s to %d (the direct write and the sudo fallback both failed): %w", key, want, err)
	}
	return nil
}

// writeProcfs is the direct write used in production: root writes the
// procfs knob regardless of its mode.
func writeProcfs(path string, value int) error {
	return os.WriteFile(path, []byte(strconv.Itoa(value)+"\n"), 0o644)
}

// sudoSysctl raises a sysctl through sudo's non-interactive mode. -n
// never prompts: a sudo that would ask for a password fails fast
// instead of hanging a create run on a hidden prompt, and the whole
// attempt carries its own deadline for the same reason.
func sudoSysctl(ctx context.Context, key string, value int) error {
	ctx, cancel := context.WithTimeout(ctx, sudoTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sudo", "-n", "sysctl", "-w", fmt.Sprintf("%s=%d", key, value))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sudo sysctl -w %s=%d: %w: %s", key, value, err, commandTail(out))
	}
	return nil
}

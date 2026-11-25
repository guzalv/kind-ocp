package kind

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// raiseSysctl is tested against an ordinary file standing in for the
// procfs knob, with the direct write and the privileged fallback
// injected: the read/compare/write semantics (never lower a value,
// write only when below the floor, fall back to the privileged path)
// do not depend on procfs itself, and whether the direct write
// succeeds depends on the caller's UID — unportable to arrange.
func TestRaiseSysctl(t *testing.T) {
	write := func(t *testing.T, path, value string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(t *testing.T, path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(b))
	}
	sudoRan := func(ran *bool) func(context.Context, string, int) error {
		return func(context.Context, string, int) error {
			*ran = true
			return nil
		}
	}

	t.Run("already sufficient leaves the file untouched", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "knob")
		write(t, path, "2048\n")
		var ran bool
		err := raiseSysctl(context.Background(), path, "test.knob", writeProcfs, sudoRan(&ran))
		if !errors.Is(err, errSysctlSufficient) {
			t.Fatalf("raiseSysctl = %v, want errSysctlSufficient", err)
		}
		if read(t, path) != "2048" {
			t.Errorf("knob = %s, want the higher value preserved (the raise is a floor, never a reduction)", read(t, path))
		}
		if ran {
			t.Error("the privileged fallback ran for an already-sufficient value")
		}
	})

	t.Run("below the floor writes the wanted value directly", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "knob")
		write(t, path, "128\n")
		err := raiseSysctl(context.Background(), path, "test.knob", writeProcfs,
			func(context.Context, string, int) error {
				return errors.New("sudo must not run when the direct write succeeded")
			})
		if err != nil {
			t.Fatalf("raiseSysctl = %v, want success", err)
		}
		if got := read(t, path); got != strconv.Itoa(wantedInotifyInstances) {
			t.Errorf("knob = %s, want %d", got, wantedInotifyInstances)
		}
	})

	t.Run("a failing direct write falls back to the privileged path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "knob")
		write(t, path, "128\n")
		var gotKey string
		var gotValue int
		err := raiseSysctl(context.Background(), path, "test.knob",
			func(string, int) error { return &os.PathError{Op: "write", Path: path, Err: os.ErrPermission} },
			func(_ context.Context, key string, value int) error {
				gotKey, gotValue = key, value
				return nil
			})
		if err != nil {
			t.Fatalf("raiseSysctl = %v, want the fallback to satisfy the raise", err)
		}
		if gotKey != "test.knob" || gotValue != 512 {
			t.Errorf("fallback raised (%q, %d), want (test.knob, 512)", gotKey, gotValue)
		}
	})

	t.Run("both paths failing is an error naming the knob", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "knob")
		write(t, path, "128\n")
		err := raiseSysctl(context.Background(), path, "test.knob",
			func(string, int) error { return errors.New("write refused") },
			func(context.Context, string, int) error { return errors.New("sudo: a password is required") })
		if err == nil || !strings.Contains(err.Error(), "test.knob") {
			t.Fatalf("raiseSysctl = %v, want an error naming the knob", err)
		}
	})

	t.Run("a cancelled context writes nothing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "knob")
		write(t, path, "128\n")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		err := raiseSysctl(ctx, path, "test.knob",
			func(string, int) error {
				t.Error("the direct write ran under a cancelled context")
				return nil
			},
			func(context.Context, string, int) error {
				t.Error("the sudo fallback ran under a cancelled context")
				return nil
			})
		if err == nil || !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("raiseSysctl = %v, want the cancellation error", err)
		}
		if got := read(t, path); got != "128" {
			t.Errorf("knob = %s, want 128 unchanged (no write under cancellation)", got)
		}
	})

	t.Run("an unreadable knob is an error, never a silent pass", func(t *testing.T) {
		err := raiseSysctl(context.Background(), filepath.Join(t.TempDir(), "missing"), "test.knob", writeProcfs, nil)
		if err == nil || !strings.Contains(err.Error(), "missing") {
			t.Fatalf("raiseSysctl = %v, want an error naming the unreadable knob", err)
		}
	})

	t.Run("a non-numeric knob is an error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "knob")
		write(t, path, "not-a-number\n")
		err := raiseSysctl(context.Background(), path, "test.knob", writeProcfs, nil)
		if err == nil || !strings.Contains(err.Error(), "invalid syntax") {
			t.Fatalf("raiseSysctl = %v, want a parse error", err)
		}
	})
}

// The Linux floor matches the PoC's raise_inotify: 512, and the knob it
// targets is fs.inotify.max_user_instances.
func TestInotifyKnobAndFloor(t *testing.T) {
	if inotifyInstancesPath != "/proc/sys/fs/inotify/max_user_instances" {
		t.Errorf("inotifyInstancesPath = %q, want the max_user_instances procfs file", inotifyInstancesPath)
	}
	if inotifyInstancesKey != "fs.inotify.max_user_instances" {
		t.Errorf("inotifyInstancesKey = %q, want the max_user_instances sysctl key", inotifyInstancesKey)
	}
	if wantedInotifyInstances != 512 {
		t.Errorf("wantedInotifyInstances = %d, want the PoC's 512", wantedInotifyInstances)
	}
	if sudoTimeout != 10*time.Second {
		t.Errorf("sudoTimeout = %s, want the 10s bound on the privileged fallback", sudoTimeout)
	}
}

// The sudo fallback must not run when its context is already cancelled:
// a create run that gives up mid-fallback must not leave a sudo process
// behind. exec.CommandContext refuses to start under a done context, so
// this exercises the plumbing without invoking sudo at all.
func TestSudoSysctlRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := sudoSysctl(ctx, "fs.inotify.max_user_instances", 512)
	if err == nil {
		t.Fatal("sudoSysctl with a cancelled context = nil, want an error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("sudoSysctl took %s under a cancelled context; it must fail fast", elapsed)
	}
}

// RaiseHostLimits on a non-Linux host is the platform note the
// integration runs observe on macOS: skipped with a note, never an
// attempt and never an error. On Linux the method attempts the real
// host knob, so its logic is unit-tested as raiseSysctl instead (and
// exercised for real by the integration run).
func TestRaiseHostLimitsNoteOnNonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("on Linux the raise attempts the real host knob")
	}
	var logs bytes.Buffer
	p := &Provisioner{log: slog.New(slog.NewTextHandler(&logs, nil))}
	p.RaiseHostLimits(context.Background())
	if !strings.Contains(logs.String(), "inotify raise skipped") {
		t.Errorf("logs = %q, want the inotify skip note on %s", logs.String(), runtime.GOOS)
	}
}

package testscratch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/guzalv/kind-ocp/internal/fscheck"
)

// restoreEnv snapshots the scratch-related environment and restores it
// when the test ends: hermetic() writes through os.Setenv (it runs from
// TestMain in real packages, where there is nothing to restore), so
// the tests here clean up after it themselves.
func restoreEnv(t *testing.T) {
	t.Helper()
	gotmpdir, hadGotmpdir := os.LookupEnv("GOTMPDIR")
	tmpdir, hadTmpdir := os.LookupEnv("TMPDIR")
	t.Cleanup(func() {
		if hadGotmpdir {
			_ = os.Setenv("GOTMPDIR", gotmpdir)
		} else {
			_ = os.Unsetenv("GOTMPDIR")
		}
		if hadTmpdir {
			_ = os.Setenv("TMPDIR", tmpdir)
		} else {
			_ = os.Unsetenv("TMPDIR")
		}
	})
}

// makeBadChain builds the cold-machine TMPDIR shape: a user-owned,
// group-writable, non-sticky directory (umask 002 leaves other
// tooling's directories this way). chmod applies the exact mode, so
// the ambient umask cannot soften it.
func makeBadChain(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "testscratch-badchain-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o775); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A TMPDIR chain the ancestor policy refuses must be re-pointed: the
// replacement is a 0700 directory directly under the system /tmp (the
// sticky bit is what the policy tolerates there), and every scratch
// base the tests can use is pointed at it.
func TestHermeticRepointsAPolicyRefusedChain(t *testing.T) {
	restoreEnv(t)
	bad := makeBadChain(t)
	_ = os.Setenv("TMPDIR", bad)
	_ = os.Unsetenv("GOTMPDIR")

	scratch := hermetic()
	if scratch == "" {
		t.Fatal("hermetic() = \"\", want a replacement scratch base for a group-writable TMPDIR")
	}
	t.Cleanup(func() { _ = os.RemoveAll(scratch) })

	if !strings.HasPrefix(scratch, "/tmp/") {
		t.Errorf("scratch = %q, want a directory directly under /tmp", scratch)
	}
	if got := os.Getenv("TMPDIR"); got != scratch {
		t.Errorf("TMPDIR = %q, want the scratch base %q", got, scratch)
	}
	if got := os.Getenv("GOTMPDIR"); got != scratch {
		t.Errorf("GOTMPDIR = %q, want the scratch base %q (t.TempDir builds on it)", got, scratch)
	}
	info, err := os.Stat(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("scratch mode = %04o, want 0700", perm)
	}
	// The replacement itself must satisfy the policy the ambient chain
	// failed — otherwise the re-point is theater.
	if _, err := fscheck.ResolveDir(scratch); err != nil {
		t.Errorf("ResolveDir(scratch) = %v, want the replacement chain accepted", err)
	}
	// And the re-point must not weaken what the policy refuses: a
	// directory under the chain that triggered it still fails
	// ResolveDir afterwards. The helper fixes where tests scratch,
	// not what the boundary refuses — the wired packages' own
	// refusal regressions pin that end to end.
	refused := filepath.Join(bad, "p")
	if err := os.Mkdir(refused, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := fscheck.ResolveDir(refused); err == nil {
		t.Error("ResolveDir below the refused chain = nil after the re-point, want the refusal to stand")
	}
}

// On a healthy ambient chain nothing changes: no re-point, no
// directory created, no environment touched.
func TestHermeticLeavesAHealthyChainAlone(t *testing.T) {
	restoreEnv(t)
	// A 0700 base directly under /tmp is accepted whatever the ambient
	// TMPDIR looks like (under a bad ambient TMPDIR, a fixture created
	// there would inherit the refused chain).
	base, err := os.MkdirTemp("/tmp", "testscratch-healthy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.Setenv("TMPDIR", base)
	_ = os.Unsetenv("GOTMPDIR")

	if scratch := hermetic(); scratch != "" {
		t.Cleanup(func() { _ = os.RemoveAll(scratch) })
		t.Fatalf("hermetic() = %q, want no replacement on a healthy chain", scratch)
	}
	if got := os.Getenv("TMPDIR"); got != base {
		t.Errorf("TMPDIR = %q, want it untouched at %q", got, base)
	}
}

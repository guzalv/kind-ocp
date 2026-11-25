//go:build linux

package fscheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// aclTool runs setfacl/getfacl; the tests skip when the acl package is
// not installed (it provides both).
func aclTool(t *testing.T, name string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s is not installed; skipping the POSIX ACL regression (%v)", name, err)
	}
	cmd := exec.Command(name, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v (%s)", name, args, err, out)
	}
	return string(out)
}

// modeOf returns path's permission bits, pinning the premise every
// test below stands on: on Linux the POSIX ACL mask is the mode's
// group bits.
func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// On Linux the POSIX ACL mask is the mode's group bits, so the mode
// policy and the ACL story are one story: an effective named grant
// widens the group bits and the mode checks refuse it, a grant the
// mask leaves ineffective grants nothing. These regressions pin that
// equivalence on real ACLs.

// A named write grant on a 0600 file is effective exactly when it
// widens the mode: setfacl -m u:4242:rw recalculates the mask to rw,
// the file reads 0660, and the managed-file policy refuses it as a
// mode violation.
func TestManagedFileRefusesEffectivePosixGrant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	makeFile(t, path, 0o600)
	aclTool(t, "setfacl", "-m", "u:4242:rw", path)
	if perm := modeOf(t, path); perm != 0o660 {
		t.Fatalf("mode after setfacl = %04o, want 0660 — the ACL mask must show in the group bits for the mode policy to cover it", perm)
	}
	wantErr(t, CheckPrivateFile(path), "mode")

	aclTool(t, "setfacl", "-b", path)
	if err := CheckPrivateFile(path); err != nil {
		t.Errorf("CheckPrivateFile after setfacl -b = %v, want nil", err)
	}
}

// A grant the mask leaves ineffective grants nothing: a directory
// created 0700 under a default ACL inherits the named entry, but the
// creation mode's group bits mask it to nothing and the directory
// still reads 0700. It is safe, and the managed-dir policy must accept
// it — refusing the entry itself would break create for every user
// templating directories with default ACLs.
func TestManagedDirAcceptsMaskedInheritedGrant(t *testing.T) {
	parent := makeDir(t, filepath.Join(t.TempDir(), "templating"), 0o755)
	aclTool(t, "setfacl", "-d", "-m", "u:4242:rw", parent)
	child := filepath.Join(parent, "state")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	// The named entry is really there — this regression is about a
	// masked entry, not a missing one.
	if out := aclTool(t, "getfacl", "-cp", child); !strings.Contains(out, "user:4242:") {
		t.Fatalf("getfacl output must carry the inherited named entry, got:\n%s", out)
	}
	if perm := modeOf(t, child); perm != 0o700 {
		t.Fatalf("child mode = %04o, want 0700 — the inherited grant must be masked by the creation mode", perm)
	}
	if err := CheckManagedDir(child); err != nil {
		t.Errorf("CheckManagedDir(masked inherited grant) = %v, want nil (the grant is ineffective)", err)
	}
}

// The same equivalence on ancestors: a write grant widens the group
// bits (0775) and the ancestor policy refuses it as group-writable,
// while a read grant leaves the mode where it was.
func TestAncestorDirPosixGrants(t *testing.T) {
	t.Run("write grant is refused through the mode", func(t *testing.T) {
		d := makeDir(t, filepath.Join(t.TempDir(), "writable"), 0o755)
		aclTool(t, "setfacl", "-m", "u:4242:rw", d)
		if perm := modeOf(t, d); perm != 0o775 {
			t.Fatalf("mode after setfacl = %04o, want 0775", perm)
		}
		wantErr(t, CheckAncestorDir(d), "group-writable")
	})
	t.Run("read grant keeps the mode and passes", func(t *testing.T) {
		d := makeDir(t, filepath.Join(t.TempDir(), "readable"), 0o755)
		aclTool(t, "setfacl", "-m", "u:4242:r", d)
		if perm := modeOf(t, d); perm != 0o755 {
			t.Fatalf("mode after setfacl = %04o, want 0755", perm)
		}
		if err := CheckAncestorDir(d); err != nil {
			t.Errorf("CheckAncestorDir(read grant) = %v, want nil (mode already permits world read)", err)
		}
	})
}

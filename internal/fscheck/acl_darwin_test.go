//go:build darwin

package fscheck

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// addACL adds one extended ACL entry with /bin/chmod's documented +a
// form. BSD chmod is used explicitly: a GNU coreutils chmod may sit
// earlier on PATH and does not understand ACL entries.
func addACL(t *testing.T, path, entry string) {
	t.Helper()
	if out, err := exec.Command("/bin/chmod", "+a", entry, path).CombinedOutput(); err != nil {
		t.Fatalf("chmod +a %q %s: %v (%s)", entry, path, err, out)
	}
}

// removeACLs strips every extended ACL entry from path.
func removeACLs(t *testing.T, path string) {
	t.Helper()
	if out, err := exec.Command("/bin/chmod", "-N", path).CombinedOutput(); err != nil {
		t.Fatalf("chmod -N %s: %v (%s)", path, err, out)
	}
}

// A mode-0600 file with an "everyone allow read,write" ACE is
// world-readable in fact: mode bits alone do not describe access on
// macOS. The managed-file policy must refuse it (the mode check passes,
// so the refusal can only come from the ACL check).
func TestCheckPrivateFileRefusesACLEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	makeFile(t, path, 0o600)
	addACL(t, path, "everyone allow read,write")
	wantErr(t, CheckPrivateFile(path), "ACL")

	// Stripping the entries restores the pass, so the refusal is about
	// the ACL, not an unusable file.
	removeACLs(t, path)
	if err := CheckPrivateFile(path); err != nil {
		t.Errorf("CheckPrivateFile after chmod -N = %v, want nil", err)
	}
}

func TestCheckManagedDirRefusesACLEntries(t *testing.T) {
	d := makeDir(t, filepath.Join(t.TempDir(), "state"), 0o700)
	addACL(t, d, "everyone allow read,write")
	wantErr(t, CheckManagedDir(d), "ACL")
	removeACLs(t, d)
	if err := CheckManagedDir(d); err != nil {
		t.Errorf("CheckManagedDir after chmod -N = %v, want nil", err)
	}
}

// The ancestor policy tolerates the entries ordinary macOS system
// directories carry — home directories ship a "group:everyone deny
// delete" entry — and refuses an entry that grants write access beyond
// the mode bits.
func TestCheckAncestorDirACLs(t *testing.T) {
	t.Run("deny-only entry is tolerated", func(t *testing.T) {
		d := makeDir(t, filepath.Join(t.TempDir(), "home-like"), 0o750)
		addACL(t, d, "everyone deny delete")
		// The deny-delete entry also blocks the temp-dir cleanup from
		// removing d — itself proof the entry took effect — so strip it
		// once the assertion is done.
		defer removeACLs(t, d)
		if err := CheckAncestorDir(d); err != nil {
			t.Errorf("CheckAncestorDir(deny-delete entry) = %v, want nil (the macOS home shape)", err)
		}
	})
	t.Run("read-only grant is tolerated", func(t *testing.T) {
		d := makeDir(t, filepath.Join(t.TempDir(), "readable"), 0o755)
		addACL(t, d, "everyone allow read")
		if err := CheckAncestorDir(d); err != nil {
			t.Errorf("CheckAncestorDir(read grant) = %v, want nil (mode already permits world read)", err)
		}
	})
	t.Run("write grant is refused", func(t *testing.T) {
		d := makeDir(t, filepath.Join(t.TempDir(), "writable"), 0o755)
		addACL(t, d, "everyone allow add_file")
		wantErr(t, CheckAncestorDir(d), "ACL")
	})
	t.Run("ACL rewrite grant is refused", func(t *testing.T) {
		d := makeDir(t, filepath.Join(t.TempDir(), "chmodable"), 0o755)
		addACL(t, d, "everyone allow writesecurity")
		wantErr(t, CheckAncestorDir(d), "ACL")
	})
}

// Ordinary system ancestors keep passing with the ACL checks in place:
// the real home directory (which carries the deny-delete entry on a
// default macOS installation) and the temp-dir chain used by tests.
func TestSystemAncestorsPassACLChecks(t *testing.T) {
	if home, err := os.UserHomeDir(); err == nil {
		if err := CheckAncestorDir(home); err != nil {
			t.Errorf("CheckAncestorDir(%s) = %v, want nil", home, err)
		}
	}
	if err := CheckAncestorDir(t.TempDir()); err != nil {
		t.Errorf("CheckAncestorDir(t.TempDir()) = %v, want nil", err)
	}
}

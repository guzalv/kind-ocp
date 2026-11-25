package fscheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The state tree's boundary, offered as a standalone verified directory:
// every test here mirrors a rule the state store's walks pin, through the
// generic entry point other in-repo tools (the StackRox example's
// credential workspace) use.

func TestOpenManagedDirCreatesAndAdopts(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "work", "per-prefix")

	got, err := OpenManagedDir(dir)
	if err != nil {
		t.Fatalf("OpenManagedDir = %v", err)
	}
	info, err := os.Stat(got)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("created dir mode = (%v, %v), want 0700", info, err)
	}
	// A second open adopts the directory the first one left behind —
	// a credential workspace persists across runs.
	again, err := OpenManagedDir(dir)
	if err != nil || again != got {
		t.Errorf("re-open = (%q, %v), want (%q, nil)", again, err, got)
	}
}

func TestOpenManagedDirRefusesAPermissiveExistingDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenManagedDir(dir); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("OpenManagedDir = %v, want a mode rejection", err)
	}
}

func TestOpenManagedDirRefusesASymlinkedDir(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "work")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	// Even a private, caller-owned target: the managed region refuses
	// the symlink itself.
	if _, err := OpenManagedDir(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("OpenManagedDir = %v, want a symlink rejection", err)
	}
}

// A directory another local user can write (world-writable, no sticky
// bit) can substitute everything below it, so it is refused as an
// ancestor even though the managed leaf itself is private.
func TestOpenManagedDirRefusesASubstitutableAncestor(t *testing.T) {
	base := t.TempDir()
	if err := os.Chmod(base, 0o777); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(base, 0o700) }()
	if _, err := OpenManagedDir(filepath.Join(base, "work")); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("OpenManagedDir = %v, want a substitutable-ancestor rejection", err)
	}
}

// A symlinked ancestor the caller owns is resolved hop by hop, and the
// resolved target is verified: a private target works (macOS $TMPDIR
// lives under the /var symlink), a substitutable one does not.
func TestOpenManagedDirResolvesTrustedAncestorSymlinks(t *testing.T) {
	t.Run("symlink to a private directory works", func(t *testing.T) {
		base := t.TempDir()
		target := filepath.Join(base, "real-home")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "alias-home")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		got, err := OpenManagedDir(filepath.Join(link, "work"))
		if err != nil {
			t.Fatalf("OpenManagedDir through an alias = %v", err)
		}
		// The walk resolves the whole chain — the /var alias under a
		// macOS temp dir included — so compare against the target's
		// own resolved form.
		resolved, err := filepath.EvalSymlinks(target)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(resolved, "work"); got != want {
			t.Errorf("resolved dir = %q, want the alias target %q", got, want)
		}
	})
	t.Run("symlink to a substitutable directory is refused", func(t *testing.T) {
		base := t.TempDir()
		target := filepath.Join(base, "shared")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		// Chmod, not Mkdir: the mode bits survive the umask this way.
		if err := os.Chmod(target, 0o777); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "alias-home")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenManagedDir(filepath.Join(link, "work")); err == nil || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("OpenManagedDir through an alias = %v, want a target rejection", err)
		}
	})
}

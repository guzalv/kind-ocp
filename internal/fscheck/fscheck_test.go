package fscheck

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeFile writes a file with exactly the requested mode (WriteFile only
// applies the mode on create, so chmod afterwards defeats the umask).
func makeFile(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte("content"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// makeDir creates a directory with exactly the requested mode.
func makeDir(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	if err := os.Mkdir(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// wantErr asserts that err is non-nil and its message mentions the reason.
func wantErr(t *testing.T, err error, reason string) {
	t.Helper()
	if err == nil {
		t.Errorf("error = nil, want a rejection mentioning %q", reason)
		return
	}
	if !strings.Contains(err.Error(), reason) {
		t.Errorf("error = %v, want it to mention %q", err, reason)
	}
}

func TestCheckPrivateFile(t *testing.T) {
	dir := t.TempDir()

	t.Run("private file passes", func(t *testing.T) {
		path := filepath.Join(dir, "private")
		makeFile(t, path, 0o600)
		if err := CheckPrivateFile(path); err != nil {
			t.Errorf("CheckPrivateFile(0600 file) = %v, want nil", err)
		}
	})

	t.Run("group-readable file is rejected", func(t *testing.T) {
		path := filepath.Join(dir, "group")
		makeFile(t, path, 0o640)
		wantErr(t, CheckPrivateFile(path), "mode")
	})

	t.Run("world-readable file is rejected", func(t *testing.T) {
		path := filepath.Join(dir, "world")
		makeFile(t, path, 0o644)
		wantErr(t, CheckPrivateFile(path), "mode")
	})

	t.Run("symlink is rejected", func(t *testing.T) {
		target := filepath.Join(dir, "private")
		makeFile(t, target, 0o600)
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		wantErr(t, CheckPrivateFile(link), "symlink")
	})

	t.Run("directory is rejected", func(t *testing.T) {
		wantErr(t, CheckPrivateFile(dir), "not a regular file")
	})

	t.Run("missing file reports not-exist", func(t *testing.T) {
		err := CheckPrivateFile(filepath.Join(dir, "absent"))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("CheckPrivateFile(absent) = %v, want fs.ErrNotExist", err)
		}
	})
}

func TestCheckManagedDir(t *testing.T) {
	dir := t.TempDir()

	t.Run("private dir passes", func(t *testing.T) {
		d := makeDir(t, filepath.Join(dir, "d700"), 0o700)
		if err := CheckManagedDir(d); err != nil {
			t.Errorf("CheckManagedDir(0700) = %v, want nil", err)
		}
	})

	t.Run("read-only private dir passes", func(t *testing.T) {
		d := makeDir(t, filepath.Join(dir, "d500"), 0o500)
		if err := CheckManagedDir(d); err != nil {
			t.Errorf("CheckManagedDir(0500) = %v, want nil (private, just not writable)", err)
		}
	})

	t.Run("world-readable dir is rejected", func(t *testing.T) {
		d := makeDir(t, filepath.Join(dir, "d755"), 0o755)
		wantErr(t, CheckManagedDir(d), "mode")
	})

	t.Run("group-writable dir is rejected", func(t *testing.T) {
		d := makeDir(t, filepath.Join(dir, "d770"), 0o770)
		wantErr(t, CheckManagedDir(d), "mode")
	})

	t.Run("symlink is rejected", func(t *testing.T) {
		target := makeDir(t, filepath.Join(dir, "target"), 0o700)
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		wantErr(t, CheckManagedDir(link), "symlink")
	})

	t.Run("file is rejected", func(t *testing.T) {
		path := filepath.Join(dir, "afile")
		makeFile(t, path, 0o600)
		wantErr(t, CheckManagedDir(path), "not a directory")
	})

	t.Run("missing dir reports not-exist", func(t *testing.T) {
		err := CheckManagedDir(filepath.Join(dir, "absent"))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("CheckManagedDir(absent) = %v, want fs.ErrNotExist", err)
		}
	})
}

func TestCheckAncestorDir(t *testing.T) {
	dir := t.TempDir()

	t.Run("shared read-only dir passes", func(t *testing.T) {
		d := makeDir(t, filepath.Join(dir, "a755"), 0o755)
		if err := CheckAncestorDir(d); err != nil {
			t.Errorf("CheckAncestorDir(0755) = %v, want nil", err)
		}
	})

	t.Run("private dir passes", func(t *testing.T) {
		d := makeDir(t, filepath.Join(dir, "a700"), 0o700)
		if err := CheckAncestorDir(d); err != nil {
			t.Errorf("CheckAncestorDir(0700) = %v, want nil", err)
		}
	})

	t.Run("group-writable dir is rejected", func(t *testing.T) {
		d := makeDir(t, filepath.Join(dir, "a770"), 0o770)
		wantErr(t, CheckAncestorDir(d), "group-writable")
		// The refusal must carry its remedy (§5.3: a cold machine
		// hits this with no way forward otherwise) and be classifiable
		// as an ancestor-policy refusal, so the state layer can offer
		// the state-root alternative on top of it.
		err := CheckAncestorDir(d)
		if !errors.Is(err, ErrUnsafeAncestor) {
			t.Errorf("CheckAncestorDir(0770) = %v, want an ErrUnsafeAncestor chain", err)
		}
		if !strings.Contains(err.Error(), "chmod g-w "+d) {
			t.Errorf("CheckAncestorDir(0770) = %v, want the chmod g-w remedy naming %s", err, d)
		}
	})

	t.Run("group- and world-writable dir is rejected with one fix", func(t *testing.T) {
		// 0777 sets both write bits: the remedy must clear them
		// together — chmod g-w alone would leave 0757, still
		// world-writable — and applying the printed command must
		// leave the directory acceptable to the policy.
		d := makeDir(t, filepath.Join(dir, "a777"), 0o777)
		err := CheckAncestorDir(d)
		wantErr(t, err, "group- and world-writable")
		if !errors.Is(err, ErrUnsafeAncestor) {
			t.Errorf("CheckAncestorDir(0777) = %v, want an ErrUnsafeAncestor chain", err)
		}
		if !strings.Contains(err.Error(), "chmod go-w "+d) {
			t.Errorf("CheckAncestorDir(0777) = %v, want the chmod go-w remedy naming %s", err, d)
		}
		if err := os.Chmod(d, 0o777&^0o022); err != nil {
			t.Fatal(err)
		}
		if err := CheckAncestorDir(d); err != nil {
			t.Errorf("CheckAncestorDir after the recommended fix = %v, want nil", err)
		}
	})

	t.Run("spaced path remedy is quoted", func(t *testing.T) {
		// The remedy must paste into a shell as printed: a path
		// containing spaces is rendered as a single-quoted word.
		d := makeDir(t, filepath.Join(dir, "has space"), 0o775)
		err := CheckAncestorDir(d)
		wantErr(t, err, "group-writable")
		if !strings.Contains(err.Error(), "chmod g-w '"+d+"'") {
			t.Errorf("CheckAncestorDir(0775 spaced) = %v, want the chmod g-w remedy single-quoting %s", err, d)
		}
	})

	t.Run("world-writable-only dir is rejected with chmod o-w", func(t *testing.T) {
		// 0002 carries no group-write bit, so this pins the
		// world-writable branch and its remedy exactly.
		d := makeDir(t, filepath.Join(dir, "a002"), 0o002)
		err := CheckAncestorDir(d)
		wantErr(t, err, "world-writable")
		if !errors.Is(err, ErrUnsafeAncestor) {
			t.Errorf("CheckAncestorDir(0002) = %v, want an ErrUnsafeAncestor chain", err)
		}
		if !strings.Contains(err.Error(), "chmod o-w "+d) {
			t.Errorf("CheckAncestorDir(0002) = %v, want the chmod o-w remedy naming %s", err, d)
		}
	})
	t.Run("sticky world-writable dir passes", func(t *testing.T) {
		// The /tmp shape: world-writable, but the sticky bit keeps
		// another user from substituting entries. The sticky bit lives
		// in os.FileMode's high bits, not in Perm().
		d := makeDir(t, filepath.Join(dir, "a1777"), 0o777|fs.ModeSticky)
		if err := CheckAncestorDir(d); err != nil {
			t.Errorf("CheckAncestorDir(1777) = %v, want nil (sticky bit)", err)
		}
	})

	t.Run("symlink is rejected", func(t *testing.T) {
		target := makeDir(t, filepath.Join(dir, "atarget"), 0o700)
		link := filepath.Join(dir, "alink")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		wantErr(t, CheckAncestorDir(link), "symlink")
	})

	t.Run("file is rejected", func(t *testing.T) {
		path := filepath.Join(dir, "afile")
		makeFile(t, path, 0o600)
		wantErr(t, CheckAncestorDir(path), "not a directory")
	})
}

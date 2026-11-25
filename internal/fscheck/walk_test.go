package fscheck

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The state tree's boundary, exercised at its own level: the tests here
// pin the resolution walk (ResolveDir — the look-before-you-manage
// companion) and the name guard every walk shares (WalkDirNames). The
// creating and no-alias walks' policies are pinned where their callers
// live: the state store's walks in internal/state, and the StackRox
// example's derived workspace in examples/stackrox.

// ResolveDir never writes and never adopts: it answers where an existing
// directory sits. The three refusals below mirror the walk's ancestor
// rules — the resolution must not depend on entries another local user
// can substitute.
func TestResolveDirResolvesWithoutCreating(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "real-home", "state")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "alias-home")
	if err := os.Symlink(filepath.Dir(target), link); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveDir(filepath.Join(link, "state"))
	if err != nil {
		t.Fatalf("ResolveDir through an alias = %v", err)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if got != resolved {
		t.Errorf("ResolveDir = %q, want the resolved target %q", got, resolved)
	}

	// A missing directory is the one honest "cannot resolve": the
	// comparison a caller needs cannot be made up for a path that does
	// not exist.
	if _, err := ResolveDir(filepath.Join(base, "absent")); err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ResolveDir on a missing dir = %v, want an fs.ErrNotExist error", err)
	}
	if _, err := ResolveDir(filepath.Join(base, "alias-home", "absent")); err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ResolveDir through an alias at a missing dir = %v, want an fs.ErrNotExist error", err)
	}

	// Nothing was created along the way.
	if _, err := os.Lstat(filepath.Join(base, "absent")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ResolveDir created %q, want no writes", filepath.Join(base, "absent"))
	}
}

func TestResolveDirRefusesUntrustedChains(t *testing.T) {
	base := t.TempDir()
	shared := filepath.Join(base, "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	// Chmod, not Mkdir: the mode bits survive the umask this way.
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	t.Run("substitutable target component", func(t *testing.T) {
		if _, err := ResolveDir(filepath.Join(shared, "dir")); err == nil || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("ResolveDir below a substitutable dir = %v, want a rejection", err)
		}
	})
	t.Run("not a directory", func(t *testing.T) {
		file := filepath.Join(base, "plain-file")
		if err := os.WriteFile(file, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveDir(file); err == nil || !strings.Contains(err.Error(), "not a directory") {
			t.Fatalf("ResolveDir on a file = %v, want a type rejection", err)
		}
	})
}

// resolvedAnchor is a symlink-free anchor for walk tests: the resolved
// form of the test's temp directory, the way a caller gets one from
// ResolveDir's output.
func resolvedAnchor(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// WalkDirNames confines itself to the names it is given: a component
// that could escape the walk — a parent reference, a separator, an
// empty name — is refused before anything on disk is touched. The
// store's and the example's callers pass components that cannot take
// these shapes; this pins the guard for the shapes no caller can
// produce.
func TestWalkDirNamesRejectsNamesThatEscapeTheWalk(t *testing.T) {
	anchor := resolvedAnchor(t)
	anchorNames := PathNames(anchor)
	for _, name := range []string{"..", "a/b", ""} {
		names := append(append([]string(nil), anchorNames...), name)
		if _, err := WalkDirNames(names, len(anchorNames), WalkOptions{Create: true}); err == nil {
			t.Errorf("WalkDirNames with component %q succeeded, want a rejection", name)
		}
	}
	// Nothing was created by the refused walks.
	if entries, err := os.ReadDir(anchor); err != nil || len(entries) != 0 {
		t.Errorf("the anchor holds %v (%v), want nothing — refused walks do not write", entries, err)
	}
}

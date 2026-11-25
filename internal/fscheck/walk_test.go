package fscheck

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The state tree's boundary, offered as a standalone verified directory:
// every test here mirrors a rule the state store's walks pin, through the
// generic entry point other in-repo tools (the StackRox example's
// credential workspace) use.

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

// resolvedAnchor is a symlink-free anchor for OpenDerivedDir tests: the
// resolved form of the test's temp directory, the way a caller gets one
// from ResolveDir's output.
func resolvedAnchor(t *testing.T) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func TestOpenDerivedDirCreatesAndAdopts(t *testing.T) {
	anchor := resolvedAnchor(t)

	got, err := OpenDerivedDir(anchor, []string{"state-suffix", "prefix"})
	if err != nil {
		t.Fatalf("OpenDerivedDir = %v", err)
	}
	if want := filepath.Join(anchor, "state-suffix", "prefix"); got != want {
		t.Errorf("OpenDerivedDir = %q, want %q", got, want)
	}
	info, err := os.Stat(got)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Errorf("created dir mode = (%v, %v), want 0700", info, err)
	}
	// A second open adopts the tree the first one left behind — a
	// credential workspace persists across runs.
	again, err := OpenDerivedDir(anchor, []string{"state-suffix", "prefix"})
	if err != nil || again != got {
		t.Errorf("re-open = (%q, %v), want (%q, nil)", again, err, got)
	}
}

// The derived names are the caller's own choice, so an alias among them
// is redirection, not resolution: the walk refuses it wherever it
// points, at the derived root and below it alike.
func TestOpenDerivedDirRefusesAliasesAmongTheDerivedNames(t *testing.T) {
	anchor := resolvedAnchor(t)
	target := filepath.Join(anchor, "elsewhere")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Run("alias at the derived root", func(t *testing.T) {
		if err := os.Symlink(target, filepath.Join(anchor, "state-suffix")); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenDerivedDir(anchor, []string{"state-suffix", "prefix"}); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("OpenDerivedDir through an aliased derived root = %v, want a symlink rejection", err)
		}
	})
	t.Run("alias below the derived root", func(t *testing.T) {
		if err := os.Mkdir(filepath.Join(anchor, "other-root"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(anchor, "other-root", "prefix")); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenDerivedDir(anchor, []string{"other-root", "prefix"}); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("OpenDerivedDir through an aliased derived leaf = %v, want a symlink rejection", err)
		}
	})
}

// The anchor's components keep the ancestor policy: a substitutable
// ancestor (world-writable, no sticky bit) can substitute everything
// below it, so the derived tree is refused even though its own
// components would be private.
func TestOpenDerivedDirRefusesASubstitutableAnchorComponent(t *testing.T) {
	anchor := resolvedAnchor(t)
	shared := filepath.Join(anchor, "shared")
	if err := os.Mkdir(shared, 0o700); err != nil {
		t.Fatal(err)
	}
	// Chmod, not Mkdir: the mode bits survive the umask this way.
	if err := os.Chmod(shared, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDerivedDir(shared, []string{"work"}); err == nil || !strings.Contains(err.Error(), "writable") {
		t.Fatalf("OpenDerivedDir below a substitutable anchor component = %v, want a rejection", err)
	}
}

// A symlink in the anchor chain is a substitution since the chain was
// resolved, and unlike OpenManagedDir the derived walk does not follow
// even a trusted one: the derivation already paid for the resolution,
// so the chain is entered exactly as it was resolved or not at all.
// This is what makes a derived tree's location independent of every
// alias — including one planted between the resolution and the open.
func TestOpenDerivedDirRefusesASymlinkedAnchorComponent(t *testing.T) {
	anchor := resolvedAnchor(t)
	target := filepath.Join(anchor, "real")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(anchor, "alias")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := OpenDerivedDir(link, []string{"work"})
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("OpenDerivedDir through a symlinked anchor = %v, want a symlink rejection", err)
	}
	// Nothing was created through the alias.
	if _, err := os.Lstat(filepath.Join(target, "work")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenDerivedDir created %q through the alias, want no writes", filepath.Join(target, "work"))
	}
}

func TestOpenDerivedDirRefusesAPermissiveDerivedDir(t *testing.T) {
	anchor := resolvedAnchor(t)
	dir := filepath.Join(anchor, "state-suffix", "prefix")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDerivedDir(anchor, []string{"state-suffix", "prefix"}); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("OpenDerivedDir on a permissive derived dir = %v, want a mode rejection", err)
	}
}

func TestOpenDerivedDirRejectsNamesThatEscapeTheAnchor(t *testing.T) {
	anchor := resolvedAnchor(t)
	for _, name := range []string{"..", "a/b", ""} {
		if _, err := OpenDerivedDir(anchor, []string{name}); err == nil {
			t.Errorf("OpenDerivedDir with derived name %q succeeded, want a rejection", name)
		}
	}
	if _, err := OpenDerivedDir(anchor, nil); err == nil {
		t.Error("OpenDerivedDir with no derived names succeeded, want a rejection")
	}
}

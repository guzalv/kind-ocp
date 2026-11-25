package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stackrox/kind-ocp/internal/fscheck"
)

func TestOpenIsLazyAndEnsureCreatesPerms(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nested", "state")
	store := Open(root)
	// Open must not create anything: opening a store for a prefix that has
	// no clusters is side-effect free (delete of a nonexistent prefix).
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatalf("Open created %s; it must be lazy", root)
	}
	if _, err := store.ensureClusterDir("p-1"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("state dir perms = %o, want 0700", perm)
	}
	cluster, err := os.Stat(store.clusterDir("p-1"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := cluster.Mode().Perm(); perm != 0o700 {
		t.Errorf("cluster dir perms = %o, want 0700", perm)
	}
}

func TestRecordRoundtrip(t *testing.T) {
	store := Open(filepath.Join(t.TempDir(), "state"))
	rec := Record{
		Name:          "kind-ocp-1",
		Context:       "kind-kind-ocp-1",
		Profile:       "full",
		Layers:        []string{"olm"},
		KindNodeImage: "kindest/node:v1.36.4@sha256:abc",
		Status:        StatusComplete,
		CreatedAt:     time.Now().UTC().Truncate(time.Second),
		InstanceID:    "instance-42",
	}
	if err := store.WriteRecord(rec); err != nil {
		t.Fatal(err)
	}

	// Record file must be 0600.
	info, err := os.Stat(store.recordPath(rec.Name))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("record perms = %o, want 0600", perm)
	}

	got, found, err := store.Record(rec.Name)
	if err != nil || !found {
		t.Fatalf("Record() = (%v, %v, %v), want (rec, true, nil)", got, found, err)
	}
	if got.Name != rec.Name || got.Context != rec.Context || got.Profile != rec.Profile ||
		len(got.Layers) != 1 || got.Layers[0] != "olm" || got.Status != StatusComplete ||
		got.CreatedAt != rec.CreatedAt || got.InstanceID != "instance-42" {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Absent record: found=false, not an error.
	_, found, err = store.Record("nope")
	if err != nil || found {
		t.Errorf("Record(nope) = (%v, %v), want (false, nil)", found, err)
	}
}

// The expected-cluster count create persists round-trips through the
// sidecar file beside the marker, and a tree that never carried one
// (an older binary's, or a missing tree) reports its absence instead
// of an error — verify degrades to a SKIP on that, never a false FAIL.
func TestExpectedClustersRoundTrip(t *testing.T) {
	store := Open(filepath.Join(t.TempDir(), "state"))
	if _, err := store.EnsureRoot(); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.ExpectedClusters(); err != nil || found {
		t.Fatalf("ExpectedClusters on a fresh root = (%v, %v), want (false, nil)", found, err)
	}
	if err := store.SetExpectedClusters(2); err != nil {
		t.Fatal(err)
	}
	got, found, err := store.ExpectedClusters()
	if err != nil || !found || got != 2 {
		t.Fatalf("ExpectedClusters after SetExpectedClusters(2) = (%d, %v, %v), want (2, true, nil)", got, found, err)
	}
	// An absent tree is no expectation, not an error.
	other := Open(filepath.Join(t.TempDir(), "gone"))
	if _, found, err := other.ExpectedClusters(); err != nil || found {
		t.Fatalf("ExpectedClusters on an absent tree = (%v, %v), want (false, nil)", found, err)
	}
}

// The count write never touches the marker: the marker is the
// ownership proof delete and every walk rely on, so recording an
// expectation must leave it byte-for-byte as it was (the count lives
// in its own file), and the sidecar it writes is private.
func TestSetExpectedClustersDoesNotTouchTheMarker(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store := Open(root)
	if _, err := store.EnsureRoot(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, markerName)
	if err := os.WriteFile(path, []byte(`{"tool":"kind-ocp","futureField":"keep me"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetExpectedClusters(3); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("the marker changed across the count write:\nbefore %s\nafter  %s", before, after)
	}
	if got, found, err := store.ExpectedClusters(); err != nil || !found || got != 3 {
		t.Fatalf("ExpectedClusters after SetExpectedClusters(3) = (%d, %v, %v), want (3, true, nil)", got, found, err)
	}
	if info, err := os.Stat(filepath.Join(root, expectedName)); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("the expectation file mode = %v err %v, want 0600", info, err)
	}
}

// A tree whose marker does not identify kind-ocp is never written
// into: the walk refuses it before the count file is touched, and the
// marker is left exactly as it was.
func TestSetExpectedClustersRefusesAForeignMarker(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store := Open(root)
	if _, err := store.EnsureRoot(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, markerName)
	foreign := `{"tool":"other-tool"}`
	if err := os.WriteFile(path, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.SetExpectedClusters(2); err == nil {
		t.Fatal("SetExpectedClusters on a foreign marker must fail")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != foreign {
		t.Errorf("the foreign marker was rewritten: (%q, %v)", b, err)
	}
	// The same refusal gates the read: a tree whose marker does not
	// validate is not kind-ocp's to read an expectation from, and the
	// error is the marker's own.
	if _, _, err := store.ExpectedClusters(); err == nil || !strings.Contains(err.Error(), "does not identify kind-ocp state") {
		t.Fatalf("ExpectedClusters on a foreign marker = %v, want the marker's own refusal", err)
	}
}

// A present-but-unreadable expectation is corrupt state, not an unsafe
// tree: ExpectedClusters reports it wrapping ErrCorruptCount (verify
// degrades to a SKIP naming it), while the marker and the rest of the
// tree still verify — the ownership proof never depends on the count's
// shape. The old in-marker shape (a string where an older binary wrote
// its count) is simply an unknown field: ignored, never fatal.
func TestExpectedClustersClassifiesCorruptCounts(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"a string", "two\n"},
		{"a fractional number", "2.5\n"},
		{"empty", ""},
		{"two integers", "2 3\n"},
		{"a number that overflows int", "99999999999999999999999999999\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			store := Open(root)
			if _, err := store.EnsureRoot(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, expectedName), []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, _, err := store.ExpectedClusters()
			if err == nil || !errors.Is(err, ErrCorruptCount) {
				t.Fatalf("ExpectedClusters = %v, want an error wrapping ErrCorruptCount", err)
			}
			// The tree is still kind-ocp's: a creating walk (the one
			// behind verify's kubeconfig check) and a removal (the
			// one behind delete) both accept it.
			if _, err := store.KubeconfigPath("c-1"); err != nil {
				t.Errorf("a corrupt count must not fail the tree's walks: %v", err)
			}
			removal, err := store.RemovableRoot()
			if err != nil {
				t.Fatalf("a corrupt count must not block removal: %v", err)
			}
			if err := removal.Remove(); err != nil {
				t.Fatalf("removal = %v", err)
			}
		})
	}
	t.Run("a wrong-typed count field inside the marker is an ignored unknown field", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		store := Open(root)
		if _, err := store.EnsureRoot(); err != nil {
			t.Fatal(err)
		}
		// The shape the reviewer's probe wrote into the marker when
		// the count lived there: ownership must not depend on it.
		marker := `{"tool":"kind-ocp","expectedClusters":"two"}`
		if err := os.WriteFile(filepath.Join(root, markerName), []byte(marker), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, found, err := store.ExpectedClusters(); err != nil || found {
			t.Fatalf("ExpectedClusters with a foreign count field in the marker = (%v, %v), want (false, nil)", found, err)
		}
		if _, err := store.KubeconfigPath("c-1"); err != nil {
			t.Errorf("an unknown marker field must not fail the tree's walks: %v", err)
		}
	})
}

// A failed count write preserves the tree exactly: the previous
// expectation stays readable, the marker is untouched, and the tree
// still verifies as removable — the failure this pins is the one the
// reviewer forced live, where an in-place truncate left a zero-byte
// marker and delete refused as unsafe. The update goes through a
// temporary file and a rename, so the live file is never opened for
// writing; the forced failure here (a directory parked at the
// temporary path) fails the update before any rename.
func TestSetExpectedClustersFailedWritePreservesState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store := Open(root)
	if _, err := store.EnsureRoot(); err != nil {
		t.Fatal(err)
	}
	if err := store.SetExpectedClusters(2); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(root, markerName)
	markerBefore, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	// The forced failure: the temporary file's path is occupied by a
	// directory, so the preflight refuses it and no write happens.
	if err := os.Mkdir(filepath.Join(root, expectedName)+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.SetExpectedClusters(3); err == nil {
		t.Fatal("SetExpectedClusters must fail when its temporary path is occupied")
	}
	if got, found, err := store.ExpectedClusters(); err != nil || !found || got != 2 {
		t.Fatalf("the previous expectation did not survive the failed write: (%d, %v, %v), want (2, true, nil)", got, found, err)
	}
	if b, err := os.ReadFile(markerPath); err != nil || string(b) != string(markerBefore) {
		t.Errorf("the marker changed across the failed write: (%q, %v)", b, err)
	}
	// The tree still verifies as kind-ocp's own: delete would run.
	removal, err := store.RemovableRoot()
	if err != nil {
		t.Fatalf("a failed count write must not make the tree unremovable: %v", err)
	}
	if err := removal.Remove(); err != nil {
		t.Fatalf("removal = %v", err)
	}
}

// A successful update replaces the expectation file rather than
// truncating it in place: the file's identity changes (the rename
// installs the fully written temporary file), no temporary file is
// left behind, and the content is exactly the new count.
func TestSetExpectedClustersReplacesTheFileAtomically(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store := Open(root)
	if _, err := store.EnsureRoot(); err != nil {
		t.Fatal(err)
	}
	if err := store.SetExpectedClusters(2); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(filepath.Join(root, expectedName))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetExpectedClusters(5); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(filepath.Join(root, expectedName))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) {
		t.Error("the expectation file kept its identity across the update; it was truncated in place, not replaced")
	}
	if got, found, err := store.ExpectedClusters(); err != nil || !found || got != 5 {
		t.Fatalf("ExpectedClusters after SetExpectedClusters(5) = (%d, %v, %v), want (5, true, nil)", got, found, err)
	}
	if _, err := os.Lstat(filepath.Join(root, expectedName) + ".tmp"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the temporary file survived the update: %v", err)
	}
}

func TestRecordsSortedAndEmpty(t *testing.T) {
	t.Run("empty store", func(t *testing.T) {
		store := Open(filepath.Join(t.TempDir(), "state"))
		recs, err := store.Records()
		if err != nil || len(recs) != 0 {
			t.Errorf("Records() on empty store = (%v, %v)", recs, err)
		}
	})

	t.Run("sorted by name", func(t *testing.T) {
		store := Open(filepath.Join(t.TempDir(), "state"))
		for _, name := range []string{"p-2", "p-10", "p-1"} {
			if err := store.WriteRecord(Record{Name: name}); err != nil {
				t.Fatal(err)
			}
		}
		recs, err := store.Records()
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"p-1", "p-10", "p-2"}
		if len(recs) != 3 || recs[0].Name != want[0] || recs[1].Name != want[1] || recs[2].Name != want[2] {
			t.Errorf("got %v, want %v", recs, want)
		}
	})
}

func TestRemoveRoot(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "state")
	store := Open(root)
	if err := store.WriteRecord(Record{Name: "p-1"}); err != nil {
		t.Fatal(err)
	}
	removal, err := store.RemovableRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = removal.Close() }()
	// t.TempDir paths sit under macOS's /var symlink, so compare
	// against the resolved root.
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if removal.Root() != resolvedRoot {
		t.Errorf("RemovableRoot() = %s, want %s", removal.Root(), resolvedRoot)
	}
	if err := removal.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Errorf("state dir still exists after Remove: %v", err)
	}
	// Nothing left: no removable root, and removal stays a no-op.
	again, err := store.RemovableRoot()
	if err != nil || again.Root() != "" {
		t.Errorf("RemovableRoot() after removal = (%q, %v), want (\"\", nil)", again.Root(), err)
	}
	defer func() { _ = again.Close() }()
	if err := again.Remove(); err != nil {
		t.Errorf("Remove with no root = %v, want nil (nothing left)", err)
	}
}

// ensureClusterDir must write the state marker into a root it creates:
// the marker is what later lets RemoveRoot prove the tree is kind-ocp's.
func TestEnsureWritesStateMarker(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store := Open(root)
	if _, err := store.ensureClusterDir("p-1"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(root, "kind-ocp-state.json"))
	if err != nil {
		t.Fatalf("state marker missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state marker perms = %o, want 0600", perm)
	}
	// A second ensure keeps the marker (adoption is not re-run).
	if _, err := store.ensureClusterDir("p-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "kind-ocp-state.json")); err != nil {
		t.Fatalf("state marker vanished: %v", err)
	}
}

// A pre-existing state root with permissive permissions must be rejected,
// not silently used: MkdirAll's 0700 only applies to directories it
// creates, and a restored or shared tree that group/other can read is not
// a safe home for cluster-admin kubeconfigs.
func TestEnsureRejectsPermissiveExistingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	store := Open(root)
	_, err := store.ensureClusterDir("p-1")
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("ensureClusterDir() = %v, want a mode rejection (the existing root is world-readable)", err)
	}
	// Nothing was created inside the rejected root.
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("the rejected root must stay untouched, got %d entries", len(entries))
	}
}

// An existing unmarked root is adopted only when empty: kind-ocp never
// writes its state into a directory with foreign content.
func TestEnsureRejectsForeignNonEmptyRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(root, "important.txt")
	if err := os.WriteFile(foreign, []byte("not kind-ocp's"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := Open(root)
	_, err := store.ensureClusterDir("p-1")
	if err == nil || !strings.Contains(err.Error(), "not kind-ocp state") {
		t.Fatalf("ensureClusterDir() = %v, want a foreign-directory rejection", err)
	}
	// The foreign content survives untouched, and no marker appeared.
	b, err := os.ReadFile(foreign)
	if err != nil || string(b) != "not kind-ocp's" {
		t.Errorf("foreign file = (%q, %v), want it preserved", b, err)
	}
	if _, err := os.Stat(filepath.Join(root, "kind-ocp-state.json")); !os.IsNotExist(err) {
		t.Error("no state marker may be written into a foreign directory")
	}
}

func TestEnsureAdoptsEmptyExistingRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store := Open(root)
	if err := store.WriteRecord(Record{Name: "p-1"}); err != nil {
		t.Fatalf("WriteRecord into an empty existing root: %v", err)
	}
	if _, found, err := store.Record("p-1"); err != nil || !found {
		t.Errorf("Record after adoption = (%v, %v), want (found, nil)", found, err)
	}
	if _, err := os.Stat(filepath.Join(root, "kind-ocp-state.json")); err != nil {
		t.Errorf("adopted root must carry the state marker: %v", err)
	}
}

// Symlinks inside the managed region must be rejected outright: a
// symlinked store root or clusters directory would redirect kubeconfig
// writes outside the verified boundary.
func TestEnsureRejectsSymlinksInManagedRegion(t *testing.T) {
	t.Run("store root is a symlink", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(dir, "state")
		if err := os.Symlink(target, root); err != nil {
			t.Fatal(err)
		}
		_, err := Open(root).ensureClusterDir("p-1")
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("ensureClusterDir() = %v, want a symlink rejection", err)
		}
	})
	t.Run("clusters directory is a symlink", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if err := os.MkdirAll(filepath.Join(root, "clusters"), 0o700); err != nil {
			t.Fatal(err)
		}
		// Mark the root as kind-ocp state (the documented layout) so
		// the walk gets past root adoption and inspects the clusters
		// component itself.
		markerContent, err := json.Marshal(marker{Tool: "kind-ocp"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, markerName), markerContent, 0o600); err != nil {
			t.Fatal(err)
		}
		// Make clusters a symlink to a private directory.
		if err := os.Remove(filepath.Join(root, "clusters")); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, "clusters")); err != nil {
			t.Fatal(err)
		}
		store := Open(root)
		_, err = store.ensureClusterDir("p-1")
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("ensureClusterDir() = %v, want a symlink rejection", err)
		}
	})
}

// Symlinked ancestors are resolved and their targets checked: macOS
// $TMPDIR lives under the /var symlink, so rejecting ancestor symlinks
// outright would break every temp-dir state root. A symlink to a
// private directory works; a symlink to a world-writable (non-sticky)
// one does not.
func TestAncestorSymlinksAreResolvedAndChecked(t *testing.T) {
	t.Run("symlink to a private directory works", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "real-state-home")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "linked-state-home")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		store := Open(filepath.Join(link, "p"))
		if _, err := store.ensureClusterDir("p-1"); err != nil {
			t.Fatalf("ensureClusterDir through a resolved ancestor symlink: %v", err)
		}
		if err := store.WriteRecord(Record{Name: "p-1"}); err != nil {
			t.Fatalf("WriteRecord through a resolved ancestor symlink: %v", err)
		}
	})
	t.Run("symlink to a world-writable directory is rejected", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "shared")
		if err := os.Mkdir(target, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, 0o777); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "linked-shared")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		store := Open(filepath.Join(link, "p"))
		_, err := store.ensureClusterDir("p-1")
		if err == nil || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("ensureClusterDir() = %v, want a rejection of the world-writable symlink target", err)
		}
	})
}

// A world-writable ancestor without the sticky bit lets another local
// user substitute state entries; the walk must reject it. With the sticky
// bit (the /tmp shape) it is tolerated.
func TestAncestorWriteBitsAreRejectedWithoutSticky(t *testing.T) {
	dir := t.TempDir()

	t.Run("group-writable ancestor", func(t *testing.T) {
		shared := filepath.Join(dir, "groupwritable")
		if err := os.Mkdir(shared, 0o770); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(shared, 0o770); err != nil {
			t.Fatal(err)
		}
		_, err := Open(filepath.Join(shared, "p")).ensureClusterDir("p-1")
		if err == nil || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("ensureClusterDir() = %v, want a group-writable ancestor rejection", err)
		}
	})
	t.Run("world-writable ancestor", func(t *testing.T) {
		shared := filepath.Join(dir, "worldwritable")
		if err := os.Mkdir(shared, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(shared, 0o777); err != nil {
			t.Fatal(err)
		}
		_, err := Open(filepath.Join(shared, "p")).ensureClusterDir("p-1")
		if err == nil || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("ensureClusterDir() = %v, want a world-writable ancestor rejection", err)
		}
	})
	t.Run("sticky world-writable ancestor is tolerated", func(t *testing.T) {
		shared := filepath.Join(dir, "sticky")
		if err := os.Mkdir(shared, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(shared, 0o777|fs.ModeSticky); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(filepath.Join(shared, "p")).ensureClusterDir("p-1"); err != nil {
			t.Fatalf("ensureClusterDir under a sticky ancestor: %v", err)
		}
	})
}

// Record reads must fail closed on a tree that stops being private: a
// chmod'ed-open cluster directory is not a trusted source of state.
func TestRecordReadsRejectUnsafeTree(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store := Open(root)
	if err := store.WriteRecord(Record{Name: "p-1"}); err != nil {
		t.Fatal(err)
	}
	cluster := store.clusterDir("p-1")
	if err := os.Chmod(cluster, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Record("p-1"); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Errorf("Record() through a non-private cluster dir = %v, want a mode rejection", err)
	}
	if _, err := store.Records(); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Errorf("Records() through a non-private cluster dir = %v, want a mode rejection", err)
	}
	// Restoring the permissions restores the reads.
	if err := os.Chmod(cluster, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.Record("p-1"); err != nil || !found {
		t.Errorf("Record() after fixing permissions = (%v, %v), want (found, nil)", found, err)
	}
}

// CheckKubeconfig guards the cached credential: the file must be a
// private, caller-owned regular file before anything hands it to kubectl.
func TestCheckKubeconfig(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store := Open(root)
	if _, err := store.ensureClusterDir("p-1"); err != nil {
		t.Fatal(err)
	}
	kc, err := store.KubeconfigPath("p-1")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("missing file reports not-exist", func(t *testing.T) {
		if _, err := store.CheckKubeconfig("p-1"); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("CheckKubeconfig(absent) = %v, want fs.ErrNotExist", err)
		}
	})
	t.Run("private file passes", func(t *testing.T) {
		if err := os.WriteFile(kc, []byte("cfg"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CheckKubeconfig("p-1"); err != nil {
			t.Errorf("CheckKubeconfig(0600) = %v, want nil", err)
		}
	})
	t.Run("exposed file is rejected", func(t *testing.T) {
		if err := os.Chmod(kc, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := store.CheckKubeconfig("p-1")
		if err == nil || !strings.Contains(err.Error(), "mode") {
			t.Errorf("CheckKubeconfig(0644) = %v, want a mode rejection", err)
		}
	})
	t.Run("symlinked file is rejected", func(t *testing.T) {
		if err := os.Remove(kc); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.WriteFile(target, []byte("cfg"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, kc); err != nil {
			t.Fatal(err)
		}
		_, err := store.CheckKubeconfig("p-1")
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Errorf("CheckKubeconfig(symlink) = %v, want a symlink rejection", err)
		}
	})
}

// A symlinked ancestor is only as safe as the entire chain below its
// target. Checking just the final target would accept a safe leaf that
// hangs below a permissive directory — a state path through a non-sticky
// 0777 parent is rejected directly, so it must be rejected through an
// ancestor symlink too.
func TestWalkChecksEntireSymlinkTargetChain(t *testing.T) {
	dir := t.TempDir()

	// mid is world-writable without the sticky bit; target inside it is
	// a perfectly safe directory on its own.
	mid := filepath.Join(dir, "mid")
	if err := os.Mkdir(mid, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(mid, 0o777); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(mid, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}

	// The direct path is refused at mid.
	t.Run("direct path through the permissive directory is rejected", func(t *testing.T) {
		_, err := Open(filepath.Join(target, "p")).ensureClusterDir("p-1")
		if err == nil || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("ensureClusterDir() = %v, want a rejection of the world-writable mid directory", err)
		}
	})
	// The aliased path must be refused the same way: the alias's
	// target runs through the permissive directory, and the walk
	// checks every component it depends on, not just the leaf.
	t.Run("aliased path through a symlink is rejected too", func(t *testing.T) {
		link := filepath.Join(dir, "alias")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		_, err := Open(filepath.Join(link, "p")).ensureClusterDir("p-1")
		if err == nil || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("ensureClusterDir() through the symlink = %v, want the same rejection of the target's chain", err)
		}
	})
}

// The paths kind-ocp hands out, writes through, and cleans up are the
// fully-resolved paths the walk verified — never the symlinked alias it
// started from. Removing the alias instead of the verified tree would
// both leave state behind and follow a link that was never checked at
// removal time.
func TestOperationsUseTheResolvedVerifiedPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "alias")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	store := Open(filepath.Join(link, "p"))

	kubeconfigPath, err := store.KubeconfigPath("p-1")
	if err != nil {
		t.Fatal(err)
	}
	// t.TempDir paths sit under macOS's /var symlink, so compare
	// against the resolved target.
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(kubeconfigPath, resolvedTarget+string(filepath.Separator)) {
		t.Fatalf("kubeconfig path = %s, want it under the resolved target %s (no symlink components)", kubeconfigPath, resolvedTarget)
	}
	if err := store.WriteRecord(Record{Name: "p-1"}); err != nil {
		t.Fatal(err)
	}
	// The state exists under the resolved target, reachable through the
	// alias only because the alias still points there.
	if _, err := os.Stat(filepath.Join(target, "p", "clusters", "p-1", "record.json")); err != nil {
		t.Fatalf("record must exist at the resolved location: %v", err)
	}

	// Cleanup removes the verified tree; the symlink entry — an
	// ancestor's, not kind-ocp's — survives.
	removal, err := store.RemovableRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = removal.Close() }()
	if err := removal.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(resolvedTarget, "p")); !os.IsNotExist(err) {
		t.Errorf("the resolved store root must be gone, got %v", err)
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("the ancestor symlink must survive cleanup: %v", err)
	}
}

// An ancestor symlink is followed only when the entry itself belongs to
// the current user or root: in a sticky directory (the /tmp shape),
// whoever owns an entry can replace it at any time, so a foreign-owned
// link could be swapped between the walk's check and the walk's use.
// The foreign-owned half needs root to fabricate and is covered by the
// ownership unit tests in fscheck; the caller-owned half is here.
func TestAncestorSymlinkEntryOwnership(t *testing.T) {
	dir := t.TempDir()
	sticky := filepath.Join(dir, "sticky")
	if err := os.Mkdir(sticky, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sticky, 0o777|fs.ModeSticky); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(sticky, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(sticky, "alias")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	store := Open(filepath.Join(link, "p"))
	if _, err := store.ensureClusterDir("p-1"); err != nil {
		t.Fatalf("a caller-owned symlink in a sticky ancestor must work: %v", err)
	}
}

// The recognized marker format is the "tool" field; markers written by
// older kind-ocp versions (with a version number) stay valid.
func TestMarkerFormatIsToolFieldOnly(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, markerName), []byte(`{"tool":"kind-ocp","version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	removal, err := Open(root).RemovableRoot()
	if err != nil {
		t.Errorf("an older marker with a version field must stay recognized: %v", err)
	}
	_ = removal.Close()
}

// An ancestor alias is only as trustworthy as every entry its
// resolution depends on. A link whose target passes through a
// permissive directory depends on an entry another local user can
// swap — even though the final target is perfectly safe on its own, and
// even though the link entry itself is caller-owned. Resolving the
// whole chain in one go (EvalSymlinks) erases exactly that intermediate
// hop; the walk resolves hop by hop and policy-checks every component
// it depends on instead.
func TestWalkChecksEveryAliasHop(t *testing.T) {
	dir := t.TempDir()
	safe := filepath.Join(dir, "safe")
	if err := os.Mkdir(safe, 0o700); err != nil {
		t.Fatal(err)
	}

	t.Run("alias through a permissive directory is refused", func(t *testing.T) {
		permissive := filepath.Join(dir, "permissive")
		if err := os.Mkdir(permissive, 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(permissive, 0o777); err != nil {
			t.Fatal(err)
		}
		inner := filepath.Join(permissive, "inner")
		if err := os.Symlink(safe, inner); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(dir, "alias")
		if err := os.Symlink(inner, alias); err != nil {
			t.Fatal(err)
		}

		// Directly, the permissive directory is already refused; the
		// alias must not launder it.
		_, err := Open(filepath.Join(inner, "p")).ensureClusterDir("p-1")
		if err == nil || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("direct path through the permissive directory = %v, want a writability rejection", err)
		}
		_, err = Open(filepath.Join(alias, "p")).ensureClusterDir("p-1")
		if err == nil || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("aliased path through the permissive directory = %v, want the same rejection", err)
		}
		// Nothing was created behind the refusal.
		if _, serr := os.Stat(filepath.Join(safe, "p")); !os.IsNotExist(serr) {
			t.Errorf("state must not be created behind a refused alias: %v", serr)
		}
	})

	t.Run("chained aliases through trusted entries are resolved", func(t *testing.T) {
		holder := filepath.Join(dir, "holder")
		if err := os.Mkdir(holder, 0o755); err != nil {
			t.Fatal(err)
		}
		inner := filepath.Join(holder, "inner")
		if err := os.Symlink(safe, inner); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(dir, "alias2")
		if err := os.Symlink(inner, alias); err != nil {
			t.Fatal(err)
		}

		got, err := Open(filepath.Join(alias, "p")).ensureClusterDir("p-1")
		if err != nil {
			t.Fatalf("a chain of caller-owned aliases over safe directories must work: %v", err)
		}
		resolvedSafe, err := filepath.EvalSymlinks(safe)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(resolvedSafe, "p", "clusters", "p-1"); got != want {
			t.Errorf("resolved dir = %s, want %s", got, want)
		}
	})

	t.Run("relative climbing target is resolved like the kernel", func(t *testing.T) {
		holder := filepath.Join(dir, "climb")
		if err := os.Mkdir(holder, 0o755); err != nil {
			t.Fatal(err)
		}
		alias := filepath.Join(holder, "up")
		if err := os.Symlink("../safe", alias); err != nil {
			t.Fatal(err)
		}

		got, err := Open(filepath.Join(alias, "p")).ensureClusterDir("p-1")
		if err != nil {
			t.Fatalf("a relative climbing alias must resolve: %v", err)
		}
		resolvedSafe, err := filepath.EvalSymlinks(safe)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(resolvedSafe, "p", "clusters", "p-1"); got != want {
			t.Errorf("resolved dir = %s, want %s", got, want)
		}
	})

	t.Run("alias loop is refused", func(t *testing.T) {
		a := filepath.Join(dir, "loopA")
		b := filepath.Join(dir, "loopB")
		if err := os.Symlink(b, a); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(a, b); err != nil {
			t.Fatal(err)
		}
		_, err := Open(filepath.Join(a, "p")).ensureClusterDir("p-1")
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("looping alias = %v, want a loop refusal", err)
		}
	})

	// The hop budget is one walk's total, not a per-branch depth: an
	// alias whose target traverses many sibling links — none nested,
	// so no recursion depth accumulates — must hit the same budget
	// the kernel's ELOOP would. Before the shared budget, each
	// sibling restarted the count and a chain of any length resolved.
	t.Run("sibling links share one hop budget", func(t *testing.T) {
		links := func(t *testing.T, prefix string, n int) []string {
			t.Helper()
			names := make([]string, 0, n)
			for i := 0; i < n; i++ {
				name := fmt.Sprintf("%s-s%d", prefix, i)
				if err := os.Symlink(".", filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
				names = append(names, name)
			}
			return names
		}

		over := links(t, "over", fscheck.MaxAliasHops+5)
		alias := filepath.Join(dir, "budget")
		if err := os.Symlink(strings.Join(over, string(filepath.Separator)), alias); err != nil {
			t.Fatal(err)
		}
		_, err := Open(filepath.Join(alias, "p")).ensureClusterDir("p-1")
		if err == nil || !strings.Contains(err.Error(), "symlink hops") {
			t.Fatalf("alias over %d sibling links = %v, want the shared-budget loop refusal", len(over), err)
		}
		// Nothing was created behind the refusal (the chain resolves
		// to dir itself, so that is where state would have appeared).
		if _, serr := os.Stat(filepath.Join(dir, "p")); !os.IsNotExist(serr) {
			t.Errorf("state must not be created behind the refused alias: %v", serr)
		}

		// A chain within the budget still resolves.
		within := links(t, "within", 5)
		fine := filepath.Join(dir, "within-alias")
		if err := os.Symlink(strings.Join(within, string(filepath.Separator)), fine); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(filepath.Join(fine, "p")).ensureClusterDir("p-1"); err != nil {
			t.Fatalf("alias over 5 sibling links = %v, want success", err)
		}
	})
}

// Cleanup removes exactly the tree that was verified when the operation
// started: if the state root resolves elsewhere by the time removal
// runs — an ancestor alias re-pointed or removed underneath — removal
// is refused, and both the verified tree and the new one survive.
// Only the verified tree itself disappearing makes removal a no-op:
// there is nothing left to remove then.
func TestRemoveRefusesWhenResolutionChanges(t *testing.T) {
	// setup builds a store behind an alias and starts a removal from
	// it, with an equally legitimate decoy tree the alias could be
	// re-pointed at.
	setup := func(t *testing.T) (removal *Removal, real, alias, decoyRoot string) {
		t.Helper()
		dir := t.TempDir()
		real = filepath.Join(dir, "real")
		if err := os.Mkdir(real, 0o700); err != nil {
			t.Fatal(err)
		}
		alias = filepath.Join(dir, "alias")
		if err := os.Symlink(real, alias); err != nil {
			t.Fatal(err)
		}
		store := Open(filepath.Join(alias, "p"))
		if err := store.WriteRecord(Record{Name: "p-1", Status: StatusComplete}); err != nil {
			t.Fatal(err)
		}
		r, err := store.RemovableRoot()
		if err != nil {
			t.Fatalf("RemovableRoot() = %v, want the verified root", err)
		}
		t.Cleanup(func() { _ = r.Close() })
		decoyRoot = filepath.Join(dir, "decoy", "p")
		decoy := Open(filepath.Join(dir, "decoy", "p"))
		if err := decoy.WriteRecord(Record{Name: "p-1", Status: StatusComplete}); err != nil {
			t.Fatal(err)
		}
		return r, real, alias, decoyRoot
	}

	t.Run("re-pointed alias is a refusal", func(t *testing.T) {
		removal, real, alias, decoyRoot := setup(t)
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Dir(decoyRoot), alias); err != nil {
			t.Fatal(err)
		}

		err := removal.Remove()
		if err == nil || !strings.Contains(err.Error(), "will not act on a tree it did not verify") {
			t.Fatalf("Remove after the alias moved = %v, want the root-switch refusal", err)
		}
		if _, serr := os.Stat(filepath.Join(real, "p")); serr != nil {
			t.Errorf("the verified tree must survive the refusal: %v", serr)
		}
		if _, serr := os.Stat(decoyRoot); serr != nil {
			t.Errorf("the decoy tree must survive the refusal: %v", serr)
		}

		// Pointing the alias back lets cleanup finish — and only the
		// verified tree goes.
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(real, alias); err != nil {
			t.Fatal(err)
		}
		if err := removal.Remove(); err != nil {
			t.Fatalf("Remove after pointing the alias back = %v, want nil", err)
		}
		if _, serr := os.Stat(filepath.Join(real, "p")); !os.IsNotExist(serr) {
			t.Errorf("the verified tree must be gone: %v", serr)
		}
		if _, serr := os.Stat(decoyRoot); serr != nil {
			t.Errorf("the decoy tree must never be touched: %v", serr)
		}
	})

	t.Run("removed alias is a refusal, not success", func(t *testing.T) {
		removal, real, _, _ := setup(t)
		// The alias disappears entirely: the verified tree survives,
		// so removal must not report success and leave it behind.
		if err := os.Remove(filepath.Join(filepath.Dir(real), "alias")); err != nil {
			t.Fatal(err)
		}

		err := removal.Remove()
		if err == nil || !strings.Contains(err.Error(), "no longer resolves") {
			t.Fatalf("Remove after the alias vanished = %v, want the no-longer-resolves refusal", err)
		}
		if _, serr := os.Stat(filepath.Join(real, "p")); serr != nil {
			t.Errorf("the verified tree must survive the refusal: %v", serr)
		}
	})

	t.Run("verified tree already gone is a no-op", func(t *testing.T) {
		removal, real, _, _ := setup(t)
		// The tree itself was removed by something else: there is
		// nothing left to take, and that is a success, not a refusal.
		if err := os.RemoveAll(real); err != nil {
			t.Fatal(err)
		}
		if err := removal.Remove(); err != nil {
			t.Fatalf("Remove with the verified tree already gone = %v, want nil", err)
		}
	})
}

// A removal's kubeconfig handoffs are pinned like its cleanup: with a
// root verified up front, a handoff after the state root moved is a
// refusal before any write. No cluster directory may appear in the
// tree the root now points at — kind would modify that tree's
// kubeconfig otherwise.
func TestRemovalHandoffsRefuseMovedRoot(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	store := Open(filepath.Join(alias, "p"))
	if err := store.WriteRecord(Record{Name: "p-1", Status: StatusComplete}); err != nil {
		t.Fatal(err)
	}
	removal, err := store.RemovableRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = removal.Close() }()

	// A second, equally legitimate tree; the alias moves to it before
	// the second handoff.
	decoyRoot := filepath.Join(dir, "decoy", "p")
	decoy := Open(filepath.Join(dir, "decoy", "p"))
	if err := decoy.WriteRecord(Record{Name: "p-1", Status: StatusComplete}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "decoy"), alias); err != nil {
		t.Fatal(err)
	}

	if _, err := removal.KubeconfigPath("p-2"); err == nil || !strings.Contains(err.Error(), "will not act on a tree it did not verify") {
		t.Fatalf("KubeconfigPath after the alias moved = %v, want the root-switch refusal", err)
	}
	// The refusal came before any write: neither tree gained a
	// p-2 cluster directory.
	for _, root := range []string{filepath.Join(real, "p"), decoyRoot} {
		if _, serr := os.Stat(filepath.Join(root, "clusters", "p-2")); !os.IsNotExist(serr) {
			t.Errorf("no cluster dir for p-2 may be created behind the refusal (root %s): %v", root, serr)
		}
	}
}

// A removal that starts with no state captures the tree its first
// handoff prepares: later handoffs and the cleanup act on the capture,
// not on whatever the state root points at by then. The deletion
// kind-ocp performs is the one it started, and only the state it
// prepared is removed.
func TestRemovalCapturesTheTreeItPrepares(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	store := Open(filepath.Join(alias, "p"))
	removal, err := store.RemovableRoot()
	if err != nil || removal.Root() != "" {
		t.Fatalf("RemovableRoot on absent state = (%q, %v), want (\"\", nil)", removal.Root(), err)
	}
	defer func() { _ = removal.Close() }()

	// The first handoff prepares the tree through the alias.
	first, err := removal.KubeconfigPath("p-1")
	if err != nil {
		t.Fatalf("first KubeconfigPath = %v, want the tree prepared", err)
	}
	resolvedReal, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(resolvedReal, "p", "clusters", "p-1", "kubeconfig"); first != want {
		t.Fatalf("first kubeconfig path = %s, want %s", first, want)
	}

	// A second tree the alias is re-pointed at mid-removal.
	decoyRoot := filepath.Join(dir, "decoy", "p")
	decoy := Open(filepath.Join(dir, "decoy", "p"))
	if err := decoy.WriteRecord(Record{Name: "p-1", Status: StatusComplete}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "decoy"), alias); err != nil {
		t.Fatal(err)
	}

	// Later handoffs act on the capture; the decoy is not written
	// into.
	second, err := removal.KubeconfigPath("p-2")
	if err != nil {
		t.Fatalf("second KubeconfigPath after the alias moved = %v, want success under the captured tree", err)
	}
	if want := filepath.Join(resolvedReal, "p", "clusters", "p-2", "kubeconfig"); second != want {
		t.Fatalf("second kubeconfig path = %s, want %s (the captured tree)", second, want)
	}
	if _, serr := os.Stat(filepath.Join(decoyRoot, "clusters", "p-2")); !os.IsNotExist(serr) {
		t.Errorf("the decoy tree must not be written into: %v", serr)
	}

	// Cleanup removes the captured tree wherever the alias points by
	// then; the decoy survives.
	if err := removal.Remove(); err != nil {
		t.Fatalf("Remove = %v, want the captured tree removed", err)
	}
	if _, serr := os.Stat(filepath.Join(resolvedReal, "p")); !os.IsNotExist(serr) {
		t.Errorf("the captured tree must be gone: %v", serr)
	}
	if _, serr := os.Stat(decoyRoot); serr != nil {
		t.Errorf("the decoy tree must survive: %v", serr)
	}
}

// A removal that prepared nothing removes nothing: a tree that appears
// under the state root after the removal started — a concurrent
// create, say — is not that removal's to take.
func TestRemovalRemovesNothingItDidNotPrepare(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "state", "p")
	removal, err := Open(root).RemovableRoot()
	if err != nil || removal.Root() != "" {
		t.Fatalf("RemovableRoot on absent state = (%q, %v), want (\"\", nil)", removal.Root(), err)
	}
	defer func() { _ = removal.Close() }()

	// State appears after the removal started: a marked, fully
	// legitimate tree.
	late := Open(root)
	if err := late.WriteRecord(Record{Name: "p-1", Status: StatusComplete}); err != nil {
		t.Fatal(err)
	}

	if err := removal.Remove(); err != nil {
		t.Fatalf("Remove = %v, want nil (this removal prepared nothing)", err)
	}
	if _, serr := os.Stat(root); serr != nil {
		t.Errorf("the tree that appeared mid-removal must survive: %v", serr)
	}
}

// The absent-root handoff prepares its tree exclusively: marked state
// that appears between the preflight and the first handoff — a
// concurrent create, say — must make the handoff fail without adding a
// cluster directory, and cleanup afterwards must leave the late state
// intact. Before the fix the handoff used the ordinary create walk,
// adopted the late tree as its capture, and Remove deleted it.
func TestRemovalHandoffRefusesLateState(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "state", "p")
	removal, err := Open(root).RemovableRoot()
	if err != nil || removal.Root() != "" {
		t.Fatalf("RemovableRoot on absent state = (%q, %v), want (\"\", nil)", removal.Root(), err)
	}
	defer func() { _ = removal.Close() }()

	// Marked, legitimate state appears after the removal started.
	late := Open(root)
	if err := late.WriteRecord(Record{Name: "p-1", Status: StatusComplete}); err != nil {
		t.Fatal(err)
	}

	// The first handoff is a refusal, not an adoption of the late tree.
	_, err = removal.KubeconfigPath("p-2")
	if err == nil || !strings.Contains(err.Error(), "state it did not prepare") {
		t.Fatalf("first handoff after state appeared = %v, want the did-not-prepare refusal", err)
	}
	if removal.Root() != "" {
		t.Errorf("a refused handoff must not capture a root; captured %q", removal.Root())
	}
	// No cluster directory was added to the late tree.
	if _, serr := os.Stat(filepath.Join(root, clustersDir, "p-2")); !os.IsNotExist(serr) {
		t.Errorf("no cluster directory may be created behind the refusal: %v", serr)
	}

	// Cleanup afterwards removes nothing; the late state is intact.
	if err := removal.Remove(); err != nil {
		t.Fatalf("Remove after the refused handoff = %v, want nil (nothing was captured)", err)
	}
	if _, serr := os.Stat(root); serr != nil {
		t.Errorf("the tree that appeared mid-removal must survive: %v", serr)
	}
	if _, serr := os.Stat(filepath.Join(root, clustersDir, "p-1", recordName)); serr != nil {
		t.Errorf("the late state's record must survive: %v", serr)
	}
}

// A removal pins the directory it verified or prepared, not the
// pathname it was reached by: a tree renamed away after a handoff and
// replaced by new, marked, fully legitimate state at the same path
// leaves the path indistinguishable — same name, same marker, same
// policies — while the directory at it is not the one the removal
// pinned. Later handoffs must refuse the replacement before any write
// (kind would otherwise strip a context from another tree's
// kubeconfig), and cleanup must refuse it too: removing the
// replacement would report success while the original survived,
// credentials included, unrecognized wherever it was moved to. Both
// halves of delete are covered — a root the preflight verified, and
// one the first handoff prepared itself.
func TestRemovalRefusesReplacedRoot(t *testing.T) {
	// replace renames the pinned root away and puts new marked state
	// at the same path, the way a concurrent process or a user script
	// could between two kind-ocp steps.
	replace := func(t *testing.T, root string) string {
		t.Helper()
		moved := filepath.Join(filepath.Dir(root), "moved")
		if err := os.Rename(root, moved); err != nil {
			t.Fatal(err)
		}
		if err := Open(root).WriteRecord(Record{Name: "decoy", Status: StatusComplete}); err != nil {
			t.Fatal(err)
		}
		return moved
	}

	t.Run("preverified root", func(t *testing.T) {
		dir := t.TempDir()
		root := filepath.Join(dir, "state", "p")
		store := Open(root)
		if err := store.WriteRecord(Record{Name: "p-1", Status: StatusComplete}); err != nil {
			t.Fatal(err)
		}
		removal, err := store.RemovableRoot()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = removal.Close() }()
		if _, err := removal.KubeconfigPath("p-1"); err != nil {
			t.Fatalf("first handoff = %v, want the pinned tree written", err)
		}
		moved := replace(t, removal.Root())

		_, err = removal.KubeconfigPath("p-2")
		if err == nil || !strings.Contains(err.Error(), "it was replaced") {
			t.Fatalf("handoff after replacement = %v, want the replacement refusal", err)
		}
		if _, serr := os.Stat(filepath.Join(removal.Root(), clustersDir, "p-2")); !os.IsNotExist(serr) {
			t.Errorf("no cluster dir may be created in the replacement: %v", serr)
		}

		if err := removal.Remove(); err == nil || !strings.Contains(err.Error(), "it was replaced") {
			t.Fatalf("Remove after replacement = %v, want the replacement refusal", err)
		}
		if _, serr := os.Stat(filepath.Join(removal.Root(), clustersDir, "decoy", recordName)); serr != nil {
			t.Errorf("the replacement must survive the refusal: %v", serr)
		}
		if _, serr := os.Stat(filepath.Join(moved, clustersDir, "p-1", recordName)); serr != nil {
			t.Errorf("the renamed original must survive the refusal: %v", serr)
		}
	})

	t.Run("prepared root", func(t *testing.T) {
		dir := t.TempDir()
		root := filepath.Join(dir, "state", "p")
		removal, err := Open(root).RemovableRoot()
		if err != nil || removal.Root() != "" {
			t.Fatalf("RemovableRoot on absent state = (%q, %v), want (\"\", nil)", removal.Root(), err)
		}
		defer func() { _ = removal.Close() }()
		if _, err := removal.KubeconfigPath("p-1"); err != nil {
			t.Fatalf("first handoff = %v, want the tree prepared", err)
		}
		moved := replace(t, removal.Root())

		_, err = removal.KubeconfigPath("p-2")
		if err == nil || !strings.Contains(err.Error(), "it was replaced") {
			t.Fatalf("handoff after replacement = %v, want the replacement refusal", err)
		}
		if _, serr := os.Stat(filepath.Join(removal.Root(), clustersDir, "p-2")); !os.IsNotExist(serr) {
			t.Errorf("no cluster dir may be created in the replacement: %v", serr)
		}

		if err := removal.Remove(); err == nil || !strings.Contains(err.Error(), "it was replaced") {
			t.Fatalf("Remove after replacement = %v, want the replacement refusal", err)
		}
		if _, serr := os.Stat(filepath.Join(removal.Root(), clustersDir, "decoy", recordName)); serr != nil {
			t.Errorf("the replacement must survive the refusal: %v", serr)
		}
		// The prepared tree carries the cluster directory the first
		// handoff created (no record: the record is a create-side
		// write).
		if _, serr := os.Stat(filepath.Join(moved, clustersDir, "p-1")); serr != nil {
			t.Errorf("the renamed original must survive the refusal: %v", serr)
		}
	})

	// A pinned root that went missing — renamed away with nothing put
	// in its place — is not rebuilt by a handoff: a fresh tree at the
	// same path would strand the original, credentials included,
	// wherever it was moved to. Cleanup on the missing root stays a
	// no-op.
	t.Run("missing pinned root is not recreated", func(t *testing.T) {
		dir := t.TempDir()
		root := filepath.Join(dir, "state", "p")
		removal, err := Open(root).RemovableRoot()
		if err != nil || removal.Root() != "" {
			t.Fatalf("RemovableRoot on absent state = (%q, %v), want (\"\", nil)", removal.Root(), err)
		}
		defer func() { _ = removal.Close() }()
		if _, err := removal.KubeconfigPath("p-1"); err != nil {
			t.Fatalf("first handoff = %v, want the tree prepared", err)
		}
		moved := filepath.Join(dir, "moved")
		if err := os.Rename(removal.Root(), moved); err != nil {
			t.Fatal(err)
		}

		_, err = removal.KubeconfigPath("p-2")
		if err == nil || !strings.Contains(err.Error(), "will not recreate") {
			t.Fatalf("handoff after the pinned root vanished = %v, want the will-not-recreate refusal", err)
		}
		if _, serr := os.Stat(removal.Root()); !os.IsNotExist(serr) {
			t.Errorf("the pinned root must not be recreated by a handoff: %v", serr)
		}
		if err := removal.Remove(); err != nil {
			t.Fatalf("Remove with the pinned tree gone = %v, want nil (nothing to remove)", err)
		}
		if _, serr := os.Stat(filepath.Join(moved, clustersDir, "p-1")); serr != nil {
			t.Errorf("the renamed tree must survive untouched: %v", serr)
		}
	})

	// Renames preserve a directory's identity, so a tree moved away
	// and restored is still the pinned one: the refusal must target
	// replacements, not legitimate restores.
	t.Run("a tree moved away and restored stays pinned", func(t *testing.T) {
		dir := t.TempDir()
		root := filepath.Join(dir, "state", "p")
		store := Open(root)
		if err := store.WriteRecord(Record{Name: "p-1", Status: StatusComplete}); err != nil {
			t.Fatal(err)
		}
		removal, err := store.RemovableRoot()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := removal.KubeconfigPath("p-1"); err != nil {
			t.Fatalf("first handoff = %v, want the pinned tree written", err)
		}
		moved := filepath.Join(dir, "moved")
		if err := os.Rename(removal.Root(), moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(moved, removal.Root()); err != nil {
			t.Fatal(err)
		}

		if _, err := removal.KubeconfigPath("p-2"); err != nil {
			t.Fatalf("handoff after the tree was restored = %v, want nil (identity survives renames)", err)
		}
		if err := removal.Remove(); err != nil {
			t.Fatalf("Remove after the tree was restored = %v, want nil", err)
		}
		if _, serr := os.Stat(removal.Root()); !os.IsNotExist(serr) {
			t.Errorf("the restored tree must be removable: %v", serr)
		}
	})
}

// The pin must survive the pinned tree being removed and new state
// created at the same path — the remove-and-recreate shape of a
// concurrent create, as opposed to the rename-and-replace one above.
// What makes this case hard is a filesystem that recycles inode
// numbers immediately (ext4 does): a pin built on a stat snapshot of
// device and inode can match the replacement exactly, and the stale
// operation then writes into the new tree and removes it — the
// reviewer's ext4 finding. A pin built on an open handle cannot: an
// inode with an open reference is never reassigned, so the replacement
// differs from the held directory even where its identity is the
// recycled one, and every handoff and the cleanup refuse it.
//
// On filesystems that do not recycle the inode this way (APFS, the
// macOS development host, hands a new directory a fresh number), the
// snapshot pin also refuses — the regression is load-bearing on
// recycling filesystems, and ext4 verification accompanies it (see the
// increment evidence); the assertions hold everywhere.
func TestRemovalRefusesRecreatedRoot(t *testing.T) {
	// recreate removes the pinned tree and writes fresh marked state
	// at the same path, the way a concurrent create could between two
	// kind-ocp steps.
	recreate := func(t *testing.T, root string) {
		t.Helper()
		if err := os.RemoveAll(root); err != nil {
			t.Fatal(err)
		}
		if err := Open(root).WriteRecord(Record{Name: "fresh", Status: StatusComplete}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("preverified root", func(t *testing.T) {
		dir := t.TempDir()
		root := filepath.Join(dir, "state", "p")
		store := Open(root)
		if err := store.WriteRecord(Record{Name: "p-1", Status: StatusComplete}); err != nil {
			t.Fatal(err)
		}
		removal, err := store.RemovableRoot()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = removal.Close() }()
		if _, err := removal.KubeconfigPath("p-1"); err != nil {
			t.Fatalf("first handoff = %v, want the pinned tree written", err)
		}
		recreate(t, removal.Root())

		_, err = removal.KubeconfigPath("p-2")
		if err == nil || !strings.Contains(err.Error(), "it was replaced") {
			t.Fatalf("handoff after recreation = %v, want the replacement refusal", err)
		}
		if _, serr := os.Stat(filepath.Join(removal.Root(), clustersDir, "p-2")); !os.IsNotExist(serr) {
			t.Errorf("no cluster dir may be created in the recreated tree: %v", serr)
		}

		if err := removal.Remove(); err == nil || !strings.Contains(err.Error(), "it was replaced") {
			t.Fatalf("Remove after recreation = %v, want the replacement refusal", err)
		}
		if _, serr := os.Stat(filepath.Join(removal.Root(), clustersDir, "fresh", recordName)); serr != nil {
			t.Errorf("the recreated tree must survive the refusal: %v", serr)
		}
	})

	t.Run("prepared root", func(t *testing.T) {
		dir := t.TempDir()
		root := filepath.Join(dir, "state", "p")
		removal, err := Open(root).RemovableRoot()
		if err != nil || removal.Root() != "" {
			t.Fatalf("RemovableRoot on absent state = (%q, %v), want (\"\", nil)", removal.Root(), err)
		}
		defer func() { _ = removal.Close() }()
		if _, err := removal.KubeconfigPath("p-1"); err != nil {
			t.Fatalf("first handoff = %v, want the tree prepared", err)
		}
		recreate(t, removal.Root())

		_, err = removal.KubeconfigPath("p-2")
		if err == nil || !strings.Contains(err.Error(), "it was replaced") {
			t.Fatalf("handoff after recreation = %v, want the replacement refusal", err)
		}
		if _, serr := os.Stat(filepath.Join(removal.Root(), clustersDir, "p-2")); !os.IsNotExist(serr) {
			t.Errorf("no cluster dir may be created in the recreated tree: %v", serr)
		}

		if err := removal.Remove(); err == nil || !strings.Contains(err.Error(), "it was replaced") {
			t.Fatalf("Remove after recreation = %v, want the replacement refusal", err)
		}
		if _, serr := os.Stat(filepath.Join(removal.Root(), clustersDir, "fresh", recordName)); serr != nil {
			t.Errorf("the recreated tree must survive the refusal: %v", serr)
		}
	})
}

// The removal's handle is the caller's to release: Close is a no-op on
// a removal that never pinned a directory, safe to call again after it
// ran, and a removal whose handle is closed refuses further steps
// instead of silently dropping its pin (the path alone is not the pin).
func TestRemovalCloseReleasesThePin(t *testing.T) {
	t.Run("rootless removal closes to a no-op", func(t *testing.T) {
		removal, err := Open(filepath.Join(t.TempDir(), "state", "p")).RemovableRoot()
		if err != nil || removal.Root() != "" {
			t.Fatalf("RemovableRoot on absent state = (%q, %v), want (\"\", nil)", removal.Root(), err)
		}
		if err := removal.Close(); err != nil {
			t.Fatalf("Close on a rootless removal = %v, want nil", err)
		}
		if err := removal.Close(); err != nil {
			t.Fatalf("second Close = %v, want nil (idempotent)", err)
		}
		if err := removal.Remove(); err != nil {
			t.Fatalf("Remove after Close = %v, want nil (nothing was pinned)", err)
		}
	})

	t.Run("closed removal refuses handoffs instead of dropping the pin", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state", "p")
		store := Open(root)
		if err := store.WriteRecord(Record{Name: "p-1", Status: StatusComplete}); err != nil {
			t.Fatal(err)
		}
		removal, err := store.RemovableRoot()
		if err != nil {
			t.Fatal(err)
		}
		if err := removal.Close(); err != nil {
			t.Fatal(err)
		}
		_, err = removal.KubeconfigPath("p-2")
		if err == nil || !strings.Contains(err.Error(), "handle on the state directory") {
			t.Fatalf("handoff after Close = %v, want the closed-handle refusal", err)
		}
		if _, serr := os.Stat(filepath.Join(root, clustersDir, "p-2")); !os.IsNotExist(serr) {
			t.Errorf("no cluster dir may be created behind the refusal: %v", serr)
		}
	})

	t.Run("retry after a refusal keeps working until Close", func(t *testing.T) {
		// A refused Remove does not end the operation: the remedy the
		// refusal names (point the state root back, re-run) must be
		// able to use the same removal, and only Close retires it.
		root := filepath.Join(t.TempDir(), "state", "p")
		store := Open(root)
		if err := store.WriteRecord(Record{Name: "p-1", Status: StatusComplete}); err != nil {
			t.Fatal(err)
		}
		removal, err := store.RemovableRoot()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = removal.Close() }()
		if err := removal.Remove(); err != nil {
			t.Fatal(err)
		}
		if _, serr := os.Stat(root); !os.IsNotExist(serr) {
			t.Fatalf("the pinned tree must be gone: %v", serr)
		}
		// The tree is gone but the pin is not released; a handoff
		// still refuses rather than recreating the tree — through
		// either refusal the ordering produces for a preverified
		// root (the resolution check fires first), the point being
		// that nothing is rebuilt behind the refusal.
		_, err = removal.KubeconfigPath("p-2")
		if err == nil {
			t.Fatal("handoff after a successful Remove succeeded, want a refusal that does not recreate the tree")
		}
		refused := strings.Contains(err.Error(), "will not recreate") || strings.Contains(err.Error(), "no longer resolves")
		if !refused {
			t.Fatalf("handoff after a successful Remove = %v, want a refusal that does not recreate the tree", err)
		}
		if _, serr := os.Stat(root); !os.IsNotExist(serr) {
			t.Errorf("the pinned tree must not be recreated behind the refusal: %v", serr)
		}
	})
}

// walkResolved re-verifies a resolved root's own chain and refuses to
// follow aliases in it: the chain was symlink-free when it was
// resolved, so a symlink component means an entry was substituted
// since, and a pinned operation must not be redirected through it.
func TestWalkResolvedRefusesSubstitutedComponents(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatal(err)
	}
	store := Open(filepath.Join(base, "state", "p"))
	if err := store.WriteRecord(Record{Name: "p-1", Status: StatusComplete}); err != nil {
		t.Fatal(err)
	}
	removal, err := store.RemovableRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = removal.Close() }()
	pinned := removal.Root()

	// While the operation runs, the base directory is replaced by a
	// symlink: the pinned path's component no longer is what was
	// verified. A walk through the configured root would follow the
	// new link; the pinned walk must refuse.
	moved := filepath.Join(dir, "moved")
	if err := os.Rename(base, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, base); err != nil {
		t.Fatal(err)
	}

	_, err = store.walkResolved(pinned, []string{clustersDir, "p-2"}, true)
	if err == nil || !strings.Contains(err.Error(), "an entry was substituted") {
		t.Fatalf("walkResolved through a substituted component = %v, want the substitution refusal", err)
	}
	// Nothing was created behind the refusal.
	if _, serr := os.Stat(filepath.Join(moved, "state", "p", "clusters", "p-2")); !os.IsNotExist(serr) {
		t.Errorf("no cluster dir may be created behind the refusal: %v", serr)
	}
}

// KubeconfigPath is the write preflight: an absent kubeconfig is fine
// (the common fresh-create case), but an existing one that is not a
// private, caller-owned regular file is refused before any provider
// call — kind merges into an existing destination, so handing it an
// unsafe path would rewrite or follow that file.
func TestKubeconfigPathPreflightsExistingFile(t *testing.T) {
	newStore := func(t *testing.T) (*Store, string) {
		t.Helper()
		store := Open(filepath.Join(t.TempDir(), "state"))
		path, err := store.KubeconfigPath("p-1")
		if err != nil {
			t.Fatalf("KubeconfigPath on a fresh store = %v, want nil (absent is fine)", err)
		}
		return store, path
	}

	t.Run("permissive existing kubeconfig is refused", func(t *testing.T) {
		store, path := newStore(t)
		if err := os.WriteFile(path, []byte("cfg"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := store.KubeconfigPath("p-1")
		if err == nil || !strings.Contains(err.Error(), "mode") {
			t.Fatalf("KubeconfigPath(0644) = %v, want a mode rejection", err)
		}
	})

	t.Run("symlinked existing kubeconfig is refused without touching the target", func(t *testing.T) {
		store, path := newStore(t)
		target := filepath.Join(t.TempDir(), "outside-state")
		if err := os.WriteFile(target, []byte("precious"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(target, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
		_, err := store.KubeconfigPath("p-1")
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("KubeconfigPath(symlink) = %v, want a symlink rejection", err)
		}
		// The refusal must not have modified anything outside state.
		info, statErr := os.Stat(target)
		if statErr != nil {
			t.Fatal(statErr)
		}
		if perm := info.Mode().Perm(); perm != 0o644 {
			t.Errorf("target perms = %o, want 0644 unchanged", perm)
		}
		if b, rerr := os.ReadFile(target); rerr != nil || string(b) != "precious" {
			t.Errorf("target content = (%q, %v), want it preserved", b, rerr)
		}
	})

	t.Run("private existing kubeconfig passes", func(t *testing.T) {
		store, path := newStore(t)
		if err := os.WriteFile(path, []byte("cfg"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := store.KubeconfigPath("p-1"); err != nil {
			t.Fatalf("KubeconfigPath(0600) = %v, want nil", err)
		}
	})
}

// record.json must not be followed through a symlink either way: a read
// through one trusts state from outside the boundary, and a write
// through one exports state outside it.
func TestRecordSymlinksAreRefused(t *testing.T) {
	store := Open(filepath.Join(t.TempDir(), "state"))
	if err := store.WriteRecord(Record{Name: "p-1", Status: StatusComplete}); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside-state")
	if err := os.WriteFile(target, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	recPath := store.recordPath("p-1")
	if err := os.Remove(recPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, recPath); err != nil {
		t.Fatal(err)
	}

	if _, found, err := store.Record("p-1"); err == nil || found {
		t.Errorf("Record(symlinked record.json) = (found=%v, err=%v), want a refusal", found, err)
	}
	if err := store.WriteRecord(Record{Name: "p-1", Status: StatusComplete}); err == nil {
		t.Error("WriteRecord(symlinked record.json) = nil, want a refusal")
	}
	// Nothing leaked through the link.
	if b, rerr := os.ReadFile(target); rerr != nil || string(b) != "precious" {
		t.Errorf("target content = (%q, %v), want it preserved", b, rerr)
	}
	// Fixing the tree (real file instead of the symlink) restores both.
	if err := os.Remove(recPath); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteRecord(Record{Name: "p-1", Status: StatusComplete}); err != nil {
		t.Fatalf("WriteRecord after replacing the symlink = %v, want nil", err)
	}
	if _, found, err := store.Record("p-1"); err != nil || !found {
		t.Errorf("Record after fixing = (found=%v, err=%v), want (true, nil)", found, err)
	}
}

// RemoveRoot must never remove a directory kind-ocp cannot prove is its
// own: unmarked foreign directories survive, and so does a marked state
// root whose permissions stopped verifying.
func TestRemoveRootRefusesUnprovenTrees(t *testing.T) {
	// remove asserts the two-step flow delete uses: resolve the
	// removable root, then remove exactly that root.
	remove := func(store *Store) error {
		removal, err := store.RemovableRoot()
		if err != nil {
			return err
		}
		defer func() { _ = removal.Close() }()
		return removal.Remove()
	}
	t.Run("foreign directory survives", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		foreign := filepath.Join(root, "important.txt")
		if err := os.WriteFile(foreign, []byte("keep me"), 0o600); err != nil {
			t.Fatal(err)
		}
		store := Open(root)
		err := remove(store)
		if err == nil || !strings.Contains(err.Error(), "not recognized as kind-ocp state") {
			t.Fatalf("removal = %v, want a refusal naming the missing marker", err)
		}
		if b, rerr := os.ReadFile(foreign); rerr != nil || string(b) != "keep me" {
			t.Errorf("foreign file = (%q, %v), want it preserved", b, rerr)
		}
		// RemovableRoot agrees, and stays a no-op for absent roots.
		if removal, err := store.RemovableRoot(); err == nil || !strings.Contains(err.Error(), "kind-ocp state") {
			_ = removal.Close()
			t.Errorf("RemovableRoot() = %v, want the same refusal", err)
		}
		absent, err := Open(filepath.Join(t.TempDir(), "absent")).RemovableRoot()
		if err != nil || absent.Root() != "" {
			t.Errorf("RemovableRoot(absent) = (%q, %v), want (\"\", nil)", absent.Root(), err)
		}
		_ = absent.Close()
	})
	t.Run("permissive marked state survives", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		store := Open(root)
		if err := store.WriteRecord(Record{Name: "p-1"}); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, 0o755); err != nil {
			t.Fatal(err)
		}
		err := remove(store)
		if err == nil || !strings.Contains(err.Error(), "mode") {
			t.Fatalf("removal = %v, want a mode refusal for the exposed state root", err)
		}
		if _, serr := os.Stat(root); serr != nil {
			t.Errorf("state root must be preserved after the refusal: %v", serr)
		}
		// Fixing the permissions lets cleanup proceed.
		if err := os.Chmod(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := remove(store); err != nil {
			t.Fatalf("removal after fixing permissions: %v", err)
		}
	})
	t.Run("malformed marker is refused", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, markerName), []byte("not json"), 0o600); err != nil {
			t.Fatal(err)
		}
		store := Open(root)
		if err := remove(store); err == nil {
			t.Fatal("removal must refuse a tree whose marker does not parse")
		}
		if _, serr := os.Stat(root); serr != nil {
			t.Errorf("state root must be preserved after the refusal: %v", serr)
		}
	})
	t.Run("foreign marker is refused", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, markerName), []byte(`{"tool":"other-tool","version":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		store := Open(root)
		if err := remove(store); err == nil || !strings.Contains(err.Error(), "kind-ocp") {
			t.Fatalf("removal = %v, want a refusal for a foreign marker", err)
		}
	})
}

func TestCacheDirFor(t *testing.T) {
	t.Run("creates the layer's cache lazily under a marked root", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		store := Open(root)
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("Open created %s; it must be lazy", root)
		}
		dir, err := store.CacheDirFor("acme-cache")("")
		if err != nil {
			t.Fatal(err)
		}
		// The walk returns the fully-resolved verified path, which on
		// macOS resolves /var to /private/var; compare the structure,
		// not the configured string.
		if !strings.HasSuffix(dir, filepath.Join("cache", "acme-cache")) {
			t.Errorf("dir = %q, want it under cache/acme-cache", dir)
		}
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("cache dir perms = %o, want 0700", perm)
		}
		// Creating a layer cache is creating state: the root carries the
		// marker, so a later delete recognizes and removes the tree.
		if _, err := readMarker(root); err != nil {
			t.Errorf("state marker: %v", err)
		}
	})

	t.Run("the pin namespaces below the layer, and the empty pin is the layer dir", func(t *testing.T) {
		store := Open(filepath.Join(t.TempDir(), "state"))
		const pin = "54a3998d81eb911e1abb2ffab8b57bfd0571a735"
		dir, err := store.CacheDirFor("acme-cache")(pin)
		if err != nil {
			t.Fatal(err)
		}
		// Resolved-path caveat as above: assert the structure.
		if !strings.HasSuffix(dir, filepath.Join("cache", "acme-cache", pin)) {
			t.Errorf("dir = %q, want it under cache/acme-cache/<pin>", dir)
		}
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			t.Fatalf("keyed cache dir missing: %v", err)
		}
		// The empty pin must not create a phantom component: the layer
		// directory itself is the cache.
		dir, err = store.CacheDirFor("acme-cache")("")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasSuffix(dir, filepath.Join("cache", "acme-cache")) {
			t.Errorf("dir = %q, want the layer dir for the empty pin", dir)
		}
	})

	t.Run("rejects unsafe identifiers without touching disk", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		store := Open(root)
		// macOS — a supported platform — resolves ACME-cache and
		// acme-cache to the same directory: two layers (or two pins of
		// one layer) differing only by case would silently share cache
		// files. Non-ASCII names can merge the same way through
		// Unicode normalization, and empty or path-shaped components
		// are not plain names at all — the safe-name rule (which the
		// registry enforces on layer names too) refuses every one of
		// them before the walk, so a refused identifier must not leave
		// anything on disk either.
		const safe = "acme-cache"
		for _, layer := range []string{
			"ACME-cache", // case variant of the safe name: the alias itself
			"Acme-Cache", // mixed case
			"acme_cache", // underscore
			"acme.cache", // dot
			"acmé",       // non-ASCII
			"",           // nothing
			".",          // current directory
			"..",         // parent directory
			"a/b",        // path separator
		} {
			_, err := store.CacheDirFor(layer)("")
			if err == nil {
				t.Errorf("CacheDirFor(%q) accepted a name that aliases or escapes", layer)
				continue
			}
			if !strings.Contains(err.Error(), "not a safe state-tree name") {
				t.Errorf("CacheDirFor(%q) = %v, want the safe-name rejection", layer, err)
			}
		}
		for _, pin := range []string{"Pin-Digest", "PIN-DIGEST", "pin_digest", "pin.digest", ".."} {
			_, err := store.CacheDirFor(safe)(pin)
			if err == nil {
				t.Errorf("CacheDirFor(%q)(%q) accepted a pin that aliases or escapes", safe, pin)
				continue
			}
			if !strings.Contains(err.Error(), "not a safe state-tree name") {
				t.Errorf("CacheDirFor(%q)(%q) = %v, want the safe-name rejection", safe, pin, err)
			}
		}
		if _, err := os.Stat(filepath.Join(root, "cache", safe)); !os.IsNotExist(err) {
			t.Fatalf("a refused identifier still created %s: %v", filepath.Join(root, "cache", safe), err)
		}
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Fatalf("a refused identifier still created the state root %s: %v", root, err)
		}
	})

	t.Run("rejects a symlinked cache directory", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		if err := os.MkdirAll(filepath.Join(root, "cache"), 0o700); err != nil {
			t.Fatal(err)
		}
		markerContent, err := json.Marshal(marker{Tool: "kind-ocp"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, markerName), markerContent, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(root, "cache")); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(t.TempDir(), "elsewhere")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(root, "cache")); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(root).CacheDirFor("acme-cache")(""); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("CacheDirFor() over a symlinked cache dir = %v, want a symlink refusal", err)
		}
	})
}

// Package state manages kind-ocp's on-disk state: one directory per prefix
// under the state root, holding each cluster's kubeconfig (cluster-admin
// credentials) and record.
//
// The tree is a verified security boundary, not just a storage
// convention. Directories above the per-prefix store root may be shared
// system directories (root- or caller-owned, writable by no one but
// their owner unless the sticky bit protects them). Symlinked ancestor
// components are resolved hop by hop, and every hop is verified before
// it is followed: the symlink entry must be caller- or root-owned
// (whoever can replace an entry must be trusted), and every directory
// component of its target must pass the ancestor policy — macOS $TMPDIR
// lives under the /var symlink, and an alias chaining through several
// links is followed only when every link in the chain checks out. The
// store root and everything below it must be caller-owned, private
// (0700 directories, 0600 files), and free of symlinks; on platforms
// whose extended ACLs can grant access beyond the mode bits (macOS)
// they must be free of ACL entries too — on Linux the POSIX ACL mask
// is the mode's group bits, so the mode checks already cover it (see
// fscheck). Anything that cannot be verified fails closed, with the
// fix in the error.
//
// Verification and use are the same path: the walk resolves ancestor
// symlinks and returns the fully-resolved directory it verified, and
// every read, write, provider handoff, and recursive cleanup acts on
// that resolved path. Validating one path and then writing through
// another (the symlinked one) would leave the verified chain behind.
//
// A delete operation goes one step further and pins the directory
// itself, not just its path: it holds the verified store root open for
// the operation's lifetime (see Removal), and every later step
// compares the held directory with whatever now sits at the pinned
// path via os.SameFile before acting — a tree renamed away and
// replaced at the same path is a different directory, and the
// operation refuses it instead of acting on whatever now occupies the
// name. Holding the directory open is what makes the comparison
// trustworthy: an inode with an open reference cannot be reassigned,
// so a replacement created after the pin — even one a filesystem that
// aggressively recycles inode numbers would hand the very same device
// and inode pair — can never be mistaken for the pinned tree.
//
// When kind-ocp first creates the store root it writes a state marker
// (kind-ocp-state.json). An existing unmarked directory is adopted only
// when it is empty, and recursive cleanup only ever removes a marked,
// verified tree — so a pre-existing unrelated directory under a custom
// state root is never deleted.
//
// The package is internal: external consumers see create results through
// the kindocp façade's Record type, not the storage layout.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stackrox/kind-ocp/internal/fscheck"
	"github.com/stackrox/kind-ocp/internal/safename"
)

// Record is the per-cluster state written by create. It is the source of
// truth for what was applied, used by reuse-compatibility checks, list, and
// verify. A record is written with StatusPending before layers run and
// rewritten with StatusComplete after they all succeed, so a cluster left
// behind by a failed create is distinguishable from a finished one.
type Record struct {
	Name          string    `json:"name"`
	Context       string    `json:"context"`
	Profile       string    `json:"profile"`
	Layers        []string  `json:"layers"`
	KindNodeImage string    `json:"kindNodeImage"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"createdAt"`
	// InstanceID identifies the cluster instance this record was written
	// for: the kube-system namespace UID (see kube.ClusterInstanceID). A
	// cluster deleted and re-created under the same name is a different
	// instance, and records carrying the old ID must not be trusted for it.
	// Create binds the ID once the cluster's API answers, before any layer
	// runs, so every complete record carries one. An empty ID means the
	// create run was interrupted before the API answered; such a record is
	// never re-adopted — recovery is delete and re-create.
	InstanceID string `json:"instanceId,omitempty"`
}

// Record statuses.
const (
	// StatusPending marks a record whose create run did not finish: the
	// cluster was provisioned and matches the recorded profile, but the
	// layers may be partially applied.
	StatusPending = "pending"
	// StatusComplete marks a record whose create run applied every layer.
	StatusComplete = "complete"
)

// The store's on-disk names.
const (
	markerName     = "kind-ocp-state.json"
	expectedName   = "expected-clusters"
	clustersDir    = "clusters"
	kubeconfigName = "kubeconfig"
	recordName     = "record.json"
	cacheDir       = "cache"
)

// marker is the content of the state marker file, written into the store
// root when kind-ocp creates it. Its presence and content are what make
// recursive cleanup safe: Removal.Remove only removes a tree kind-ocp
// marked as its own. The recognized format is a JSON object whose "tool" is
// "kind-ocp"; unknown fields (a version number, in older trees) are
// ignored rather than interpreted.
//
// The marker is written once, at root creation, and never rewritten:
// the expected-cluster count lives in a separate private file beside it
// (see SetExpectedClusters), so no later write can invalidate the
// ownership proof — and a field this binary does not know can never
// make the marker unreadable, whatever shape it carries.
type marker struct {
	Tool string `json:"tool"`
}

// ErrCorruptCount is returned by ExpectedClusters when the persisted
// expectation is present but not an integer count create could have
// written — a non-numeric value, a fractional number, or a number that
// overflows int. The marker's validity is independent of it (ownership
// never depends on the count), so callers classify this as corrupt
// state to report and recover from, never as a reason to refuse the
// tree.
var ErrCorruptCount = errors.New("a corrupt expected-cluster count")

// Store is the state directory for one prefix:
// <stateRoot>/<prefix>/clusters/<name>/{kubeconfig,record.json}.
type Store struct {
	root string
	// rootNames are root's path components from the filesystem root down.
	// The components before the last are the store's ancestors; the last
	// is the store root, where the managed (strict) region begins.
	rootNames []string
}

// Open returns the store for one prefix. It does not touch the filesystem:
// directories are created on first write, so opening a store for a prefix
// that turns out to have nothing to do is side-effect free.
func Open(root string) *Store {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	return &Store{root: root, rootNames: fscheck.PathNames(root)}
}

// Root returns the store's root directory as configured. It is for
// messages and display; file operations go through the walk-verified
// resolved paths (see walk).
func (s *Store) Root() string { return s.root }

func (s *Store) clusterDir(name string) string {
	return filepath.Join(s.root, clustersDir, name)
}

func (s *Store) recordPath(name string) string {
	return filepath.Join(s.clusterDir(name), recordName)
}

// walk verifies — and when create is set, creates — every directory from
// the filesystem root down to the store root joined with extra, applying
// the boundary policy on the way down, and returns the fully-resolved
// path it verified (fscheck.WalkDirNames carries the policy; the store
// adds its marker through the walk's root hook):
//
//   - components above the store root get the ancestor policy. An
//     ancestor symlink is trusted only when the entry itself is caller-
//     or root-owned, and is then resolved hop by hop: every directory
//     component of each target is checked with the ancestor policy, and
//     a target component that is itself a symlink must be a trusted
//     entry whose own target is resolved the same way;
//   - the store root and everything below get the managed policy
//     (caller-owned, private, no symlinks; no ACL entries where the
//     platform's ACLs can outdo the mode bits — see the fscheck
//     package doc).
//
// The returned path is what later operations must use. It contains no
// symlinks — ancestor components were resolved away and managed
// components reject them — so writers, readers, and recursive cleanup
// act on exactly the chain that was verified. The entries that chain is
// built from are not substitutable by another local user: every
// component is caller- or root-owned, non-sticky ancestors are writable
// by their owner alone, and sticky ancestors protect their entries.
//
// With create, missing components are created 0700 and the store root is
// marked: an existing unmarked root is adopted only when empty, and a
// non-empty unmarked directory is refused — kind-ocp never mixes its
// state into a directory it cannot prove is its own. Without create, a
// missing component ends the walk with an error wrapping fs.ErrNotExist:
// nothing below it can exist, and callers treat that as "no state".
//
// Creation races fail closed: a component that appears between the
// not-exist check and Mkdir makes Mkdir fail or gets re-inspected, and a
// directory another user slipped in does not pass the policy checks.
// Callers that create clusters concurrently must ensure the store root
// (marker included) once before fanning out, so per-cluster walks only
// ever find an already-marked root.
func (s *Store) walk(extra []string, create bool) (string, error) {
	if len(s.rootNames) == 0 {
		return "", fmt.Errorf("state root %s is the filesystem root; refusing to manage it", s.root)
	}
	names := append(append([]string(nil), s.rootNames...), extra...)
	// The configured root may be reached through ancestor aliases, so
	// they are trusted and resolved hop by hop.
	return fscheck.WalkDirNames(names, len(s.rootNames)-1, s.walkOptions(create, true))
}

// walkResolved verifies — and when create is set, creates below — the
// chain of root, a fully-resolved store root a previous walk returned,
// joined with extra, applying the same policies as walk. It follows no
// symlinks: the resolved chain contained none when it was verified, so
// a symlink component now means an entry was substituted since, and
// the operation is refused instead of redirected. It never consults
// the configured state root, so a re-pointed alias cannot move an
// operation pinned to root onto another tree: what is created,
// written, and removed stays under root.
func (s *Store) walkResolved(root string, extra []string, create bool) (string, error) {
	rootNames := fscheck.PathNames(root)
	if len(rootNames) == 0 {
		return "", fmt.Errorf("state root %s is the filesystem root; refusing to manage it", root)
	}
	names := append(append([]string(nil), rootNames...), extra...)
	return fscheck.WalkDirNames(names, len(rootNames)-1, s.walkOptions(create, false))
}

// walkOptions builds the store's walk options. The state marker is
// written exactly on creating walks — where the walk wrote it before
// the loop moved into fscheck — so a read-only walk never marks, and a
// creating walk below an already-marked root re-verifies the marker for
// free.
func (s *Store) walkOptions(create, followAliases bool) fscheck.WalkOptions {
	o := fscheck.WalkOptions{Create: create, FollowAliases: followAliases}
	if create {
		o.OnManagedRoot = ensureMarker
	}
	return o
}

// prepareRoot verifies the ancestors of the store root (creating
// missing ones, as walk does) and creates the store root itself
// exclusively — the returned tree is this call's own fresh creation or
// the call fails. It is how a removal that found no state prepares the
// tree its handoffs and cleanup stay pinned to: a root that appeared
// since the removal started (a concurrent create, say) is refused
// rather than adopted, so the removal can never capture — and later
// remove — a tree it did not prepare. The Mkdir rejects exactly the
// state present at creation time; a separate existence check followed
// by a create would race with whatever appears in between. What appears
// after the Mkdir — between it and the walk's inspection of the new
// directory — is not this function's to catch: the removal's held
// handle detects a replaced root at its next step, and callers should
// serialize create and delete for the same prefix rather than rely on
// either check.
func (s *Store) prepareRoot() (string, error) {
	if len(s.rootNames) == 0 {
		return "", fmt.Errorf("state root %s is the filesystem root; refusing to manage it", s.root)
	}
	o := s.walkOptions(true, true)
	o.ExclusiveRoot = true
	return fscheck.WalkDirNames(s.rootNames, len(s.rootNames)-1, o)
}

// EnsureRoot verifies the store's ancestors and the store root itself,
// creating whatever is missing (0700, marker included). It is idempotent:
// an already-marked root is verified and returned unchanged.
//
// It is the once-per-run step concurrent cluster creation must take
// before fanning out (see walk): per-cluster walks then only ever find
// an already-marked root, so no two of them race to create it. It
// returns the fully-resolved root path.
func (s *Store) EnsureRoot() (string, error) {
	return s.walk(nil, true)
}

// SetExpectedClusters records the number of clusters create is setting
// out to build for this prefix, in a private file beside the state
// marker the root already carries (the creating walk wrote or validated
// the marker). Verify's whole-prefix count assertion reads this
// expectation — never a count resolved at verify time — so it reports
// what create built, whatever count a later verify invocation would
// resolve.
//
// The expectation deliberately does not live in the marker: the marker
// is the ownership proof delete and every walk rely on, and a failed
// count write must never be able to invalidate it. The sidecar is
// instead replaced atomically — written to a private temporary file
// os.CreateTemp stages in the same directory (the root this function
// just walked, so the rename never crosses a filesystem) and renamed
// over the target only after a successful write and close — so a
// failed update leaves the previous expectation (and the marker, which
// is not touched at all) exactly as they were; the live file is never
// opened for writing, never truncated in place. The staging name is
// unique per writer and the file is created exclusively (0600), so
// nothing can be parked at it and overlapping updates cannot open,
// truncate, or rename each other's staging file: whatever reaches the
// target is always one writer's complete write. The target itself is
// preflighted (CheckWriteTarget) before anything is staged: a symlink
// or shared file parked there is a refusal, not something the rename
// replaces sight unseen. A marker that does not validate is an error
// too — the walk refuses the tree before this function writes
// anything.
func (s *Store) SetExpectedClusters(count int) error {
	root, err := s.walk(nil, true)
	if err != nil {
		return err
	}
	path := filepath.Join(root, expectedName)
	if err := fscheck.CheckWriteTarget(path); err != nil {
		return fmt.Errorf("recording the expected cluster count: %w", err)
	}
	b := []byte(strconv.Itoa(count) + "\n")
	tmp, err := os.CreateTemp(root, "."+expectedName+".tmp-*")
	if err != nil {
		return fmt.Errorf("recording the expected cluster count: %w", err)
	}
	// Unconditional and deferred: after a successful rename the staging
	// name is already gone and removing it is a no-op, while any
	// failure — write, close, or the rename itself — never leaves this
	// writer's staging file behind.
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("recording the expected cluster count: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("recording the expected cluster count: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("recording the expected cluster count: %w", err)
	}
	return nil
}

// ExpectedClusters returns the number of clusters the last create set
// out to build for this prefix, from the private file create wrote
// beside the marker. The marker must validate first — ownership is the
// gate every other read runs behind, and the count read is no
// exception — but only the marker's own validity: the count's shape
// never affects it (see ErrCorruptCount). found is false when the tree
// carries no such value — a state root written by an older binary —
// and that is not an error: verify degrades to a SKIP, never a false
// FAIL. A value that is present but not an integer (a string, a
// fraction, an overflow) wraps ErrCorruptCount: the same corruption
// class as an integer outside any range create validates, and like it
// the caller's to classify (see config.IsCreatableCount). A file
// longer than any count create writes is the same class — the read is
// bounded, so a corrupt file's length never becomes this process's
// allocation. A tree or file that fails the boundary checks is a plain
// error, reported as itself.
func (s *Store) ExpectedClusters() (count int, found bool, err error) {
	root, err := s.walk(nil, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	// A tree whose marker does not validate is not kind-ocp's to read
	// an expectation from: the error is the marker's own, reported as
	// a read failure (the same refusal every walk-backed read makes).
	// A valid marker with unknown fields — including a count field an
	// older binary wrote, in whatever shape — passes untouched.
	if found, err := readMarker(root); err != nil || !found {
		return 0, false, err
	}
	path := filepath.Join(root, expectedName)
	if err := fscheck.CheckPrivateFile(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	defer func() { _ = f.Close() }()
	// The read is bounded by maxExpectedBytes, one byte past the bound:
	// that distinguishes "at the bound" from "over it" without a stat
	// call — a size read before the open is a bound the file can change
	// after, and the read is the honest one. A file longer than any
	// count create writes classifies as corrupt right here, the same
	// class as a non-integer, so a corrupt file's length never becomes
	// this process's allocation.
	b, err := io.ReadAll(io.LimitReader(f, maxExpectedBytes+1))
	if err != nil {
		return 0, false, err
	}
	if len(b) > maxExpectedBytes {
		return 0, false, fmt.Errorf("%w in %s: the file is longer than any count create writes", ErrCorruptCount, path)
	}
	count, err = strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false, fmt.Errorf("%w in %s: the file holds %s, not an integer create could have written",
			ErrCorruptCount, path, holdPreview(b))
	}
	return count, true, nil
}

// holdPreview names what a corrupt expectation file holds, for its
// error message. The read that produced b is bounded
// (maxExpectedBytes), so the preview is bounded with it.
func holdPreview(b []byte) string {
	if s := strings.TrimSpace(string(b)); s != "" {
		return strconv.Quote(s)
	}
	return "nothing"
}

// maxExpectedBytes bounds how much of the expectation file is read: a
// count create writes is at most three bytes ("64\n"), so anything
// longer cannot be one, and the file's length is on-disk state this
// run did not write — no read allocates for more of it than a count
// could occupy.
const maxExpectedBytes = 32

// ensureClusterDir creates the cluster's state directory (0700, parents
// included, state marker in place) if it does not exist, after verifying
// every existing component of the path against the boundary policy, and
// returns the fully-resolved verified directory.
func (s *Store) ensureClusterDir(name string) (string, error) {
	dir, err := s.walk([]string{clustersDir, name}, true)
	return dir, err
}

// CacheDirFor returns the cache-directory callback bound to one layer
// (layers.CacheDirFunc's shape): calling it with the layer's content pin
// ("" for none) returns the walk-verified directory
// <root>/cache/<layer>[/<pin>], creating it (0700, state marker included)
// if it does not exist. Layers cache content they fetch from pinned
// upstream sources there: one fetch shared by the prefix's clusters
// instead of one per cluster, and gone with the rest of the prefix's
// state when delete removes the tree.
//
// Binding the layer name here — instead of exporting it to callers — is
// what keeps a layer inside its own cache: the callback the layer
// receives can address only its own directory, under the one pin it
// passes. The layer name and the pin go through the same component
// validation and boundary policy as the rest of the store's paths: a
// component that is not a plain single name is rejected before it can
// reach a filesystem join, and every directory the path traverses below
// the store root must be caller-owned, private, and free of symlinks —
// the same rules the walk applies to cluster directories. Both are also
// held to the safe state-tree alphabet (safename.Name), and that is what
// makes the mapping unambiguous: macOS — a supported platform — resolves
// ACME-cache and acme-cache to one directory, so two layers (or two pins
// of one layer) differing only by case would silently share cache files,
// and non-ASCII names can merge the same way through Unicode
// normalization. Such identifiers are refused here, before any disk
// touch, never silently renamed: the identifier is also the key the
// registry and the config file know the layer by. kindocp hands the
// callback to layers through layers.Env.CacheDir; layers namespace
// below the returned directory with regular files only.
func (s *Store) CacheDirFor(layer string) func(pin string) (string, error) {
	return func(pin string) (string, error) {
		if err := safename.Name(layer); err != nil {
			return "", fmt.Errorf("layer cache directory: %w", err)
		}
		if pin != "" {
			if err := safename.Name(pin); err != nil {
				return "", fmt.Errorf("cache pin: %w", err)
			}
			extra := []string{cacheDir, layer, pin}
			return s.walk(extra, true)
		}
		return s.walk([]string{cacheDir, layer}, true)
	}
}

// KubeconfigPath ensures the cluster's state directory and returns the
// fully-resolved path of its kubeconfig file — the path every writer of
// the credential (the provisioner, kind underneath it) must be handed,
// and the path handed back to callers. It is also the write preflight:
// kind merges into an existing destination, so a kubeconfig that already
// exists must verify as a private, caller-owned regular file before any
// provider call — an exposed, foreign, or symlinked cached credential is
// refused instead of being rewritten through or around.
func (s *Store) KubeconfigPath(name string) (string, error) {
	dir, err := s.ensureClusterDir(name)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, kubeconfigName)
	if err := fscheck.CheckWriteTarget(path); err != nil {
		return "", err
	}
	return path, nil
}

// WriteRecord persists the cluster record (0600), creating the cluster
// directory as needed. An existing record.json must verify as a private,
// caller-owned regular file first: a symlinked or shared record is
// refused instead of written through.
func (s *Store) WriteRecord(rec Record) error {
	dir, err := s.ensureClusterDir(rec.Name)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, recordName)
	if err := fscheck.CheckWriteTarget(path); err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("writing record %s: %w", path, err)
	}
	return nil
}

// Record returns the record for a cluster. found is false when no record
// exists; that is not an error. The state chain down to the cluster
// directory must pass the boundary checks first, and the record file
// itself must be a private, caller-owned regular file — a record read
// through a non-private, symlinked, or foreign file is not trusted.
func (s *Store) Record(name string) (rec Record, found bool, err error) {
	dir, err := s.walk([]string{clustersDir, name}, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}
	path := filepath.Join(dir, recordName)
	if err := fscheck.CheckPrivateFile(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Record{}, false, err
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return Record{}, false, fmt.Errorf("parsing record %s: %w", path, err)
	}
	return rec, true, nil
}

// Records returns all cluster records, sorted by name. An empty or missing
// store yields no records and no error. The state chain down to the
// clusters directory must pass the boundary checks first.
func (s *Store) Records() ([]Record, error) {
	dir, err := s.walk([]string{clustersDir}, false)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		rec, found, err := s.Record(e.Name())
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// CheckKubeconfig verifies the cached kubeconfig of a cluster and returns
// its fully-resolved path: the state chain down to the cluster directory
// must pass the boundary checks, and the file must be a private,
// caller-owned regular file. A missing chain or file is reported as an
// error wrapping fs.ErrNotExist; anything that fails verification is
// reported as itself.
func (s *Store) CheckKubeconfig(name string) (string, error) {
	dir, err := s.walk([]string{clustersDir, name}, false)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, kubeconfigName)
	if err := fscheck.CheckPrivateFile(path); err != nil {
		return "", err
	}
	return path, nil
}

// requireMarker refuses trees that do not carry a valid state marker:
// recursive removal is restricted to trees kind-ocp can prove are its
// own, so a pre-existing unrelated directory under a custom state root
// is never deleted.
func requireMarker(root string) error {
	found, err := readMarker(root)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf(
			"%s carries no %s; it is not recognized as kind-ocp state — kind-ocp will not remove it; if it is not needed, remove it manually",
			root, markerName)
	}
	return nil
}

// openRootDir takes an open handle on the directory a walk just
// verified (or a handoff just prepared) — the pin a removal holds for
// its lifetime. The handle is what makes the pin resistant to inode
// recycling: an inode with an open reference cannot be reassigned, so
// whatever later sits at the pinned path can be compared against the
// held directory (os.SameFile) and told apart from it even on a
// filesystem that would hand a replacement the freed directory's very
// own identity. The handle is taken on the resolved path right after
// verification; a substitution in the window between the walk's
// inspections and this open is not detected by the pin (see
// fscheck.WalkDirNames).
func openRootDir(resolved string) (*os.File, error) {
	f, err := os.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("holding the state directory %s open: %w", resolved, err)
	}
	return f, nil
}

// Removal is the state half of one delete operation: the verified tree
// its kubeconfig handoffs and its cleanup are pinned to — by path and
// by an open handle on the directory itself.
//
// RemovableRoot starts it before any lifecycle side effect. When state
// exists, the tree verified then is the tree every later step acts on:
// a configured state root that no longer resolves to exactly it —
// re-pointed, removed, or failing its checks — is a refusal before any
// further write, never a switch onto whatever the root resolves to
// now. When no state exists, the first handoff prepares the tree
// (created exclusively, verified, marker included) and captures it;
// handoffs and cleanup then act on that capture wherever the state
// root points by then, so cleanup removes only state the delete
// itself prepared — a tree that appears before the first handoff is
// refused, never adopted.
//
// The pin is the directory, not the name: the handle opened at
// verification or preparation (see openRootDir) is compared — via
// os.SameFile against whatever currently sits at the pinned path —
// before every handoff and before the cleanup. A rename carries the
// held directory and its identity along, so moving the tree away and
// back does not disturb the operation. A rename away plus a
// replacement at the old path — the same name holding a different
// directory — is a refusal, and so is a pinned root that went missing
// before a handoff: the handoff never recreates the tree it is pinned
// to, because the original would survive the cleanup unrecognized,
// credentials included, wherever it was moved to.
//
// The caller owns the handle's lifetime: Close releases it, and the
// delete operation (kindocp.Delete) must call Close on every return
// path, including the ones that never reach Remove.
//
// The pin makes a replacement detectable, not impossible: the checks
// and path-based operations between them are sequential, not one
// atomic snapshot, so a substitution that lands between two steps is
// caught at the next comparison. Callers should serialize create and
// delete for the same prefix rather than rely on that ordering.
type Removal struct {
	store *Store
	root  string
	// rootDir is the open handle on the root directory this removal
	// verified or prepared. Later steps compare the directory it
	// holds with whatever sits at the pinned path, so a replacement
	// there is a refusal, not a tree to act on — and the held
	// directory's own identity cannot be reassigned while the handle
	// is open (see openRootDir). Nil after Close.
	rootDir *os.File
	// captured marks a root this removal resolved and prepared itself
	// (no state existed when it started), as opposed to one verified
	// up front by RemovableRoot. A captured root is this removal's
	// own, so its steps act on it directly instead of re-checking
	// what the configured root resolves to.
	captured bool
}

// Root returns the resolved tree this removal is pinned to — "" while
// it has none (no state existed when it started and no handoff has
// run yet).
func (r *Removal) Root() string { return r.root }

// Close releases the removal's handle on its pinned directory. It is
// safe to call on a removal that never held one (no state existed and
// no handoff ran) and more than once; after it, the removal's further
// steps are refused instead of silently losing their pin. The delete
// operation calls it on every return path.
func (r *Removal) Close() error {
	if r.rootDir == nil {
		return nil
	}
	err := r.rootDir.Close()
	r.rootDir = nil
	return err
}

// RemovableRoot verifies that the per-prefix state directory is either
// absent (nothing to remove; a removal with no root) or verifiably
// kind-ocp state, and returns the delete operation pinned to the
// resolved root — which means holding the verified directory open
// until the operation's Close. The boundary check happens before any
// lifecycle side effect, so a tree kind-ocp cannot prove its own
// refuses the whole delete rather than end up deleting the clusters
// and stranding unrecognized state behind them.
func (s *Store) RemovableRoot() (*Removal, error) {
	resolved, err := s.walk(nil, false)
	if errors.Is(err, fs.ErrNotExist) {
		return &Removal{store: s}, nil
	}
	if err != nil {
		return nil, err
	}
	// The pin is taken as close behind verification as it can be, so
	// the marker check runs with the directory already held.
	f, err := openRootDir(resolved)
	if err != nil {
		return nil, err
	}
	if err := requireMarker(resolved); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Removal{store: s, root: resolved, rootDir: f}, nil
}

// requirePinnedResolution verifies that the configured state root
// still resolves to exactly root — the tree an operation verified and
// is pinned to. A root that resolves elsewhere, or that no longer
// resolves at all, is a refusal before any further write: the
// operation will neither hand the provisioner a path under a tree it
// did not verify nor remove one.
func (s *Store) requirePinnedResolution(root string) error {
	now, err := s.walk(nil, false)
	switch {
	case err == nil && now == root:
		return nil
	case err == nil:
		return fmt.Errorf(
			"state root %s now resolves to %s, not the %s verified at the start of this operation — kind-ocp will not act on a tree it did not verify; point the state root back and re-run",
			s.root, now, root)
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf(
			"state root %s no longer resolves — this operation is pinned to the tree %s it verified and will not act on any other; point the state root back and re-run",
			s.root, root)
	default:
		return fmt.Errorf("state root %s no longer verifies: %w", s.root, err)
	}
}

// KubeconfigPath ensures the cluster's state directory and returns the
// fully-resolved kubeconfig path to hand the provisioner while
// deleting name. It is the write preflight of Store.KubeconfigPath:
// kind strips the cluster's context from that file, so an existing
// kubeconfig that is not a private, caller-owned regular file is
// refused before the provider is invoked, and the directory is ensured
// so kind's locking never creates a missing parent 0755. The path is
// always explicit — kind would otherwise fall back to the user's
// default kubeconfig.
//
// The path is pinned to the tree this removal verified — to the
// directory, not just the pathname. With a root verified up front, the
// configured state root must still resolve to exactly that tree — a
// re-pointed or removed ancestor alias is a refusal before any write,
// so kind is never handed a path under a tree the operation did not
// verify. With no state at the start, the first handoff prepares the
// tree itself — creating the store root exclusively, so state that
// appeared since the removal started is a refusal, not a tree to
// capture — and later handoffs act on the capture without consulting
// the state root again, so kind only ever modifies the one tree. Every
// handoff then checks the pinned root is still the directory it was at
// verification or preparation: one that went missing is not recreated,
// and a replacement at the same path is a refusal before any write
// (see requirePinnedRoot).
func (r *Removal) KubeconfigPath(name string) (string, error) {
	if r.root == "" {
		root, err := r.store.prepareRoot()
		if err != nil {
			return "", err
		}
		f, err := openRootDir(root)
		if err != nil {
			return "", err
		}
		r.root, r.rootDir, r.captured = root, f, true
	} else if !r.captured {
		if err := r.store.requirePinnedResolution(r.root); err != nil {
			return "", err
		}
	}
	if err := r.requirePinnedRoot(); err != nil {
		return "", err
	}
	dir, err := r.store.walkResolved(r.root, []string{clustersDir, name}, true)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, kubeconfigName)
	if err := fscheck.CheckWriteTarget(path); err != nil {
		return "", err
	}
	return path, nil
}

// requirePinnedRoot verifies, immediately before a handoff, that the
// directory at the pinned root is still the one this removal verified
// or prepared. A root that is gone is a refusal — the handoff will not
// recreate the tree it is pinned to, because the original would then
// survive the cleanup unrecognized wherever it was moved to. A
// different directory at the same path — the original renamed away
// and a replacement put in its place, or the original removed and new
// state created at the same path — is a refusal too: the path
// matches, the directory does not, and the removal will not write
// into a tree it did not verify or prepare.
func (r *Removal) requirePinnedRoot() error {
	cur, err := os.Lstat(r.root)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf(
			"the state directory %s this operation is pinned to is gone — kind-ocp will not recreate it; re-run the operation",
			r.root)
	}
	if err != nil {
		return fmt.Errorf("checking the state directory %s: %w", r.root, err)
	}
	return r.requireSameEntry(cur)
}

// requireSameEntry refuses a directory at the pinned root that is not
// the one this removal's handle holds: os.SameFile compares the held
// directory (by handle) with cur (the pinned path's current entry), a
// comparison a rename does not disturb — the held directory keeps its
// identity wherever it is moved to — but a replacement never passes,
// because the held directory's own identity cannot be reassigned while
// the handle is open.
func (r *Removal) requireSameEntry(cur os.FileInfo) error {
	if r.rootDir == nil {
		return fmt.Errorf("the removal's handle on the state directory %s is closed; the operation is finished — re-run it", r.root)
	}
	pinned, err := r.rootDir.Stat()
	if err != nil {
		return fmt.Errorf("checking the state directory %s this operation is pinned to: %w", r.root, err)
	}
	if !os.SameFile(pinned, cur) {
		return fmt.Errorf(
			"the directory at %s is not the kind-ocp state tree this operation verified or prepared — it was replaced; kind-ocp will not act on it; re-run the operation",
			r.root)
	}
	return nil
}

// Remove removes the tree this removal is pinned to — never a
// different one. It does not end the operation: a refusal leaves the
// pin in place, so the caller can fix what the refusal named (point
// the state root back, say) and call Remove again; releasing the pin
// is the caller's job (Close).
//
// The pinned tree is identified before anything else: already gone
// means there is nothing to remove (a no-op, never a recreation), and
// a different directory at the pinned path — the original renamed
// away and a replacement put in its place, or the original removed
// and new state created at the same path — is a refusal that leaves
// every tree in place, because removing the replacement would report
// success while erasing state this removal never verified. A root
// verified up front must also still be what the configured state root
// resolves to: an ancestor alias re-pointed or removed since is a
// refusal that leaves every tree in place (point it back and re-run).
// A captured root — state this removal prepared itself — is removed
// as it stands, wherever the state root points by then. A removal
// that never got a root (no state existed and no handoff ran) removes
// nothing, so a tree that appears meanwhile is never swept up. The
// tree is re-verified — chain, policies, and marker — immediately
// before the removal; that re-verification, the identity comparison,
// and the removal itself are sequential steps, not one atomic
// snapshot, so a substitution between them is caught by the held
// handle at the next step, never prevented here.
func (r *Removal) Remove() error {
	if r.root == "" {
		return nil
	}
	cur, err := os.Lstat(r.root)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // the pinned tree is already gone; nothing to remove
	}
	if err != nil {
		return err
	}
	if err := r.requireSameEntry(cur); err != nil {
		return err
	}
	if !r.captured {
		if err := r.store.requirePinnedResolution(r.root); err != nil {
			return err
		}
	}
	if _, err := r.store.walkResolved(r.root, nil, false); err != nil {
		return err
	}
	if err := requireMarker(r.root); err != nil {
		return err
	}
	if err := os.RemoveAll(r.root); err != nil {
		return fmt.Errorf("removing state dir %s: %w", r.store.root, err)
	}
	return nil
}

// ensureMarker makes sure the store root carries a valid state marker. A
// present but unreadable, malformed, or foreign marker is an error, not a
// rewrite: a tree kind-ocp cannot prove its own is one it must not touch.
func ensureMarker(root string) error {
	found, err := readMarker(root)
	if err != nil || found {
		return err
	}
	// No marker: only an empty root may be adopted.
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("inspecting %s: %w", root, err)
	}
	if len(entries) != 0 {
		return fmt.Errorf(
			"%s exists but is not kind-ocp state (no %s and not empty); kind-ocp will not write into it — remove it, or point the state root elsewhere",
			root, markerName)
	}
	if err := writeMarker(root); err != nil {
		return fmt.Errorf("writing the state marker in %s: %w", root, err)
	}
	return nil
}

// readMarker reads and validates the state marker in root. found is
// false without an error when no marker exists; a present but
// unreadable, malformed, or foreign marker is an error. Only the
// "tool" field is interpreted — every other field, whatever shape it
// carries, is ignored: the marker proves ownership and nothing else,
// so a count this binary cannot decode (or any field a newer binary
// writes) can never make a tree kind-ocp owns unreadable to it.
func readMarker(root string) (found bool, err error) {
	path := filepath.Join(root, markerName)
	if err := fscheck.CheckPrivateFile(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	var m marker
	if err := json.Unmarshal(b, &m); err != nil {
		return false, fmt.Errorf("parsing %s: %w", path, err)
	}
	if m.Tool != "kind-ocp" {
		return false, fmt.Errorf("%s does not identify kind-ocp state (tool %q)", path, m.Tool)
	}
	return true, nil
}

// writeMarker creates the state marker (0600). A concurrent writer that
// got there first is fine as long as its marker is valid.
func writeMarker(root string) error {
	b, err := json.Marshal(marker{Tool: "kind-ocp"})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(root, markerName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if !os.IsExist(err) {
			return err
		}
		_, err = readMarker(root)
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

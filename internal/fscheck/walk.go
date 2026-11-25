package fscheck

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// PathNames splits an absolute path into its components, dropping empty
// and "." entries ("/" itself yields none).
func PathNames(path string) []string {
	var names []string
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		if part != "" && part != "." {
			names = append(names, part)
		}
	}
	return names
}

// WalkOptions configures WalkDirNames.
type WalkOptions struct {
	// Create makes the walk create missing components as 0700
	// directories, re-inspecting whatever a lost creation race leaves
	// (only what passes the checks is used). Without it, a missing
	// component fails the walk with an error wrapping fs.ErrNotExist.
	Create bool

	// FollowAliases lets the walk follow ancestor symlinks that pass
	// CheckAncestorSymlink, resolving them hop by hop (see the package
	// doc). Without it, any symlink in the chain is a refusal: a
	// previous walk returned a symlink-free resolved path, so a symlink
	// there now means an entry was substituted since.
	FollowAliases bool

	// ExclusiveRoot (with Create) creates the component at managedFrom
	// exclusively: a directory that already exists there — or that wins
	// the Mkdir race — is a refusal, so the walk proceeds only on a tree
	// it created itself. The refusal is final — no retry, no adoption
	// after a failed inspection — and the Mkdir rules out pre-existing
	// state only: a directory substituted after the Mkdir, before the
	// walk's subsequent checks on the new tree, is not caught here.
	ExclusiveRoot bool

	// OnManagedRoot, when set, runs right after the component at
	// managedFrom passes the managed policy — before the walk descends
	// below it. The state store uses it to mark its root; a walk without
	// a marker leaves it nil.
	OnManagedRoot func(path string) error
}

// WalkDirNames is the verification loop behind the state tree's walks: it
// walks names from the filesystem root down, applying the ancestor policy
// (CheckAncestorDir) to components above managedFrom and the managed
// policy (CheckManagedDir) from managedFrom on, and returns the
// fully-resolved path it verified.
//
// The returned path is what later operations must use. It contains no
// symlinks — ancestor components were resolved away and managed
// components reject them — so writers, readers, and recursive cleanup act
// on exactly the chain that was verified. The entries that chain is built
// from are not substitutable by another local user: every component is
// caller- or root-owned, non-sticky ancestors are writable by their owner
// alone, and sticky ancestors protect their entries.
//
// With Create, missing components are created 0700. Creation races fail
// closed: a component that appears between the not-exist check and Mkdir
// makes Mkdir fail or gets re-inspected, and a directory another user
// slipped in does not pass the policy checks. Callers that create
// concurrently below one root must ensure the root once before fanning
// out, so per-name walks only ever find it already verified.
//
// The inspections are sequential, not one atomic snapshot: this loop
// Lstats each component for its own symlink check, and each policy check
// performs its own Lstat of the same path afterwards. The walk therefore
// verifies the chain as a sequence of observations; it does not detect a
// substitution that happens between two of its inspections within one
// call. Cross-call substitution is what the callers guard: operations
// that must stay pinned to one tree hold the verified directory open and
// compare (see the state store's Removal).
func WalkDirNames(names []string, managedFrom int, o WalkOptions) (string, error) {
	if err := validateNames(names); err != nil {
		return "", err
	}
	budget := newHopBudget()
	cur := "/"
	for i, name := range names {
		next := filepath.Join(cur, name)
		managed := i >= managedFrom

		var info fs.FileInfo
		var err error
		if o.ExclusiveRoot && i == managedFrom {
			// The Mkdir is both the check and the create: an
			// existing directory — pre-existing or race-won — is
			// refused, never adopted. A separate existence check
			// followed by a create would race with whatever
			// appears in between.
			if mkErr := os.Mkdir(next, 0o700); mkErr != nil {
				if os.IsExist(mkErr) {
					return "", fmt.Errorf(
						"directory %s exists but was not created by this operation — kind-ocp will not act on state it did not prepare; re-run the operation",
						next)
				}
				return "", fmt.Errorf("creating directory %s: %w", next, mkErr)
			}
			// next exists because this walk just created it, and it is
			// inspected exactly once: whatever that inspection reports
			// — including that next is suddenly gone — is final, with
			// no retry and no adoption. A directory substituted after
			// the inspection passes is not caught by this walk (the
			// checks are sequential, not atomic); a caller that must
			// stay pinned to one tree holds it open and compares.
			if info, err = os.Lstat(next); err != nil {
				return "", fmt.Errorf("checking directory %s: %w", next, err)
			}
		} else {
			info, err = os.Lstat(next)
			if os.IsNotExist(err) {
				if !o.Create {
					return "", fmt.Errorf("%s: %w", next, fs.ErrNotExist)
				}
				if err := os.Mkdir(next, 0o700); err != nil && !os.IsExist(err) {
					return "", fmt.Errorf("creating directory %s: %w", next, err)
				}
				// Re-inspect whatever is there now (also after a lost
				// Mkdir race): only what passes the checks is used.
				if info, err = os.Lstat(next); err != nil {
					return "", fmt.Errorf("checking directory %s: %w", next, err)
				}
			} else if err != nil {
				return "", fmt.Errorf("inspecting %s: %w", next, err)
			}
		}

		if info.Mode()&fs.ModeSymlink != 0 {
			if managed {
				return "", fmt.Errorf("%s is a symlink; kind-ocp refuses symlinks inside a directory it manages", next)
			}
			if !o.FollowAliases {
				return "", fmt.Errorf(
					"%s is a symlink, but the verified path is symlink-free — an entry was substituted; refusing to follow it",
					next)
			}
			// Trust the entry, then resolve it: an untrusted entry
			// (foreign-owned, swappable in a sticky parent) is
			// refused before anything follows it.
			if err := CheckAncestorSymlink(next); err != nil {
				return "", err
			}
			if err := budget.charge(next); err != nil {
				return "", err
			}
			target, err := os.Readlink(next)
			if err != nil {
				return "", fmt.Errorf("reading symlink %s: %w", next, err)
			}
			resolved, err := resolveAncestorTarget(next, target, cur, budget)
			if err != nil {
				return "", err
			}
			if resolved == "/" {
				return "", fmt.Errorf("symlink %s resolves to the filesystem root; refusing to manage a directory below it", next)
			}
			cur = resolved
			continue
		}

		if managed {
			if err := CheckManagedDir(next); err != nil {
				return "", err
			}
			if i == managedFrom && o.OnManagedRoot != nil {
				if err := o.OnManagedRoot(next); err != nil {
					return "", err
				}
			}
		} else if err := CheckAncestorDir(next); err != nil {
			return "", err
		}
		cur = next
	}
	return cur, nil
}

// validateNames rejects path components that are not plain single names:
// anything else would escape the directory the walk is confined to.
func validateNames(names []string) error {
	for _, part := range names {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, filepath.Separator) {
			return fmt.Errorf("invalid path component %q", part)
		}
	}
	return nil
}

// ResolveDir resolves an existing directory through the ancestor policy:
// trusted aliases are followed hop by hop, every component of the chain
// (the target's included) is checked, and nothing is created or written.
// It returns the fully-resolved path — where the directory really sits,
// not the alias it was named by.
//
// It is the look-before-you-manage companion to a creating walk, for
// callers that must keep two trees apart by their resolved locations:
// the StackRox example resolves the kind-ocp state root before deriving
// its credential workspace beside it, so no alias of either path can
// nest one tree inside the other unnoticed. A missing component fails
// with an error wrapping fs.ErrNotExist; the walk requires existence
// because a path that does not resolve cannot be compared. A relative
// path is resolved against the working directory first.
//
// The resolution preserves spelling, not identity: two names that a
// case-insensitive filesystem resolves to one directory stay two names
// after the walk, so callers that must keep the resolved location apart
// from another tree cannot compare paths as strings — they derive the
// other tree from the resolved location instead, through a WalkDirNames
// call that follows no alias and therefore never lands on a spelling
// the derivation did not choose.
func ResolveDir(path string) (string, error) {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	names := PathNames(path)
	if len(names) == 0 {
		return "", fmt.Errorf("%s is the filesystem root; refusing to resolve it", path)
	}
	return WalkDirNames(names, len(names), WalkOptions{FollowAliases: true})
}

// MaxAliasHops is the walk's own work limit: the total number of
// symlink expansions one walk may perform. It is a bound kind-ocp
// chose for itself — the walk resolves every hop itself with Lstat and
// Readlink, so the kernel's ELOOP limit never applies — and reaching
// it is not proof of a loop: the walk refuses work no legitimate state
// path needs, loop or not. Forty sits comfortably above any real alias
// chain, platform aliases included (macOS $TMPDIR costs one hop for
// /var).
const MaxAliasHops = 40

// hopBudget is one walk's share of symlink expansions. Every link the
// walk follows — the ancestor aliases in the path itself and every
// nested link their targets traverse — spends from the same budget, so
// sibling links cannot each restart it: one walk resolves at most
// maxAliasHops links in total, not per branch.
type hopBudget struct{ remaining int }

func newHopBudget() *hopBudget { return &hopBudget{remaining: MaxAliasHops} }

// charge spends one expansion on link, or reports the budget exhausted
// (the walk stops there, as it would for a loop).
func (h *hopBudget) charge(link string) error {
	if h.remaining <= 0 {
		return fmt.Errorf("resolving symlink %s needs more than %d symlink hops; refusing to resolve further", link, MaxAliasHops)
	}
	h.remaining--
	return nil
}

// resolveAncestorTarget resolves target — the readlink result of the
// symlink at link, whose containing directory base has already been
// verified — to a symlink-free directory, checking every entry the
// resolution depends on:
//
//   - each directory component of the target must pass the ancestor
//     policy (CheckAncestorDir): a target hanging below a permissive
//     directory is refused, not just a permissive leaf;
//   - a target component that is itself a symlink must be a trusted
//     entry (CheckAncestorSymlink — whoever can replace an entry must
//     be trusted) and is then resolved the same way, recursively.
//
// The resolution follows the kernel's rules: components are handled in
// order, ".." climbs from the position the previous components resolved
// to, and a relative target is relative to base. Every position is
// policy-checked before the resolution descends into it, so the
// returned path depends only on entries the walk has verified — an
// alias whose way runs through a swappable entry in an untrusted
// directory cannot pass, and every walk of the same path verifies the
// same chain. Every link followed spends from the walk's shared hop
// budget, so the total number of expansions — not just the nesting
// depth — is bounded.
func resolveAncestorTarget(link, target, base string, budget *hopBudget) (string, error) {
	cur := "/"
	if !filepath.IsAbs(target) {
		cur = base
	}
	for _, name := range strings.Split(target, string(filepath.Separator)) {
		switch name {
		case "", ".":
			continue
		case "..":
			if cur != "/" {
				cur = filepath.Dir(cur)
			}
			continue
		}
		next := filepath.Join(cur, name)
		info, err := os.Lstat(next)
		if err != nil {
			return "", fmt.Errorf("resolving symlink %s: %w", link, err)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			// Another hop: trust the entry, then resolve its own
			// target the same way.
			if err := CheckAncestorSymlink(next); err != nil {
				return "", err
			}
			if err := budget.charge(next); err != nil {
				return "", err
			}
			sub, err := os.Readlink(next)
			if err != nil {
				return "", fmt.Errorf("reading symlink %s: %w", next, err)
			}
			if cur, err = resolveAncestorTarget(next, sub, cur, budget); err != nil {
				return "", err
			}
			continue
		}
		if !info.IsDir() {
			return "", fmt.Errorf("symlink %s points at %s, which is not a directory", link, next)
		}
		if err := CheckAncestorDir(next); err != nil {
			return "", err
		}
		cur = next
	}
	return cur, nil
}

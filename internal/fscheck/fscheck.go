// Package fscheck enforces kind-ocp's on-disk security boundary.
//
// The state tree holds cluster-admin kubeconfigs, so every file kind-ocp
// reads, writes, or hands out, and every directory it trusts, must be
// verifiably private and caller-owned. A shared, restored, or tampered
// state tree fails closed here instead of exposing credentials or letting
// another local user substitute state.
//
// There are two policies. Managed entries (the per-prefix state directory
// and everything below it, plus the credential files inside) must be
// owned by the current user, carry no group or other access bits, and
// not be symlinks; on platforms whose extended ACLs can grant access
// the mode bits do not show (macOS) they must carry no ACL entries
// either. Ancestors above the per-prefix directory may be shared
// system directories: they must be owned by the current user or root
// and writable by no one but their owner, unless the sticky bit
// protects entry substitution (as on /tmp); ACL entries that grant
// write access beyond the mode bits are refused there too, while
// denying entries and read-only grants are tolerated (macOS home
// directories carry a harmless "everyone deny delete" entry). Ancestors
// are used through their symlink-resolved targets.
//
// The ACL half of the policies is per-platform (acl_*.go). On macOS an
// extended ACL entry can grant access the mode bits do not show, so the
// checks read the ACL through libc. On Linux they do not need to:
// POSIX ACL access is reflected in the mode's group bits (the ACL
// mask), so the mode policy already refuses every effective named
// grant, and grants the mask leaves ineffective grant nothing — Linux
// intentionally accepts masked POSIX entries rather than parsing them.
package fscheck

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// CheckPrivateFile verifies that path is a regular file, owned by the
// current user, with no group or other access bits and no extended ACL
// entries on platforms whose ACLs can outdo the mode bits (macOS; on
// Linux the ACL mask is the mode's group bits — see the package doc) —
// the policy for every credential file kind-ocp reads or returns
// (kubeconfigs, the state marker). A missing path is reported as an
// error wrapping fs.ErrNotExist.
func CheckPrivateFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if err := checkPrivateFileInfo(path, info); err != nil {
		return err
	}
	return checkManagedACL(path)
}

// CheckWriteTarget verifies that path is safe to (re)write as a private
// credential: either absent, or already a regular, caller-owned file
// with no group or other access bits and no extended ACL entries on
// platforms whose ACLs can outdo the mode bits (macOS; on Linux the
// ACL mask is the mode's group bits — see the package doc). Writers
// that merge into or truncate an existing destination (kind's
// kubeconfig writer among them) must be handed a path that passes this
// check first, so they cannot be made to follow a symlink or rewrite a
// shared file — and a refusal happens before any write, not after.
func CheckWriteTarget(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := checkPrivateFileInfo(path, info); err != nil {
		return err
	}
	return checkManagedACL(path)
}

// checkPrivateFileInfo applies the mode, ownership, and type checks
// shared by every managed-file policy.
func checkPrivateFileInfo(path string, info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is a %s, not a regular file", path, kindOf(info))
	}
	if err := ownedByCurrentUser(path, info); err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s has mode %04o; group and other access must be off (0600) — fix the permissions, or delete it and let kind-ocp recreate it", path, perm)
	}
	return nil
}

// CheckManagedDir verifies that path is a real directory, owned by the
// current user, with no group or other access bits and no extended ACL
// entries on platforms whose ACLs can outdo the mode bits (macOS; on
// Linux the ACL mask is the mode's group bits — see the package doc) —
// the policy for every directory kind-ocp creates or trusts inside its
// state tree. A missing path is reported as an error wrapping
// fs.ErrNotExist.
func CheckManagedDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is a %s, not a directory", path, kindOf(info))
	}
	if err := ownedByCurrentUser(path, info); err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%s has mode %04o; group and other access must be off (0700) — fix the permissions, or delete it and let kind-ocp recreate it", path, perm)
	}
	return checkManagedACL(path)
}

// ErrUnsafeAncestor is wrapped by the ancestor policy's refusals of a
// writable, non-sticky directory: a directory above the managed region
// whose permissions would let another local user substitute or race the
// entries kind-ocp's safety depends on. The refusal's own message names
// the chmod remedy; a caller that knows what the chain leads to (the
// state root) can use the sentinel to offer the further alternative of
// pointing its root at a location the policy already accepts — a user
// who cannot or will not tighten a shared home directory still has a
// way forward.
var ErrUnsafeAncestor = errors.New("unsafe ancestor directory")

// CheckAncestorDir verifies that path is usable as an ancestor of the
// state tree: a real directory, owned by the current user or by root, and
// writable by no one but its owner unless the sticky bit is set (the
// sticky bit prevents another user from substituting or removing entries,
// which is what makes /tmp tolerable in the chain). path must already be
// symlink-resolved; callers resolve ancestor symlinks and check the
// resolved target, so a symlink here is an error.
func CheckAncestorDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is a %s, not a directory", path, kindOf(info))
	}
	if err := ownedByUserOrRoot(path, info); err != nil {
		return err
	}
	mode := info.Mode()
	perm := mode.Perm()
	if mode&fs.ModeSticky == 0 {
		// Without the sticky bit, write access for anyone but the
		// owner means another local user can substitute entries.
		if perm&0o020 != 0 {
			return fmt.Errorf("%w: %s is group-writable (mode %04o); fix with chmod g-w %s",
				ErrUnsafeAncestor, path, perm, path)
		}
		if perm&0o002 != 0 {
			return fmt.Errorf("%w: %s is world-writable (mode %04o); fix with chmod o-w %s",
				ErrUnsafeAncestor, path, perm, path)
		}
	}
	return checkAncestorACL(path)
}

// CheckAncestorSymlink verifies a symlink entry the state walk is about
// to resolve and rely on: it must be owned by the current user or root.
// Ownership is what makes an entry trustworthy — in a sticky directory
// (the /tmp shape) whoever owns an entry can replace it at any time, and
// in a non-sticky ancestor only the owner can write at all — so an
// entry owned by anyone else could be swapped for another target
// between the walk's check and the walk's use. The entry's target is
// the caller's to verify separately.
func CheckAncestorSymlink(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		return fmt.Errorf("%s is a %s, not a symlink", path, kindOf(info))
	}
	return ownedByUserOrRoot(path, info)
}

// kindOf names a directory entry for error messages.
func kindOf(info os.FileInfo) string {
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		return "symlink"
	case info.IsDir():
		return "directory"
	default:
		return "file"
	}
}

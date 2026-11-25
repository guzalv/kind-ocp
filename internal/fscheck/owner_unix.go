//go:build unix

package fscheck

import (
	"fmt"
	"os"
	"syscall"
)

// ownedByCurrentUser reports whether info's entry belongs to the invoking
// user. A state entry owned by anyone else (a tree restored with different
// ownership, a directory another user pre-created) is not trusted.
func ownedByCurrentUser(path string, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: file ownership cannot be determined", path)
	}
	if st.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%s is owned by uid %d, not by the current user (uid %d)", path, st.Uid, os.Getuid())
	}
	return nil
}

// ownedByUserOrRoot reports whether info's entry belongs to the invoking
// user or to root (system directories such as /, /usr, or /tmp).
func ownedByUserOrRoot(path string, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: file ownership cannot be determined", path)
	}
	if st.Uid != uint32(os.Getuid()) && st.Uid != 0 {
		return fmt.Errorf("%s is owned by uid %d, not by the current user (uid %d) or root", path, st.Uid, os.Getuid())
	}
	return nil
}

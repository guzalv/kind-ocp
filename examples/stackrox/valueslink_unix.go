//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// checkValuesLinkCount refuses a values file whose inode carries more
// than one name (its link count). The run rewrites several files in
// place — kind-ocp's cluster records and exported kubeconfigs among
// them — and a hard link makes the checked file and such a target one
// inode, so the write would replace the contents the startup check
// accepted. The check cannot enumerate a file's other names to tell
// the safe links from the clobbering ones, so every multiply-linked
// values file is refused; the remedy is an independent copy, which
// `cp` makes and `ln` (or `cp -l`) does not.
func checkValuesLinkCount(path string, info os.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: the link count of the values file cannot be determined", path)
	}
	if st.Nlink > 1 {
		return fmt.Errorf("the values file %s has %d links: one of its names may be a file this run rewrites in place (a cluster record, a kubeconfig), which would replace its contents after the check accepted them — make an independent copy (cp, not ln) and pass the copy",
			path, st.Nlink)
	}
	return nil
}

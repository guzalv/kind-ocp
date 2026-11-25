//go:build !unix

package fscheck

import "os"

// Ownership has no portable representation on this platform, so the
// ownership half of the policy is skipped there; the mode and symlink
// checks still apply. kind-ocp's supported runtimes (macOS, Linux) are
// all unix, where the ownership checks are enforced.

func ownedByCurrentUser(_ string, _ os.FileInfo) error { return nil }

func ownedByUserOrRoot(_ string, _ os.FileInfo) error { return nil }

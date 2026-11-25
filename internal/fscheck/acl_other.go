//go:build !darwin

package fscheck

// Only macOS reads extended ACLs (acl_darwin.go): there an ACL entry can
// grant access the mode bits do not show. On Linux, POSIX ACL access is
// reflected in the mode's group bits — the ACL mask — so an effective
// named grant widens them (setfacl -m u:other:rw turns 0600 into 0660)
// and the mode policy already refuses it; a grant the mask leaves
// ineffective changes nothing and grants nothing. Elsewhere the mode,
// ownership, and symlink checks apply and these hooks are no-ops.
func checkManagedACL(string) error { return nil }

func checkAncestorACL(string) error { return nil }

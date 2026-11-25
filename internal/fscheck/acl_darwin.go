//go:build darwin && cgo

package fscheck

/*
#include <errno.h>
#include <stdlib.h>
#include <sys/acl.h>

// acl_check inspects path's macOS extended ACL against one of kind-ocp's
// boundary policies.
//
// managed != 0: the managed-entry policy. kind-ocp creates its state
// entries without ACL entries, so any entry at all — granting, denying,
// or inherited — is a modification from outside and fails.
//
// managed == 0: the ancestor policy. An ALLOW entry that grants a
// permission capable of overriding the mode checks fails: writing or
// removing entries (write/append/delete/delete-child), or rewriting the
// ACL itself (write-security/change-owner). DENY entries only restrict,
// and read-only grants are equivalent to what the mode bits already
// permit on shared system directories — macOS home directories carry a
// "group:everyone deny delete" entry — so both are tolerated.
//
// Returns 0 when the ACL satisfies the policy, 1 when it does not, and
// -1 with *err set to errno when the ACL cannot be read.
static int acl_check(const char *path, int managed, int *err) {
	*err = 0;
	errno = 0;
	acl_t acl = acl_get_file(path, ACL_TYPE_EXTENDED);
	if (acl == NULL) {
		if (errno == ENOENT) {
			// macOS reports "no extended ACL on this file" as a NULL
			// return with ENOENT.
			return 0;
		}
		*err = errno != 0 ? errno : EINVAL;
		return -1;
	}
	int rc = 0; // 0 = policy satisfied, 1 = violation, -1 = read error
	acl_entry_t entry;
	int entry_id = ACL_FIRST_ENTRY;
	while (rc == 0 && acl_get_entry(acl, entry_id, &entry) == 0) {
		entry_id = ACL_NEXT_ENTRY;
		if (managed) {
			rc = 1;
			continue;
		}
		acl_tag_t tag;
		if (acl_get_tag_type(entry, &tag) != 0) {
			*err = EINVAL;
			rc = -1;
			continue;
		}
		if (tag != ACL_EXTENDED_ALLOW)
			continue;
		acl_permset_t perms;
		if (acl_get_permset(entry, &perms) != 0) {
			*err = EINVAL;
			rc = -1;
			continue;
		}
		if (acl_get_perm_np(perms, ACL_WRITE_DATA) ||
		    acl_get_perm_np(perms, ACL_APPEND_DATA) ||
		    acl_get_perm_np(perms, ACL_DELETE) ||
		    acl_get_perm_np(perms, ACL_DELETE_CHILD) ||
		    acl_get_perm_np(perms, ACL_WRITE_SECURITY) ||
		    acl_get_perm_np(perms, ACL_CHANGE_OWNER))
			rc = 1;
	}
	acl_free(acl);
	return rc;
}
*/
import "C"

import (
	"fmt"
	"syscall"
	"unsafe"
)

// checkManagedACL enforces the managed-entry ACL policy on macOS: an
// extended ACL entry on a file or directory kind-ocp manages is a
// modification from outside its own writes and fails closed, because an
// entry can grant access the mode bits do not show (a 0600 kubeconfig
// with an "everyone allow read,write" entry is world-readable in fact).
func checkManagedACL(path string) error {
	return aclCheck(path, true)
}

// checkAncestorACL enforces the ancestor ACL policy on macOS: an ALLOW
// entry granting write-class rights would let another local user
// substitute entries in a directory the mode bits call safe.
func checkAncestorACL(path string) error {
	return aclCheck(path, false)
}

func aclCheck(path string, managed bool) error {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	managedFlag := C.int(0)
	if managed {
		managedFlag = 1
	}
	var cerrno C.int
	switch rc := C.acl_check(cpath, managedFlag, &cerrno); rc {
	case 0:
		return nil
	case 1:
		if managed {
			return fmt.Errorf("%s carries extended ACL entries; kind-ocp never adds them — remove them (chmod -N %s) or delete the entry and let kind-ocp recreate it", path, path)
		}
		return fmt.Errorf("%s carries an ACL entry granting write access beyond its mode bits — remove it (chmod -N %s)", path, path)
	default:
		return fmt.Errorf("inspecting the ACL of %s: %w", path, syscall.Errno(cerrno))
	}
}

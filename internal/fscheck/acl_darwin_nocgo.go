//go:build darwin && !cgo

package fscheck

import "fmt"

// Building without cgo compiles nothing that can read macOS extended
// ACLs. Silently skipping the checks would reopen exactly the hole they
// close — an "everyone allow read,write" entry on a 0600 kubeconfig —
// so the boundary checks fail closed instead. cgo is the default on
// macOS; only an explicit CGO_ENABLED=0 build reaches this file.
func checkManagedACL(path string) error {
	return fmt.Errorf("%s: the macOS ACL privacy checks require a cgo build; rebuild with CGO_ENABLED=1", path)
}

func checkAncestorACL(path string) error {
	return checkManagedACL(path)
}

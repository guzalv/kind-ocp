// Package safename defines the one rule every layer-cache name and
// content pin must satisfy: the two identifiers that name directories
// in kind-ocp's state tree — a layer's cache directory and the pin
// subdirectory under it. The rule is shared by the two places those
// identifiers enter — layers.Registry.Register for layer names,
// state.Store.CacheDirFor for the layer and its pin — so the registry's
// exact-string keys and the store's directory mapping can never
// disagree about what two distinct identifiers are.
package safename

import (
	"fmt"
	"regexp"
)

// namePattern is the safe alphabet: lowercase ASCII letters and digits,
// with '-' inside, starting and ending alphanumeric — the same shape as
// a cluster-name prefix (config.ValidatePrefix, without its length
// cap). Everything kind-ocp names today matches it: layer names like
// "openshift-api-crds" and content pins like a git commit SHA.
var namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// Name checks that id is safe to use as a directory name inside
// kind-ocp's state tree.
//
// Lowercase-only ASCII is what makes the mapping unambiguous on the
// filesystems kind-ocp runs on: macOS (a supported platform) resolves
// ACME-cache and acme-cache to the same directory, so two identifiers
// differing only by case would silently share files, and non-ASCII
// names can collide the same way through Unicode normalization. The
// response is rejection, never normalization: the identifier is also
// the key the registry and the config file know the thing by, so
// silently lowercasing it would exchange one name collision for
// another.
func Name(id string) error {
	if namePattern.MatchString(id) {
		return nil
	}
	return fmt.Errorf(
		"%q is not a safe state-tree name: it must be lowercase ASCII letters and digits, with '-' inside (the shape of %q or a commit SHA) — on a case-insensitive filesystem, two names that differ only by case are one directory, so kind-ocp refuses them instead of letting them share files",
		id, "openshift-api-crds")
}

package safename

import (
	"strconv"
	"strings"
	"testing"
)

func TestName(t *testing.T) {
	// Everything kind-ocp names today, plus the shapes near the rule's
	// edges.
	valid := []string{
		"a",
		"0",
		"olm",
		"config-openshift",
		"node-labels",
		"openshift-api-crds",
		"a-1",
		// The pin shape: a full git commit SHA, as the
		// openshift-api-crds layer pins its content.
		"54a3998d81eb911e1abb2ffab8b57bfd0571a735",
	}
	for _, id := range valid {
		if err := Name(id); err != nil {
			t.Errorf("Name(%q) = %v, want accepted", id, err)
		}
	}

	// Each rejection is either a name that would collide with another
	// name on a case-insensitive or normalization-insensitive
	// filesystem, or not a plain single path component at all.
	invalid := []string{
		"",           // nothing
		".",          // current directory
		"..",         // parent directory
		"-",          // leading hyphen
		"a-",         // trailing hyphen
		"OLM",        // aliases "olm" on macOS
		"Acme-Cache", // aliases "acme-cache"
		"acme_cache", // underscore
		"acme cache", // space
		"acme/cache", // path separator
		"acme.cache", // dot (not a pin shape kind-ocp uses)
		"acmé",       // non-ASCII: NFC/NFD normalization collisions
		"\tacme",     // control characters
	}
	for _, id := range invalid {
		err := Name(id)
		if err == nil {
			t.Errorf("Name(%q) accepted, want rejected", id)
			continue
		}
		// The error carries the identifier in %q form, escaped
		// control characters included.
		if quoted := strconv.Quote(id); !strings.Contains(err.Error(), quoted) {
			t.Errorf("Name(%q) error %q does not name the identifier", id, err)
		}
	}
}

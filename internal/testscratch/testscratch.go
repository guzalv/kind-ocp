// Package testscratch keeps kind-ocp's unit tests hermetic against the
// ambient temporary-directory environment.
//
// The state-tree boundary policy is correct to refuse a scratch chain
// that another local user can substitute entries in, and the test
// suites deliberately build state trees under t.TempDir() to exercise
// it. That makes them depend on two ambient facts a fresh machine can
// break (the §5.3 cold-machine findings, both observed on a clean
// Ubuntu install with umask 002, the Debian usergroups default):
//
//   - a permissive process umask: t.TempDir's per-test subdirectories
//     are created with mode 0777 by the testing package, so umask 002
//     makes them 0775 — user-owned, group-writable, non-sticky
//     ancestors the policy rightly refuses;
//   - a TMPDIR chain that fails the ancestor policy outright (a
//     user-owned, group-writable, non-sticky TMPDIR).
//
// The helper neutralizes both, and only both. It never weakens a
// deliberately unsafe construction: tests that chmod a directory into a
// bad shape keep failing as designed, because chmod applies exactly the
// requested mode regardless of the umask — the same discipline the
// suites' makeDir/makeFile helpers already follow.
//
// It is internal test infrastructure: the seven packages whose suites
// walk state trees under t.TempDir() (internal/cli, internal/state,
// internal/fscheck, internal/provider/kind, kindocp, test/publicapi,
// examples/stackrox) call it from their TestMain.
package testscratch

import (
	"os"
	"testing"

	"github.com/guzalv/kind-ocp/internal/fscheck"
)

// Main is the TestMain for a package whose tests must be hermetic
// against the ambient scratch environment. It neutralizes the two
// ambient facts above, runs the tests, removes the replacement scratch
// directory when it created one, and exits with the suite's status.
//
// On a healthy ambient chain with a normal umask nothing changes: no
// environment variable is touched and no directory is created.
func Main(m *testing.M) {
	scratch := hermetic()
	code := m.Run()
	if scratch != "" {
		_ = os.RemoveAll(scratch)
	}
	os.Exit(code)
}

// hermetic is the body of Main, split out so the package's own tests
// can exercise it. It returns the replacement scratch directory — a
// 0700 directory directly under the system /tmp, whose sticky bit the
// ancestor policy deliberately tolerates — or "" when the ambient
// environment is already fine (or cannot be improved on, in which case
// the tests report what they see).
func hermetic() string {
	maskAmbientWriteBits()
	if ambientScratchAccepted() {
		return ""
	}
	scratch, err := os.MkdirTemp("/tmp", "kind-ocp-test-")
	if err != nil {
		// No replacement available; the tests run against the ambient
		// chain and report its refusals themselves.
		return ""
	}
	// t.TempDir builds on GOTMPDIR; os.MkdirTemp("") and os.CreateTemp("")
	// build on TMPDIR. Point both at the fresh scratch so every scratch
	// path a test can create lands on the policy-accepted chain. A
	// failed set leaves the ambient chain in place, and the tests
	// report its refusals themselves.
	_ = os.Setenv("GOTMPDIR", scratch)
	_ = os.Setenv("TMPDIR", scratch)
	return scratch
}

// ambientScratchAccepted reports whether every ambient scratch base
// passes the boundary policy's ancestor checks, resolved through
// symlinks exactly the way the state walk resolves them. GOTMPDIR (the
// base t.TempDir uses when set) and os.TempDir() (TMPDIR, or /tmp when
// unset — the base for the MkdirTemp/CreateTemp "" forms) can differ,
// so both are checked: a test suite only stays hermetic when every
// base it can scratch on is accepted.
func ambientScratchAccepted() bool {
	bases := []string{os.Getenv("GOTMPDIR"), os.TempDir()}
	seen := make(map[string]bool, len(bases))
	for _, base := range bases {
		if base == "" || seen[base] {
			continue
		}
		seen[base] = true
		if _, err := fscheck.ResolveDir(base); err != nil {
			return false
		}
	}
	return true
}

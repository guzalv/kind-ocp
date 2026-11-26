package fscheck_test

import (
	"testing"

	"github.com/guzalv/kind-ocp/internal/testscratch"
)

// TestMain keeps this suite hermetic against the ambient scratch
// environment (umask 002, a policy-refused TMPDIR chain). See
// internal/testscratch.
//
// It lives in the external test package because testscratch checks the
// ambient chain through fscheck itself — an in-package TestMain would
// close an import cycle.
func TestMain(m *testing.M) {
	testscratch.Main(m)
}

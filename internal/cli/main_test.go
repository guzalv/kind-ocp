package cli

import (
	"testing"

	"github.com/guzalv/kind-ocp/internal/testscratch"
)

// TestMain keeps this suite hermetic against the ambient scratch
// environment (umask 002, a policy-refused TMPDIR chain). See
// internal/testscratch.
func TestMain(m *testing.M) {
	testscratch.Main(m)
}

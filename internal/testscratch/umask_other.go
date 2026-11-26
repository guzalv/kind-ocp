//go:build !unix

package testscratch

// maskAmbientWriteBits is a no-op where the umask has no portable
// representation. kind-ocp's supported platforms (Linux, macOS) are
// unix and enforce it; the build merely stays compilable elsewhere,
// matching the ownership and ACL stubs in internal/fscheck.
func maskAmbientWriteBits() {}

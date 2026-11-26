//go:build unix

package testscratch

import "syscall"

// groupOtherWrite is the umask fragment that keeps implicitly created
// scratch entries from carrying group or other write access: without
// it, t.TempDir's 0777 subdirectories come out group-writable under a
// permissive ambient umask (002), and the boundary policy — rightly —
// refuses them as ancestors of a state tree.
const groupOtherWrite = 0o022

// maskAmbientWriteBits adds the group- and other-write bits to the
// test binary's umask. The umask is process state every Mkdir and
// WriteFile inherits, so masking it once in TestMain covers every
// implicitly created mode in the suite. Explicit chmods are not
// umask-subject, so tests that deliberately construct unsafe shapes
// are unaffected (the B2 guarantee).
//
// Bits already masked stay masked: a stricter ambient umask (077) is
// preserved, never loosened.
func maskAmbientWriteBits() {
	cur := syscall.Umask(0)
	syscall.Umask(cur | groupOtherWrite)
}

//go:build unix

package testscratch

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// After the mask, implicitly created directories carry no group or
// other write access: t.TempDir's 0777 subdirectories are the reason
// the mask exists, so the test creates one the same way (Mkdir with
// 0777 and no chmod) and checks the resulting mode.
func TestMaskAmbientWriteBitsMasksImplicitModes(t *testing.T) {
	maskAmbientWriteBits()
	dir := t.TempDir()
	sub := filepath.Join(dir, "numbered-like-testing-does")
	if err := os.Mkdir(sub, 0o777); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(sub)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o022 != 0 {
		t.Errorf("Mkdir(0777) mode = %04o, want no group or other write bits", perm)
	}
}

// The mask never loosens a stricter ambient umask.
func TestMaskAmbientWriteBitsPreservesStricterMasks(t *testing.T) {
	prev := syscall.Umask(0o077)
	defer syscall.Umask(prev)
	maskAmbientWriteBits()
	// Reading the umask requires setting one; set it to the value it
	// must already hold, so the read leaves nothing behind.
	if cur := syscall.Umask(0o077); cur != 0o077 {
		t.Errorf("umask = %04o, want the stricter 0077 preserved", cur)
	}
}

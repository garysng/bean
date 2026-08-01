package image

import (
	"os"
	"syscall"
	"testing"
)

// allocatedBytes reports how much disk a file actually occupies, which is what
// distinguishes a sparse rootfs from one written out in full. Both darwin and
// linux expose it as Blocks in 512-byte units.
func allocatedBytes(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no stat block count on this platform")
	}
	return st.Blocks * 512
}

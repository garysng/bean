//go:build linux

package runtime

import (
	"os"
	"syscall"
	"testing"
)

// allocatedSize reports how much disk a file occupies, which is what
// distinguishes a sparse file from one written out in full.
func allocatedSize(t *testing.T, path string) int64 {
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

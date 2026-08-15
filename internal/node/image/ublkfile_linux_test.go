//go:build linux

package image

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

// newTestBackend builds a base image of known content plus an empty overlay.
func newTestBackend(t *testing.T, size int64, fill func([]byte)) *fileBackend {
	t.Helper()
	dir := t.TempDir()
	basePath := filepath.Join(dir, "base.img")
	base := make([]byte, size)
	if fill != nil {
		fill(base)
	}
	if err := os.WriteFile(basePath, base, 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := newFileBackend(basePath, filepath.Join(dir, "overlay.img"), size)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// TestFileBackendReadsBaseUntilWritten is the copy-on-write invariant.
//
// Tested here rather than only through a device because the consequence of getting it
// wrong is silent: the guest reads plausible bytes that are not the ones it wrote, with no
// error at any layer. That exact failure has happened in this codebase before, on the
// dm-snapshot path, and was only caught by reading through the block device after
// dropping the cache (decisions.md 3.0).
func TestFileBackendReadsBaseUntilWritten(t *testing.T) {
	const size = 64 << 10
	// A recognisable pattern rather than zeros, so "read the base" and "read a hole" are
	// distinguishable outcomes.
	b := newTestBackend(t, size, func(p []byte) {
		for i := range p {
			p[i] = byte('A' + i%26)
		}
	})

	got := make([]byte, 128)
	if _, err := b.ReadAt(got, 0); err != nil {
		t.Fatalf("read before any write: %v", err)
	}
	for i, c := range got {
		if c != byte('A'+i%26) {
			t.Fatalf("byte %d is %q, want the base's content %q -- an unwritten block "+
				"must come from the base", i, c, byte('A'+i%26))
		}
	}

	// A write inside one block, then a read spanning that block and the next. The second
	// block is still the base's, so this catches a backend that switches whole ranges
	// rather than blocks.
	want := []byte("overlay-owns-this")
	if _, err := b.WriteAt(want, 100); err != nil {
		t.Fatalf("write: %v", err)
	}

	span := make([]byte, fileBackendBlockSize+64)
	if _, err := b.ReadAt(span, 0); err != nil {
		t.Fatalf("read spanning two blocks: %v", err)
	}
	if !bytes.Equal(span[100:100+len(want)], want) {
		t.Errorf("the written bytes did not come back: got %q", span[100:100+len(want)])
	}
	// Before the write, inside the same block: must still be the base, which is what a
	// missing read-modify-write would destroy.
	for i := 0; i < 100; i++ {
		if span[i] != byte('A'+i%26) {
			t.Fatalf("byte %d of the written block is %q, want %q. A partial write must "+
				"copy the base's block in first, or the untouched part reads as zeros",
				i, span[i], byte('A'+i%26))
		}
	}
	// After the write, in the next block: untouched, so still the base.
	for i := fileBackendBlockSize; i < len(span); i++ {
		if span[i] != byte('A'+i%26) {
			t.Fatalf("byte %d is %q, want the base's %q -- the block after the written "+
				"one was never written and must still come from the base",
				i, span[i], byte('A'+i%26))
		}
	}
}

// TestFileBackendRoundTripsRandomData checks alignment cases a pattern would hide.
//
// Random data at unaligned offsets and lengths, because the block loop is where an
// off-by-one lives: a chunk computed from the wrong base would still produce readable
// output for a repeating pattern.
func TestFileBackendRoundTripsRandomData(t *testing.T) {
	const size = 1 << 20
	b := newTestBackend(t, size, nil)

	cases := []struct{ off, length int64 }{
		{0, 512},                         // aligned, one block
		{1, 4095},                        // unaligned start, ends at a block boundary
		{4095, 2},                        // straddles two blocks by one byte
		{fileBackendBlockSize - 1, 4098}, // straddles three
		{100000, 65536},                  // large and unaligned
		{size - 512, 512},                // the last block
	}
	for _, tc := range cases {
		want := make([]byte, tc.length)
		if _, err := rand.Read(want); err != nil {
			t.Fatal(err)
		}
		if _, err := b.WriteAt(want, tc.off); err != nil {
			t.Fatalf("write at %d len %d: %v", tc.off, tc.length, err)
		}
		got := make([]byte, tc.length)
		if _, err := b.ReadAt(got, tc.off); err != nil {
			t.Fatalf("read at %d len %d: %v", tc.off, tc.length, err)
		}
		if !bytes.Equal(got, want) {
			first := 0
			for first < len(got) && got[first] == want[first] {
				first++
			}
			t.Errorf("off=%d len=%d: first difference at %d", tc.off, tc.length, first)
		}
	}
}

// TestFileBackendRefusesWritesPastTheEnd checks the boundary rather than trusting it.
//
// A write past the device's size must fail. Extending the overlay instead would give the
// guest a disk larger than the one it was told about, and the mismatch would surface as a
// filesystem error much later.
func TestFileBackendRefusesWritesPastTheEnd(t *testing.T) {
	const size = 8 << 10
	b := newTestBackend(t, size, nil)

	if _, err := b.WriteAt(make([]byte, 512), size-256); err == nil {
		t.Error("a write extending past the device size was accepted")
	}
	// Reads past the end are not an error: a guest may read the tail of its last block.
	n, err := b.ReadAt(make([]byte, 512), size-256)
	if err != nil {
		t.Errorf("read at the boundary failed: %v", err)
	}
	if n != 256 {
		t.Errorf("read %d bytes at the boundary, want 256 (the rest is past the end)", n)
	}
}

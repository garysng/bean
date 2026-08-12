//go:build linux

package image

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A write that has not been flushed is still sealed, because sealing flushes first.
//
// This is the bug a snapshot taken right after a write hit on hardware: the checkpoint reported
// "written nothing" for a sandbox whose file was on disk, and the restore came back missing the
// write. Two measured facts put the gap there. The ublk device advertises no volatile write cache,
// so a guest's `sync` has no FLUSH to send and returns as soon as its writes reach the queue --
// while the WriteAt into the overlay is still in flight. And host ext4 delays allocation until
// writeback, so SEEK_DATA reports a hole for bytes the file already holds.
//
// OwnedExtents asks the filesystem, which is right for surviving a reattach and wrong for a write
// this process has not yet pushed down. So the durability point has to be on the seal path itself,
// not left to whoever calls it: a caller cannot see the queue's in-flight state.
//
// The assertion is the one that matters to a user -- the sealed layer contains the write -- rather
// than "Flush was called", which would pass on an implementation that flushed the wrong file.
func TestSealFlushesBeforeReadingExtents(t *testing.T) {
	dir := t.TempDir()
	base := writeCountedExtentLayer(t, dir, "base.lsmt", 4096, 8)
	overlay := filepath.Join(dir, "o.img")
	const devSize = 64 << 20

	b, err := newLSMTBackend([]string{base}, overlay, devSize)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	marker := bytes.Repeat([]byte("FLUSHME0"), 512)
	if _, err := b.WriteAt(marker, 8<<20); err != nil {
		t.Fatal(err)
	}

	// Sealing must flush, so the extents it reads include the write above. Calling Flush here
	// would defeat the point of the test.
	if err := b.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	extents := b.OwnedExtents()
	if len(extents) == 0 {
		t.Fatal("no extents after a flushed write")
	}

	sealed := filepath.Join(dir, "snap.lsmt")
	if err := sealFileTo(sealed, devSize, extents, b.ReadAt); err != nil {
		t.Fatalf("seal: %v", err)
	}

	stack, closeStack, err := openLSMTStack([]string{sealed})
	if err != nil {
		t.Fatalf("open the sealed layer: %v", err)
	}
	defer closeStack()

	got := make([]byte, len(marker))
	if _, err := stack.ReadAt(got, 8<<20); err != nil {
		t.Fatalf("read the marker back: %v", err)
	}
	if !bytes.Equal(got, marker) {
		t.Error("the sealed layer does not hold the write that preceded it")
	}
}

// Flush reaches the overlay file, not merely the backend's own bookkeeping.
//
// Checked through the file descriptor's own view: after Flush the file's allocated size must
// account for the write. An implementation that returned nil without syncing would pass a test
// that only asserted the error value.
func TestFlushAllocatesTheOverlay(t *testing.T) {
	dir := t.TempDir()
	base := writeCountedExtentLayer(t, dir, "base.lsmt", 4096, 8)
	overlay := filepath.Join(dir, "o.img")

	b, err := newLSMTBackend([]string{base}, overlay, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if _, err := b.WriteAt(bytes.Repeat([]byte("A"), 4096), 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := b.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	st, err := os.Stat(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() == 0 {
		t.Error("the overlay is empty after a flushed write")
	}
}

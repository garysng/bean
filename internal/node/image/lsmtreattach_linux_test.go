//go:build linux

package image

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A backend attached over an overlay that already holds data reports it as owned.
//
// This is the case that broke a snapshot on the ublk route. The ownership bitmap is built by
// WriteAt, so it knows only about writes this process saw -- and a sandbox restored from a
// snapshot reattaches with a fresh backend over an overlay that already holds the snapshot's
// bytes. Sealing from the bitmap then reported "written nothing" for a sandbox whose filesystem
// was sitting on disk, which read as a lost write rather than lost bookkeeping.
//
// The filesystem already knows which regions of a sparse file are allocated, and that answer
// survives a reattach because it belongs to the file rather than to this process.
func TestOwnedExtentsSurvivesAReattach(t *testing.T) {
	dir := t.TempDir()
	base := writeCountedExtentLayer(t, dir, "base.lsmt", 4096, 8)
	overlay := filepath.Join(dir, "o.img")
	const devSize = 64 << 20

	// First attach: write something, and confirm the bitmap sees it.
	first, err := newLSMTBackend([]string{base}, overlay, devSize)
	if err != nil {
		t.Fatal(err)
	}
	marker := bytes.Repeat([]byte("REATTACH"), 512)
	if _, err := first.WriteAt(marker, 8<<20); err != nil {
		t.Fatal(err)
	}
	if got := first.OwnedExtents(); len(got) == 0 {
		t.Fatal("the first attach reported no extents for a write it just made")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Second attach over the same overlay, as a restore does. newLSMTBackend is O_EXCL, so a
	// reattach opens the file directly -- which is exactly what the provider does when it
	// hands an existing overlay to a new device.
	f, err := os.OpenFile(overlay, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	stack, closeStack, err := openLSMTStack([]string{base})
	if err != nil {
		t.Fatal(err)
	}
	defer closeStack()
	blocks := int64(devSize) / fileBackendBlockSize
	second := &lsmtBackend{
		stack:     stack,
		overlay:   f,
		size:      devSize,
		blockSize: fileBackendBlockSize,
		owned:     make([]uint64, (blocks+63)/64), // deliberately empty, as a reattach has it
	}
	defer f.Close()

	got := second.OwnedExtents()
	if len(got) == 0 {
		t.Fatal("a reattached backend reports no extents, so a snapshot of a restored sandbox " +
			"would claim its filesystem is empty while the data sits in the overlay")
	}

	// And the reported extent actually covers the write.
	want := uint64(8<<20) / lsmtAlignment
	covered := false
	for _, e := range got {
		if e.offset <= want && want < e.offset+uint64(e.length) {
			covered = true
		}
	}
	if !covered {
		t.Errorf("extents %+v do not cover the written sector %d", got, want)
	}
}

// The overlay's own allocation, not the bitmap, is what answers.
//
// Asserted separately because the bitmap path would also pass the test above when both agree.
// Here they disagree on purpose: the bitmap is empty and the file is not.
func TestOwnedExtentsPrefersTheOverlayOverTheBitmap(t *testing.T) {
	dir := t.TempDir()
	base := writeCountedExtentLayer(t, dir, "base.lsmt", 4096, 8)
	b, err := newLSMTBackend([]string{base}, filepath.Join(dir, "o.img"), 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// Write through the file directly, so WriteAt never runs and the bitmap stays empty.
	if _, err := b.overlay.WriteAt(bytes.Repeat([]byte{'z'}, 4096), 4<<20); err != nil {
		t.Fatal(err)
	}
	if got := b.OwnedExtents(); len(got) == 0 {
		t.Error("extents came from the bitmap, which is empty, rather than from the overlay's " +
			"allocation -- so a reattached sandbox's data stays invisible")
	}
}

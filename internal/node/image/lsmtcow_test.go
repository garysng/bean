package image

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// newTestLSMTBackend builds a two-layer chain and a backend over it.
func newTestLSMTBackend(t *testing.T, sizeSectors int64) (*lsmtBackend, string) {
	t.Helper()
	dir := t.TempDir()
	base := writeCountedExtentLayer(t, dir, "base.lsmt", uint64(sizeSectors), 8)
	overlayPath := filepath.Join(dir, "overlay.img")

	b, err := newLSMTBackend([]string{base}, overlayPath, sizeSectors*lsmtAlignment)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, overlayPath
}

// A read before any write comes from the layers.
func TestLSMTBackendReadsThroughToLayers(t *testing.T) {
	b, _ := newTestLSMTBackend(t, 64)

	got := make([]byte, 8*lsmtAlignment)
	if _, err := b.ReadAt(got, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	for s := 0; s < 8; s++ {
		if want := byte('0' + s); got[s*lsmtAlignment] != want {
			t.Errorf("sector %d is %q, want %q", s, got[s*lsmtAlignment], want)
		}
	}
}

// A written block comes back from the overlay, and its neighbours still come from the
// layers.
func TestLSMTBackendWriteThenReadIsCopyOnWrite(t *testing.T) {
	b, _ := newTestLSMTBackend(t, 64)

	// Overwrite the whole of sector 2.
	want := bytes.Repeat([]byte{'W'}, lsmtAlignment)
	if _, err := b.WriteAt(want, 2*lsmtAlignment); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := make([]byte, 8*lsmtAlignment)
	if _, err := b.ReadAt(got, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got[2*lsmtAlignment:3*lsmtAlignment], want) {
		t.Error("the written sector did not come back from the overlay")
	}
	for _, s := range []int{0, 1, 3, 7} {
		if wantB := byte('0' + s); got[s*lsmtAlignment] != wantB {
			t.Errorf("sector %d is %q after writing sector 2, want %q -- the write "+
				"disturbed a block it does not own", s, got[s*lsmtAlignment], wantB)
		}
	}
}

// A partial write fills the rest of its block from the layers first.
//
// Without that fill, a one-byte write zeroes the remainder of the 4 KiB block around
// it. On a guest filesystem that destroys whatever shared the block, and nothing
// reports an error: the device returns exactly what it was asked to store.
func TestLSMTBackendPartialWritePreservesTheRestOfTheBlock(t *testing.T) {
	b, _ := newTestLSMTBackend(t, 64)

	// One byte at the very start of the device. The block is 4 KiB, spanning sectors
	// 0..7, so everything after byte 0 must survive.
	if _, err := b.WriteAt([]byte{'Z'}, 0); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := make([]byte, fileBackendBlockSize)
	if _, err := b.ReadAt(got, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got[0] != 'Z' {
		t.Fatalf("the written byte is %q, want 'Z'", got[0])
	}
	// Byte 1 onwards belongs to sector 0, which the layer fills with '0'.
	for i := 1; i < lsmtAlignment; i++ {
		if got[i] != '0' {
			t.Fatalf("byte %d is %q after a one-byte write, want '0': the rest of the "+
				"block was not filled from the layers", i, got[i])
		}
	}
	// And the later sectors inside the same 4 KiB block survive too.
	for s := 1; s < fileBackendBlockSize/lsmtAlignment; s++ {
		if want := byte('0' + s); got[s*lsmtAlignment] != want {
			t.Fatalf("sector %d inside the written block is %q, want %q",
				s, got[s*lsmtAlignment], want)
		}
	}
}

// The layers are untouched by anything the sandbox writes.
//
// The base is shared between every sandbox using the image, so a write reaching it
// would corrupt every other sandbox at once. Asserted on the bytes rather than on the
// file mode, because a read-only open is a claim and this is the consequence.
func TestLSMTBackendLeavesTheLayerFileUnchanged(t *testing.T) {
	dir := t.TempDir()
	base := writeCountedExtentLayer(t, dir, "base.lsmt", 64, 8)
	before, err := os.ReadFile(base)
	if err != nil {
		t.Fatalf("read the layer: %v", err)
	}

	b, err := newLSMTBackend([]string{base}, filepath.Join(dir, "overlay.img"), 64*lsmtAlignment)
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	defer b.Close()

	for s := 0; s < 8; s++ {
		if _, err := b.WriteAt(bytes.Repeat([]byte{'X'}, lsmtAlignment), int64(s)*lsmtAlignment); err != nil {
			t.Fatalf("write sector %d: %v", s, err)
		}
	}
	if err := b.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	after, err := os.ReadFile(base)
	if err != nil {
		t.Fatalf("re-read the layer: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the shared layer file changed, so a write reached the base every other " +
			"sandbox on this image is reading")
	}
}

// A device larger than its image reads zeros past the image's end.
//
// This is how a sandbox gets a bigger disk than the image it came from. Returning an
// error instead would fail every create whose requested size exceeds the image.
func TestLSMTBackendReadsZeroPastTheEndOfTheLayers(t *testing.T) {
	b, _ := newTestLSMTBackend(t, 64)

	// The layers hold 8 sectors; read well past them.
	got := make([]byte, 4*lsmtAlignment)
	if _, err := b.ReadAt(got, 32*lsmtAlignment); err != nil {
		t.Fatalf("read past the layers: %v", err)
	}
	for i, v := range got {
		if v != 0 {
			t.Fatalf("byte %d past the end of the layers is %q, want zero", i, v)
		}
	}
}

// A write past the end of the layers, but inside the device, works and reads back.
func TestLSMTBackendWritePastTheLayersReadsBack(t *testing.T) {
	b, _ := newTestLSMTBackend(t, 64)

	want := bytes.Repeat([]byte{'E'}, lsmtAlignment)
	off := int64(40) * lsmtAlignment
	if _, err := b.WriteAt(want, off); err != nil {
		t.Fatalf("write past the layers: %v", err)
	}
	got := make([]byte, lsmtAlignment)
	if _, err := b.ReadAt(got, off); err != nil {
		t.Fatalf("read it back: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("a write past the end of the layers did not read back")
	}
}

// A device smaller than the filesystem in its layers is refused.
//
// The guest kernel's answer to this is a geometry error the caller never sees, so it
// has to be caught here. This codebase paid for it once already on the tcmu path.
func TestNewLSMTBackendRefusesADeviceSmallerThanItsLayers(t *testing.T) {
	dir := t.TempDir()
	// A layer whose filesystem claims 128 sectors.
	base := writeSealedLayer(t, dir, "big.lsmt", 128, []layerRun{
		{sector: 0, sectors: 2, fill: 'A'},
	})

	_, err := newLSMTBackend([]string{base}, filepath.Join(dir, "overlay.img"), 64*lsmtAlignment)
	if err == nil {
		t.Fatal("a device smaller than the filesystem in its layers was accepted, and the " +
			"guest would refuse to boot from it")
	}
}

// An existing overlay is not reused.
//
// O_EXCL, so a sandbox id colliding with a leftover directory fails loudly instead of
// inheriting another sandbox's writes.
func TestNewLSMTBackendRefusesAnExistingOverlay(t *testing.T) {
	dir := t.TempDir()
	base := writeCountedExtentLayer(t, dir, "base.lsmt", 64, 8)
	overlayPath := filepath.Join(dir, "overlay.img")
	if err := os.WriteFile(overlayPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("plant a stale overlay: %v", err)
	}

	if _, err := newLSMTBackend([]string{base}, overlayPath, 64*lsmtAlignment); err == nil {
		t.Error("an existing overlay was reused, so this sandbox would inherit another " +
			"sandbox's writes")
	}
}

// The ownership bitmap converts to the extents a seal needs, coalescing adjacent blocks.
//
// This is what replaces the overlaybd daemon's index on this route. One extent per 4 KiB block
// would make an index larger than the data it describes, so adjacent blocks have to merge -- and
// a sandbox writes files, which is exactly the adjacent case.
func TestOwnedExtentsCoalescesAdjacentBlocks(t *testing.T) {
	b, _ := newTestLSMTBackend(t, 4096)

	// Nothing written yet: nothing to seal.
	if got := b.OwnedExtents(); len(got) != 0 {
		t.Errorf("a backend with no writes reports %d extents, want none", len(got))
	}

	// Three consecutive blocks, then a gap, then one more.
	bs := b.blockSize
	for _, off := range []int64{0, bs, 2 * bs, 10 * bs} {
		if _, err := b.WriteAt(bytes.Repeat([]byte{'w'}, int(bs)), off); err != nil {
			t.Fatalf("write at %d: %v", off, err)
		}
	}

	got := b.OwnedExtents()
	if len(got) != 2 {
		t.Fatalf("got %d extents, want 2 (three adjacent blocks merged, plus one apart): %+v",
			len(got), got)
	}
	spb := uint32(bs / lsmtAlignment)
	if got[0].offset != 0 || got[0].length != 3*spb {
		t.Errorf("first extent is %+v, want offset 0 length %d", got[0], 3*spb)
	}
	if want := uint64(10 * bs / lsmtAlignment); got[1].offset != want || got[1].length != spb {
		t.Errorf("second extent is %+v, want offset %d length %d", got[1], want, spb)
	}
}

// What the bitmap reports is what the sealed layer contains.
//
// The two halves of this route's snapshot -- the bitmap and the writer -- are only useful
// together, and a mismatch between them would seal the wrong regions while both halves passed
// their own tests.
func TestOwnedExtentsSealIntoAReadableLayer(t *testing.T) {
	dir := t.TempDir()
	base := writeCountedExtentLayer(t, dir, "base.lsmt", 4096, 8)
	b, err := newLSMTBackend([]string{base}, filepath.Join(dir, "o.img"), 4096*lsmtAlignment)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	// A recognisable write, one block in from the start so a wrong offset is visible.
	marker := bytes.Repeat([]byte("SEALME00"), int(b.blockSize)/8)
	if _, err := b.WriteAt(marker, b.blockSize); err != nil {
		t.Fatal(err)
	}

	sealed := filepath.Join(dir, "snap.lsmt")
	if err := sealFileTo(sealed, uint64(4096*lsmtAlignment), b.OwnedExtents(), b.ReadAt); err != nil {
		t.Fatalf("seal from the bitmap: %v", err)
	}

	stack, closeStack, err := openLSMTStack([]string{sealed})
	if err != nil {
		t.Fatalf("open the sealed snapshot: %v", err)
	}
	defer closeStack()

	got := make([]byte, len(marker))
	if _, err := stack.ReadAt(got, b.blockSize); err != nil {
		t.Fatalf("read the marker back: %v", err)
	}
	if !bytes.Equal(got, marker) {
		t.Error("the sealed layer does not contain what the sandbox wrote at that offset")
	}
}

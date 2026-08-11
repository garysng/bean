package image

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// A layer this code writes is one this code can read, with the bytes intact.
//
// The round trip is the whole test: the writer and the reader were written from the same
// upstream struct definitions, so if either misread the layout they would have to misread it in
// the same direction to still agree here -- and the reader is independently pinned against a
// layer `overlaybd-commit` produced (TestOpenRealSealedLayer). That pairing is what makes this
// meaningful rather than circular.
func TestSealedLayerRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sealed.lsmt")

	// Two runs with a hole between them, which is what a sandbox's writes actually look like:
	// some metadata blocks near the start, a file somewhere in the middle, nothing else.
	const virtualSectors = 4096
	extents := []sealedExtent{
		{offset: 2, length: 4},
		{offset: 100, length: 8},
	}
	src := make([]byte, virtualSectors*lsmtAlignment)
	for i := range src {
		src[i] = byte(i*31 + i/509)
	}
	read := func(p []byte, off int64) (int, error) {
		if off >= int64(len(src)) {
			return 0, nil
		}
		return copy(p, src[off:]), nil
	}

	if err := sealFileTo(path, virtualSectors*lsmtAlignment, extents, read); err != nil {
		t.Fatalf("seal: %v", err)
	}

	stack, closeStack, err := openLSMTStack([]string{path})
	if err != nil {
		t.Fatalf("open what was just sealed: %v", err)
	}
	defer closeStack()

	if want := int64(virtualSectors * lsmtAlignment); stack.virtualSize != want {
		t.Errorf("virtualSize = %d, want %d", stack.virtualSize, want)
	}

	// Every sealed sector reads back exactly.
	for _, e := range extents {
		got := make([]byte, int64(e.length)*lsmtAlignment)
		if _, err := stack.ReadAt(got, int64(e.offset)*lsmtAlignment); err != nil {
			t.Fatalf("read sealed extent at %d: %v", e.offset, err)
		}
		from := int64(e.offset) * lsmtAlignment
		if !bytes.Equal(got, src[from:from+int64(len(got))]) {
			t.Errorf("extent at sector %d did not round trip", e.offset)
		}
	}

	// And a sector nobody sealed reads as zeros rather than as another extent's data, which is
	// the failure a wrong physical offset produces.
	hole := make([]byte, lsmtAlignment)
	if _, err := stack.ReadAt(hole, 50*lsmtAlignment); err != nil {
		t.Fatalf("read a hole: %v", err)
	}
	for i, b := range hole {
		if b != 0 {
			t.Fatalf("byte %d of an unsealed sector is %q, want zero -- an extent is being "+
				"served at the wrong virtual offset", i, b)
		}
	}
}

// A run longer than the format's 14-bit length is split across entries.
//
// 16383 sectors is just under 8 MiB, so any sandbox that writes a large file produces a run past
// it. Emitting one entry with a truncated length would silently seal a fraction of the data and
// leave the rest reading as zeros.
func TestSealedLayerSplitsOversizedRuns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.lsmt")

	const runSectors = (1 << 14) + 500 // past the limit, so it must split
	const virtualSectors = runSectors + 100
	src := make([]byte, virtualSectors*lsmtAlignment)
	for i := range src {
		src[i] = byte(i % 251)
	}
	read := func(p []byte, off int64) (int, error) {
		if off >= int64(len(src)) {
			return 0, nil
		}
		return copy(p, src[off:]), nil
	}

	if err := sealFileTo(path, virtualSectors*lsmtAlignment,
		[]sealedExtent{{offset: 0, length: runSectors}}, read); err != nil {
		t.Fatalf("seal: %v", err)
	}

	stack, closeStack, err := openLSMTStack([]string{path})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer closeStack()

	if len(stack.mappings) < 2 {
		t.Errorf("a %d-sector run produced %d mappings, want at least 2: the length field "+
			"holds only 14 bits", runSectors, len(stack.mappings))
	}

	// The whole run reads back, including across the split boundary -- which is where a wrong
	// physical offset for the second entry would show.
	got := make([]byte, int64(runSectors)*lsmtAlignment)
	if _, err := stack.ReadAt(got, 0); err != nil {
		t.Fatalf("read the split run: %v", err)
	}
	if !bytes.Equal(got, src[:len(got)]) {
		for i := range got {
			if got[i] != src[i] {
				t.Fatalf("first mismatch at byte %d (sector %d, %d sectors into the run): "+
					"the second entry's physical offset is wrong",
					i, i/lsmtAlignment, i/lsmtAlignment)
			}
		}
	}
}

// Sealing nothing is refused.
//
// An empty layer is precisely the artifact the tcmu path produced silently -- metadata promising
// a filesystem it did not hold -- and the reason this writer exists is so such a layer never
// reaches a store again.
func TestSealRefusesAnEmptyExtentList(t *testing.T) {
	dir := t.TempDir()
	read := func(p []byte, _ int64) (int, error) { return len(p), nil }

	err := sealFileTo(filepath.Join(dir, "empty.lsmt"), 1<<20, nil, read)
	if err == nil {
		t.Fatal("sealing with no extents was accepted, which is the empty-layer artifact this " +
			"writer exists to prevent")
	}
	// And nothing was left on disk to be published by mistake.
	if _, serr := os.Stat(filepath.Join(dir, "empty.lsmt")); serr == nil {
		t.Error("a refused seal left its file behind")
	}

	// A list of zero-length extents is the same thing by another route.
	err = sealFileTo(filepath.Join(dir, "zero.lsmt"), 1<<20,
		[]sealedExtent{{offset: 0, length: 0}}, read)
	if err == nil {
		t.Error("sealing extents that are all zero-length was accepted")
	}
}

// An existing file is never overwritten.
//
// The layer is content-addressed by the bytes produced here, so two writers to one path would
// make that digest describe something other than what was published.
func TestSealRefusesAnExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "taken.lsmt")
	if err := os.WriteFile(path, []byte("someone else's bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	read := func(p []byte, _ int64) (int, error) { return len(p), nil }
	if err := sealFileTo(path, 1<<20, []sealedExtent{{offset: 0, length: 1}}, read); err == nil {
		t.Error("sealing over an existing file was accepted")
	}
	// The original is untouched.
	b, _ := os.ReadFile(path)
	if string(b) != "someone else's bytes" {
		t.Error("a refused seal modified the file that was already there")
	}
}

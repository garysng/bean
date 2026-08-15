package image

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// writeSealedLayer builds a sealed LSMT layer whose data is uncompressed, and returns
// its path. Uncompressed because these tests are about the merge, and a ZFile in the
// middle would only obscure which layer a byte came from.
func writeSealedLayer(t *testing.T, dir, name string, virtualSectors uint64, runs []layerRun) string {
	t.Helper()

	var data []byte
	var mappings []lsmtMapping
	for _, r := range runs {
		if r.zeroed {
			mappings = append(mappings, lsmtMapping{
				offset: r.sector, length: r.sectors, zeroed: true,
			})
			continue
		}
		moffset := uint64(len(data)) / lsmtAlignment
		body := bytes.Repeat([]byte{r.fill}, int(r.sectors)*lsmtAlignment)
		data = append(data, body...)
		mappings = append(mappings, lsmtMapping{
			offset: r.sector, length: r.sectors, moffset: moffset,
		})
	}

	// Data starts after the reserved header region, so the physical offsets recorded
	// above have to be shifted by it.
	for i := range mappings {
		if !mappings[i].zeroed {
			mappings[i].moffset += lsmtHeaderSpace / lsmtAlignment
		}
	}

	raw := buildSealedLayer(t, virtualSectors, mappings, data)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write layer %s: %v", name, err)
	}
	return path
}

type layerRun struct {
	sector  uint64
	sectors uint32
	fill    byte
	zeroed  bool
}

// writeCountedExtentLayer writes a layer holding ONE extent of n sectors, where sector
// i is filled with byte '0'+i.
//
// One extent rather than n one-sector extents, because that is what makes a front trim
// possible: splitting a multi-sector extent has to advance the physical offset into the
// middle of it. With per-sector extents the trim is always at an extent boundary and the
// offset arithmetic is never exercised.
func writeCountedExtentLayer(t *testing.T, dir, name string, virtualSectors uint64, sectors uint32) string {
	t.Helper()

	data := make([]byte, 0, int(sectors)*lsmtAlignment)
	for i := uint32(0); i < sectors; i++ {
		data = append(data, bytes.Repeat([]byte{byte('0' + i)}, lsmtAlignment)...)
	}
	mappings := []lsmtMapping{{
		offset:  0,
		length:  sectors,
		moffset: lsmtHeaderSpace / lsmtAlignment,
	}}

	raw := buildSealedLayer(t, virtualSectors, mappings, data)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write layer %s: %v", name, err)
	}
	return path
}

// Splitting one extent advances the physical offset for the part after the hole.
//
// The tail of a split extent starts partway into the layer's data, so its physical
// offset has to move by exactly the number of sectors cut away. Keeping the original
// offset returns the right number of bytes from the wrong place -- silently, because
// those bytes are valid data from earlier in the same layer.
//
// This needs a single multi-sector extent: with one extent per sector every cut lands
// on a boundary and the arithmetic is never reached.
func TestLSMTStackAdvancesPhysicalOffsetWhenSplittingOneExtent(t *testing.T) {
	dir := t.TempDir()
	base := writeCountedExtentLayer(t, dir, "counted.lsmt", 64, 8)
	top := writeSealedLayer(t, dir, "top.lsmt", 64, []layerRun{
		{sector: 3, sectors: 2, fill: 'N'},
	})

	stack, closeAll, err := openLSMTStack([]string{base, top})
	if err != nil {
		t.Fatalf("open stack: %v", err)
	}
	defer closeAll()

	got := make([]byte, 8*lsmtAlignment)
	if _, err := stack.ReadAt(got, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	for s := 0; s < 8; s++ {
		want := byte('0' + s)
		if s == 3 || s == 4 {
			want = 'N'
		}
		if got[s*lsmtAlignment] != want {
			t.Errorf("sector %d is %q, want %q: the tail of the split extent is being read "+
				"from the wrong physical offset", s, got[s*lsmtAlignment], want)
		}
	}
}

// The newer layer wins where two layers map the same range.
//
// This is the merge's entire purpose, and getting the direction wrong is silent: the
// base image's version of a replaced file is structurally valid data, so nothing
// downstream can tell it is the wrong answer.
func TestLSMTStackNewerLayerWins(t *testing.T) {
	dir := t.TempDir()
	base := writeSealedLayer(t, dir, "base.lsmt", 64, []layerRun{
		{sector: 0, sectors: 8, fill: 'A'},
	})
	top := writeSealedLayer(t, dir, "top.lsmt", 64, []layerRun{
		{sector: 2, sectors: 4, fill: 'B'},
	})

	// Oldest first, as a manifest lists them.
	stack, closeAll, err := openLSMTStack([]string{base, top})
	if err != nil {
		t.Fatalf("open stack: %v", err)
	}
	defer closeAll()

	got := make([]byte, 8*lsmtAlignment)
	if _, err := stack.ReadAt(got, 0); err != nil {
		t.Fatalf("read: %v", err)
	}

	want := bytes.Repeat([]byte{'A'}, 8*lsmtAlignment)
	copy(want[2*lsmtAlignment:], bytes.Repeat([]byte{'B'}, 4*lsmtAlignment))
	if !bytes.Equal(got, want) {
		// Report where they first differ, so a wrong-direction merge is obvious.
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("byte %d is %q, want %q: the older layer is winning, so the merge "+
					"order is reversed", i, got[i], want[i])
			}
		}
	}
}

// A range no layer maps reads as zeros rather than failing.
//
// A sparse image maps only what it holds, and a filesystem reads its unallocated
// blocks expecting zeros. Returning an error here would fail a create on a normal
// image.
func TestLSMTStackUnmappedRangeReadsZero(t *testing.T) {
	dir := t.TempDir()
	base := writeSealedLayer(t, dir, "sparse.lsmt", 64, []layerRun{
		{sector: 0, sectors: 2, fill: 'X'},
		{sector: 32, sectors: 2, fill: 'Y'},
	})

	stack, closeAll, err := openLSMTStack([]string{base})
	if err != nil {
		t.Fatalf("open stack: %v", err)
	}
	defer closeAll()

	got := make([]byte, 4*lsmtAlignment)
	if _, err := stack.ReadAt(got, 8*lsmtAlignment); err != nil {
		t.Fatalf("read a hole: %v", err)
	}
	for i, b := range got {
		if b != 0 {
			t.Fatalf("byte %d of an unmapped range is %q, want zero", i, b)
		}
	}
}

// An explicitly zeroed extent reads as zeros, and does not read from the layer.
//
// A zeroed mapping has no backing bytes: its physical offset is meaningless. Serving
// it as data would read from a position derived from nothing.
func TestLSMTStackZeroedExtentMasksLowerLayer(t *testing.T) {
	dir := t.TempDir()
	base := writeSealedLayer(t, dir, "base.lsmt", 64, []layerRun{
		{sector: 0, sectors: 8, fill: 'A'},
	})
	// The top layer deletes the middle by mapping it as zeroed.
	top := writeSealedLayer(t, dir, "top.lsmt", 64, []layerRun{
		{sector: 2, sectors: 4, zeroed: true},
	})

	stack, closeAll, err := openLSMTStack([]string{base, top})
	if err != nil {
		t.Fatalf("open stack: %v", err)
	}
	defer closeAll()

	got := make([]byte, 8*lsmtAlignment)
	if _, err := stack.ReadAt(got, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	for i := 2 * lsmtAlignment; i < 6*lsmtAlignment; i++ {
		if got[i] != 0 {
			t.Fatalf("byte %d is %q inside a zeroed extent, want zero: the lower layer is "+
				"still being served there", i, got[i])
		}
	}
	// Outside the zeroed range the base still shows through.
	if got[0] != 'A' || got[7*lsmtAlignment] != 'A' {
		t.Error("the zeroed extent masked more than its own range")
	}
}

// A read that starts mid-sector still returns the right bytes.
//
// The index is in sectors, so the query is widened to sector bounds and the result
// copied out of the middle. An implementation that forgets the shift returns data from
// the start of the sector, which is the right length from the wrong place.
func TestLSMTStackUnalignedReadIsCorrect(t *testing.T) {
	dir := t.TempDir()
	// Two adjacent runs with different fills, so a misplaced read is visible.
	path := writeSealedLayer(t, dir, "two.lsmt", 64, []layerRun{
		{sector: 0, sectors: 1, fill: 'P'},
		{sector: 1, sectors: 1, fill: 'Q'},
	})

	stack, closeAll, err := openLSMTStack([]string{path})
	if err != nil {
		t.Fatalf("open stack: %v", err)
	}
	defer closeAll()

	// Straddle the boundary: the last 4 bytes of sector 0 and the first 4 of sector 1.
	got := make([]byte, 8)
	if _, err := stack.ReadAt(got, lsmtAlignment-4); err != nil {
		t.Fatalf("unaligned read: %v", err)
	}
	want := []byte("PPPPQQQQ")
	if !bytes.Equal(got, want) {
		t.Errorf("unaligned read returned %q, want %q", got, want)
	}
}

// The stack's size comes from the largest layer, not the first.
//
// A later layer can grow the filesystem. Sizing from the base presents a device
// smaller than the filesystem on it, which this codebase has already paid for once:
// the guest refuses to boot with a geometry error the caller never sees.
func TestLSMTStackSizeComesFromTheLargestLayer(t *testing.T) {
	dir := t.TempDir()
	small := writeSealedLayer(t, dir, "small.lsmt", 16, []layerRun{
		{sector: 0, sectors: 2, fill: 'A'},
	})
	large := writeSealedLayer(t, dir, "large.lsmt", 128, []layerRun{
		{sector: 0, sectors: 2, fill: 'B'},
	})

	stack, closeAll, err := openLSMTStack([]string{small, large})
	if err != nil {
		t.Fatalf("open stack: %v", err)
	}
	defer closeAll()

	if want := int64(128 * lsmtAlignment); stack.virtualSize != want {
		t.Errorf("virtualSize = %d, want %d", stack.virtualSize, want)
	}
}

// Partly overlapping ranges are trimmed, and the physical offset moves with the trim.
//
// The trim is where a merge quietly goes wrong: keeping the original physical offset
// after cutting the front off a range reads the right number of bytes from the wrong
// place inside the layer.
func TestLSMTStackTrimsPartialOverlapAndAdvancesOffset(t *testing.T) {
	dir := t.TempDir()
	// Base holds 8 sectors of a recognisable pattern, one fill per sector.
	var runs []layerRun
	for i := uint64(0); i < 8; i++ {
		runs = append(runs, layerRun{sector: i, sectors: 1, fill: byte('0' + i)})
	}
	base := writeSealedLayer(t, dir, "pattern.lsmt", 64, runs)
	// Top covers sectors 0..3, so the base contributes 4..7 and must be trimmed.
	top := writeSealedLayer(t, dir, "top.lsmt", 64, []layerRun{
		{sector: 0, sectors: 4, fill: 'T'},
	})

	stack, closeAll, err := openLSMTStack([]string{base, top})
	if err != nil {
		t.Fatalf("open stack: %v", err)
	}
	defer closeAll()

	got := make([]byte, 8*lsmtAlignment)
	if _, err := stack.ReadAt(got, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	for s := 0; s < 4; s++ {
		if got[s*lsmtAlignment] != 'T' {
			t.Fatalf("sector %d is %q, want the top layer's 'T'", s, got[s*lsmtAlignment])
		}
	}
	for s := 4; s < 8; s++ {
		want := byte('0' + s)
		if got[s*lsmtAlignment] != want {
			t.Fatalf("sector %d is %q, want %q: the base's physical offset did not move "+
				"with the trim", s, got[s*lsmtAlignment], want)
		}
	}
}

// A newer range landing inside an older one splits it into two.
//
// This is the shape a real image has -- a small file replaced inside a large base
// extent -- and it is where a merge that only trims at one end fails. The first
// version of this code kept a single high-water mark, which let the older extent claim
// the whole span before the newer one was considered, and the older layer won. Both
// halves of the base must survive, each with its own physical offset.
func TestLSMTStackSplitsAnOlderExtentAroundANewerOne(t *testing.T) {
	dir := t.TempDir()
	// One 8-sector base extent with a distinct fill per sector, so a misplaced
	// physical offset shows up as the wrong digit.
	var runs []layerRun
	for i := uint64(0); i < 8; i++ {
		runs = append(runs, layerRun{sector: i, sectors: 1, fill: byte('0' + i)})
	}
	base := writeSealedLayer(t, dir, "base.lsmt", 64, runs)
	// The newer layer replaces sectors 3 and 4, in the middle.
	top := writeSealedLayer(t, dir, "top.lsmt", 64, []layerRun{
		{sector: 3, sectors: 2, fill: 'N'},
	})

	stack, closeAll, err := openLSMTStack([]string{base, top})
	if err != nil {
		t.Fatalf("open stack: %v", err)
	}
	defer closeAll()

	got := make([]byte, 8*lsmtAlignment)
	if _, err := stack.ReadAt(got, 0); err != nil {
		t.Fatalf("read: %v", err)
	}

	for s := 0; s < 8; s++ {
		want := byte('0' + s)
		if s == 3 || s == 4 {
			want = 'N'
		}
		if got[s*lsmtAlignment] != want {
			t.Errorf("sector %d is %q, want %q", s, got[s*lsmtAlignment], want)
		}
	}
}

// The merge emits a non-overlapping, ordered index.
//
// Asserted structurally rather than through a read, because overlapping entries do not
// always change what a read returns -- the first matching entry may happen to be the
// right one -- so a read-based test can pass over a broken index.
func TestMergeLSMTLayersProducesDisjointOrderedMappings(t *testing.T) {
	newest := &lsmtLayer{mappings: []lsmtMapping{
		{offset: 3, length: 2, moffset: 100},
		{offset: 20, length: 5, moffset: 200},
	}}
	middle := &lsmtLayer{mappings: []lsmtMapping{
		{offset: 0, length: 8, moffset: 300},
		{offset: 18, length: 4, moffset: 400},
	}}
	oldest := &lsmtLayer{mappings: []lsmtMapping{
		{offset: 0, length: 40, moffset: 500},
	}}

	merged := mergeLSMTLayers([]*lsmtLayer{oldest, middle, newest})
	if len(merged) == 0 {
		t.Fatal("the merge produced nothing")
	}
	for i := 1; i < len(merged); i++ {
		if merged[i].offset < merged[i-1].offset {
			t.Fatalf("entry %d starts at %d, before entry %d at %d: the index is not ordered",
				i, merged[i].offset, i-1, merged[i-1].offset)
		}
		if merged[i].offset < merged[i-1].end() {
			t.Fatalf("entry %d starts at %d, inside entry %d which ends at %d: the index "+
				"overlaps", i, merged[i].offset, i-1, merged[i-1].end())
		}
	}

	// Every sector 0..40 is covered exactly once, and by the newest layer that claims it.
	owner := map[uint64]int{}
	for _, m := range merged {
		for s := m.offset; s < m.end(); s++ {
			if prev, dup := owner[s]; dup {
				t.Fatalf("sector %d is mapped twice, by layer %d and layer %d", s, prev, m.layer)
			}
			owner[s] = m.layer
		}
	}
	for s := uint64(0); s < 40; s++ {
		want := 0 // oldest
		switch {
		case s >= 3 && s < 5, s >= 20 && s < 25:
			want = 2 // newest
		case s < 8, s >= 18 && s < 22:
			want = 1 // middle
		}
		// 20..21 is claimed by both newest and middle; newest must win.
		if s >= 20 && s < 22 {
			want = 2
		}
		if owner[s] != want {
			t.Errorf("sector %d is served by layer %d, want layer %d", s, owner[s], want)
		}
	}
}

func TestOpenLSMTStackRefusesEmptyChain(t *testing.T) {
	if _, _, err := openLSMTStack(nil); err == nil {
		t.Error("an empty layer chain was accepted")
	}
}

func TestOpenLSMTStackReportsAMissingLayer(t *testing.T) {
	dir := t.TempDir()
	ok := writeSealedLayer(t, dir, "ok.lsmt", 16, []layerRun{{sector: 0, sectors: 1, fill: 'A'}})
	_, _, err := openLSMTStack([]string{ok, filepath.Join(dir, "absent.lsmt")})
	if err == nil {
		t.Fatal("a chain naming a layer that does not exist was opened")
	}
}

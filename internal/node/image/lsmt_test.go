package image

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// buildLSMTHeader assembles a header or trailer the way upstream writes one, so the
// parser is tested against the byte layout rather than against itself.
func buildLSMTHeader(flags uint32, indexOffset, indexSize, virtualSize uint64) []byte {
	b := make([]byte, lsmtHeaderSpace)
	binary.LittleEndian.PutUint64(b[0:8], lsmtMagic0)
	copy(b[8:24], lsmtMagic1[:])
	binary.LittleEndian.PutUint32(b[24:28], lsmtHeaderSize)
	binary.LittleEndian.PutUint32(b[28:32], flags)
	binary.LittleEndian.PutUint64(b[32:40], indexOffset)
	binary.LittleEndian.PutUint64(b[40:48], indexSize)
	binary.LittleEndian.PutUint64(b[48:56], virtualSize)
	return b
}

func buildLSMTEntry(m lsmtMapping) []byte {
	b := make([]byte, lsmtIndexEntrySize)
	low := (m.offset & lsmtOffsetMask) | ((uint64(m.length) & lsmtLengthMask) << 50)
	high := m.moffset & lsmtMOffsetMask
	if m.zeroed {
		high |= 1 << 55
	}
	high |= uint64(m.tag) << 56
	binary.LittleEndian.PutUint64(b[0:8], low)
	binary.LittleEndian.PutUint64(b[8:16], high)
	return b
}

// A mapping survives the bit-packed round trip at the edge of every field.
//
// The widths are the reason: offset is 50 bits and length 14 in one word, moffset 55
// with a flag and a tag above it in the other. A field read one bit wide takes the
// neighbouring field's low bit and produces an offset that is plausible but wrong,
// which is the failure this asserts against.
func TestLSMTMappingRoundTripAtFieldEdges(t *testing.T) {
	cases := []lsmtMapping{
		{offset: 0, length: 1, moffset: 0},
		{offset: (1 << 50) - 2, length: (1 << 14) - 1, moffset: (1 << 55) - 1, tag: 255},
		{offset: 1 << 49, length: 1 << 13, moffset: 1 << 54, zeroed: true, tag: 1},
		{offset: 12345, length: 8, moffset: 999, zeroed: false, tag: 3},
	}
	for _, want := range cases {
		got := parseLSMTMapping(buildLSMTEntry(want))
		if got != want {
			t.Errorf("round trip changed the mapping:\n got %+v\nwant %+v", got, want)
		}
	}
}

// The tag field is 8 bits at the top of the high word, above the zeroed flag.
//
// Asserted on its own because a tag decoded with a mask instead of a shift, or read
// one bit low, still yields a small plausible number -- and the tag decides which
// layer a block comes from, so a wrong one serves the wrong image's data.
func TestLSMTMappingTagDoesNotBleedIntoZeroed(t *testing.T) {
	m := parseLSMTMapping(buildLSMTEntry(lsmtMapping{offset: 8, length: 8, moffset: 16, tag: 1}))
	if m.zeroed {
		t.Error("tag 1 was decoded as the zeroed flag, so the two fields overlap")
	}
	if m.tag != 1 {
		t.Errorf("tag = %d, want 1", m.tag)
	}

	z := parseLSMTMapping(buildLSMTEntry(lsmtMapping{offset: 8, length: 8, zeroed: true}))
	if !z.zeroed {
		t.Error("the zeroed flag did not survive")
	}
	if z.tag != 0 {
		t.Errorf("tag = %d for a zeroed entry with no tag, want 0", z.tag)
	}
}

func TestParseLSMTHeaderRejectsForeignFile(t *testing.T) {
	b := make([]byte, lsmtHeaderSpace)
	copy(b, []byte("not an lsmt file at all"))
	if _, err := parseLSMTHeader(b); err == nil {
		t.Fatal("a file with no lsmt magic was accepted")
	}

	// Magic0 right, magic1 wrong. Checked because the two are separate fields and a
	// parser that stops at the first would accept a file with a coincidental prefix.
	half := buildLSMTHeader(lsmtFlagHeader, 0, 0, 0)
	half[8] ^= 0xff
	if _, err := parseLSMTHeader(half); err == nil {
		t.Fatal("a file with a corrupt magic1 was accepted")
	}
}

// buildSealedLayer assembles a complete layer: header, data, index, trailer.
func buildSealedLayer(t *testing.T, virtualSectors uint64, mappings []lsmtMapping, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	buf.Write(buildLSMTHeader(lsmtFlagHeader|lsmtFlagData|lsmtFlagSealed, 0, 0, virtualSectors*lsmtAlignment))
	buf.Write(data)

	indexOffset := uint64(buf.Len())
	for _, m := range mappings {
		buf.Write(buildLSMTEntry(m))
	}
	buf.Write(buildLSMTHeader(lsmtFlagData|lsmtFlagSealed, indexOffset,
		uint64(len(mappings)), virtualSectors*lsmtAlignment))
	return buf.Bytes()
}

func TestOpenLSMTLayerReadsTheTrailerIndex(t *testing.T) {
	mappings := []lsmtMapping{
		{offset: 0, length: 2, moffset: 0},
		{offset: 8, length: 2, moffset: 2},
	}
	raw := buildSealedLayer(t, 16, mappings, make([]byte, 4*lsmtAlignment))

	l, err := openLSMTLayer(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("open a well-formed sealed layer: %v", err)
	}
	if len(l.mappings) != 2 {
		t.Fatalf("loaded %d mappings, want 2", len(l.mappings))
	}
	if l.virtualSize != 16*lsmtAlignment {
		t.Errorf("virtualSize = %d, want %d", l.virtualSize, 16*lsmtAlignment)
	}
}

// An unsealed layer is refused by name.
//
// Upstream surfaces this as ENOENT from a configfs write, which gives an operator
// nothing to act on. The message has to say what is wrong, so the test asserts on it.
func TestOpenLSMTLayerRefusesUnsealed(t *testing.T) {
	var buf bytes.Buffer
	buf.Write(buildLSMTHeader(lsmtFlagHeader|lsmtFlagData, 0, 0, 512))
	buf.Write(make([]byte, lsmtAlignment))
	indexOffset := uint64(buf.Len())
	buf.Write(buildLSMTEntry(lsmtMapping{offset: 0, length: 1}))
	buf.Write(buildLSMTHeader(lsmtFlagData, indexOffset, 1, 512)) // no sealed bit

	_, err := openLSMTLayer(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err == nil {
		t.Fatal("an unsealed layer was opened, so its index came out of the reserved region")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("not sealed")) {
		t.Errorf("the refusal does not say the layer is unsealed, so an operator cannot act "+
			"on it: %v", err)
	}
}

// Padding entries are skipped rather than mapped.
//
// Upstream pads the index to a fixed count with entries whose offset is 2^50-1. A
// reader that keeps them gets a mapping at the far end of the address space, and a
// lookup near the end of a large disk then reads from a physical offset that is not
// in the file.
func TestLoadLSMTIndexSkipsPadding(t *testing.T) {
	mappings := []lsmtMapping{
		{offset: 0, length: 2, moffset: 0},
		{offset: lsmtInvalidOffset, length: 0},
		{offset: 4, length: 2, moffset: 2},
		{offset: lsmtInvalidOffset, length: 0},
	}
	raw := buildSealedLayer(t, 16, mappings, make([]byte, 4*lsmtAlignment))

	l, err := openLSMTLayer(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if len(l.mappings) != 2 {
		t.Fatalf("kept %d mappings, want the 2 real ones", len(l.mappings))
	}
	for _, m := range l.mappings {
		if m.offset == lsmtInvalidOffset {
			t.Error("a padding entry was kept as a mapping")
		}
	}
}

// A lookup clips a straddling mapping and advances its physical offset by the same
// amount.
//
// This is the arithmetic a caller would otherwise have to repeat at every read site.
// Getting it wrong reads the right number of bytes from the wrong place, which is
// silent: the guest sees plausible data from elsewhere in the layer.
func TestLSMTLookupClipsAndAdvancesPhysicalOffset(t *testing.T) {
	l := &lsmtLayer{mappings: []lsmtMapping{
		{offset: 0, length: 100, moffset: 1000},
	}}

	got := l.lookup(10, 20)
	if len(got) != 1 {
		t.Fatalf("lookup returned %d mappings, want 1", len(got))
	}
	m := got[0]
	if m.offset != 10 || m.length != 20 {
		t.Errorf("clipped to offset %d length %d, want 10 and 20", m.offset, m.length)
	}
	if m.moffset != 1010 {
		t.Errorf("moffset = %d, want 1010: it must advance by the same 10 sectors the "+
			"virtual offset did", m.moffset)
	}
}

// A zeroed mapping's physical offset does not advance when it is clipped.
//
// It has no backing bytes, so advancing it would produce an offset that means
// nothing -- and if a later change ever reads from it, it would read from a
// position derived from the guest's request rather than from the layer.
func TestLSMTLookupDoesNotAdvanceZeroedMapping(t *testing.T) {
	l := &lsmtLayer{mappings: []lsmtMapping{
		{offset: 0, length: 100, moffset: 1000, zeroed: true},
	}}
	got := l.lookup(10, 20)
	if len(got) != 1 {
		t.Fatalf("lookup returned %d mappings, want 1", len(got))
	}
	if got[0].moffset != 1000 {
		t.Errorf("moffset = %d for a clipped zeroed run, want it unchanged at 1000",
			got[0].moffset)
	}
}

func TestLSMTLookupSkipsRangesWithNoMapping(t *testing.T) {
	l := &lsmtLayer{mappings: []lsmtMapping{
		{offset: 0, length: 4, moffset: 0},
		{offset: 100, length: 4, moffset: 4},
	}}
	if got := l.lookup(10, 50); len(got) != 0 {
		t.Errorf("lookup over a hole returned %d mappings, want none", len(got))
	}
	if got := l.lookup(0, 200); len(got) != 2 {
		t.Errorf("lookup spanning both runs returned %d mappings, want 2", len(got))
	}
}

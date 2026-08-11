package image

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"
)

// buildZFileHeader assembles a 512-byte header or trailer the way upstream writes one.
func buildZFileHeader(h zfileHeader) []byte {
	b := make([]byte, zfileHeaderSpace)
	binary.LittleEndian.PutUint64(b[0:8], zfileMagic0)
	copy(b[8:24], zfileMagic1[:])
	binary.LittleEndian.PutUint32(b[24:28], 96)
	binary.LittleEndian.PutUint64(b[32:40], h.flags)
	binary.LittleEndian.PutUint64(b[40:48], h.indexOffset)
	binary.LittleEndian.PutUint64(b[48:56], h.indexSize)
	binary.LittleEndian.PutUint64(b[56:64], h.originalFileSize)
	binary.LittleEndian.PutUint32(b[64:68], h.indexCRC)
	binary.LittleEndian.PutUint32(b[72:76], h.blockSize)
	b[76] = h.algo
	b[78] = h.useDict
	binary.LittleEndian.PutUint32(b[84:88], h.dictSize)
	b[88] = h.verify
	return b
}

// zfileFromBlocks assembles a complete ZFile from already-compressed blocks.
//
// The blocks come from the lz4 CLI via lz4RealVectors, so a test built on this is
// reading a file whose payload no code here produced.
func zfileFromBlocks(blocks [][]byte, blockSize uint32, originalSize uint64, withCRC bool) []byte {
	var body bytes.Buffer
	lengths := make([]uint32, 0, len(blocks))
	for _, blk := range blocks {
		out := blk
		if withCRC {
			sum := crc32.Update(^zfileCRCSalt, crc32.MakeTable(crc32.Castagnoli), blk) ^ 0xffffffff
			tail := make([]byte, 4)
			binary.LittleEndian.PutUint32(tail, sum)
			out = append(append([]byte{}, blk...), tail...)
		}
		body.Write(out)
		lengths = append(lengths, uint32(len(out)))
	}

	index := make([]byte, len(lengths)*4)
	for i, n := range lengths {
		binary.LittleEndian.PutUint32(index[i*4:], n)
	}

	verify := uint8(0)
	if withCRC {
		verify = 1
	}
	h := zfileHeader{
		flags:            zfileFlagHeader | zfileFlagData | zfileFlagSealed | zfileFlagCalcDigest,
		blockSize:        blockSize,
		algo:             zfileAlgoLZ4,
		verify:           verify,
		originalFileSize: originalSize,
	}

	var out bytes.Buffer
	out.Write(buildZFileHeader(h))
	out.Write(body.Bytes())

	indexOffset := uint64(out.Len())
	out.Write(index)

	trailer := h
	trailer.flags = zfileFlagData | zfileFlagSealed | zfileFlagCalcDigest
	trailer.indexOffset = indexOffset
	trailer.indexSize = uint64(len(lengths))
	trailer.indexCRC = crc32.Checksum(index, crc32.MakeTable(crc32.Castagnoli))
	out.Write(buildZFileHeader(trailer))
	return out.Bytes()
}

// A ZFile built from reference LZ4 blocks reads back byte for byte.
//
// Each vector becomes its own single-block file, so the header, index and block
// decode are all exercised against data produced outside this codebase.
func TestZFileReadsReferenceBlocks(t *testing.T) {
	for _, v := range lz4RealVectors {
		blockSize := uint32(len(v.plain))
		raw := zfileFromBlocks([][]byte{v.block}, blockSize, uint64(len(v.plain)), false)

		z, err := openZFile(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			t.Errorf("%s: open: %v", v.name, err)
			continue
		}
		if z.size() != int64(len(v.plain)) {
			t.Errorf("%s: size = %d, want %d", v.name, z.size(), len(v.plain))
		}
		got := make([]byte, len(v.plain))
		n, err := z.ReadAt(got, 0)
		if err != nil {
			t.Errorf("%s: read: %v", v.name, err)
			continue
		}
		if n != len(v.plain) || !bytes.Equal(got[:n], v.plain) {
			t.Errorf("%s: read %d bytes and the content differs", v.name, n)
		}
	}
}

// A read spanning several blocks is stitched from all of them.
//
// The per-block loop is where an off-by-one shows up: an offset computed against the
// whole file rather than the block returns the right length from the wrong place,
// with no error.
func TestZFileReadAcrossBlockBoundaries(t *testing.T) {
	// Three blocks of the same repeated-text vector, so the expected content is that
	// vector three times.
	var v *struct {
		name         string
		plain, block []byte
	}
	for i := range lz4RealVectors {
		if lz4RealVectors[i].name == "text_repeated" {
			v = &lz4RealVectors[i]
		}
	}
	if v == nil {
		t.Skip("the text_repeated vector is not present")
	}

	blockSize := uint32(len(v.plain))
	blocks := [][]byte{v.block, v.block, v.block}
	want := bytes.Repeat(v.plain, 3)
	raw := zfileFromBlocks(blocks, blockSize, uint64(len(want)), false)

	z, err := openZFile(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// Whole file at once.
	got := make([]byte, len(want))
	if _, err := z.ReadAt(got, 0); err != nil {
		t.Fatalf("read the whole file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("reading the whole file across three blocks returned the wrong content")
	}

	// A range starting inside block 0 and ending inside block 2, kept inside the file.
	start := int64(blockSize) - 10
	length := int(blockSize) + 20
	mid := make([]byte, length)
	if _, err := z.ReadAt(mid, start); err != nil {
		t.Fatalf("read across boundaries: %v", err)
	}
	if !bytes.Equal(mid, want[start:start+int64(length)]) {
		t.Error("a read spanning three blocks returned the wrong content, so the offset " +
			"within a block is being computed against the whole file")
	}
}

// Per-block checksums are verified, and a corrupt block is refused.
//
// Without this a bad block decodes to plausible bytes and the guest sees a corrupt
// filesystem with nothing in the logs to say why.
func TestZFileVerifiesBlockChecksum(t *testing.T) {
	v := lz4RealVectors[0]
	raw := zfileFromBlocks([][]byte{v.block}, uint32(len(v.plain)), uint64(len(v.plain)), true)

	z, err := openZFile(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("open a checksummed zfile: %v", err)
	}
	if _, err := z.ReadAt(make([]byte, len(v.plain)), 0); err != nil {
		t.Fatalf("read a checksummed zfile: %v", err)
	}

	// Flip a byte inside the compressed block, leaving its checksum in place.
	corrupt := append([]byte{}, raw...)
	corrupt[zfileHeaderSpace+2] ^= 0xff
	z2, err := openZFile(bytes.NewReader(corrupt), int64(len(corrupt)))
	if err != nil {
		t.Fatalf("open the corrupted zfile: %v", err)
	}
	if _, err := z2.ReadAt(make([]byte, len(v.plain)), 0); err == nil {
		t.Error("a block whose bytes do not match its checksum was served to the caller")
	}
}

// A corrupt index is caught at open, not at the first read.
//
// Every block offset derives from the index, so one bad entry misplaces every block
// after it. Catching it once at open is cheaper than a wrong answer per read.
func TestZFileVerifiesIndexChecksum(t *testing.T) {
	v := lz4RealVectors[0]
	raw := zfileFromBlocks([][]byte{v.block}, uint32(len(v.plain)), uint64(len(v.plain)), false)

	// The index sits directly before the trailer.
	indexPos := len(raw) - zfileHeaderSpace - 4
	corrupt := append([]byte{}, raw...)
	corrupt[indexPos] ^= 0x01

	if _, err := openZFile(bytes.NewReader(corrupt), int64(len(corrupt))); err == nil {
		t.Error("a zfile with a corrupt index was opened, so every block offset after the " +
			"bad entry is wrong")
	}
}

func TestOpenZFileRejectsUnsupportedVariants(t *testing.T) {
	v := lz4RealVectors[0]

	zstd := zfileFromBlocks([][]byte{v.block}, uint32(len(v.plain)), uint64(len(v.plain)), false)
	// Patch the algorithm byte in both the header and the trailer.
	zstd[76] = zfileAlgoZstd
	zstd[len(zstd)-zfileHeaderSpace+76] = zfileAlgoZstd
	if _, err := openZFile(bytes.NewReader(zstd), int64(len(zstd))); err == nil {
		t.Error("a zstd zfile was accepted by a reader that only decodes lz4")
	}

	dict := zfileFromBlocks([][]byte{v.block}, uint32(len(v.plain)), uint64(len(v.plain)), false)
	dict[78] = 1
	dict[len(dict)-zfileHeaderSpace+78] = 1
	if _, err := openZFile(bytes.NewReader(dict), int64(len(dict))); err == nil {
		t.Error("a zfile using a compression dictionary was accepted, and it would decode " +
			"to garbage")
	}
}

func TestOpenZFileRejectsForeignFile(t *testing.T) {
	junk := make([]byte, zfileHeaderSpace*3)
	copy(junk, []byte("this is not a zfile"))
	if _, err := openZFile(bytes.NewReader(junk), int64(len(junk))); err == nil {
		t.Error("a file with no zfile magic was opened")
	}
}

func TestZFileReadPastEndReturnsEOF(t *testing.T) {
	v := lz4RealVectors[0]
	raw := zfileFromBlocks([][]byte{v.block}, uint32(len(v.plain)), uint64(len(v.plain)), false)
	z, err := openZFile(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := z.ReadAt(make([]byte, 8), z.size()); err == nil {
		t.Error("a read starting past the end did not report EOF")
	}
	// A read that starts inside and runs past the end is clipped, not an error.
	got := make([]byte, 64)
	n, err := z.ReadAt(got, z.size()-4)
	if err != nil {
		t.Errorf("a read clipped by the end of the file failed: %v", err)
	}
	if n != 4 {
		t.Errorf("clipped read returned %d bytes, want 4", n)
	}
}

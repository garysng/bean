package image

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"sync"
)

// ZFile is overlaybd's block-compressed file format. This reads it.
//
// A sealed layer in bean is produced by `overlaybd-commit -z`, so its data is a
// ZFile: fixed-size uncompressed blocks, each compressed independently, with an
// index giving every compressed block's length. Independent compression is the
// property that makes random access possible -- block N can be found and expanded
// without touching block N-1.
//
// Only reading is implemented. Writing one is `overlaybd-commit`'s job.
const (
	// zfileHeaderSpace is the reserved region for the header, and the size of the
	// trailer at the end of the file. Both are 512 bytes with a 96-byte body.
	zfileHeaderSpace = 512

	// zfileCRCSalt seeds the per-block checksum. Upstream calls it a "well known
	// prime" and feeds it in as the initial CRC state.
	zfileCRCSalt uint32 = 100007
)

// zfileMagic0 is "ZFile\0\x01\0" read as a little-endian uint64.
var zfileMagic0 = binary.LittleEndian.Uint64([]byte{'Z', 'F', 'i', 'l', 'e', 0, 0x01, 0})

// zfileMagic1 is the format's second magic: an email address, in ASCII.
var zfileMagic1 = [16]byte{
	0x74, 0x75, 0x6a, 0x69, 0x2e, 0x79, 0x79, 0x66,
	0x40, 0x41, 0x6c, 0x69, 0x62, 0x61, 0x62, 0x61,
}

// Flag bits in the ZFile header's flags word.
const (
	zfileFlagHeader          = 1 << 0 // set: header, clear: trailer
	zfileFlagData            = 1 << 1
	zfileFlagSealed          = 1 << 2
	zfileFlagHeaderOverwrite = 1 << 3
	zfileFlagCalcDigest      = 1 << 4
)

// Compression algorithms, from CompressOptions.
const (
	zfileAlgoLZ4  = 1
	zfileAlgoZstd = 2
)

// zfileHeader is the parsed 96-byte body of a ZFile header or trailer.
type zfileHeader struct {
	flags            uint64
	indexOffset      uint64
	indexSize        uint64
	originalFileSize uint64
	indexCRC         uint32

	blockSize uint32
	algo      uint8
	useDict   uint8
	dictSize  uint32
	verify    uint8
}

func (h zfileHeader) isHeader() bool          { return h.flags&zfileFlagHeader != 0 }
func (h zfileHeader) isTrailer() bool         { return !h.isHeader() }
func (h zfileHeader) isData() bool            { return h.flags&zfileFlagData != 0 }
func (h zfileHeader) isSealed() bool          { return h.flags&zfileFlagSealed != 0 }
func (h zfileHeader) isHeaderOverwrite() bool { return h.flags&zfileFlagHeaderOverwrite != 0 }
func (h zfileHeader) digestEnabled() bool     { return h.flags&zfileFlagCalcDigest != 0 }

// parseZFileHeader decodes the 512-byte header or trailer region.
//
// Field offsets are spelled out rather than derived from a Go struct: the upstream
// layout is a packed C struct whose CompressOptions sub-struct has padding Go would
// place differently.
func parseZFileHeader(b []byte) (zfileHeader, error) {
	if len(b) < zfileHeaderSpace {
		return zfileHeader{}, fmt.Errorf("image: zfile header needs %d bytes, got %d",
			zfileHeaderSpace, len(b))
	}
	if got := binary.LittleEndian.Uint64(b[0:8]); got != zfileMagic0 {
		return zfileHeader{}, fmt.Errorf("image: not a zfile: magic0 %#x, want %#x",
			got, zfileMagic0)
	}
	var m1 [16]byte
	copy(m1[:], b[8:24])
	if m1 != zfileMagic1 {
		return zfileHeader{}, errors.New("image: not a zfile: magic1 mismatch")
	}

	// magic0 0, magic1 8, size 24, digest 28, flags 32, index_offset 40,
	// index_size 48, original_file_size 56, index_crc 64, reserved 68,
	// then CompressOptions at 72.
	h := zfileHeader{
		flags:            binary.LittleEndian.Uint64(b[32:40]),
		indexOffset:      binary.LittleEndian.Uint64(b[40:48]),
		indexSize:        binary.LittleEndian.Uint64(b[48:56]),
		originalFileSize: binary.LittleEndian.Uint64(b[56:64]),
		indexCRC:         binary.LittleEndian.Uint32(b[64:68]),

		// CompressOptions: block_size 0, algo 4, level 5, use_dict 6,
		// reserved 8, dict_size 12, verify 16 -- all relative to 72.
		blockSize: binary.LittleEndian.Uint32(b[72:76]),
		algo:      b[76],
		useDict:   b[78],
		dictSize:  binary.LittleEndian.Uint32(b[84:88]),
		verify:    b[88],
	}
	if h.blockSize == 0 {
		return zfileHeader{}, errors.New("image: zfile block size is zero")
	}
	return h, nil
}

// zfileReader serves the uncompressed contents of a ZFile.
//
// It implements io.ReaderAt so it can stand in for a plain file wherever a layer is
// read, which is what lets the LSMT reader stay unaware of compression.
type zfileReader struct {
	src    io.ReaderAt
	header zfileHeader

	// blockOffset holds the physical start of every block, plus a final entry for
	// the end of the last one, so a block's compressed length is the difference
	// between consecutive entries.
	//
	// Materialised rather than kept as upstream's partial-offset-plus-delta jump
	// table: at 4 KiB blocks a 1 GiB layer needs 262145 entries, which is 2 MiB of
	// uint64. The table's compactness matters when a process holds thousands of
	// layers; a node holds a handful, and paying 2 MiB removes a two-level lookup
	// from every guest read.
	blockOffset []uint64

	// buf guards the scratch buffers a read borrows. One pair per reader rather
	// than per call: a 4 KiB allocation per block on the guest's read path would
	// put the garbage collector between the guest and its disk.
	buf sync.Pool
}

// openZFile reads a ZFile's header, trailer and block index.
func openZFile(src io.ReaderAt, fileSize int64) (*zfileReader, error) {
	if fileSize < zfileHeaderSpace*2 {
		return nil, fmt.Errorf("image: zfile is %d bytes, too small to hold a header and trailer",
			fileSize)
	}

	buf := make([]byte, zfileHeaderSpace)
	if _, err := src.ReadAt(buf, 0); err != nil && !isEOF(err) {
		return nil, fmt.Errorf("image: read zfile header: %w", err)
	}
	header, err := parseZFileHeader(buf)
	if err != nil {
		return nil, err
	}
	if !header.isHeader() {
		return nil, errors.New("image: zfile starts with a trailer, not a header")
	}

	// The index location comes from the trailer unless the header was overwritten in
	// place after sealing, in which case the header carries it.
	selected := header
	if !header.isHeaderOverwrite() {
		if !header.isData() {
			return nil, errors.New("image: zfile is not a data file")
		}
		if _, err := src.ReadAt(buf, fileSize-zfileHeaderSpace); err != nil && !isEOF(err) {
			return nil, fmt.Errorf("image: read zfile trailer: %w", err)
		}
		trailer, err := parseZFileHeader(buf)
		if err != nil {
			return nil, err
		}
		switch {
		case !trailer.isTrailer():
			return nil, errors.New("image: zfile ends with a header, not a trailer")
		case !trailer.isData():
			return nil, errors.New("image: zfile trailer is not marked as a data file")
		case !trailer.isSealed():
			return nil, errors.New("image: zfile is not sealed, so its index is not written yet")
		}
		if trailer.indexOffset > uint64(fileSize)-zfileHeaderSpace {
			return nil, fmt.Errorf("image: zfile index offset %d is past the trailer",
				trailer.indexOffset)
		}
		selected = trailer
	}

	if selected.useDict != 0 {
		// A dictionary changes how every block decodes. Refused rather than ignored,
		// because ignoring it produces garbage that looks like data.
		return nil, errors.New("image: zfile uses a compression dictionary, which this " +
			"reader does not implement")
	}
	switch selected.algo {
	case zfileAlgoLZ4:
	case zfileAlgoZstd:
		return nil, errors.New("image: zfile is zstd-compressed; this reader implements lz4, " +
			"which is what `overlaybd-commit -z` produces by default")
	default:
		return nil, fmt.Errorf("image: zfile compression algorithm %d is unknown", selected.algo)
	}

	offsets, err := loadZFileIndex(src, selected)
	if err != nil {
		return nil, err
	}

	blockSize := int(selected.blockSize)
	return &zfileReader{
		src:         src,
		header:      selected,
		blockOffset: offsets,
		buf: sync.Pool{New: func() any {
			// Compressed data can exceed the block size on incompressible input, and
			// the checksum adds four more bytes.
			return &zfileScratch{
				comp:  make([]byte, blockSize*2+64),
				plain: make([]byte, blockSize),
			}
		}},
	}, nil
}

type zfileScratch struct {
	comp  []byte
	plain []byte
}

// loadZFileIndex turns the array of compressed block lengths into absolute offsets.
func loadZFileIndex(src io.ReaderAt, h zfileHeader) ([]uint64, error) {
	if h.indexSize == 0 {
		return nil, errors.New("image: zfile has an empty index")
	}
	// Bounded before allocating: the count comes off the disk.
	const maxBlocks = 1 << 26 // 64 Mi blocks, 256 GiB at 4 KiB
	if h.indexSize > maxBlocks {
		return nil, fmt.Errorf("image: zfile index claims %d blocks, past the %d limit",
			h.indexSize, maxBlocks)
	}

	raw := make([]byte, h.indexSize*4)
	if _, err := src.ReadAt(raw, int64(h.indexOffset)); err != nil && !isEOF(err) {
		return nil, fmt.Errorf("image: read zfile index: %w", err)
	}

	if h.digestEnabled() {
		// Castagnoli, matching upstream's crc32c. A mismatch here means every block
		// offset is suspect, so it is worth checking once at open.
		if got := crc32.Checksum(raw, crc32.MakeTable(crc32.Castagnoli)); got != h.indexCRC {
			return nil, fmt.Errorf("image: zfile index checksum is %#08x, want %#08x",
				got, h.indexCRC)
		}
	}

	// The first block starts after the reserved header region and any dictionary.
	offsets := make([]uint64, 0, h.indexSize+1)
	pos := uint64(zfileHeaderSpace) + uint64(h.dictSize)
	offsets = append(offsets, pos)
	for i := 0; i < int(h.indexSize); i++ {
		n := binary.LittleEndian.Uint32(raw[i*4 : i*4+4])
		if n == 0 {
			return nil, fmt.Errorf("image: zfile block %d has zero compressed length", i)
		}
		pos += uint64(n)
		offsets = append(offsets, pos)
	}
	return offsets, nil
}

// size is the length of the uncompressed contents.
func (z *zfileReader) size() int64 { return int64(z.header.originalFileSize) }

// ReadAt serves uncompressed bytes, expanding whichever blocks the range touches.
func (z *zfileReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("image: zfile read at a negative offset")
	}
	if off >= z.size() {
		return 0, io.EOF
	}
	if rem := z.size() - off; int64(len(p)) > rem {
		p = p[:rem]
	}

	blockSize := int64(z.header.blockSize)
	scratch := z.buf.Get().(*zfileScratch)
	defer z.buf.Put(scratch)

	done := 0
	for done < len(p) {
		pos := off + int64(done)
		idx := int(pos / blockSize)

		plain, err := z.block(idx, scratch)
		if err != nil {
			return done, err
		}

		inBlock := int(pos % blockSize)
		if inBlock >= len(plain) {
			// The block decoded shorter than where this read starts, which means the
			// index and the header disagree about the file's length.
			return done, fmt.Errorf("image: zfile block %d decoded to %d bytes, short of "+
				"offset %d within it", idx, len(plain), inBlock)
		}
		n := copy(p[done:], plain[inBlock:])
		done += n
	}
	return done, nil
}

// block expands one compressed block into the scratch buffer.
func (z *zfileReader) block(idx int, scratch *zfileScratch) ([]byte, error) {
	if idx < 0 || idx+1 >= len(z.blockOffset) {
		return nil, fmt.Errorf("image: zfile block %d is outside the %d it holds",
			idx, len(z.blockOffset)-1)
	}
	start, end := z.blockOffset[idx], z.blockOffset[idx+1]
	compLen := int(end - start)
	if compLen <= 0 || compLen > len(scratch.comp) {
		return nil, fmt.Errorf("image: zfile block %d has an implausible compressed length %d",
			idx, compLen)
	}

	comp := scratch.comp[:compLen]
	if _, err := z.src.ReadAt(comp, int64(start)); err != nil && !isEOF(err) {
		return nil, fmt.Errorf("image: read zfile block %d: %w", idx, err)
	}

	if z.header.verify != 0 {
		if len(comp) <= 4 {
			return nil, fmt.Errorf("image: zfile block %d is too short to hold its checksum", idx)
		}
		body := comp[:len(comp)-4]
		want := binary.LittleEndian.Uint32(comp[len(comp)-4:])
		// Upstream seeds the running CRC state with the salt rather than hashing it,
		// so the seed is the salt's complement.
		got := crc32.Update(^zfileCRCSalt, crc32.MakeTable(crc32.Castagnoli), body) ^ 0xffffffff
		if got != want {
			return nil, fmt.Errorf("image: zfile block %d checksum is %#08x, want %#08x",
				idx, got, want)
		}
		comp = body
	}

	n, err := lz4Decompress(comp, scratch.plain)
	if err != nil {
		return nil, fmt.Errorf("image: decompress zfile block %d: %w", idx, err)
	}
	return scratch.plain[:n], nil
}

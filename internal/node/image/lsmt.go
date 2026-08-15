package image

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
)

// LSMT is the on-disk format of a sealed overlaybd layer. This file reads it.
//
// It exists so a ublk device can serve an overlaybd image without the overlaybd
// daemon: upstream's reader is a C++ program that speaks tcmu, and tcmu is the
// transport this codebase measured at 4.0 s to tear down 128 devices, identically
// on kernel 5.15 and 6.8, because its daemon serialises on a netlink socket. That
// cost is in the transport, so the way around it is to stop using the transport,
// which means reading the format here.
//
// Only the read path is implemented. Sealing a layer stays with the upstream
// `overlaybd-commit` tooling, which already runs on the node.
const (
	// lsmtAlignment is the unit of every offset and length in an index entry.
	// Sectors, not bytes: the format stores a 50-bit offset, which reaches 512 TiB
	// at this alignment and only 1 PiB/2048 if it were bytes.
	lsmtAlignment = 512

	// lsmtHeaderSpace is the reserved region at the start of a layer, and also the
	// size of the trailer at the end. The struct itself is 390 bytes; the rest is
	// padding the format reserves.
	lsmtHeaderSpace = 4096

	// lsmtHeaderSize is sizeof(HeaderTrailer): the bytes that are actually parsed
	// out of the 4096-byte region.
	lsmtHeaderSize = 390

	// lsmtIndexEntrySize is sizeof(DiskSegmentMapping), two bit-packed uint64s.
	lsmtIndexEntrySize = 16
)

// lsmtMagic0 is "LSMT\0\1\2" read as a little-endian uint64.
const lsmtMagic0 uint64 = 0x00020100544d534c

// lsmtMagic1 is the format's second magic, a fixed UUID.
var lsmtMagic1 = [16]byte{
	0x65, 0x7e, 0x63, 0xd2,
	0x94, 0x44,
	0x08, 0x4c,
	0xa2, 0xd2,
	0xc8, 0xec, 0x4f, 0xcf, 0xae, 0x8a,
}

// Flag bits in HeaderTrailer.flags.
const (
	lsmtFlagHeader = 1 << 0 // set: header, clear: trailer
	lsmtFlagData   = 1 << 1 // set: data file, clear: index file
	lsmtFlagSealed = 1 << 2
)

// lsmtInvalidOffset marks a padding entry. The index is padded to a fixed count,
// and a reader must skip these rather than treat them as a mapping at 2^50-1.
const lsmtInvalidOffset uint64 = (1 << 50) - 1

// lsmtHeader is the 390-byte header and trailer of a layer.
//
// Parsed field by field rather than cast over the bytes: the C++ struct is
// `#[repr(C, packed)]` with a 37-byte UUID string and a uint16 next to two uint8s,
// so a Go struct with the same fields would be laid out differently. Explicit
// offsets are also the only form in which the layout can be checked against the
// upstream definition by reading it.
type lsmtHeader struct {
	flags       uint32
	indexOffset uint64
	indexSize   uint64
	virtualSize uint64
}

func (h lsmtHeader) isHeader() bool  { return h.flags&lsmtFlagHeader != 0 }
func (h lsmtHeader) isTrailer() bool { return !h.isHeader() }
func (h lsmtHeader) isData() bool    { return h.flags&lsmtFlagData != 0 }
func (h lsmtHeader) isSealed() bool  { return h.flags&lsmtFlagSealed != 0 }

// parseLSMTHeader decodes a header or trailer and verifies its magic.
func parseLSMTHeader(b []byte) (lsmtHeader, error) {
	if len(b) < lsmtHeaderSize {
		return lsmtHeader{}, fmt.Errorf("image: lsmt header needs %d bytes, got %d",
			lsmtHeaderSize, len(b))
	}
	if got := binary.LittleEndian.Uint64(b[0:8]); got != lsmtMagic0 {
		return lsmtHeader{}, fmt.Errorf("image: not an lsmt layer: magic0 %#x, want %#x",
			got, lsmtMagic0)
	}
	var m1 [16]byte
	copy(m1[:], b[8:24])
	if m1 != lsmtMagic1 {
		return lsmtHeader{}, errors.New("image: not an lsmt layer: magic1 mismatch")
	}
	// Field offsets follow the packed struct: magic0 0, magic1 8, size 24, flags 28,
	// index_offset 32, index_size 40, virtual_size 48.
	return lsmtHeader{
		flags:       binary.LittleEndian.Uint32(b[28:32]),
		indexOffset: binary.LittleEndian.Uint64(b[32:40]),
		indexSize:   binary.LittleEndian.Uint64(b[40:48]),
		virtualSize: binary.LittleEndian.Uint64(b[48:56]),
	}, nil
}

// lsmtMapping is one index entry: a run of the virtual disk, and where it lives.
//
// Offsets and lengths stay in sectors, as the format stores them, and are converted
// to bytes only where a read is issued. Converting on parse would make every entry
// carry a value 512x larger than the format allows, and the 14-bit length would then
// have to be range-checked in a second place.
type lsmtMapping struct {
	// offset and length locate this run in the virtual disk, in sectors.
	offset uint64
	length uint32
	// moffset is where the data sits in the layer file, in sectors.
	moffset uint64
	// zeroed marks a run that reads as zeros with no backing data.
	zeroed bool
	// tag identifies the layer. Lower is newer: tag 0 is the topmost layer, which
	// is the opposite of the order layers are listed in a config.
	tag uint8
}

func (m lsmtMapping) end() uint64 { return m.offset + uint64(m.length) }

// Bit layout of a 16-byte index entry, from the upstream DiskSegmentMapping:
//
//	low:  offset[0:50]  length[50:64]
//	high: moffset[0:55] zeroed[55:56] tag[56:64]
const (
	lsmtOffsetMask  = (uint64(1) << 50) - 1
	lsmtLengthMask  = (uint64(1) << 14) - 1
	lsmtMOffsetMask = (uint64(1) << 55) - 1
)

func parseLSMTMapping(b []byte) lsmtMapping {
	low := binary.LittleEndian.Uint64(b[0:8])
	high := binary.LittleEndian.Uint64(b[8:16])
	return lsmtMapping{
		offset:  low & lsmtOffsetMask,
		length:  uint32((low >> 50) & lsmtLengthMask),
		moffset: high & lsmtMOffsetMask,
		zeroed:  (high>>55)&1 != 0,
		tag:     uint8(high >> 56),
	}
}

// lsmtLayer is one opened layer file and the index that maps into it.
type lsmtLayer struct {
	src      io.ReaderAt
	mappings []lsmtMapping
	// virtualSize is the size of the disk this layer presents, in bytes.
	virtualSize uint64
}

// openLSMTLayer reads a sealed layer's trailer and index.
//
// The trailer is read rather than the header because only the trailer of a sealed
// layer carries the index location: sealing is what moves the index into the file
// and records where it went. A layer that is not sealed is refused here rather than
// later, since its index offset points at the reserved region and the entries read
// out of it are zeros -- which parse as a valid mapping of length 0 and produce a
// device that silently reads as empty.
func openLSMTLayer(src io.ReaderAt, fileSize int64) (*lsmtLayer, error) {
	if fileSize < lsmtHeaderSpace*2 {
		return nil, fmt.Errorf("image: lsmt layer is %d bytes, too small to hold a header and trailer",
			fileSize)
	}

	buf := make([]byte, lsmtHeaderSpace)
	if _, err := src.ReadAt(buf, 0); err != nil && !isEOF(err) {
		return nil, fmt.Errorf("image: read lsmt header: %w", err)
	}
	header, err := parseLSMTHeader(buf)
	if err != nil {
		return nil, err
	}
	if !header.isHeader() {
		return nil, errors.New("image: lsmt layer starts with a trailer, not a header")
	}

	if _, err := src.ReadAt(buf, fileSize-lsmtHeaderSpace); err != nil && !isEOF(err) {
		return nil, fmt.Errorf("image: read lsmt trailer: %w", err)
	}
	trailer, err := parseLSMTHeader(buf)
	if err != nil {
		return nil, err
	}
	switch {
	case !trailer.isTrailer():
		return nil, errors.New("image: lsmt layer ends with a header, not a trailer")
	case !trailer.isData():
		return nil, errors.New("image: lsmt layer is an index file, not a data file")
	case !trailer.isSealed():
		// The specific failure an unsealed layer produces upstream is an ENOENT from
		// a configfs write, which says nothing. Name it here instead.
		return nil, errors.New("image: lsmt layer is not sealed, so its trailer has no index " +
			"(seal it with `overlaybd-commit -z -t`)")
	}

	mappings, err := loadLSMTIndex(src, trailer)
	if err != nil {
		return nil, err
	}
	return &lsmtLayer{src: src, mappings: mappings, virtualSize: trailer.virtualSize}, nil
}

// loadLSMTIndex reads the index array and drops its padding.
func loadLSMTIndex(src io.ReaderAt, trailer lsmtHeader) ([]lsmtMapping, error) {
	if trailer.indexSize == 0 {
		return nil, errors.New("image: lsmt layer has an empty index")
	}
	// Bounded before allocating: indexSize comes off the disk, and a corrupt value
	// would otherwise be a request to allocate an arbitrary amount of memory.
	const maxIndexEntries = 1 << 26 // 64 Mi entries, 1 GiB of index
	if trailer.indexSize > maxIndexEntries {
		return nil, fmt.Errorf("image: lsmt index claims %d entries, past the %d limit",
			trailer.indexSize, maxIndexEntries)
	}

	raw := make([]byte, trailer.indexSize*lsmtIndexEntrySize)
	if _, err := src.ReadAt(raw, int64(trailer.indexOffset)); err != nil && !isEOF(err) {
		return nil, fmt.Errorf("image: read lsmt index: %w", err)
	}

	mappings := make([]lsmtMapping, 0, trailer.indexSize)
	for off := 0; off+lsmtIndexEntrySize <= len(raw); off += lsmtIndexEntrySize {
		m := parseLSMTMapping(raw[off : off+lsmtIndexEntrySize])
		// Padding entries carry the invalid offset or a zero length. Upstream writes
		// them to round the index to a fixed size, so they are expected, not damage.
		if m.length == 0 || m.offset == lsmtInvalidOffset {
			continue
		}
		mappings = append(mappings, m)
	}
	if len(mappings) == 0 {
		return nil, errors.New("image: lsmt index has no usable entries")
	}

	// Sorted so a lookup can binary-search. Upstream writes them in order, but a
	// lookup that assumes order would go quietly wrong on a file that is not, and
	// sorting an already-sorted slice costs one pass.
	sort.Slice(mappings, func(i, j int) bool { return mappings[i].offset < mappings[j].offset })
	return mappings, nil
}

// isEOF reports a read that stopped because the file ended.
//
// Compared against io.EOF rather than by message text: os.File.ReadAt returns exactly
// that sentinel, and a string comparison would break the moment the wrapping changes.
func isEOF(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// lookup returns the mappings overlapping a query range, clipped to it.
//
// The clipping is what makes a partial overlap usable: a mapping that starts before
// the query has its physical offset advanced by the same amount, so the caller can
// read straight from moffset without repeating the arithmetic.
func (l *lsmtLayer) lookup(offset uint64, length uint32) []lsmtMapping {
	if length == 0 || len(l.mappings) == 0 {
		return nil
	}
	end := offset + uint64(length)

	// First mapping that reaches into the query.
	i := sort.Search(len(l.mappings), func(i int) bool { return l.mappings[i].end() > offset })

	var out []lsmtMapping
	for ; i < len(l.mappings); i++ {
		m := l.mappings[i]
		if m.offset >= end {
			break
		}
		if m.offset < offset {
			delta := offset - m.offset
			m.offset = offset
			m.length -= uint32(delta)
			// A zeroed run has no backing bytes to advance past.
			if !m.zeroed {
				m.moffset += delta
			}
		}
		if m.end() > end {
			m.length = uint32(end - m.offset)
		}
		if m.length > 0 {
			out = append(out, m)
		}
	}
	return out
}

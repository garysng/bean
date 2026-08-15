package image

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
)

// Writing a sealed LSMT layer, which is what lets this process take a snapshot without the
// overlaybd daemon.
//
// Both snapshot gaps came from the same dependency: sealing meant running `overlaybd-commit`
// over a `writable.data` + `writable.index` pair. The ublk route has no such pair -- its
// writable layer is a sparse file plus an in-process ownership bitmap -- so snapshots could not
// work there at all. And on the tcmu route the daemon keeps that index in memory while the
// device is attached, so sealing during a checkpoint read an empty index and captured nothing.
//
// This side-steps both, because the bitmap is the index. This process knows exactly which
// blocks the sandbox wrote, with no daemon to ask and nothing to flush: the extents can be
// emitted directly.
//
// Written uncompressed. A ZFile would save space on the wire, but it puts an LZ4 encoder and a
// jump table between a snapshot and correctness, and the reader accepts a bare LSMT layer --
// openZFile is tried and skipped when the payload is not one. Compression is worth adding when
// snapshot size is measured to matter; it is not worth adding blind.
const (
	// lsmtWriteFlags marks what the reader insists on: a sealed data file, and a trailer
	// rather than a header at the end. Anything less is refused by openLSMTLayer, which is
	// the behaviour the tcmu path's unsealed-layer failure taught.
	lsmtWriteFlagsHeader  = lsmtFlagHeader | lsmtFlagData | lsmtFlagSealed
	lsmtWriteFlagsTrailer = lsmtFlagData | lsmtFlagSealed
)

// sealedExtent is one run of the virtual disk to write into a layer.
type sealedExtent struct {
	// offset and length are in sectors of the virtual disk.
	offset uint64
	length uint32
}

// writeSealedLSMT builds a sealed layer from the extents a source provides.
//
// virtualSize is the disk the layer belongs to, in bytes, and read supplies its bytes at a
// virtual offset -- the same shape as io.ReaderAt, so a copy-on-write overlay can be handed in
// directly.
//
// Extents are written in ascending order because the reader binary-searches the index; it sorts
// defensively, but an index that arrives sorted is one less thing for a future change to break.
func writeSealedLSMT(dst io.WriteSeeker, virtualSize uint64, extents []sealedExtent, read func(p []byte, off int64) (int, error)) error {
	if virtualSize == 0 {
		return errors.New("image: a sealed layer needs a non-zero virtual size")
	}
	if len(extents) == 0 {
		// Refused rather than written. An empty layer is exactly the artifact the tcmu path
		// produced silently -- 36 KiB of metadata promising a filesystem it did not hold --
		// and the whole point here is that such a layer never reaches a store again.
		return errors.New("image: refusing to seal a layer with no extents, which would " +
			"promise a filesystem it does not contain")
	}

	sorted := make([]sealedExtent, len(extents))
	copy(sorted, extents)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].offset < sorted[j].offset })

	// The header occupies a fixed reserved region, and data starts after it. Written last,
	// because its index offset is only known once the data is laid down.
	if _, err := dst.Seek(lsmtHeaderSpace, io.SeekStart); err != nil {
		return fmt.Errorf("image: seek past the header region: %w", err)
	}

	mappings := make([]lsmtMapping, 0, len(sorted))
	physSector := uint64(lsmtHeaderSpace / lsmtAlignment)
	buf := make([]byte, 0, 1<<20)

	for _, e := range sorted {
		if e.length == 0 {
			continue
		}
		// The format stores a length in 14 bits of sectors, so a longer run has to be split.
		// Not a hypothetical: 16383 sectors is just under 8 MiB, and a sandbox that writes a
		// large file produces runs well past that.
		remaining := e.length
		virt := e.offset
		for remaining > 0 {
			chunk := remaining
			if uint64(chunk) > lsmtLengthMask {
				chunk = uint32(lsmtLengthMask)
			}

			n := int64(chunk) * lsmtAlignment
			if int64(cap(buf)) < n {
				buf = make([]byte, n)
			}
			b := buf[:n]
			got, err := read(b, int64(virt)*lsmtAlignment)
			if err != nil && !isEOF(err) {
				return fmt.Errorf("image: read extent at sector %d: %w", virt, err)
			}
			// A short read is zero-filled rather than treated as an error: the overlay is
			// sparse, so a block it owns but never wrote past reads as a hole.
			for i := got; i < len(b); i++ {
				b[i] = 0
			}
			if _, err := dst.Write(b); err != nil {
				return fmt.Errorf("image: write extent at sector %d: %w", virt, err)
			}

			mappings = append(mappings, lsmtMapping{
				offset:  virt,
				length:  chunk,
				moffset: physSector,
			})
			physSector += uint64(chunk)
			virt += uint64(chunk)
			remaining -= chunk
		}
	}

	if len(mappings) == 0 {
		return errors.New("image: every extent was empty, so there is nothing to seal")
	}

	indexOffset := physSector * lsmtAlignment
	for _, m := range mappings {
		if _, err := dst.Write(encodeLSMTMapping(m)); err != nil {
			return fmt.Errorf("image: write index: %w", err)
		}
	}

	// The trailer carries the index's location and the virtual size, and is what the reader
	// looks at first. It sits in its own reserved region at the end.
	trailer := lsmtHeader{
		flags:       lsmtWriteFlagsTrailer,
		indexOffset: indexOffset,
		indexSize:   uint64(len(mappings)),
		virtualSize: virtualSize,
	}
	if _, err := dst.Write(encodeLSMTHeader(trailer)); err != nil {
		return fmt.Errorf("image: write trailer: %w", err)
	}

	// The header goes in last. Its own index fields are not what a reader uses -- the trailer
	// is authoritative for a sealed layer -- but the magic and the flags have to be there or
	// openLSMTLayer refuses the file before reaching the trailer.
	if _, err := dst.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("image: seek back to the header: %w", err)
	}
	header := lsmtHeader{
		flags:       lsmtWriteFlagsHeader,
		indexOffset: indexOffset,
		indexSize:   uint64(len(mappings)),
		virtualSize: virtualSize,
	}
	if _, err := dst.Write(encodeLSMTHeader(header)); err != nil {
		return fmt.Errorf("image: write header: %w", err)
	}
	return nil
}

// encodeLSMTHeader lays out the 4096-byte header or trailer region.
func encodeLSMTHeader(h lsmtHeader) []byte {
	b := make([]byte, lsmtHeaderSpace)
	binary.LittleEndian.PutUint64(b[0:8], lsmtMagic0)
	copy(b[8:24], lsmtMagic1[:])
	binary.LittleEndian.PutUint32(b[24:28], lsmtHeaderSize)
	binary.LittleEndian.PutUint32(b[28:32], h.flags)
	binary.LittleEndian.PutUint64(b[32:40], h.indexOffset)
	binary.LittleEndian.PutUint64(b[40:48], h.indexSize)
	binary.LittleEndian.PutUint64(b[48:56], h.virtualSize)
	// version 1, sub-version 1, at the offsets the packed struct puts them.
	b[386] = 1
	b[387] = 1
	return b
}

// encodeLSMTMapping packs one index entry, the inverse of parseLSMTMapping.
func encodeLSMTMapping(m lsmtMapping) []byte {
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

// sealFileTo writes a sealed layer to a path, creating it exclusively.
//
// O_EXCL so a second seal of the same sandbox cannot silently overwrite the first: a snapshot
// is content-addressed by the bytes this produces, and two writers to one path would make that
// digest a lie.
func sealFileTo(path string, virtualSize uint64, extents []sealedExtent, read func(p []byte, off int64) (int, error)) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("image: create sealed layer %s: %w", path, err)
	}
	if err := writeSealedLSMT(f, virtualSize, extents, read); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	// Synced before the digest is taken: the caller keys the layer by the sha256 of these
	// bytes and publishes them, so a partially written file would be published under a digest
	// that does not describe it.
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("image: sync sealed layer: %w", err)
	}
	return f.Close()
}

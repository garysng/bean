//go:build linux

package runtime

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// Sparse files are carried as an extent list rather than byte-for-byte.
//
// A sandbox's writable layer is provisioned large and used lightly: a 20 GiB
// copy-on-write store holding 150 KiB is normal. Writing it literally means
// producing 20 GiB of zeroes for the compressor to eat, which measured at 15s
// per checkpoint — the sandbox is paused for all of it. Writing only the
// allocated ranges makes the cost proportional to what the sandbox actually
// wrote, which is the only thing that scales.
//
// The format is a header, then a run of (offset, length, bytes) records. It is
// deliberately minimal: the file it describes is a block image with no internal
// structure worth modelling.

const sparseMagic = "BEANSPRS"

// sparseHeader precedes the extents. LogicalSize is the file's full size, which
// restore needs in order to reproduce it at the right length.
type sparseHeader struct {
	LogicalSize int64
	ExtentCount int64
}

// writeSparse writes f as an extent list. It returns the number of bytes of real
// data, which is what a caller should report as the snapshot's size.
func writeSparse(w io.Writer, f *os.File, size int64) (int64, error) {
	extents, err := findExtents(f, size)
	if err != nil {
		return 0, err
	}

	if err := binary.Write(w, binary.LittleEndian, []byte(sparseMagic)); err != nil {
		return 0, err
	}
	hdr := sparseHeader{LogicalSize: size, ExtentCount: int64(len(extents))}
	if err := binary.Write(w, binary.LittleEndian, hdr); err != nil {
		return 0, err
	}

	var dataBytes int64
	for _, e := range extents {
		if err := binary.Write(w, binary.LittleEndian, e); err != nil {
			return dataBytes, err
		}
		if _, err := f.Seek(e.Offset, io.SeekStart); err != nil {
			return dataBytes, err
		}
		n, err := io.CopyN(w, f, e.Length)
		dataBytes += n
		if err != nil {
			return dataBytes, err
		}
	}
	return dataBytes, nil
}

// readSparse reconstructs a file from an extent list. The destination keeps its
// holes: only the recorded ranges are written, so a restored layer costs what
// the original did rather than its provisioned size.
func readSparse(r io.Reader, dest *os.File) error {
	magic := make([]byte, len(sparseMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return fmt.Errorf("read sparse magic: %w", err)
	}
	if string(magic) != sparseMagic {
		return errors.New("not a sparse extent stream")
	}

	var hdr sparseHeader
	if err := binary.Read(r, binary.LittleEndian, &hdr); err != nil {
		return fmt.Errorf("read sparse header: %w", err)
	}

	for i := int64(0); i < hdr.ExtentCount; i++ {
		var e extent
		if err := binary.Read(r, binary.LittleEndian, &e); err != nil {
			return fmt.Errorf("read extent %d: %w", i, err)
		}
		if e.Length < 0 || e.Offset < 0 || e.Offset+e.Length > hdr.LogicalSize {
			return fmt.Errorf("extent %d out of range: offset=%d length=%d size=%d",
				i, e.Offset, e.Length, hdr.LogicalSize)
		}
		if _, err := dest.Seek(e.Offset, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(dest, r, e.Length); err != nil {
			return fmt.Errorf("write extent %d: %w", i, err)
		}
	}

	// The destination may be shorter than the original if it was freshly
	// created; a block device is already the right size and rejects truncation,
	// so the failure is ignored rather than treated as fatal.
	if err := dest.Truncate(hdr.LogicalSize); err != nil {
		if _, statErr := dest.Stat(); statErr != nil {
			return err
		}
	}
	return dest.Sync()
}

// extent is one allocated range.
type extent struct {
	Offset int64
	Length int64
}

// findExtents locates the allocated ranges with SEEK_DATA/SEEK_HOLE, so the
// kernel does the work of knowing which blocks exist.
func findExtents(f *os.File, size int64) ([]extent, error) {
	var extents []extent
	offset := int64(0)
	for offset < size {
		dataStart, err := f.Seek(offset, unix.SEEK_DATA)
		if err != nil {
			// ENXIO means there is no data at or after offset: done.
			if errors.Is(err, unix.ENXIO) {
				break
			}
			// A filesystem without hole support reports the whole file as data,
			// which is correct if pessimistic.
			return []extent{{Offset: 0, Length: size}}, nil
		}

		holeStart, err := f.Seek(dataStart, unix.SEEK_HOLE)
		if err != nil {
			return nil, err
		}
		if holeStart > size {
			holeStart = size
		}
		if holeStart <= dataStart {
			break
		}
		extents = append(extents, extent{Offset: dataStart, Length: holeStart - dataStart})
		offset = holeStart
	}
	return extents, nil
}

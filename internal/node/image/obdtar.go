package image

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"strings"
)

// A layer bean seals is wrapped in a tar, so its payload does not start at offset 0.
//
// `overlaybd-commit -z -t` is what produces every layer this node writes: -z compresses
// the data to a ZFile, and -t wraps the result in a tar so the file is a valid OCI blob
// and can be pushed to a registry as-is. The wrapper is why a reader that opens a real
// layer at offset 0 sees a tar header where it expects a magic number, and refuses a file
// that is perfectly good.
//
// Measured on hardware rather than deduced: a layer sealed by `overlaybd-commit -z -t`
// begins with a pax header named "overlaybd.pax", then an "overlaybd.commit" entry, and
// the ZFile magic appears at offset 1536. Reading it needs the tar walked, not a constant
// added.
const (
	// sealedPayloadEntry is the tar member holding the sealed layer.
	sealedPayloadEntry = "overlaybd.commit"
	// tarBlockSize is the tar record size; a member's body starts one record after its
	// header and is padded up to a multiple of it.
	tarBlockSize = 512
)

// sectionReaderAt adapts a region of a file to io.ReaderAt with offsets relative to that
// region, so the readers above it need not know they are looking inside a container.
type sectionReaderAt struct {
	src  io.ReaderAt
	base int64
	size int64
}

func (s *sectionReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("image: negative offset")
	}
	if off >= s.size {
		return 0, io.EOF
	}
	if rem := s.size - off; int64(len(p)) > rem {
		p = p[:rem]
	}
	return s.src.ReadAt(p, s.base+off)
}

// openSealedLayerPayload returns a reader over the layer payload inside a sealed file,
// along with its length.
//
// A file that is not a tar is returned unchanged, so a layer sealed without -t still
// works: the wrapper is a property of how bean seals, not of the format, and a reader
// that required it would refuse a hand-sealed layer.
func openSealedLayerPayload(src io.ReaderAt, fileSize int64) (io.ReaderAt, int64, error) {
	base, size, err := findSealedPayload(src, fileSize)
	if err != nil {
		if errors.Is(err, errNotTarWrapped) {
			return src, fileSize, nil
		}
		return nil, 0, err
	}
	return &sectionReaderAt{src: src, base: base, size: size}, size, nil
}

// errNotTarWrapped means the file is the bare payload, with no tar around it.
var errNotTarWrapped = errors.New("image: sealed layer is not tar-wrapped")

// findSealedPayload locates the payload member's offset and length.
//
// The body offset comes from the section reader's position after Next returns, not from a
// running total kept here. Counting records by hand is what my first version did and it
// was wrong: a pax-extended member carries an extra header/body record pair that the Go
// reader consumes transparently, so a manual counter lands one pair short and reads the
// pax body ("13 size=...") as the payload. The reader's own position already accounts for
// everything it consumed.
func findSealedPayload(src io.ReaderAt, fileSize int64) (int64, int64, error) {
	sr := io.NewSectionReader(src, 0, fileSize)
	tr := tar.NewReader(sr)

	for {
		h, err := tr.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				// A tar with no payload member is not a layer this code can read, but it
				// is also not evidence the file is bare -- say which.
				return 0, 0, fmt.Errorf("image: sealed layer is a tar with no %q member",
					sealedPayloadEntry)
			}
			// Not a tar at all: the caller falls back to reading the file directly.
			return 0, 0, errNotTarWrapped
		}

		if strings.TrimPrefix(h.Name, "./") != sealedPayloadEntry {
			continue
		}

		// Next leaves the section reader positioned at the start of this member's body.
		bodyAt, serr := sr.Seek(0, io.SeekCurrent)
		if serr != nil {
			return 0, 0, fmt.Errorf("image: locate sealed payload: %w", serr)
		}
		// A zero-length member is the form upstream writes: the payload is the bytes
		// after the header rather than the member's declared body, so it runs to the end
		// of the file. Measured on a real layer -- `overlaybd.commit` declares size 0 and
		// the ZFile magic sits at its body offset.
		if h.Size == 0 {
			return bodyAt, fileSize - bodyAt, nil
		}
		return bodyAt, h.Size, nil
	}
}

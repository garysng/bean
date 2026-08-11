package image

import (
	"errors"
	"fmt"
	"io"
	"os"
)

// lsmtBackend serves a ublk device from a chain of overlaybd layers plus one writable
// overlay.
//
// The difference from fileBackend is only where reads come from when the sandbox has
// not written a block: fileBackend reads a single flattened ext4, this reads the merged
// layer stack. Writes are identical, and deliberately so -- the overlay is a sparse
// file with a bitmap of the blocks it owns, which is what makes a sandbox that writes
// nothing cost nothing, and what a checkpoint captures.
//
// What this changes about cost is the base, not the sandbox: the layers stay separate
// and shared, so three images over one debian base occupy that base once instead of
// three times. The measured figure on this codebase's own images is 392 MiB down to
// 118 MiB for three python -slim images, and conversion CPU from a flat 2.2 s per
// image to 1.37/0.49/0.44 s as the shared layer is reused.
// fileBackendBlockSize is the granularity of copy-on-write ownership, shared by both
// ublk backends. 4 KiB matches the page size and the filesystem block size in bean's
// images, so a guest write never straddles two blocks and forces a read-modify-write.
//
// Declared here rather than beside fileBackend because that file is linux-only, and
// the layer readers in this one are portable.
const fileBackendBlockSize = 4 << 10

type lsmtBackend struct {
	stack   *lsmtStack
	closeUp func() error
	overlay *os.File
	size    int64

	blockSize int64
	owned     []uint64
}

// newLSMTBackend opens a layer chain, oldest first, and creates the sandbox's overlay.
//
// size may exceed the stack's virtual size, which is how a sandbox gets a disk larger
// than the image it came from. It must never be smaller: a device shorter than the
// filesystem on it is refused by the guest kernel with a geometry error the caller
// never sees, which this codebase has already paid for once on the tcmu path.
func newLSMTBackend(layerPaths []string, overlayPath string, size int64) (*lsmtBackend, error) {
	stack, closeUp, err := openLSMTStack(layerPaths)
	if err != nil {
		return nil, err
	}
	b, err := newLSMTBackendOverStack(stack, closeUp, overlayPath, size)
	if err != nil {
		_ = closeUp()
		return nil, err
	}
	return b, nil
}

// newLSMTBackendOverStack builds a backend over a chain that is already open.
//
// Separate from newLSMTBackend so a caller that needs the chain's own size before
// choosing the device size -- which is every caller reconciling a requested size with
// the filesystem the image was sealed at -- can read it off the stack instead of
// opening the layers a second time.
func newLSMTBackendOverStack(stack *lsmtStack, closeUp func() error, overlayPath string, size int64) (*lsmtBackend, error) {
	if size < stack.virtualSize {
		return nil, fmt.Errorf("image: a %d-byte device cannot hold the %d-byte filesystem "+
			"in its layers", size, stack.virtualSize)
	}

	overlay, err := os.OpenFile(overlayPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("image: create overlay %s: %w", overlayPath, err)
	}
	if err := overlay.Truncate(size); err != nil {
		overlay.Close()
		return nil, fmt.Errorf("image: size overlay: %w", err)
	}

	blocks := (size + fileBackendBlockSize - 1) / fileBackendBlockSize
	return &lsmtBackend{
		stack:     stack,
		closeUp:   closeUp,
		overlay:   overlay,
		size:      size,
		blockSize: fileBackendBlockSize,
		owned:     make([]uint64, (blocks+63)/64),
	}, nil
}

func (b *lsmtBackend) Close() error {
	var errs []error
	if b.overlay != nil {
		errs = append(errs, b.overlay.Close())
		b.overlay = nil
	}
	if b.closeUp != nil {
		errs = append(errs, b.closeUp())
		b.closeUp = nil
	}
	return errors.Join(errs...)
}

func (b *lsmtBackend) isOwned(block int64) bool {
	i, bit := block/64, uint(block%64)
	if i >= int64(len(b.owned)) {
		return false
	}
	return b.owned[i]&(1<<bit) != 0
}

func (b *lsmtBackend) setOwned(block int64) {
	i, bit := block/64, uint(block%64)
	if i < int64(len(b.owned)) {
		b.owned[i] |= 1 << bit
	}
}

// ReadAt serves each block from the overlay if the sandbox owns it, else from the
// layers.
func (b *lsmtBackend) ReadAt(p []byte, off int64) (int, error) {
	if off >= b.size {
		return 0, nil
	}
	if off+int64(len(p)) > b.size {
		p = p[:b.size-off]
	}

	done := 0
	for done < len(p) {
		pos := off + int64(done)
		block := pos / b.blockSize
		// Clipped to the block so each chunk comes from exactly one source.
		chunk := b.blockSize - pos%b.blockSize
		if int64(len(p)-done) < chunk {
			chunk = int64(len(p) - done)
		}
		dst := p[done : done+int(chunk)]

		if b.isOwned(block) {
			n, err := b.overlay.ReadAt(dst, pos)
			if err != nil && !isEOF(err) {
				return done, err
			}
			// A hole inside the overlay's extent reads as zero.
			for i := n; i < len(dst); i++ {
				dst[i] = 0
			}
			done += int(chunk)
			continue
		}

		// Past the end of the layers is not an error: the device can be larger than
		// the image, and those blocks are zero until something writes them.
		if pos >= b.stack.virtualSize {
			for i := range dst {
				dst[i] = 0
			}
			done += int(chunk)
			continue
		}
		n, err := b.stack.ReadAt(dst, pos)
		if err != nil && !errors.Is(err, io.EOF) {
			return done, err
		}
		for i := n; i < len(dst); i++ {
			dst[i] = 0
		}
		done += int(chunk)
	}
	return done, nil
}

// WriteAt sends every write to the overlay and records the blocks it now owns.
//
// A partial write of a block the overlay does not own has to fill the rest of that
// block from the layers first. Skipping it leaves the untouched part of the block as
// zeros, so a one-byte write to a file destroys the rest of the block around it, and
// nothing reports an error.
func (b *lsmtBackend) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > b.size {
		return 0, errors.New("image: write past the end of the device")
	}

	done := 0
	for done < len(p) {
		pos := off + int64(done)
		block := pos / b.blockSize
		blockStart := block * b.blockSize
		chunk := b.blockSize - pos%b.blockSize
		if int64(len(p)-done) < chunk {
			chunk = int64(len(p) - done)
		}

		if !b.isOwned(block) && chunk < b.blockSize {
			buf := make([]byte, b.blockSize)
			if blockStart < b.stack.virtualSize {
				n, err := b.stack.ReadAt(buf, blockStart)
				if err != nil && !errors.Is(err, io.EOF) {
					return done, err
				}
				for i := n; i < len(buf); i++ {
					buf[i] = 0
				}
			}
			if _, err := b.overlay.WriteAt(buf, blockStart); err != nil {
				return done, err
			}
		}

		if _, err := b.overlay.WriteAt(p[done:done+int(chunk)], pos); err != nil {
			return done, err
		}
		b.setOwned(block)
		done += int(chunk)
	}
	return done, nil
}

// Flush syncs the overlay. The layers are read-only, so there is nothing to flush there.
func (b *lsmtBackend) Flush() error {
	if b.overlay == nil {
		return nil
	}
	return b.overlay.Sync()
}

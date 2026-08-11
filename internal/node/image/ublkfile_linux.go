//go:build linux

package image

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// fileBackend serves a ublk device from two files: a shared read-only base and a
// per-sandbox writable overlay.
//
// This is the same copy-on-write shape dm-snapshot gives, implemented in userspace. It
// exists because the measured cost of the dm path is not the copying -- the layers are
// sparse and cost kilobytes -- but the process spawning: `losetup` twice and `dmsetup`
// once per sandbox, at ~26 ms per call, which is 3.8 s of a 4.5 s create at 256-way
// concurrency. A ublk device needs none of them.
//
// The overlay is a sparse file plus a bitmap of which blocks it owns. A block the sandbox
// has written comes from the overlay; anything else comes from the base. That is exactly
// dm-snapshot's exception table, kept in this process instead of in the kernel.
//
// Deliberately unlocked, and safe only because of who calls it. The ublk queue serves a
// backend inline on its single pinned thread unless the backend reports MayBlock(); this one
// does not, because a pread of a local file is microseconds and a worker handoff would cost
// more than the read. Its requests are therefore serialised by the queue, and the bitmap
// needs no mutex.
//
// If that changes -- if this backend is ever handed to workers, or reused somewhere
// concurrent -- it needs the lock lsmtBackend carries. The failure would not look like a race:
// two writers to different halves of one unowned block both fill it from the base and the
// second erases the first, and a torn bitmap serves a block from the wrong file. On hardware
// that reached the guest as "EXT4-fs error: reading directory" and a virtio I/O error, naming
// neither the bitmap nor the concurrency.
type fileBackend struct {
	base    *os.File
	overlay *os.File
	size    int64

	// blockSize is the granularity of ownership. 4 KiB matches the page size and the
	// filesystem block size in bean's images, so a guest write never straddles two
	// blocks and forces a read-modify-write.
	blockSize int64

	// owned marks blocks the overlay holds. A bitmap rather than a map: at 4 KiB blocks
	// a 20 GiB sandbox needs 640 KiB of bitmap, where a map with per-block overhead
	// would be tens of megabytes and would grow on the IO path.
	owned []uint64
}

// newFileBackend opens a base image and creates a sandbox's overlay.
//
// The overlay is created sparse and never preallocated: a sandbox that writes nothing
// costs nothing, which is the property the 44 KiB-per-sandbox figure comes from.
func newFileBackend(basePath, overlayPath string, size int64) (*fileBackend, error) {
	base, err := os.Open(basePath)
	if err != nil {
		return nil, fmt.Errorf("image: open base %s: %w", basePath, err)
	}
	overlay, err := os.OpenFile(overlayPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		base.Close()
		return nil, fmt.Errorf("image: create overlay %s: %w", overlayPath, err)
	}
	// Sized but not allocated. The file has to be as large as the device or a write near
	// the end would extend it past what the guest was told it has.
	if err := overlay.Truncate(size); err != nil {
		base.Close()
		overlay.Close()
		return nil, fmt.Errorf("image: size overlay: %w", err)
	}

	blocks := (size + fileBackendBlockSize - 1) / fileBackendBlockSize
	return &fileBackend{
		base:      base,
		overlay:   overlay,
		size:      size,
		blockSize: fileBackendBlockSize,
		owned:     make([]uint64, (blocks+63)/64),
	}, nil
}

func (b *fileBackend) Close() error {
	var errs []error
	if b.base != nil {
		errs = append(errs, b.base.Close())
		b.base = nil
	}
	if b.overlay != nil {
		errs = append(errs, b.overlay.Close())
		b.overlay = nil
	}
	return errors.Join(errs...)
}

func (b *fileBackend) isOwned(block int64) bool {
	i, bit := block/64, uint(block%64)
	if i >= int64(len(b.owned)) {
		return false
	}
	return b.owned[i]&(1<<bit) != 0
}

func (b *fileBackend) setOwned(block int64) {
	i, bit := block/64, uint(block%64)
	if i < int64(len(b.owned)) {
		b.owned[i] |= 1 << bit
	}
}

// ReadAt serves each block from whichever file owns it.
//
// Block by block rather than one read of the whole range, because a range can span owned
// and unowned blocks and the two live in different files. The loop is the cost of not
// having the kernel's exception table.
func (b *fileBackend) ReadAt(p []byte, off int64) (int, error) {
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
		// Read to the end of this block or the end of the request, whichever comes
		// first, so each chunk comes from exactly one file.
		chunk := b.blockSize - pos%b.blockSize
		if int64(len(p)-done) < chunk {
			chunk = int64(len(p) - done)
		}

		src := b.base
		if b.isOwned(block) {
			src = b.overlay
		}
		n, err := src.ReadAt(p[done:done+int(chunk)], pos)
		if err != nil && n == 0 {
			// A short base image is not an error: the guest may read past what the
			// image holds, and those blocks are zero. Distinguishing that from a real
			// failure is what the n == 0 check is for.
			if isEOF(err) {
				for i := done; i < done+int(chunk); i++ {
					p[i] = 0
				}
				done += int(chunk)
				continue
			}
			return done, err
		}
		// A short read inside the file's extent means the rest of the chunk is a hole,
		// which reads as zero.
		for i := done + n; i < done+int(chunk); i++ {
			p[i] = 0
		}
		done += int(chunk)
	}
	return done, nil
}

// WriteAt sends every write to the overlay and records the blocks it now owns.
//
// A partially written block has to be filled from the base first. Skipping that step is
// the dm-snapshot bug this codebase already hit from the other direction: the device
// serves the base for a block the overlay owns, and the guest sees stale bytes with no
// error anywhere.
func (b *fileBackend) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > b.size {
		return 0, unix.ENOSPC
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

		// Copy the base's block in before a partial write, so the untouched part of the
		// block is the base's content rather than zeros.
		if !b.isOwned(block) && chunk < b.blockSize {
			buf := make([]byte, b.blockSize)
			n, err := b.base.ReadAt(buf, blockStart)
			if err != nil && !isEOF(err) {
				return done, err
			}
			for i := n; i < len(buf); i++ {
				buf[i] = 0
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

// Flush syncs the overlay. The base is read-only, so there is nothing to flush there.
func (b *fileBackend) Flush() error {
	if b.overlay == nil {
		return nil
	}
	return b.overlay.Sync()
}

// isEOF lives in lsmt.go, which is portable: the LSMT reader needs it too, and that
// file compiles on every platform where the format can be parsed.

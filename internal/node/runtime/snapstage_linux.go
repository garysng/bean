//go:build linux

package runtime

import (
	"fmt"
	"os"
)

// A restore has to establish the sandbox's writable layer before the block
// device is assembled from it, which means the bundle is consumed before the
// image provider runs rather than after.
//
// The order is not a preference. A device-mapper snapshot loads its exception
// table into kernel memory when the device is activated and never reads it
// again, so exceptions written to the copy-on-write store after that point are
// invisible: the device keeps serving the base image. Nothing reports an error —
// the restored guest's own metadata still describes its files, so they appear
// with the right sizes and read back as zeroes once the guest's page cache is
// reclaimed. Staging first is what makes the restored filesystem real.

// snapshotStage holds a bundle that has been unpacked but not yet applied.
//
// The filesystem is no longer staged here: a restore resolves it from the
// snapshot's sealed overlaybd layer chain, named by the manifest digest on the
// spec, so nothing about the writable layer travels in the bundle. What remains
// is the guest memory and machine state, the one part no image can supply.
type snapshotStage struct {
	// entry locates the machine state and memory image, shared across restores
	// of the same snapshot. Both paths are empty for a filesystem-only
	// checkpoint, which tells the caller to boot instead of load.
	entry snapEntry
	dir   string
	// unpin releases the cache entry once the memory image has been opened. Until
	// then the entry is only a path, and a sweep that removed it would turn the
	// open into ENOENT with the restore's stream already consumed.
	unpin func()
}

// stageSnapshot unpacks a chain into dir without touching any device.
func (r *FCRuntime) stageSnapshot(dir string, spec *Spec, layers []SnapshotLayer) (*snapshotStage, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("fc: create snapshot staging dir: %w", err)
	}
	stage := &snapshotStage{dir: dir}

	// Pinned across the whole staging-to-load span, not just the lookup: the entry
	// is a set of paths until loadSnapshot opens the memory image, and a sweep in
	// between would leave this restore with nothing to rebuild from.
	if spec != nil && spec.SnapshotID != "" {
		stage.unpin = r.snapshots.Pin(spec.SnapshotID)
	}

	entry, err := r.snapshotState(dir, spec, layers)
	if err != nil {
		stage.Close()
		return nil, err
	}
	stage.entry = entry
	return stage, nil
}

// Close releases the cache pin. No files are removed here: an uncached bundle
// leaves its machine state and memory image in this directory, and the VM is
// still using them -- Firecracker faults guest pages from the memory image for as
// long as it runs. They go when the sandbox directory does.
//
// Releasing the pin here is safe even though the VM is still running, because by
// this point the memory image is open and mapped. An unlinked file's inode
// survives until the last mapping goes away, so a sweep after this point costs
// the next restore a re-unpack and costs this VM nothing.
func (s *snapshotStage) Close() {
	if s == nil {
		return
	}
	if s.unpin != nil {
		s.unpin()
		s.unpin = nil
	}
}

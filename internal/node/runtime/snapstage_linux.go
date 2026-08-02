//go:build linux

package runtime

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
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
type snapshotStage struct {
	// entry locates the machine state and memory image, shared across restores
	// of the same snapshot. Both paths are empty for a filesystem-only
	// checkpoint, which tells the caller to boot instead of load.
	entry snapEntry
	// rootfs is the writable layer's extent stream, held verbatim as it arrived
	// so applying it is a straight replay onto whatever the provider creates.
	rootfs string
	dir    string
}

// stageSnapshot unpacks a bundle into dir without touching any device.
func (r *FCRuntime) stageSnapshot(dir string, spec *Spec, src io.Reader) (*snapshotStage, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("fc: create snapshot staging dir: %w", err)
	}
	stage := &snapshotStage{dir: dir, rootfs: filepath.Join(dir, snapshotRootfsFile)}

	entry, err := r.snapshotState(stage.rootfs, spec, src)
	if err != nil {
		stage.Close()
		return nil, err
	}
	stage.entry = entry

	// A bundle with no writable layer is not an error: a checkpoint of a sandbox
	// that wrote nothing has no extents to carry. Clearing the path keeps the
	// provider from being handed a seed that would find no file.
	if _, err := os.Stat(stage.rootfs); err != nil {
		stage.rootfs = ""
	}
	return stage, nil
}

// SeedWritable replays the staged extents onto the layer the provider created.
// It is passed to Prepare so it runs at the one point where the writes are
// guaranteed to be visible to the assembled device.
func (s *snapshotStage) SeedWritable(dest string) error {
	if s == nil || s.rootfs == "" {
		return nil
	}
	src, err := os.Open(s.rootfs)
	if err != nil {
		return err
	}
	defer src.Close()

	// The layer already exists at its provisioned size, so it is written in
	// place: truncating it would discard that size, and for a block device the
	// truncation would fail outright.
	f, err := os.OpenFile(dest, os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := readSparse(src, f); err != nil {
		return fmt.Errorf("replay writable layer: %w", err)
	}
	return nil
}

// Close drops the staged extent stream once it has been replayed. It is the only
// part removed: an uncached bundle leaves its machine state and memory image in
// this directory, and the VM is still using them — Firecracker faults guest
// pages from the memory image for as long as it runs. They go when the sandbox
// directory does.
func (s *snapshotStage) Close() {
	if s == nil || s.rootfs == "" {
		return
	}
	os.Remove(s.rootfs)
	s.rootfs = ""
}

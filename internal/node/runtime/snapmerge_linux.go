//go:build linux

package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// A diff checkpoint holds only the guest pages written since its base, so
// restoring one means reconstructing a full memory image from a chain of them.
//
// The reconstruction happens here, once per snapshot per node, rather than in the
// page-fault handler. Serving faults from a layered stack would avoid the copy,
// but it would put a lookup on the hottest path in the system — the one where a
// mistake hands the guest a page of someone else's memory and nothing reports it.
// Merging up front keeps uffd_linux.go serving one flat image, which is the same
// code a full snapshot has always used.
//
// The copy is paid once because the result lands in the snapshot cache under the
// leaf's id: a batch fanning out from one prepared checkpoint merges on the first
// restore and every later one reuses it. That is the case diff snapshots exist
// for, so it is the case the caching is arranged around.

// mergeChain reconstructs a full memory image and machine state from an ordered
// chain, writing them into dir. The filesystem is not part of a chain here: it is
// resolved from the snapshot's sealed overlaybd layers, so these bundles carry
// only guest memory and machine state.
//
// Order is load-bearing and not verifiable from the layers themselves: a later
// page legitimately overwrites an earlier one, so a reversed chain produces a
// coherent-looking image built from stale pages. The caller owns the ordering,
// which is why the chain is passed as a slice rather than assembled here from
// parent links discovered along the way.
func mergeChain(layers []SnapshotLayer, dir string) (snapEntry, error) {
	if len(layers) == 0 {
		return snapEntry{}, errors.New("fc: snapshot chain is empty")
	}

	memPath := filepath.Join(dir, snapshotMemFile)
	var entry snapEntry

	for i, layer := range layers {
		paths, err := readSnapshotBundle(layer.Data, dir)
		if err != nil {
			return snapEntry{}, fmt.Errorf("fc: read layer %d (%s) of %d: %w",
				i+1, layer.ID, len(layers), err)
		}

		if diffPath := paths[snapshotMemDiffFile]; diffPath != "" {
			if i == 0 {
				// A chain rooted in a diff has no complete image to build on.
				// This means the chain was assembled wrongly or its base was
				// deleted; either way the guest cannot be reconstructed, and
				// starting from a hole-filled image would corrupt it silently.
				return snapEntry{}, fmt.Errorf(
					"fc: chain starts at %s, which is a diff checkpoint and has no base to layer onto",
					layer.ID)
			}
			if err := applyMemDiff(diffPath, memPath); err != nil {
				return snapEntry{}, fmt.Errorf("fc: apply layer %d (%s): %w", i+1, layer.ID, err)
			}
		} else if paths[snapshotMemFile] == "" {
			return snapEntry{}, fmt.Errorf("fc: layer %d (%s) carries no memory image", i+1, layer.ID)
		}

		// The machine state comes from the last layer that has one, which is the
		// leaf: Firecracker's own guidance is to pair a memory image with the
		// state written by the same create call, so an earlier layer's state
		// would describe devices as they were before the diffs were taken.
		if p := paths[snapshotStateFile]; p != "" {
			entry.StatePath = p
		}
	}

	if entry.StatePath == "" {
		return snapEntry{}, errors.New("fc: snapshot chain carries no machine state")
	}
	entry.MemPath = memPath
	return entry, nil
}

// applyMemDiff overlays one layer's dirtied pages onto the accumulated image.
//
// The diff is an extent list, so this writes only the recorded ranges and leaves
// everything else as the base had it — which is the whole point. A dense format
// could not express the difference between "the guest zeroed this page" and "the
// guest never touched it", and would flatten the base wherever it was untouched.
func applyMemDiff(diffPath, memPath string) error {
	src, err := os.Open(diffPath)
	if err != nil {
		return err
	}
	defer src.Close()

	// Opened without O_TRUNC: the accumulated image is what is being modified,
	// not replaced.
	dest, err := os.OpenFile(memPath, os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer dest.Close()

	// readSparse truncates its destination to the stream's logical size, which is
	// correct when it is materialising a file and wrong here: a layer recorded at
	// a different guest size would silently shorten the accumulated image, and a
	// guest resumed against a truncated image faults on memory that vanished.
	// Both are the guest's memory size, so a mismatch means the chain does not
	// belong together.
	before, err := dest.Stat()
	if err != nil {
		return err
	}
	if err := readSparse(src, dest); err != nil {
		return err
	}
	after, err := dest.Stat()
	if err != nil {
		return err
	}
	if before.Size() != after.Size() {
		return fmt.Errorf("layer is sized for a %d-byte guest but the image is %d bytes",
			after.Size(), before.Size())
	}
	// The staged extent stream is consumed. Leaving it would double the cache
	// entry's size for a file nothing reads again.
	return os.Remove(diffPath)
}

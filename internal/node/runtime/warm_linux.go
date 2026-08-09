//go:build linux

package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/garysng/bean/internal/logging"
)

// FCRuntime's half of warm snapshots: deriving the key for an image, looking one
// up, and writing one from a running sandbox.
//
// The orchestration -- booting a guest, waiting for its agent, and destroying it
// once captured -- is the Manager's, because the readiness gate lives there and a
// warm snapshot is only worth having if it holds a guest that was actually ready.
// This file is what the Manager calls into.

// WarmEnabled reports whether this node produces and consults warm snapshots.
func (r *FCRuntime) WarmEnabled() bool { return r.WarmSnapshots }

// warmKeyFor builds the key for an image on this node.
//
// The CPU identity is read per call rather than cached at construction. It is
// stable for a process, so this is not about correctness of the value but about
// not having a constructor that can fail: a node whose CPU cannot be identified
// should decline to warm, not refuse to start.
func (r *FCRuntime) warmKeyFor(imageRef string) (warmKey, error) {
	if r.Images == nil {
		return warmKey{}, errors.New("fc: no image provider")
	}
	digest, err := r.Images.Digest(imageRef)
	if err != nil {
		return warmKey{}, fmt.Errorf("fc: read image digest: %w", err)
	}
	vendor, family, err := HostCPUIdentity()
	if err != nil {
		return warmKey{}, fmt.Errorf("fc: identify host cpu: %w", err)
	}
	return warmKey{
		Digest:   digest,
		Vendor:   vendor,
		Family:   family,
		Template: r.CPUTemplate,
	}, nil
}

// WarmKeyFor reports the key an image's warm snapshot would have here, and whether
// the image can be warmed at all.
//
// ok=false with a nil error is the ordinary "this image has no digest" case, which
// is not a failure: a build's output and a commit have no manifest, and an image
// converted before digests were recorded has none either. All three boot.
func (r *FCRuntime) WarmKeyFor(imageRef string) (string, bool, error) {
	key, err := r.warmKeyFor(imageRef)
	if err != nil {
		return "", false, err
	}
	if !key.warmable() {
		return "", false, nil
	}
	return key.snapshotID(), true, nil
}

// WarmLookup returns the layer a create should restore from, or ok=false.
//
// The returned release closes the bundle. It is separate from the layer rather than
// wrapped into an io.ReadCloser because SnapshotLayer.Data is an io.Reader by
// design -- the restore path consumes layers from a gRPC stream in the ordinary
// case, where there is nothing to close -- and giving the caller an explicit
// release keeps a local file from depending on the restore path noticing it is
// closeable.
//
// Every failure here is a miss, not an error. A node that cannot read its own warm
// bundle should boot, because the alternative is one bad file making an image
// unusable on this node until somebody notices.
func (r *FCRuntime) WarmLookup(imageRef string) (SnapshotLayer, func(), bool) {
	if !r.WarmSnapshots {
		return SnapshotLayer{}, nil, false
	}
	key, err := r.warmKeyFor(imageRef)
	if err != nil || !key.warmable() {
		return SnapshotLayer{}, nil, false
	}
	path, ok := r.warm.Lookup(key)
	if !ok {
		return SnapshotLayer{}, nil, false
	}
	// Held before opening, so a sweep running concurrently cannot unlink it between
	// the two. Unlinking an open file succeeds on Linux, so the restore would work
	// and the bundle would silently vanish -- the node goes cold for that image with
	// nothing reporting why.
	unhold := r.warm.hold(key.filename())
	f, err := os.Open(path)
	if err != nil {
		unhold()
		slog.Warn("warm snapshot present but unreadable; booting instead",
			logging.KeyImage, imageRef, logging.KeyError, err)
		return SnapshotLayer{}, nil, false
	}
	// Marks the bundle as recently used, which is what eviction orders by. Done at
	// lookup rather than on success: a restore that fails still says this image is
	// one creates are asking for, and demoting it for failing would evict the entry
	// whose failure most wants investigating.
	r.warm.Touch(key)
	return SnapshotLayer{ID: key.snapshotID(), Data: f},
		func() { _ = f.Close(); unhold() }, true
}

// WarmStore checkpoints a running sandbox as the warm snapshot for an image.
//
// With memory, deliberately: a filesystem-only checkpoint restores by booting
// (loadSnapshot dispatches on whether the bundle has a memory member), so it would
// save storage and none of the ~5 CPU-seconds this exists to remove. That is also
// why the result is bound to this node's CPU, and why the key carries it.
//
// Written through warmStore's temporary-then-rename, so a create running
// concurrently either sees the previous bundle or none -- never this one half
// written.
func (r *FCRuntime) WarmStore(ctx context.Context, imageRef, sandboxID string) error {
	if !r.WarmSnapshots {
		return errors.New("fc: warm snapshots are not enabled on this node")
	}
	key, err := r.warmKeyFor(imageRef)
	if err != nil {
		return err
	}
	if !key.warmable() {
		// Not an error: the caller cannot know this before asking, and the answer is
		// "boot every time", which is what already happens.
		return nil
	}

	f, commit, err := r.warm.Create(key)
	if err != nil {
		return err
	}
	// Removed on any failure path, so an aborted warm does not leave a temporary for
	// Clean to find at the next startup.
	defer func() {
		if commit != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()

	if _, err := r.Checkpoint(ctx, sandboxID, f, CheckpointOptions{IncludeMemory: true}); err != nil {
		return fmt.Errorf("fc: checkpoint for warm snapshot: %w", err)
	}
	done := commit
	commit = nil
	if err := done(); err != nil {
		return err
	}
	slog.Info("warm snapshot stored", logging.KeyImage, imageRef,
		logging.KeySnapshot, key.snapshotID())

	// Swept after adding rather than before. Refusing a prewarm because the store is
	// full would mean the first N images a node ever prewarms are the only ones it can
	// ever have, and which N depends on the order requests happened to arrive.
	// Evicting something colder instead keeps the set of warm images tracking what
	// creates actually ask for.
	//
	// The bundle just written is passed as keep, so this sweep cannot remove it.
	// Without that, a low mark below one bundle's size evicts the entry this call was
	// asked to produce -- measured on a node with a 10 MiB low mark and 15 MB
	// bundles, which emptied the store and still logged the prewarm as successful.
	//
	// Failure is logged, not returned: the bundle this call was asked to produce
	// exists, and reporting a sweep error as a failed warm would make an operator
	// think the image is cold when it is not.
	if freed, evicted, serr := r.warm.Sweep(r.WarmEviction, r.warm.inUse,
		key.filename()); serr != nil {
		slog.Warn("warm store sweep incomplete", logging.KeyError, serr,
			"freedBytes", freed, "evicted", evicted)
	} else if evicted > 0 {
		slog.Info("evicted warm snapshots to stay under the disk bound",
			"evicted", evicted, "freedBytes", freed)
	}
	return nil
}

// WarmBytes reports the space warm snapshots hold on this node.
//
// Reported because it is otherwise invisible: a warm bundle consumes no
// commitment, so a node can fill its disk with them while placement still believes
// it has room -- the same reasoning as the snapshot cache's own accounting.
func (r *FCRuntime) WarmBytes() (int64, error) {
	sizes, err := r.warm.List()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, n := range sizes {
		total += n
	}
	return total, nil
}

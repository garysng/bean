//go:build linux

package image

import (
	"context"
	"testing"
)

// A snapshot's sealed layer is readable from the store whether or not lazy pull is on.
//
// The two kinds of layer differ in whether the store is the only source. An image layer can
// always be converted from the registry, so refusing to read it remotely falls back to owning it
// locally -- which is the trade lazy pull exists to let an operator make. A snapshot's sealed
// layer has no registry blob behind it: it *is* the only form the filesystem exists in, so
// refusing does not fall back to anything, it makes the restore impossible.
//
// That is what happened. Publication is unconditional, so a checkpoint published its layer and
// the restore then declined to fetch it, failing with "in neither the node nor the store" about a
// digest that was in the store. Verified on hardware before this fix.
func TestSnapshotLayersAreReadableWithoutLazyPull(t *testing.T) {
	p := NewOverlaybdProvider(t.TempDir(), t.TempDir(), t.TempDir(), nil, nil, 2048)
	p.Blobs = presentBlobStore{}
	p.LazyPull = false

	if _, ok := p.requiredRemoteLayer(context.Background(), "sha256:sealed"); !ok {
		t.Error("a snapshot's sealed layer was refused with lazy pull off, so a restore cannot " +
			"fetch the layer its own checkpoint published")
	}

	// An image layer is still gated, because it has somewhere else to come from.
	if _, ok := p.remoteLayer(context.Background(), "sha256:image"); ok {
		t.Error("an image layer was read remotely with lazy pull off, which removes the " +
			"deployment choice lazy pull exists to offer")
	}

	// With lazy pull on, both are readable.
	p.LazyPull = true
	if _, ok := p.remoteLayer(context.Background(), "sha256:image"); !ok {
		t.Error("an image layer was refused with lazy pull on")
	}
}

// Neither lookup invents a layer the store does not have.
//
// The failure to avoid is a restore that resolves a chain naming a layer nobody holds, which
// produces a device with a hole in it rather than an error.
func TestRemoteLookupsRefuseAnAbsentLayer(t *testing.T) {
	p := NewOverlaybdProvider(t.TempDir(), t.TempDir(), t.TempDir(), nil, nil, 2048)
	p.Blobs = absentBlobStore{}
	p.LazyPull = true

	if _, ok := p.requiredRemoteLayer(context.Background(), "sha256:nope"); ok {
		t.Error("a layer absent from the store was reported as present")
	}
	if _, ok := p.remoteLayer(context.Background(), "sha256:nope"); ok {
		t.Error("a layer absent from the store was reported as present")
	}

	// And with no store configured at all, neither claims anything.
	p.Blobs = nil
	if _, ok := p.requiredRemoteLayer(context.Background(), "sha256:nope"); ok {
		t.Error("a layer was reported present with no store configured")
	}
}

type presentBlobStore struct{ stubBlobStore }

func (presentBlobStore) Stat(context.Context, string) (int64, bool, error) { return 4096, true, nil }

type absentBlobStore struct{ stubBlobStore }

func (absentBlobStore) Stat(context.Context, string) (int64, bool, error) { return 0, false, nil }

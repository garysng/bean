//go:build linux

package image

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Sealing refuses a writable layer whose index is still empty.
//
// The daemon keeps the index in memory while the device is attached and writes it on close, so
// during a checkpoint the on-disk index is zero bytes while the data file already holds every
// write. overlaybd-commit reads the index, so it would seal a layer of pure metadata -- measured
// at 36 KiB for a sandbox that had written a file.
//
// What makes that worth a guard rather than a comment is how it fails: the snapshot records an
// FS digest, the manifest lists base plus sealed layer, both blobs reach the store, and the
// restore boots on the base image alone. Nothing reports a problem, and the filesystem the
// snapshot existed to keep is gone.
func TestSealRefusesAnEmptyWritableIndex(t *testing.T) {
	base := t.TempDir()
	p := NewOverlaybdProvider(base, t.TempDir(), t.TempDir(), nil, nil, 2048)
	// Blobs and Index have to be non-nil or SealSnapshotFS returns early by design.
	p.Blobs = stubBlobStore{}
	p.Index = stubImageIndex{}
	p.attached["sbx-empty"] = &tcmuDevice{Name: "sbx-empty"}

	dir := filepath.Join(base, "sbx-empty")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The shape a checkpoint actually sees: data written, index not yet flushed.
	if err := os.WriteFile(filepath.Join(dir, "writable.data"), []byte("guest wrote this"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "writable.index"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, _, err := p.SealSnapshotFS(context.Background(), "sbx-empty", "alpine:3.20")
	if err == nil {
		t.Fatal("sealing an empty index was accepted, so the snapshot would record a digest " +
			"for a layer holding no filesystem and the restore would silently boot the base image")
	}
	if !strings.Contains(err.Error(), "empty writable index") {
		t.Errorf("the refusal does not name the cause, so an operator cannot tell it from a "+
			"transient failure: %v", err)
	}
	// And it says this is known rather than transient, so nobody retries it forever.
	if !strings.Contains(err.Error(), "known gap") {
		t.Errorf("the refusal does not say the gap is known: %v", err)
	}
}

// A non-empty index is not refused by this guard.
//
// The guard has to be narrow: it fires on the one state that produces a silent empty layer, and
// anything else proceeds to the sealer, which reports its own errors. A guard that also rejected
// a populated index would make snapshots impossible rather than honest.
func TestSealDoesNotRefuseAPopulatedIndex(t *testing.T) {
	base := t.TempDir()
	p := NewOverlaybdProvider(base, t.TempDir(), t.TempDir(), nil, nil, 2048)
	p.Blobs = stubBlobStore{}
	p.Index = stubImageIndex{}
	p.attached["sbx-full"] = &tcmuDevice{Name: "sbx-full"}

	dir := filepath.Join(base, "sbx-full")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "writable.data"), []byte("data"), 0o600)
	os.WriteFile(filepath.Join(dir, "writable.index"), []byte("an index with content"), 0o600)

	_, _, _, err := p.SealSnapshotFS(context.Background(), "sbx-full", "alpine:3.20")
	// It will still fail -- there is no overlaybd-commit binary here and no real base chain --
	// but it must not fail for the empty-index reason.
	if err != nil && strings.Contains(err.Error(), "empty writable index") {
		t.Errorf("a populated index was refused as empty: %v", err)
	}
}

// stubBlobStore and stubImageIndex exist only to get past SealSnapshotFS's early return, which
// treats a nil store as "publication is not configured" and skips sealing entirely. Neither is
// reached by these tests: the guard fires first.
type stubBlobStore struct{}

func (stubBlobStore) Stat(context.Context, string) (int64, bool, error) { return 0, false, nil }
func (stubBlobStore) Put(context.Context, string, int64, io.Reader) error {
	return errors.New("stub store")
}
func (stubBlobStore) BlobURL() string                     { return "http://stub/blobs" }
func (stubBlobStore) CheckReadable(context.Context) error { return nil }

type stubImageIndex struct{}

func (stubImageIndex) PutManifest(context.Context, string, *StoredManifest) error { return nil }
func (stubImageIndex) GetManifest(context.Context, string) (*StoredManifest, error) {
	return nil, nil
}
func (stubImageIndex) PutTag(context.Context, Reference, string) error   { return nil }
func (stubImageIndex) GetTag(context.Context, Reference) (string, error) { return "", nil }

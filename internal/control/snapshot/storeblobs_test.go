package snapshot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/garysng/bean/internal/control/s3"
)

// storeBlobs is an adapter over any s3.ObjectStore; the DirStore is a real
// filesystem-backed ObjectStore, so the whole adapter -- key scheme, streaming
// writer, not-found mapping -- is exercised here without a live S3 endpoint.
// The S3-specific behaviour (multipart abort) is covered separately by the
// endpoint-gated tests in s3blobs_test.go.
func dirBackedBlobs(t *testing.T) Blobs {
	t.Helper()
	store, err := s3.NewDirStore(t.TempDir())
	if err != nil {
		t.Fatalf("dir store: %v", err)
	}
	return NewBlobs(context.Background(), store)
}

func TestStoreBlobsRoundTrip(t *testing.T) {
	b := dirBackedBlobs(t)
	const id = "snap_roundtrip"
	payload := []byte("checkpoint bytes")

	w, err := b.Writer(id)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	if _, err := w.Write(payload); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	size, err := b.Size(id)
	if err != nil {
		t.Fatalf("size: %v", err)
	}
	if size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", size, len(payload))
	}

	r, err := b.Reader(id)
	if err != nil {
		t.Fatalf("reader: %v", err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("read back %q, want %q", got, payload)
	}

	if err := b.Delete(id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := b.Reader(id); !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("reader after delete = %v, want ErrBlobNotFound", err)
	}
}

func TestStoreBlobsMissingReportsNotFound(t *testing.T) {
	b := dirBackedBlobs(t)
	if _, err := b.Reader("snap_absent"); !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("reader missing = %v, want ErrBlobNotFound", err)
	}
	if _, err := b.Size("snap_absent"); !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("size missing = %v, want ErrBlobNotFound", err)
	}
	// Deleting a missing blob stays idempotent.
	if err := b.Delete("snap_absent"); err != nil {
		t.Errorf("delete missing = %v, want nil", err)
	}
}

// TestStoreBlobsRejectsUnsafeIDs guards the id-to-key mapping: a
// platform-generated id is trusted, but a path-traversing value must be
// refused before it becomes an object key.
func TestStoreBlobsRejectsUnsafeIDs(t *testing.T) {
	b := dirBackedBlobs(t)
	for _, id := range []string{"", "..", "a/b", "a\\b", "snap.1"} {
		if _, err := b.Writer(id); err == nil {
			t.Errorf("Writer(%q) accepted an unsafe id", id)
		}
		if _, err := b.Reader(id); err == nil {
			t.Errorf("Reader(%q) accepted an unsafe id", id)
		}
		if _, err := b.Size(id); err == nil {
			t.Errorf("Size(%q) accepted an unsafe id", id)
		}
		if err := b.Delete(id); err == nil {
			t.Errorf("Delete(%q) accepted an unsafe id", id)
		}
	}
}

// TestNewS3BlobsRejectsBadClient covers the NewS3Blobs constructor's error
// path: a client with no usable endpoint cannot ensure its bucket.
func TestNewS3BlobsRejectsBadClient(t *testing.T) {
	if _, err := NewS3Blobs(context.Background(), nil, "bean-test"); err == nil {
		t.Error("NewS3Blobs with a nil client returned no error")
	}
}

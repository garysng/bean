package snapshot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/garysng/bean/internal/control/s3"
)

// S3Blobs is tested against a real object store: the interesting behaviour —
// that an aborted upload publishes nothing, that a missing blob reports
// ErrBlobNotFound — is behaviour of the server, not of this wrapper.
//
// Set BEAN_S3_ENDPOINT to enable; otherwise these skip.
func testS3Blobs(t *testing.T) *S3Blobs {
	t.Helper()
	endpoint := os.Getenv("BEAN_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("BEAN_S3_ENDPOINT not set; skipping object-store snapshot test")
	}
	client, err := s3.New(s3.Config{
		Endpoint:  endpoint,
		Region:    "us-east-1",
		AccessKey: os.Getenv("BEAN_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("BEAN_S3_SECRET_KEY"),
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("s3 client: %v", err)
	}
	bucket := os.Getenv("BEAN_S3_TEST_BUCKET")
	if bucket == "" {
		bucket = "bean-test"
	}
	blobs, err := NewS3Blobs(context.Background(), client, bucket)
	if err != nil {
		t.Fatalf("new s3 blobs: %v", err)
	}
	return blobs
}

func TestS3BlobsRoundTrip(t *testing.T) {
	b := testS3Blobs(t)
	const id = "snap_s3_roundtrip"
	payload := []byte("checkpoint bytes")

	w, err := b.Writer(id)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	t.Cleanup(func() { _ = b.Delete(id) })
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
}

// TestS3BlobsAbortedWriteIsNotReadable is the property that keeps a failed
// snapshot from being restorable as a truncated blob.
func TestS3BlobsAbortedWriteIsNotReadable(t *testing.T) {
	b := testS3Blobs(t)
	const id = "snap_s3_aborted"

	w, err := b.Writer(id)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	if _, err := w.Write([]byte("partial")); err != nil {
		t.Fatalf("write: %v", err)
	}
	AbortWrite(b, id, w)

	if _, err := b.Reader(id); !errors.Is(err, ErrBlobNotFound) {
		_ = b.Delete(id)
		t.Errorf("reader after abort = %v, want ErrBlobNotFound", err)
	}
}

// TestS3BlobsUnpublishedWriteIsNotReadable checks the same property for the
// ordinary path: bytes written but not yet closed must not be visible.
func TestS3BlobsUnpublishedWriteIsNotReadable(t *testing.T) {
	b := testS3Blobs(t)
	const id = "snap_s3_unpublished"

	w, err := b.Writer(id)
	if err != nil {
		t.Fatalf("writer: %v", err)
	}
	if _, err := w.Write([]byte("in flight")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := b.Size(id); !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("size before close = %v, want ErrBlobNotFound", err)
	}
	AbortWrite(b, id, w)
}

func TestS3BlobsMissingAndDelete(t *testing.T) {
	b := testS3Blobs(t)

	if _, err := b.Reader("snap_s3_absent"); !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("reader missing = %v, want ErrBlobNotFound", err)
	}
	if _, err := b.Size("snap_s3_absent"); !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("size missing = %v, want ErrBlobNotFound", err)
	}
	// Deleting a missing blob must succeed so cleanup stays idempotent.
	if err := b.Delete("snap_s3_absent"); err != nil {
		t.Errorf("delete missing = %v, want nil", err)
	}
}

// TestS3BlobsRejectsUnsafeIDs guards the id-to-key mapping. Ids are
// platform-generated, but a path-traversing value must not reach the key.
func TestS3BlobsRejectsUnsafeIDs(t *testing.T) {
	b := testS3Blobs(t)
	for _, id := range []string{"", "..", "a/b", "a\\b", "snap.1"} {
		if _, err := b.Writer(id); err == nil {
			t.Errorf("Writer(%q) accepted an unsafe id", id)
		}
		if _, err := b.Reader(id); err == nil {
			t.Errorf("Reader(%q) accepted an unsafe id", id)
		}
		if err := b.Delete(id); err == nil {
			t.Errorf("Delete(%q) accepted an unsafe id", id)
		}
	}
}

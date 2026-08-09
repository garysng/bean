package s3

import (
	"context"
	"errors"
	"io"
)

// ObjectStore is the one contract every artifact store bottoms out on:
// snapshot blobs, overlaybd layers and build outputs. Keys are opaque; each
// caller owns its own key scheme (snapshots/<id>/data, blobs/<digest>, ...),
// so the store has no opinion on naming and the three concerns stay legible
// and independently reclaimable.
//
// This is the abstraction docs/s3-storage.md section 8 settles on. Two
// implementations satisfy it: BucketStore over the real S3 client for
// production, and a local-directory store for dev and CI -- the same
// DirBlobs/S3 equivalence snapshots already rely on, lifted to one type.
type ObjectStore interface {
	// Writer streams an object to key. The returned writer must be Closed;
	// nothing is readable at the key until Close returns nil -- the
	// half-product guarantee both prior implementations already made. A writer
	// that also satisfies Aborter can discard a partial write (multipart
	// abort, or the local temp file) instead of publishing it. This is the
	// streaming primitive: a snapshot bundle can be guest-RAM-sized, so it is
	// never buffered whole.
	Writer(ctx context.Context, key string) (io.WriteCloser, error)
	// Get opens an object, returning ErrNotFound if absent.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// GetRange fetches bytes [off, off+length). This is the block-level read
	// overlaybd lazy-pull depends on.
	GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, error)
	// Head reports an object's size, or ErrNotFound. It doubles as an
	// existence check.
	Head(ctx context.Context, key string) (int64, error)
	// Delete removes an object; deleting a missing object is not an error, so
	// cleanup is idempotent.
	Delete(ctx context.Context, key string) error
}

// Aborter lets a caller discard a partial Writer instead of publishing it. S3
// aborts its multipart upload; the local store deletes its temp file. It is a
// type assertion rather than part of the interface because only the write path
// needs it -- see AbortWriter.
type Aborter interface {
	Abort()
}

// AbortWriter discards a partial write if the writer supports it, else falls
// back to closing and deleting the key.
func AbortWriter(ctx context.Context, store ObjectStore, key string, w io.WriteCloser) {
	if a, ok := w.(Aborter); ok {
		a.Abort()
		return
	}
	_ = w.Close()
	_ = store.Delete(ctx, key)
}

// Put is a convenience over Writer for callers that already hold a reader (a
// sealed overlaybd layer, a build output). size is advisory today. On a copy
// failure the partial write is aborted so no truncated object is published.
func Put(ctx context.Context, store ObjectStore, key string, r io.Reader, size int64) error {
	_ = size
	w, err := store.Writer(ctx, key)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		AbortWriter(ctx, store, key, w)
		return err
	}
	return w.Close()
}

// SizeUnknown is passed as Put's size when the caller cannot state the length
// up front. Named rather than a bare -1 so call sites read as intent.
const SizeUnknown int64 = -1

// BucketStore binds a Client to one bucket and presents it as an ObjectStore.
// The bucket is ensured once at construction so a fresh deployment needs no
// out-of-band setup, matching what NewS3Blobs did per snapshot store.
type BucketStore struct {
	client *Client
	bucket string
}

// NewBucketStore returns a store over bucket, creating the bucket if absent.
func NewBucketStore(ctx context.Context, client *Client, bucket string) (*BucketStore, error) {
	if client == nil {
		return nil, errors.New("s3: object store needs a client")
	}
	if bucket == "" {
		return nil, errors.New("s3: object store needs a bucket")
	}
	if err := client.EnsureBucket(ctx, bucket); err != nil {
		return nil, err
	}
	return &BucketStore{client: client, bucket: bucket}, nil
}

// Writer streams to the key via multipart upload. The returned *Uploader
// already satisfies Aborter, so AbortWriter discards the parts rather than
// publishing. Nothing is readable at the key until Close (which completes the
// multipart upload) succeeds.
func (b *BucketStore) Writer(ctx context.Context, key string) (io.WriteCloser, error) {
	return b.client.NewUploader(ctx, b.bucket, key)
}

func (b *BucketStore) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return b.client.GetObject(ctx, b.bucket, key)
}

func (b *BucketStore) GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, error) {
	return b.client.GetRange(ctx, b.bucket, key, off, length)
}

func (b *BucketStore) Head(ctx context.Context, key string) (int64, error) {
	return b.client.HeadObject(ctx, b.bucket, key)
}

func (b *BucketStore) Delete(ctx context.Context, key string) error {
	return b.client.DeleteObject(ctx, b.bucket, key)
}

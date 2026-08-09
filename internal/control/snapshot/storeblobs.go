package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/garysng/bean/internal/control/s3"
)

// storeBlobs implements Blobs over the unified s3.ObjectStore, applying the
// snapshot key scheme. It replaces the once-separate S3Blobs and DirBlobs
// bodies: both are now this one adapter over a different store, which is the
// whole point of the unified contract (docs/s3-storage.md section 8).
type storeBlobs struct {
	store s3.ObjectStore
	ctx   context.Context
}

// NewBlobs wraps any object store with the snapshot key scheme. The context
// bounds streaming uploads and downloads, which in the S3 case outlive the
// request that started them.
func NewBlobs(ctx context.Context, store s3.ObjectStore) Blobs {
	return &storeBlobs{store: store, ctx: ctx}
}

// NewS3Blobs builds a snapshot store backed by an S3 bucket, ensuring the
// bucket exists. It is the object-store facade for production, kept as a named
// constructor so callers do not assemble the BucketStore themselves.
func NewS3Blobs(ctx context.Context, client *s3.Client, bucket string) (Blobs, error) {
	store, err := s3.NewBucketStore(ctx, client, bucket)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}
	return NewBlobs(ctx, store), nil
}

// key maps a snapshot id to its object key. The trailing element leaves room
// for the multi-part snapshot layout in docs/snapshot-resume.md section 3.1,
// where a checkpoint gains a manifest and a rootfs diff alongside this blob.
func (b *storeBlobs) key(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	return "snapshots/" + id + "/data", nil
}

// Writer streams to the object store; nothing is readable at the key until
// Close. It delegates straight to the store's streaming writer, so a
// guest-RAM-sized bundle is never buffered whole. The store's writer already
// satisfies s3.Aborter, and *storeBlobs re-exposes that through AbortWrite.
func (b *storeBlobs) Writer(id string) (io.WriteCloser, error) {
	key, err := b.key(id)
	if err != nil {
		return nil, err
	}
	return b.store.Writer(b.ctx, key)
}

func (b *storeBlobs) Reader(id string) (io.ReadCloser, error) {
	key, err := b.key(id)
	if err != nil {
		return nil, err
	}
	r, err := b.store.Get(b.ctx, key)
	if errors.Is(err, s3.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrBlobNotFound, id)
	}
	return r, err
}

func (b *storeBlobs) Size(id string) (int64, error) {
	key, err := b.key(id)
	if err != nil {
		return 0, err
	}
	n, err := b.store.Head(b.ctx, key)
	if errors.Is(err, s3.ErrNotFound) {
		return 0, fmt.Errorf("%w: %s", ErrBlobNotFound, id)
	}
	return n, err
}

func (b *storeBlobs) Delete(id string) error {
	key, err := b.key(id)
	if err != nil {
		return err
	}
	return b.store.Delete(b.ctx, key)
}

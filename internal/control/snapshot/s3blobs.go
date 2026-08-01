package snapshot

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/garysng/bean/internal/control/s3"
)

// S3Blobs stores checkpoint blobs in an S3-compatible bucket. This is the
// production implementation: control-plane replicas are interchangeable
// only if snapshot data does not live on any one replica's disk.
type S3Blobs struct {
	client *s3.Client
	bucket string
	// ctx bounds the lifetime of streaming uploads and downloads, which
	// outlive the request that started them in the multipart case.
	ctx context.Context
}

// NewS3Blobs returns a store over bucket, creating the bucket if absent so
// a fresh deployment needs no out-of-band setup.
func NewS3Blobs(ctx context.Context, client *s3.Client, bucket string) (*S3Blobs, error) {
	if bucket == "" {
		return nil, errors.New("snapshot: bucket required")
	}
	if err := client.EnsureBucket(ctx, bucket); err != nil {
		return nil, fmt.Errorf("snapshot: ensure bucket %s: %w", bucket, err)
	}
	return &S3Blobs{client: client, bucket: bucket, ctx: ctx}, nil
}

// key maps a snapshot id to its object key. The trailing element leaves room
// for the multi-part snapshot layout in docs/snapshot-resume.md §3.1, where a
// checkpoint gains a manifest and a rootfs diff alongside this blob.
func (s *S3Blobs) key(id string) (string, error) {
	if err := validateID(id); err != nil {
		return "", err
	}
	return "snapshots/" + id + "/data", nil
}

// Writer streams an object via multipart upload. Nothing is readable at the
// key until Close succeeds, so an aborted snapshot cannot leave a truncated
// blob that would fail a restore confusingly.
func (s *S3Blobs) Writer(id string) (io.WriteCloser, error) {
	key, err := s.key(id)
	if err != nil {
		return nil, err
	}
	// The returned *s3.Uploader already satisfies Aborter, so AbortWrite
	// discards the parts rather than publishing and deleting.
	return s.client.NewUploader(s.ctx, s.bucket, key)
}

func (s *S3Blobs) Reader(id string) (io.ReadCloser, error) {
	key, err := s.key(id)
	if err != nil {
		return nil, err
	}
	r, err := s.client.GetObject(s.ctx, s.bucket, key)
	if errors.Is(err, s3.ErrNotFound) {
		return nil, fmt.Errorf("%w: %s", ErrBlobNotFound, id)
	}
	return r, err
}

func (s *S3Blobs) Size(id string) (int64, error) {
	key, err := s.key(id)
	if err != nil {
		return 0, err
	}
	n, err := s.client.HeadObject(s.ctx, s.bucket, key)
	if errors.Is(err, s3.ErrNotFound) {
		return 0, fmt.Errorf("%w: %s", ErrBlobNotFound, id)
	}
	return n, err
}

func (s *S3Blobs) Delete(id string) error {
	key, err := s.key(id)
	if err != nil {
		return err
	}
	return s.client.DeleteObject(s.ctx, s.bucket, key)
}

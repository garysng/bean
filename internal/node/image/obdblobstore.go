package image

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Lazy pull needs sealed layers to live somewhere the overlaybd daemon can read over
// HTTP, because a node's create can only be a metadata operation if the bytes are
// already reachable without it. This file is that "somewhere" behind an interface.
//
// The shape was settled by measurement rather than by reading overlaybd's source.
// The daemon takes the config's `repoBlobUrl` and requests `<repoBlobUrl>/<digest>`,
// with Range headers -- verified on hardware against MinIO: 12 HTTP 206 responses,
// and a device that mounted and served bytes that existed nowhere locally. So the
// store does not have to speak the registry's v2 API, implement token auth, or push
// manifests. Any HTTP server that answers `<prefix>/<digest>` with ranges will do,
// which is why this is an object store and not a registry client.
//
// The requirement that comes with that: **the daemon reads anonymously.** It goes
// through registryfs, which knows nothing of SigV4, so credentials this process holds
// do not help it. A private bucket answers 403, registryfs looks for a
// WWW-Authenticate challenge it can follow, finds none, and the create fails with
// ENOENT from the kernel -- naming neither the bucket nor its policy. CheckReadable
// exists to turn that into a startup error instead.

// BlobStore publishes sealed overlaybd layers where the daemon can range-read them.
//
// Keyed by the layer's own digest so publication is idempotent and shared across
// images: two images referencing one layer publish it once, which is the same
// property that makes local conversion cheap on the second image.
type BlobStore interface {
	// Stat reports whether a layer is published and how long it is.
	//
	// The size is part of the answer rather than a separate call because a create
	// that decides to read a layer remotely needs it in the same breath: overlaybd
	// range-reads against a declared length, and a layer whose length is unknown
	// cannot be referenced at all. The store learns it from the same HEAD that
	// answers the first question.
	//
	// An error means the answer is unknown, which callers treat as "not published"
	// rather than failing -- republishing identical bytes under a digest-derived key
	// is harmless, and converting locally is always available.
	Stat(ctx context.Context, digest string) (size int64, ok bool, err error)
	// Put publishes a sealed layer. The reader is consumed once.
	Put(ctx context.Context, digest string, size int64, r io.Reader) error
	// BlobURL is the prefix to hand overlaybd as repoBlobUrl. The daemon appends
	// "/<digest>" itself, so this must not include the digest.
	BlobURL() string
	// CheckReadable reports whether the daemon will be able to read what this store
	// publishes.
	//
	// Separate from Stat because they ask different questions of different clients:
	// Stat is this process, signing with credentials, while the daemon reads
	// anonymously over plain HTTP and knows nothing about SigV4. A bucket this
	// process can write is routinely one the daemon cannot read, and that difference
	// is invisible until a create fails.
	CheckReadable(ctx context.Context) error
}

// s3BlobStore publishes layers to an S3-compatible bucket.
//
// It exists rather than a registry client because bean already has a hand-written
// SigV4 client and already keeps snapshots in a bucket, and because overlaybd turned
// out not to need registry semantics. A registry would mean a push protocol, token
// auth and manifests for the sake of a GET this already satisfies.
type s3BlobStore struct {
	client    blobPutter
	bucket    string
	prefix    string
	publicURL string
}

// blobPutter is the part of the S3 client this needs, named locally so the image
// package does not depend on the whole control-plane client surface -- and so a test
// can substitute one without a bucket.
type blobPutter interface {
	HeadObject(ctx context.Context, bucket, key string) (int64, error)
	PutObject(ctx context.Context, bucket, key string, body []byte) error
	GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error)
	EnsureBucket(ctx context.Context, bucket string) error
}

// NewS3BlobStore publishes to bucket under prefix, and reports publicURL as the
// prefix overlaybd should read from.
//
// publicURL is separate from the client's endpoint on purpose: the daemon resolves
// it, not this process, and the two can legitimately differ -- a node may write
// through an internal endpoint while the daemon reads through one the guest network
// can reach. Getting this wrong produces a device whose reads fail with the reason
// only in overlaybd's log, so it is required rather than derived.
func NewS3BlobStore(client blobPutter, bucket, prefix, publicURL string) (BlobStore, error) {
	if client == nil {
		return nil, errors.New("image: blob store needs an s3 client")
	}
	if bucket == "" {
		return nil, errors.New("image: blob store needs a bucket")
	}
	if publicURL == "" {
		return nil, errors.New("image: blob store needs the URL overlaybd will read from")
	}
	if prefix == "" {
		prefix = "blobs"
	}
	return &s3BlobStore{
		client:    client,
		bucket:    bucket,
		prefix:    strings.Trim(prefix, "/"),
		publicURL: strings.TrimRight(publicURL, "/"),
	}, nil
}

// key is the object name for a digest.
//
// The digest is kept verbatim, colon included, because the daemon appends it to
// repoBlobUrl unchanged: a key rewritten here would publish to a name the reader never
// asks for. Uploading such a key used to fail signing, which is fixed in the S3 signer
// rather than worked around here -- see canonicalPath in internal/control/s3.
func (s *s3BlobStore) key(digest string) string {
	return s.prefix + "/" + digest
}

func (s *s3BlobStore) Stat(ctx context.Context, digest string) (int64, bool, error) {
	size, err := s.client.HeadObject(ctx, s.bucket, s.key(digest))
	if err != nil {
		return 0, false, nil
	}
	if size <= 0 {
		// A zero-length blob is not usable as a layer and would be referenced as one,
		// producing a device whose reads fail with an error naming zfile structure. It
		// is reported as absent so the caller converts instead.
		return 0, false, nil
	}
	return size, true, nil
}

func (s *s3BlobStore) Put(ctx context.Context, digest string, size int64, r io.Reader) error {
	if err := s.client.EnsureBucket(ctx, s.bucket); err != nil {
		return fmt.Errorf("image: ensure blob bucket: %w", err)
	}
	// Read whole rather than streamed because a sealed layer is the compressed form
	// of one OCI layer -- tens of MiB for the images this is for -- and the S3
	// client's streaming path is a multipart uploader whose part bookkeeping is not
	// worth it at that size. A layer large enough to matter should switch to
	// NewUploader rather than raising this limit.
	body, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("image: read sealed layer: %w", err)
	}
	if size > 0 && int64(len(body)) != size {
		// Checked because a short read publishes a truncated blob under a digest
		// that claims otherwise, and overlaybd would then fail to open it with an
		// error naming zfile structure rather than the upload.
		return fmt.Errorf("image: sealed layer is %d bytes, expected %d", len(body), size)
	}
	if err := s.client.PutObject(ctx, s.bucket, s.key(digest), body); err != nil {
		return fmt.Errorf("image: publish layer %s: %w", digest, err)
	}
	return nil
}

func (s *s3BlobStore) BlobURL() string {
	return s.publicURL + "/" + s.bucket + "/" + s.prefix
}

// CheckReadable probes the read path the way the daemon will: a plain unsigned range
// request.
//
// A signed request proves nothing here. overlaybd's registryfs has no notion of SigV4 --
// it reads anonymously, and on a 401 or 403 it looks for a WWW-Authenticate challenge to
// follow. A private bucket answers 403 with no such header, so the daemon reports
// "connection failed" and then ENOENT, and the create fails with an error naming neither
// the bucket nor its policy. That is what a private bucket cost on first measurement.
//
// A missing object is not a failure: an empty store is the normal state before the first
// prewarm. What is checked is whether the *store* answers anonymously at all.
func (s *s3BlobStore) CheckReadable(ctx context.Context) error {
	url := s.BlobURL() + "/probe-readability"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	// Range-limited so a store that does answer never sends a body.
	req.Header.Set("Range", "bytes=0-0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("image: blob store unreachable at %s: %w", s.BlobURL(), err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))

	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("image: blob store at %s rejects anonymous reads (HTTP %d); "+
			"overlaybd reads it without credentials, so the published prefix must allow "+
			"anonymous GET -- for MinIO, `mc anonymous set download <alias>/%s`",
			s.BlobURL(), resp.StatusCode, s.bucket)
	default:
		// Anything else, including 404 for the probe key, means the store answered
		// without demanding credentials, which is the property being checked.
		return nil
	}
}

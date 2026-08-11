package image

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/garysng/bean/internal/control/s3"
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

// s3BlobStore publishes layers to a bucket, over the unified s3.ObjectStore.
//
// It exists rather than a registry client because bean already has a hand-written
// SigV4 client and already keeps snapshots in a bucket, and because overlaybd turned
// out not to need registry semantics. A registry would mean a push protocol, token
// auth and manifests for the sake of a GET this already satisfies.
//
// The byte operations go through the shared ObjectStore (the same contract snapshots
// use); bucket and publicURL are kept only to build BlobURL, which is overlaybd's
// anonymous-read prefix and has no meaning to the shared core -- see docs/s3-storage.md
// section 8.2.
type s3BlobStore struct {
	store     s3.ObjectStore
	bucket    string
	prefix    string
	publicURL string
}

// NewS3BlobStore publishes to store under prefix, and reports publicURL/bucket as the
// prefix overlaybd should read from.
//
// publicURL is separate from the client's endpoint on purpose: the daemon resolves
// it, not this process, and the two can legitimately differ -- a node may write
// through an internal endpoint while the daemon reads through one the guest network
// can reach. Getting this wrong produces a device whose reads fail with the reason
// only in overlaybd's log, so it is required rather than derived.
//
// bucket is passed alongside the store because the store hides it, yet the read URL
// overlaybd is handed must name it: BlobURL is publicURL/bucket/prefix.
func NewS3BlobStore(store s3.ObjectStore, bucket, prefix, publicURL string) (BlobStore, error) {
	if store == nil {
		return nil, errors.New("image: blob store needs an object store")
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
		store:     store,
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
	size, err := s.store.Head(ctx, s.key(digest))
	if err != nil {
		// Absent and unreachable are both "not published": the caller's next move is
		// to convert locally either way, and republishing under a digest-derived key
		// is harmless.
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
	// Streamed through the object store's writer rather than buffered whole: the
	// shared contract's Writer is the streaming primitive, so a large layer never
	// sits in memory. The writer publishes nothing at the key until Close, so a
	// short-read abort below leaves no truncated blob.
	key := s.key(digest)
	w, err := s.store.Writer(ctx, key)
	if err != nil {
		return fmt.Errorf("image: open layer writer: %w", err)
	}
	n, err := io.Copy(w, r)
	if err != nil {
		s3.AbortWriter(ctx, s.store, key, w)
		return fmt.Errorf("image: read sealed layer: %w", err)
	}
	if size > 0 && n != size {
		// Checked because a short read publishes a truncated blob under a digest
		// that claims otherwise, and overlaybd would then fail to open it with an
		// error naming zfile structure rather than the upload. Aborted so nothing
		// lands at the key.
		s3.AbortWriter(ctx, s.store, key, w)
		return fmt.Errorf("image: sealed layer is %d bytes, expected %d", n, size)
	}
	if err := w.Close(); err != nil {
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

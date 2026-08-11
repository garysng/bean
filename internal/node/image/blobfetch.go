package image

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
)

// registryRangeFetcher reads byte ranges of a registry blob over HTTP.
//
// This is what makes lazy pull possible on the ublk route: bean reads the layer itself
// there, so it needs the range client the overlaybd daemon already had. It reuses
// Registry.do, which carries the token exchange and the per-host credentials -- the
// alternative was a second HTTP path with its own idea of authentication, and two of those
// diverge.
type registryRangeFetcher struct {
	reg    *Registry
	ref    Reference
	digest string

	sizeOnce sync.Once
	size     int64
	sizeErr  error
}

func newRegistryRangeFetcher(reg *Registry, ref Reference, digest string, size int64) *registryRangeFetcher {
	f := &registryRangeFetcher{reg: reg, ref: ref, digest: digest}
	// A manifest already carries every layer's size, so the usual case needs no HEAD at
	// all. Discovering it costs a round trip before the first read.
	if size > 0 {
		f.size = size
		f.sizeOnce.Do(func() {})
	}
	return f
}

// CacheKey identifies this blob for the chunk cache.
//
// The digest alone, not the host or repository: a blob is content-addressed, so the same
// digest is the same bytes wherever it came from, and keying by location would hold one
// copy per mirror of an identical layer.
func (f *registryRangeFetcher) CacheKey() string { return f.digest }

func (f *registryRangeFetcher) Size(ctx context.Context) (int64, error) {
	f.sizeOnce.Do(func() {
		f.size, f.sizeErr = f.headSize(ctx)
	})
	return f.size, f.sizeErr
}

func (f *registryRangeFetcher) headSize(ctx context.Context) (int64, error) {
	u := fmt.Sprintf("https://%s/v2/%s/blobs/%s", f.ref.Host, f.ref.Repository, f.digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := f.reg.do(ctx, req, f.ref)
	if err != nil {
		return 0, err
	}
	defer closeBody(resp)
	if resp.StatusCode != http.StatusOK {
		return 0, statusError(resp, "head blob "+f.digest)
	}
	if resp.ContentLength < 0 {
		return 0, fmt.Errorf("image: %s did not report a length for blob %s",
			f.ref.Host, f.digest)
	}
	return resp.ContentLength, nil
}

// FetchRange fills p with the blob's bytes starting at off.
func (f *registryRangeFetcher) FetchRange(ctx context.Context, p []byte, off int64) error {
	if len(p) == 0 {
		return nil
	}
	u := fmt.Sprintf("https://%s/v2/%s/blobs/%s", f.ref.Host, f.ref.Repository, f.digest)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	// Closed-ended, unlike the resume path's open-ended "bytes=N-": this asks for a
	// bounded chunk, and an open range would stream the rest of a layer that may be
	// gigabytes for want of a few kilobytes.
	end := off + int64(len(p)) - 1
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, end))

	resp, err := f.reg.do(ctx, req, f.ref)
	if err != nil {
		return err
	}
	defer closeBody(resp)

	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK:
		// The registry ignored the Range header and is sending the whole blob from byte
		// zero. Reading len(p) bytes off it would return the *start* of the layer for a
		// request about its middle -- the right length from the wrong place, which no
		// checksum downstream would catch until the guest saw a corrupt filesystem.
		//
		// Refused rather than worked around by reading and discarding the prefix: that
		// turns every read into a full-blob transfer, which is the opposite of lazy pull
		// and would look like a mysterious slowness rather than a missing feature.
		return fmt.Errorf("image: %s ignored the Range header for blob %s, so it cannot "+
			"serve a layer lazily; prewarm this image on this node instead",
			f.ref.Host, f.digest)
	default:
		return statusError(resp, fmt.Sprintf("range %d-%d of blob %s", off, end, f.digest))
	}

	// ReadFull, so a truncated response is an error rather than a short read the caller
	// mistakes for data.
	if _, err := io.ReadFull(resp.Body, p); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("image: %s returned fewer bytes than the requested range "+
				"%d-%d of blob %s", f.ref.Host, off, end, f.digest)
		}
		return err
	}
	return nil
}

package image

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
)

// rangeFetcher fetches a byte range of one blob.
//
// An interface so the layer readers can be driven by a registry, an object store, or a
// test, without any of them appearing in the format code. It is the seam that makes lazy
// pull possible at all: LSMT and ZFile already read through io.ReaderAt, so the only thing
// standing between them and a remote layer is something that turns "bytes X..Y" into a
// request.
//
// agentenv reaches the same shape with a VirtualFile trait and a RegistryFileV2 backend
// that formats `bytes={offset}-{end}`; io.ReaderAt is Go's version of the same idea, so
// this stays deliberately close to it.
type rangeFetcher interface {
	// FetchRange returns exactly the bytes in [off, off+len(p)), or an error. A short
	// read is an error rather than a partial success: the caller is a filesystem, and a
	// block it believes it read is worse than one it knows it did not.
	FetchRange(ctx context.Context, p []byte, off int64) error
	// Size is the blob's total length.
	Size(ctx context.Context) (int64, error)
}

// remoteBlobReader presents a remote blob as an io.ReaderAt.
//
// Reads are served in fixed-size chunks through a cache rather than issued verbatim.
// A guest read is often a few hundred bytes and an HTTP round trip is milliseconds, so
// forwarding each one would make a boot thousands of round trips. Chunking turns the
// access pattern a filesystem actually has -- many small reads clustered in a few places
// -- into a handful of requests.
type remoteBlobReader struct {
	fetch rangeFetcher
	ctx   context.Context

	// chunkSize is the granularity of a fetch and of the cache. 1 MiB: large enough that
	// a boot's reads coalesce into few requests, small enough that touching one file does
	// not pull megabytes that are never read. It is independent of ZFile's block size --
	// a chunk holds many blocks, which is the point.
	chunkSize int64

	sizeOnce sync.Once
	size     int64
	sizeErr  error

	cache *chunkCache
}

const defaultRemoteChunkSize = 1 << 20

func newRemoteBlobReader(ctx context.Context, fetch rangeFetcher, cache *chunkCache) *remoteBlobReader {
	return &remoteBlobReader{
		fetch:     fetch,
		ctx:       ctx,
		chunkSize: defaultRemoteChunkSize,
		cache:     cache,
	}
}

func (r *remoteBlobReader) blobSize() (int64, error) {
	r.sizeOnce.Do(func() {
		r.size, r.sizeErr = r.fetch.Size(r.ctx)
	})
	return r.size, r.sizeErr
}

// ReadAt fills p from the blob, fetching and caching whole chunks.
func (r *remoteBlobReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("image: remote blob read at a negative offset")
	}
	if len(p) == 0 {
		return 0, nil
	}
	size, err := r.blobSize()
	if err != nil {
		return 0, err
	}
	if off >= size {
		return 0, io.EOF
	}
	// Clipped rather than refused, so a caller reading the tail of a blob does not have
	// to know its length first.
	if rem := size - off; int64(len(p)) > rem {
		p = p[:rem]
	}

	done := 0
	for done < len(p) {
		pos := off + int64(done)
		idx := pos / r.chunkSize
		chunk, cerr := r.chunk(idx, size)
		if cerr != nil {
			return done, cerr
		}
		inChunk := int(pos - idx*r.chunkSize)
		if inChunk >= len(chunk) {
			return done, fmt.Errorf("image: remote chunk %d is %d bytes, short of offset %d",
				idx, len(chunk), inChunk)
		}
		done += copy(p[done:], chunk[inChunk:])
	}
	return done, nil
}

// chunk returns one cached chunk, fetching it if absent.
func (r *remoteBlobReader) chunk(idx, size int64) ([]byte, error) {
	if b, ok := r.cache.get(r.fetch, idx); ok {
		return b, nil
	}

	start := idx * r.chunkSize
	end := start + r.chunkSize
	if end > size {
		end = size
	}
	buf := make([]byte, end-start)
	if err := r.fetch.FetchRange(r.ctx, buf, start); err != nil {
		return nil, fmt.Errorf("image: fetch bytes %d-%d: %w", start, end-1, err)
	}
	r.cache.put(r.fetch, idx, buf)
	return buf, nil
}

// chunkCache holds fetched chunks, bounded by total bytes.
//
// Bounded and shared across layers rather than per-reader: a node serving many sandboxes
// from one image reads the same chunks of the same layers, so a per-reader cache would
// hold N copies and evict them independently. The bound is what keeps a burst of creates
// from turning the cache into the node's memory problem.
//
// Eviction is by insertion order, not by recency. A layer's hot chunks are its
// superblock, its inode tables and its metadata blocks, all of which are read early and
// then repeatedly; insertion order keeps them and discards the sequential scan that
// happened to run through afterwards. Tracking recency would need a touch on every read,
// which is the hot path.
type chunkCache struct {
	mu      sync.Mutex
	maxSize int64
	size    int64
	entries map[string][]byte
	order   []string
}

func newChunkCache(maxBytes int64) *chunkCache {
	if maxBytes <= 0 {
		maxBytes = 256 << 20
	}
	return &chunkCache{maxSize: maxBytes, entries: map[string][]byte{}}
}

// keyer lets a fetcher name itself for cache keys, so two blobs never share an entry.
type keyer interface{ CacheKey() string }

func cacheKey(f rangeFetcher, idx int64) string {
	if k, ok := f.(keyer); ok {
		return fmt.Sprintf("%s#%d", k.CacheKey(), idx)
	}
	// Without a stable identity a fetcher gets no caching rather than a shared one: an
	// address-derived key would collide after a GC cycle reuses the address, and serving
	// one layer's bytes for another is the exact failure this codebase already paid for
	// once with tcmu serials.
	return ""
}

func (c *chunkCache) get(f rangeFetcher, idx int64) ([]byte, bool) {
	key := cacheKey(f, idx)
	if key == "" {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.entries[key]
	return b, ok
}

func (c *chunkCache) put(f rangeFetcher, idx int64, b []byte) {
	key := cacheKey(f, idx)
	if key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[key]; exists {
		return
	}
	for c.size+int64(len(b)) > c.maxSize && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		c.size -= int64(len(c.entries[oldest]))
		delete(c.entries, oldest)
	}
	// A chunk larger than the whole budget is not cached rather than evicting everything
	// to hold one entry.
	if int64(len(b)) > c.maxSize {
		return
	}
	c.entries[key] = b
	c.order = append(c.order, key)
	c.size += int64(len(b))
}

// stats reports the cache's occupancy, for the metric that says whether it is working.
func (c *chunkCache) stats() (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries), c.size
}

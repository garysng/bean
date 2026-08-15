package image

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// fakeFetcher serves a byte slice and counts what was asked of it.
type fakeFetcher struct {
	data []byte
	key  string

	mu       sync.Mutex
	requests []string // "off-end" per call, so coalescing is visible
	bytes    int64
}

func (f *fakeFetcher) CacheKey() string { return f.key }

func (f *fakeFetcher) Size(context.Context) (int64, error) { return int64(len(f.data)), nil }

func (f *fakeFetcher) FetchRange(_ context.Context, p []byte, off int64) error {
	if off < 0 || off+int64(len(p)) > int64(len(f.data)) {
		return fmt.Errorf("range %d-%d outside the %d-byte blob",
			off, off+int64(len(p))-1, len(f.data))
	}
	f.mu.Lock()
	f.requests = append(f.requests, fmt.Sprintf("%d-%d", off, off+int64(len(p))-1))
	f.bytes += int64(len(p))
	f.mu.Unlock()
	copy(p, f.data[off:])
	return nil
}

func (f *fakeFetcher) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

// patterned returns n bytes whose value depends on position, so a read served from the
// wrong offset produces visibly wrong content rather than plausible bytes.
func patterned(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

func TestRemoteBlobReaderServesExactBytes(t *testing.T) {
	data := patterned(5 << 20) // 5 MiB, so several chunks
	f := &fakeFetcher{data: data, key: "sha256:test"}
	r := newRemoteBlobReader(context.Background(), f, newChunkCache(64<<20))

	for _, tc := range []struct{ off, n int }{
		{0, 10},                           // start
		{1, 4095},                         // unaligned, inside one chunk
		{defaultRemoteChunkSize - 5, 10},  // straddling a chunk boundary
		{defaultRemoteChunkSize * 2, 100}, // exactly on a boundary
		{len(data) - 10, 10},              // the very end
		{3 << 20, 1 << 20},                // a whole chunk from the middle
	} {
		got := make([]byte, tc.n)
		n, err := r.ReadAt(got, int64(tc.off))
		if err != nil {
			t.Errorf("read %d at %d: %v", tc.n, tc.off, err)
			continue
		}
		want := data[tc.off : tc.off+tc.n]
		if n != tc.n || !bytes.Equal(got[:n], want) {
			t.Errorf("read %d bytes at %d: content differs", tc.n, tc.off)
		}
	}
}

// A read past the end reports EOF, and one that runs off the end is clipped.
func TestRemoteBlobReaderEdges(t *testing.T) {
	data := patterned(1000)
	f := &fakeFetcher{data: data, key: "k"}
	r := newRemoteBlobReader(context.Background(), f, newChunkCache(1<<20))

	if _, err := r.ReadAt(make([]byte, 8), 1000); err == nil {
		t.Error("a read starting at the end did not report EOF")
	}
	got := make([]byte, 50)
	n, err := r.ReadAt(got, 980)
	if err != nil {
		t.Fatalf("clipped read: %v", err)
	}
	if n != 20 || !bytes.Equal(got[:n], data[980:]) {
		t.Errorf("clipped read returned %d bytes, want 20 with the tail's content", n)
	}
	if _, err := r.ReadAt(make([]byte, 8), -1); err == nil {
		t.Error("a negative offset was accepted")
	}
}

// Many small reads inside one chunk cost one request.
//
// This is the property lazy pull depends on. A filesystem boot issues hundreds of small
// reads clustered in a few places; forwarding each one verbatim would be hundreds of
// round trips, which is slower than downloading the layer and would make the feature
// pointless.
func TestRemoteBlobReaderCoalescesSmallReads(t *testing.T) {
	data := patterned(4 << 20)
	f := &fakeFetcher{data: data, key: "k"}
	r := newRemoteBlobReader(context.Background(), f, newChunkCache(64<<20))

	// 200 reads of 512 bytes, all within the first chunk.
	for i := 0; i < 200; i++ {
		got := make([]byte, 512)
		if _, err := r.ReadAt(got, int64(i*512)); err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if !bytes.Equal(got, data[i*512:i*512+512]) {
			t.Fatalf("read %d returned the wrong bytes", i)
		}
	}
	if calls := f.calls(); calls != 1 {
		t.Errorf("200 reads inside one chunk cost %d requests, want 1: without coalescing "+
			"a boot is thousands of round trips", calls)
	}
}

// A chunk is fetched once even when read repeatedly.
func TestRemoteBlobReaderCachesChunks(t *testing.T) {
	data := patterned(3 << 20)
	f := &fakeFetcher{data: data, key: "k"}
	r := newRemoteBlobReader(context.Background(), f, newChunkCache(64<<20))

	for i := 0; i < 5; i++ {
		if _, err := r.ReadAt(make([]byte, 100), 1<<20); err != nil {
			t.Fatal(err)
		}
	}
	if calls := f.calls(); calls != 1 {
		t.Errorf("the same chunk was fetched %d times, want 1", calls)
	}
}

// Two blobs never share a cache entry.
//
// Sharing one would serve one layer's bytes for another, which is the failure this
// codebase already paid for once through tcmu serials that collided.
func TestChunkCacheKeysByBlob(t *testing.T) {
	cache := newChunkCache(64 << 20)
	a := &fakeFetcher{data: bytes.Repeat([]byte{'A'}, 4096), key: "sha256:aaa"}
	b := &fakeFetcher{data: bytes.Repeat([]byte{'B'}, 4096), key: "sha256:bbb"}

	ra := newRemoteBlobReader(context.Background(), a, cache)
	rb := newRemoteBlobReader(context.Background(), b, cache)

	gotA := make([]byte, 16)
	if _, err := ra.ReadAt(gotA, 0); err != nil {
		t.Fatal(err)
	}
	gotB := make([]byte, 16)
	if _, err := rb.ReadAt(gotB, 0); err != nil {
		t.Fatal(err)
	}
	if gotA[0] != 'A' || gotB[0] != 'B' {
		t.Errorf("blob A read %q and blob B read %q: the two share a cache entry",
			gotA[0], gotB[0])
	}
}

// A fetcher with no stable identity gets no caching rather than a shared entry.
func TestChunkCacheRefusesAnonymousFetchers(t *testing.T) {
	cache := newChunkCache(64 << 20)
	// A bare rangeFetcher with no CacheKey method.
	var f rangeFetcher = anonFetcher{data: patterned(4096)}
	if _, ok := cache.get(f, 0); ok {
		t.Error("an unkeyed fetcher got a cache hit")
	}
	cache.put(f, 0, []byte("x"))
	if n, _ := cache.stats(); n != 0 {
		t.Errorf("an unkeyed fetcher inserted %d entries, want 0: an address-derived key "+
			"collides after a GC cycle reuses the address", n)
	}
}

type anonFetcher struct{ data []byte }

func (a anonFetcher) Size(context.Context) (int64, error) { return int64(len(a.data)), nil }
func (a anonFetcher) FetchRange(_ context.Context, p []byte, off int64) error {
	copy(p, a.data[off:])
	return nil
}

// The cache stays within its budget.
func TestChunkCacheEvictsToStayInBudget(t *testing.T) {
	const budget = 4 << 20
	cache := newChunkCache(budget)
	data := patterned(16 << 20)
	f := &fakeFetcher{data: data, key: "k"}
	r := newRemoteBlobReader(context.Background(), f, cache)

	// Walk 16 MiB through a 4 MiB cache.
	for off := 0; off < len(data); off += defaultRemoteChunkSize {
		if _, err := r.ReadAt(make([]byte, 64), int64(off)); err != nil {
			t.Fatal(err)
		}
	}
	_, held := cache.stats()
	if held > budget {
		t.Errorf("cache holds %d bytes, past its %d budget", held, budget)
	}
	if held == 0 {
		t.Error("cache holds nothing, so it is evicting everything it inserts")
	}
}

// A chunk larger than the whole budget is not cached, rather than emptying the cache.
func TestChunkCacheSkipsOversizedEntries(t *testing.T) {
	cache := newChunkCache(1024)
	f := &fakeFetcher{data: patterned(4096), key: "k"}
	cache.put(f, 0, make([]byte, 4096))
	if n, b := cache.stats(); n != 0 || b != 0 {
		t.Errorf("an oversized chunk was cached (%d entries, %d bytes)", n, b)
	}
}

// A fetch failure surfaces rather than becoming zeros.
func TestRemoteBlobReaderPropagatesFetchErrors(t *testing.T) {
	f := &failingFetcher{size: 4096}
	r := newRemoteBlobReader(context.Background(), f, newChunkCache(1<<20))
	if _, err := r.ReadAt(make([]byte, 16), 0); err == nil {
		t.Error("a failed fetch was reported as a successful read, so the caller would " +
			"see zeros as data")
	}
}

type failingFetcher struct{ size int64 }

func (f *failingFetcher) CacheKey() string                    { return "failing" }
func (f *failingFetcher) Size(context.Context) (int64, error) { return f.size, nil }
func (f *failingFetcher) FetchRange(context.Context, []byte, int64) error {
	return errors.New("network is down")
}

// A reader keeps working after the context that built it is cancelled.
//
// The device serves IO for the sandbox's whole life, so a reader holding the create's request
// context dies the moment that create returns. Measured on hardware, and the symptom named
// nothing useful: the guest booted, mounted its root, started its agent, and then every
// subsequent read failed with `context canceled` -- which the queue turns into EIO and ext4
// reports as `EXT4-fs error: reading directory`. Reads issued during the create succeeded,
// which made it look like a corrupt region of the layer rather than a lifetime mistake.
func TestRemoteBlobReaderOutlivesItsCreateContext(t *testing.T) {
	data := patterned(4 << 20)
	f := &fakeCtxFetcher{data: data}

	ctx, cancel := context.WithCancel(context.Background())
	r := newRemoteBlobReader(context.WithoutCancel(ctx), f, newChunkCache(16<<20))

	// One read while the create is still in flight, as a boot's first reads are.
	if _, err := r.ReadAt(make([]byte, 512), 0); err != nil {
		t.Fatalf("read during the create: %v", err)
	}

	// The create returns; its context is cancelled.
	cancel()

	// Every later read must still work: a different chunk, so it cannot be served from cache.
	got := make([]byte, 512)
	n, err := r.ReadAt(got, 2<<20)
	if err != nil {
		t.Fatalf("read after the create's context was cancelled: %v -- the device would "+
			"return EIO for the rest of the sandbox's life", err)
	}
	if !bytes.Equal(got[:n], data[2<<20:(2<<20)+512]) {
		t.Error("the read after cancellation returned the wrong bytes")
	}
}

// fakeCtxFetcher honours its context, as a real HTTP client does.
type fakeCtxFetcher struct{ data []byte }

func (f *fakeCtxFetcher) CacheKey() string { return "ctx" }

func (f *fakeCtxFetcher) Size(context.Context) (int64, error) { return int64(len(f.data)), nil }

func (f *fakeCtxFetcher) FetchRange(ctx context.Context, p []byte, off int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	copy(p, f.data[off:])
	return nil
}

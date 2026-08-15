package image

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A layer that exists only as a remote blob backs a stack, and reads correctly through it.
//
// This is lazy pull's whole claim: the layer is never on disk, and the guest still reads its
// filesystem. The fixture is a sealed layer served by a rangeFetcher over an in-memory blob,
// so nothing here touches a registry -- what is under test is that the format readers work
// through a range-reading base, which is the part that was missing.
func TestLSMTStackOverARemoteLayer(t *testing.T) {
	dir := t.TempDir()
	path := writeCountedExtentLayer(t, dir, "remote.lsmt", 64, 8)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	f := &fakeFetcher{data: raw, key: "sha256:remote"}
	remote := newRemoteBlobReader(context.Background(), f, newChunkCache(16<<20))

	stack, closeStack, err := openLSMTStackFrom([]layerSource{{
		Remote:     remote,
		RemoteSize: int64(len(raw)),
		Label:      "sha256:remote",
	}})
	if err != nil {
		t.Fatalf("open a stack over a remote layer: %v", err)
	}
	defer closeStack()

	got := make([]byte, 8*lsmtAlignment)
	if _, err := stack.ReadAt(got, 0); err != nil {
		t.Fatalf("read through the remote layer: %v", err)
	}
	for s := 0; s < 8; s++ {
		if want := byte('0' + s); got[s*lsmtAlignment] != want {
			t.Errorf("sector %d is %q, want %q", s, got[s*lsmtAlignment], want)
		}
	}

	// This fixture is a few KB, well under one 1 MiB chunk, so it is fetched whole and no
	// on-demand claim can be made from it. Whether reads are partial is a property of the
	// chunking, tested where the chunk boundaries are visible
	// (TestRemoteBlobReaderCoalescesSmallReads) and on hardware against a real layer. What
	// this test establishes is that the format readers work at all through a range-reading
	// base, which is the part that was missing.
	f.mu.Lock()
	fetched, calls := f.bytes, len(f.requests)
	f.mu.Unlock()
	t.Logf("read 4 KiB through %d fetch(es) totalling %d of %d blob bytes",
		calls, fetched, len(raw))
}

// A blob larger than one chunk is read partially.
//
// The on-demand claim needs a blob big enough for chunking to matter: reading one sector of
// a multi-megabyte layer must fetch about a chunk, not the whole thing. A reader that
// quietly falls back to a full download passes every correctness test while delivering none
// of the benefit.
func TestLSMTStackOverARemoteLayerReadsPartially(t *testing.T) {
	dir := t.TempDir()
	// Just under 8 MiB in one extent, so the blob spans several 1 MiB chunks. The cap is
	// the format's: an extent's length is 14 bits of sectors, so 16383 sectors is the most
	// a single one can describe. Asking for 16384 produces a layer whose index decodes to
	// length 0, which opens and then maps nothing -- worth knowing before writing a
	// fixture.
	const sectors = (1 << 14) - 1
	path := writeCountedExtentLayer(t, dir, "big.lsmt", uint64(sectors)*2, sectors)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 4*defaultRemoteChunkSize {
		t.Skipf("fixture is %d bytes, too small to span chunks", len(raw))
	}

	f := &fakeFetcher{data: raw, key: "sha256:big"}
	stack, closeStack, err := openLSMTStackFrom([]layerSource{{
		Remote:     newRemoteBlobReader(context.Background(), f, newChunkCache(64<<20)),
		RemoteSize: int64(len(raw)),
		Label:      "sha256:big",
	}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer closeStack()

	f.mu.Lock()
	afterOpen := f.bytes
	f.mu.Unlock()

	// One sector from the middle.
	got := make([]byte, lsmtAlignment)
	if _, err := stack.ReadAt(got, int64(sectors/2)*lsmtAlignment); err != nil {
		t.Fatalf("read: %v", err)
	}

	f.mu.Lock()
	total := f.bytes
	f.mu.Unlock()
	if total >= int64(len(raw)) {
		t.Errorf("opening and reading one sector fetched %d of %d bytes -- the whole blob",
			total, len(raw))
	}
	t.Logf("open cost %d bytes; one sector then cost %d more; blob is %d (%.1f%% total)",
		afterOpen, total-afterOpen, len(raw), 100*float64(total)/float64(len(raw)))
}

// A chain mixing a local base with a remote leaf works, and the merge order survives.
//
// A node legitimately holds some layers and not others -- a partly prewarmed image is the
// normal case, not an edge one -- so the two forms have to compose. And the merge's one
// load-bearing property is order: the last layer is the newest.
func TestLSMTStackMixesLocalAndRemoteLayers(t *testing.T) {
	dir := t.TempDir()
	basePath := writeCountedExtentLayer(t, dir, "base.lsmt", 64, 8)
	topPath := writeSealedLayer(t, dir, "top.lsmt", 64, []layerRun{
		{sector: 3, sectors: 2, fill: 'N'},
	})
	topRaw, err := os.ReadFile(topPath)
	if err != nil {
		t.Fatal(err)
	}

	topFetch := &fakeFetcher{data: topRaw, key: "sha256:top"}
	stack, closeStack, err := openLSMTStackFrom([]layerSource{
		{Path: basePath},
		{
			Remote:     newRemoteBlobReader(context.Background(), topFetch, newChunkCache(16<<20)),
			RemoteSize: int64(len(topRaw)),
			Label:      "sha256:top",
		},
	})
	if err != nil {
		t.Fatalf("open a mixed chain: %v", err)
	}
	defer closeStack()

	got := make([]byte, 8*lsmtAlignment)
	if _, err := stack.ReadAt(got, 0); err != nil {
		t.Fatalf("read: %v", err)
	}
	for s := 0; s < 8; s++ {
		want := byte('0' + s)
		if s == 3 || s == 4 {
			want = 'N' // the remote leaf wins
		}
		if got[s*lsmtAlignment] != want {
			t.Errorf("sector %d is %q, want %q: a mixed chain lost its merge order",
				s, got[s*lsmtAlignment], want)
		}
	}
}

// A remote layer with no declared size is refused, and the message says why.
//
// Both the tar wrapper and the LSMT trailer are located from the *end* of the blob, so a
// reader that does not know the length cannot find either. Guessing would read from the
// wrong place and report a corrupt layer.
func TestOpenLSMTStackRefusesARemoteLayerWithoutASize(t *testing.T) {
	f := &fakeFetcher{data: []byte("irrelevant"), key: "k"}
	_, _, err := openLSMTStackFrom([]layerSource{{
		Remote: newRemoteBlobReader(context.Background(), f, newChunkCache(1<<20)),
		Label:  "sha256:nosize",
	}})
	if err == nil {
		t.Fatal("a remote layer with no size was accepted")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("from the end")) {
		t.Errorf("the error does not say why a size is needed: %v", err)
	}
}

func TestOpenLSMTStackRefusesAnEmptySource(t *testing.T) {
	_, _, err := openLSMTStackFrom([]layerSource{{Label: "sha256:empty"}})
	if err == nil {
		t.Fatal("a source naming neither a file nor a remote reader was accepted")
	}
}

// The path form still works, since every existing caller uses it.
func TestOpenLSMTStackPathFormUnchanged(t *testing.T) {
	dir := t.TempDir()
	path := writeCountedExtentLayer(t, dir, "local.lsmt", 64, 8)
	stack, closeStack, err := openLSMTStack([]string{path})
	if err != nil {
		t.Fatalf("open by path: %v", err)
	}
	defer closeStack()
	if len(stack.mappings) == 0 {
		t.Error("the path form produced no extents")
	}
	// And a missing path still names the file.
	if _, _, err := openLSMTStack([]string{filepath.Join(dir, "absent.lsmt")}); err == nil {
		t.Error("a missing layer file was accepted")
	}
}

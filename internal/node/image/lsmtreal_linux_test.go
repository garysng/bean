//go:build linux

package image

import (
	"bytes"
	"context"
	"os"
	"testing"
)

// The reader opens a layer that upstream's own `overlaybd-commit` produced.
//
// Every other test in this package builds its input, which cannot catch the test's idea
// of the format and the reader's idea of it being wrong in the same way. This one reads
// bytes this codebase had no hand in writing, which is the only way that class of error
// shows up.
//
// Gated on a path rather than skipped silently: producing the fixture needs the overlaybd
// binaries and root, so it cannot run in CI, and a test that quietly skips everywhere is
// indistinguishable from one that does not exist. Produce it with
// `hack/obd-seal-a-layer.sh` and point BEAN_SEALED_LAYER at the result.
func TestOpenRealSealedLayer(t *testing.T) {
	path := os.Getenv("BEAN_SEALED_LAYER")
	if path == "" {
		t.Skip("set BEAN_SEALED_LAYER to a layer sealed by overlaybd-commit " +
			"(hack/obd-seal-a-layer.sh produces one)")
	}

	// Through openLSMTStack, which is the function a create calls -- not through the
	// unwrapping helpers one at a time.
	//
	// The first version of this test called openSealedLayerPayload, openZFile and
	// openLSMTLayer itself, and it passed while a real create still failed: production
	// opened the layer file directly and never unwrapped the tar. A test that assembles
	// the pipeline itself verifies the pieces and not the wiring, which is the more likely
	// thing to be wrong.
	stack, closeStack, err := openLSMTStack([]string{path})
	if err != nil {
		t.Fatalf("open a real sealed layer through the production path: %v", err)
	}
	defer closeStack()

	t.Logf("stack opened: virtual size %d, %d merged extents",
		stack.virtualSize, len(stack.mappings))
	if stack.virtualSize == 0 {
		t.Error("the layer reports a zero virtual size, so nothing could be served from it")
	}
	if len(stack.mappings) == 0 {
		t.Fatal("the layer has no extents, so the fixture holds no content: seal one with " +
			"overlaybd-apply rather than by writing into the data file, which is an extent " +
			"store and does not register bytes poked into it")
	}

	// Opening the index is not the same claim as reading the right bytes through it. A
	// wrong extent offset still opens, and still returns data -- just data from the wrong
	// place, which is the failure mode this format is easiest to get wrong in.
	//
	// The layer holds an ext4 filesystem (overlaybd-create --mkfs), so its superblock is a
	// fixed, known quantity: magic 0xef53 at byte 56 of the superblock, which lives at
	// offset 1024. Anything else means the extents resolve to the wrong physical bytes.
	sb := make([]byte, 1024)
	if _, err := readFullAt(stack, sb, 1024); err != nil {
		t.Fatalf("read the superblock through the layer: %v", err)
	}
	if magic := uint16(sb[56]) | uint16(sb[57])<<8; magic != 0xef53 {
		t.Errorf("ext4 magic through the layer is %#04x, want 0xef53: the extents are "+
			"resolving to the wrong physical offsets", magic)
	} else {
		t.Log("ext4 superblock magic reads correctly through the layer")
	}
}

// The same real sealed layer reads correctly when served over range requests.
//
// This is the claim lazy pull rests on, and the one a create cannot make on its own: a
// create with the layer absent reports RUNNING whether or not the device serves valid bytes,
// because the sandbox exists either way and an unreachable agent looks identical to a broken
// filesystem from outside.
//
// So the same fixture used by TestOpenRealSealedLayer is served through a rangeFetcher
// instead of a file, and the ext4 superblock magic is read through the whole stack. If the
// remote path mangles an offset anywhere -- the tar walk, the chunk arithmetic, a ZFile block
// boundary -- this is where it shows, with no kernel or guest involved.
func TestOpenRealSealedLayerOverRangeRequests(t *testing.T) {
	path := os.Getenv("BEAN_SEALED_LAYER")
	if path == "" {
		t.Skip("set BEAN_SEALED_LAYER to a layer sealed by overlaybd-commit " +
			"(hack/obd-seal-a-layer.sh produces one)")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the sealed layer: %v", err)
	}

	f := &fakeFetcher{data: raw, key: "sha256:real-remote"}
	stack, closeStack, err := openLSMTStackFrom([]layerSource{{
		Remote:     newRemoteBlobReader(context.Background(), f, newChunkCache(64<<20)),
		RemoteSize: int64(len(raw)),
		Label:      "sha256:real-remote",
	}})
	if err != nil {
		t.Fatalf("open a real sealed layer over range requests: %v", err)
	}
	defer closeStack()

	t.Logf("stack opened remotely: virtual size %d, %d merged extents",
		stack.virtualSize, len(stack.mappings))

	sb := make([]byte, 1024)
	if _, err := readFullAt(stack, sb, 1024); err != nil {
		t.Fatalf("read the superblock over range requests: %v", err)
	}
	if magic := uint16(sb[56]) | uint16(sb[57])<<8; magic != 0xef53 {
		t.Errorf("ext4 magic read remotely is %#04x, want 0xef53: the remote path resolves "+
			"to different bytes than the local one", magic)
	} else {
		t.Log("ext4 superblock magic reads correctly through the remote path")
	}

	f.mu.Lock()
	fetched, calls := f.bytes, len(f.requests)
	f.mu.Unlock()
	t.Logf("cost %d fetch(es), %d of %d blob bytes (%.1f%%)",
		calls, fetched, len(raw), 100*float64(fetched)/float64(len(raw)))
}

// readFullAt fills p from a ReaderAt, tolerating short reads.
//
// The layer serves a range at a time and a request can span several extents, so a single
// ReadAt is not guaranteed to fill the buffer.
func readFullAt(r interface {
	ReadAt(p []byte, off int64) (int, error)
}, p []byte, off int64) (int, error) {
	done := 0
	for done < len(p) {
		n, err := r.ReadAt(p[done:], off+int64(done))
		done += n
		if err != nil {
			if done == len(p) {
				return done, nil
			}
			return done, err
		}
		if n == 0 {
			return done, nil
		}
	}
	return done, nil
}

// Local and remote paths agree on every read of a *real* sealed layer.
//
// The synthetic comparison passes because a hand-built layer's ZFile is uncompressed and its
// extents are contiguous. A real layer is neither: blocks are LZ4-compressed with a jump
// table, and the index is sparse. So this is where a discrepancy in the chunking or the block
// boundaries shows, and it is the read pattern that produced
// `EXT4-fs error: __ext4_find_entry: reading directory lblock 0` on hardware.
func TestRealLayerRemoteAndLocalAgree(t *testing.T) {
	path := os.Getenv("BEAN_SEALED_LAYER")
	if path == "" {
		t.Skip("set BEAN_SEALED_LAYER to a layer sealed by overlaybd-commit")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	local, closeLocal, err := openLSMTStack([]string{path})
	if err != nil {
		t.Fatalf("open locally: %v", err)
	}
	defer closeLocal()

	f := &fakeFetcher{data: raw, key: "sha256:real-cmp"}
	remote, closeRemote, err := openLSMTStackFrom([]layerSource{{
		Remote:     newRemoteBlobReader(context.Background(), f, newChunkCache(64<<20)),
		RemoteSize: int64(len(raw)),
		Label:      "sha256:real-cmp",
	}})
	if err != nil {
		t.Fatalf("open remotely: %v", err)
	}
	defer closeRemote()

	mismatches := 0
	for _, size := range []int64{1024, 4096, 65536} {
		for off := int64(0); off < local.virtualSize; off += size * 13 {
			lbuf := make([]byte, size)
			rbuf := make([]byte, size)
			ln, lerr := local.ReadAt(lbuf, off)
			rn, rerr := remote.ReadAt(rbuf, off)

			if (lerr == nil) != (rerr == nil) {
				t.Errorf("read %d at %d: local err=%v, remote err=%v", size, off, lerr, rerr)
				mismatches++
			} else if lerr == nil && (ln != rn || !bytes.Equal(lbuf[:ln], rbuf[:rn])) {
				t.Errorf("read %d at %d differs: %d vs %d bytes", size, off, ln, rn)
				mismatches++
			}
			if mismatches > 5 {
				t.Fatal("too many mismatches; stopping")
			}
		}
	}
	if mismatches == 0 {
		t.Log("every read agrees between the local and remote paths")
	}
}

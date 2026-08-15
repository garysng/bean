package image

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A remote-backed device answers every read a guest can issue, or says which one it cannot.
//
// The bug this exists for: a guest booted from a lazily-pulled layer, mounted its root, ran
// beand, and then failed with `EXT4-fs error (device vdb): __ext4_find_entry: reading
// directory lblock 0` and virtio `I/O error`. The superblock read fine and the device was the
// right size, so the failure was not the format or the geometry -- it was a *particular* read
// returning an error where the local path returns bytes.
//
// The queue turns any error with a short count into EIO, and a filesystem treats EIO on a
// directory block as corruption. So this walks the whole device the way a mount does, through
// both backends, and compares: same offsets, same lengths, both must succeed and agree.
func TestRemoteAndLocalBackendsAgreeOnEveryRead(t *testing.T) {
	dir := t.TempDir()
	path := writeCountedExtentLayer(t, dir, "cmp.lsmt", 128, 64)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	local, closeLocal, err := openLSMTStack([]string{path})
	if err != nil {
		t.Fatalf("open locally: %v", err)
	}
	defer closeLocal()

	f := &fakeFetcher{data: raw, key: "sha256:cmp"}
	remote, closeRemote, err := openLSMTStackFrom([]layerSource{{
		Remote:     newRemoteBlobReader(context.Background(), f, newChunkCache(16<<20)),
		RemoteSize: int64(len(raw)),
		Label:      "sha256:cmp",
	}})
	if err != nil {
		t.Fatalf("open remotely: %v", err)
	}
	defer closeRemote()

	if local.virtualSize != remote.virtualSize {
		t.Fatalf("virtual sizes differ: local %d, remote %d",
			local.virtualSize, remote.virtualSize)
	}

	// The read sizes a filesystem actually issues: a 1 KiB superblock read, 4 KiB blocks,
	// and a 128 KiB readahead. Offsets deliberately include past the end of the layer's
	// extents, which is where a device larger than its image spends most of its space --
	// and where an EOF must become zeros rather than an error.
	for _, size := range []int64{1024, 4096, 131072} {
		for off := int64(0); off < local.virtualSize; off += size * 7 {
			lbuf := make([]byte, size)
			rbuf := make([]byte, size)
			ln, lerr := local.ReadAt(lbuf, off)
			rn, rerr := remote.ReadAt(rbuf, off)

			if (lerr == nil) != (rerr == nil) {
				t.Fatalf("read %d at %d: local err=%v, remote err=%v -- the remote path "+
					"fails a read the local one serves, which the queue turns into EIO and "+
					"a filesystem reads as corruption", size, off, lerr, rerr)
			}
			if lerr != nil {
				continue
			}
			if ln != rn {
				t.Fatalf("read %d at %d returned %d bytes locally and %d remotely",
					size, off, ln, rn)
			}
			if !bytes.Equal(lbuf[:ln], rbuf[:rn]) {
				t.Fatalf("read %d at %d returned different content", size, off)
			}
		}
	}
}

// The same comparison through the copy-on-write backend, which is what the device really is.
//
// lsmtBackend sits between the queue and the stack: it decides per block whether to serve the
// overlay or the layers, and it is the thing whose ReadAt the queue calls. A discrepancy that
// the stack comparison misses can still live here -- for instance in the "past the end of the
// layers" branch, which is reached constantly because the device is ten times the size of the
// image.
func TestRemoteAndLocalCowBackendsAgree(t *testing.T) {
	dir := t.TempDir()
	path := writeCountedExtentLayer(t, dir, "cow.lsmt", 128, 64)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A device far larger than the layer, as a create makes: 2 GiB requested over a 64 KiB
	// image is the ordinary case.
	const devSize = 8 << 20

	lb, err := newLSMTBackend([]string{path}, filepath.Join(dir, "local.img"), devSize)
	if err != nil {
		t.Fatalf("local backend: %v", err)
	}
	defer lb.Close()

	f := &fakeFetcher{data: raw, key: "sha256:cow"}
	stack, closeStack, err := openLSMTStackFrom([]layerSource{{
		Remote:     newRemoteBlobReader(context.Background(), f, newChunkCache(16<<20)),
		RemoteSize: int64(len(raw)),
		Label:      "sha256:cow",
	}})
	if err != nil {
		t.Fatalf("remote stack: %v", err)
	}
	rb, err := newLSMTBackendOverStack(stack, closeStack, filepath.Join(dir, "remote.img"), devSize)
	if err != nil {
		t.Fatalf("remote backend: %v", err)
	}
	defer rb.Close()

	if !rb.MayBlock() {
		t.Error("a backend over a remote stack does not report MayBlock(), so its reads " +
			"would run inline on the queue's single thread")
	}
	if lb.MayBlock() {
		t.Error("a purely local backend reports MayBlock(), so it would pay for a handoff " +
			"it does not need")
	}

	for _, size := range []int64{1024, 4096, 131072} {
		for off := int64(0); off+size <= devSize; off += size * 11 {
			lbuf := make([]byte, size)
			rbuf := make([]byte, size)
			ln, lerr := lb.ReadAt(lbuf, off)
			rn, rerr := rb.ReadAt(rbuf, off)

			if (lerr == nil) != (rerr == nil) {
				t.Fatalf("cow read %d at %d: local err=%v, remote err=%v", size, off, lerr, rerr)
			}
			if lerr != nil {
				continue
			}
			if ln != rn || !bytes.Equal(lbuf[:ln], rbuf[:rn]) {
				t.Fatalf("cow read %d at %d differs: %d vs %d bytes", size, off, ln, rn)
			}
		}
	}
}

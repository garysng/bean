//go:build linux

package image

import (
	"io"
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

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open the sealed layer: %v", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}

	// A layer bean produced is `overlaybd-commit -z -t`: zfile-compressed and wrapped in
	// a tar so it is a valid OCI blob. So the outer container is a tar, and the reader has
	// to find the payload inside it rather than assuming offset 0.
	src, size, err := openSealedLayerPayload(f, st.Size())
	if err != nil {
		t.Fatalf("locate the payload inside the sealed layer: %v", err)
	}

	z, zerr := openZFile(src, size)
	if zerr != nil {
		t.Fatalf("open the zfile inside a real sealed layer: %v", zerr)
	}
	t.Logf("zfile opened: %d bytes uncompressed, block size %d, algo %d",
		z.size(), z.header.blockSize, z.header.algo)

	l, lerr := openLSMTLayer(z, z.size())
	if lerr != nil {
		t.Fatalf("open the lsmt index inside a real sealed layer: %v", lerr)
	}
	t.Logf("lsmt opened: virtual size %d, %d mappings", l.virtualSize, len(l.mappings))
	if l.virtualSize == 0 {
		t.Error("the layer reports a zero virtual size, so nothing could be served from it")
	}
	if len(l.mappings) == 0 {
		t.Fatal("the layer has no extents, so the fixture holds no content: seal one with " +
			"overlaybd-apply rather than by writing into the data file, which is an extent " +
			"store and does not register bytes poked into it")
	}

	// Opening the index is not the same claim as reading the right bytes through it. A
	// wrong extent offset still opens, and still returns data -- just data from the wrong
	// place, which is the failure mode this whole format is easiest to get wrong in.
	//
	// The layer holds an ext4 filesystem (overlaybd-create --mkfs), so its superblock is a
	// fixed, known quantity: magic 0xef53 at byte 56 of the superblock, which lives at
	// offset 1024. Anything other than that means the extents are being resolved to the
	// wrong physical bytes.
	// Read through a one-layer stack rather than the layer directly, because the stack is
	// what actually serves a device: it owns the merge and the sector-to-byte conversion,
	// and reading the layer's index without them would test less than production does.
	stack := &lsmtStack{
		mappings:    mergeLSMTLayers([]*lsmtLayer{l}),
		layers:      []io.ReaderAt{z},
		virtualSize: int64(l.virtualSize),
	}
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

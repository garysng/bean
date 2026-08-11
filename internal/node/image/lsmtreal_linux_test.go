//go:build linux

package image

import (
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

//go:build linux

package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// sparseFile writes a file with data at the given offsets and holes elsewhere.
func sparseFile(t *testing.T, size int64, writes map[int64]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sparse.img")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}
	for off, data := range writes {
		if _, err := f.WriteAt([]byte(data), off); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSparseRoundTripPreservesContent is the correctness property: a restored
// file must read back identically, holes included.
func TestSparseRoundTripPreservesContent(t *testing.T) {
	const size = 8 << 20
	writes := map[int64]string{
		0:          "at-the-start",
		1 << 20:    "in-the-middle",
		size - 100: "near-the-end",
	}
	src := sparseFile(t, size, writes)

	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var buf bytes.Buffer
	dataBytes, err := writeSparse(&buf, f, size)
	if err != nil {
		t.Fatalf("writeSparse: %v", err)
	}

	// The stream must be far smaller than the file: that is the whole point.
	if int64(buf.Len()) >= size {
		t.Errorf("stream is %d bytes for an %d byte file; holes were not skipped",
			buf.Len(), size)
	}
	if dataBytes == 0 {
		t.Error("reported zero data bytes for a file with content")
	}

	dst := filepath.Join(t.TempDir(), "restored.img")
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := readSparse(&buf, out); err != nil {
		t.Fatalf("readSparse: %v", err)
	}

	original, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, restored) {
		t.Error("restored file differs from the original")
	}
}

// TestSparseRestorePreservesHoles checks the restored file stays sparse. Without
// this a restore costs the provisioned size on disk even though the snapshot
// only carried the written blocks.
func TestSparseRestorePreservesHoles(t *testing.T) {
	const size = 64 << 20
	src := sparseFile(t, size, map[int64]string{0: "small"})

	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var buf bytes.Buffer
	if _, err := writeSparse(&buf, f, size); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(t.TempDir(), "restored.img")
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := readSparse(&buf, out); err != nil {
		t.Fatal(err)
	}

	info, err := out.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != size {
		t.Errorf("restored size = %d, want %d", info.Size(), size)
	}
	if allocated := allocatedSize(t, dst); allocated > 4<<20 {
		t.Errorf("restored file allocated %d bytes, expected it to stay sparse", allocated)
	}
}

// TestSparseCostFollowsDataNotSize is the regression this format exists for: a
// 20 GiB store holding a few kilobytes must not produce a 20 GiB stream, which
// previously cost 15 seconds of paused-sandbox time per checkpoint.
func TestSparseCostFollowsDataNotSize(t *testing.T) {
	const size = 20 << 30
	src := sparseFile(t, size, map[int64]string{4096: "a few bytes"})

	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var buf bytes.Buffer
	if _, err := writeSparse(&buf, f, size); err != nil {
		t.Fatalf("writeSparse: %v", err)
	}
	if buf.Len() > 1<<20 {
		t.Errorf("stream is %d bytes for a nearly empty 20 GiB file", buf.Len())
	}
}

func TestSparseHandlesFullyEmptyFile(t *testing.T) {
	const size = 1 << 20
	src := sparseFile(t, size, nil)

	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var buf bytes.Buffer
	dataBytes, err := writeSparse(&buf, f, size)
	if err != nil {
		t.Fatalf("writeSparse: %v", err)
	}
	if dataBytes != 0 {
		t.Errorf("reported %d data bytes for an empty file", dataBytes)
	}

	dst := filepath.Join(t.TempDir(), "restored.img")
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if err := readSparse(&buf, out); err != nil {
		t.Fatalf("readSparse: %v", err)
	}
	info, err := out.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != size {
		t.Errorf("restored size = %d, want %d", info.Size(), size)
	}
}

// TestSparseRejectsCorruptStream matters because a restore writes at the offsets
// the stream names: an out-of-range extent must be refused rather than scribbled
// somewhere unexpected.
func TestSparseRejectsCorruptStream(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "target.img")
	out, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	if err := readSparse(bytes.NewReader([]byte("not a sparse stream at all")), out); err == nil {
		t.Error("readSparse accepted a stream with the wrong magic")
	}
	if err := readSparse(bytes.NewReader(nil), out); err == nil {
		t.Error("readSparse accepted an empty stream")
	}

	// A well-formed header whose extent runs past the logical size.
	var buf bytes.Buffer
	buf.WriteString(sparseMagic)
	writeInt64(&buf, 1024) // logical size
	writeInt64(&buf, 1)    // one extent
	writeInt64(&buf, 512)  // offset
	writeInt64(&buf, 4096) // length, well past the end
	buf.Write(make([]byte, 4096))
	if err := readSparse(&buf, out); err == nil {
		t.Error("readSparse accepted an extent beyond the logical size")
	}
}

func writeInt64(buf *bytes.Buffer, v int64) {
	b := make([]byte, 8)
	for i := 0; i < 8; i++ {
		b[i] = byte(v >> (8 * i))
	}
	buf.Write(b)
}

package snapshot

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteReadRoundTrip(t *testing.T) {
	b, err := NewDirBlobs(filepath.Join(t.TempDir(), "snaps"))
	if err != nil {
		t.Fatal(err)
	}
	w, err := b.Writer("snap_1")
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.Repeat("checkpoint-data", 1000)
	if _, err := io.WriteString(w, payload); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	size, err := b.Size("snap_1")
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", size, len(payload))
	}

	r, err := b.Reader("snap_1")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != payload {
		t.Error("payload mismatch")
	}
}

func TestPartialWriteIsNotReadable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "snaps")
	b, _ := NewDirBlobs(dir)
	w, err := b.Writer("snap_partial")
	if err != nil {
		t.Fatal(err)
	}
	io.WriteString(w, "half-written")
	// Aborting must leave nothing a restore could pick up.
	AbortWrite(b, "snap_partial", w)

	if _, err := b.Reader("snap_partial"); !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("err = %v, want ErrBlobNotFound after abort", err)
	}
	// No leftover temp files either.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "partial") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestReaderMissingBlob(t *testing.T) {
	b, _ := NewDirBlobs(t.TempDir())
	if _, err := b.Reader("snap_nope"); !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("err = %v, want ErrBlobNotFound", err)
	}
	if _, err := b.Size("snap_nope"); !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("size err = %v, want ErrBlobNotFound", err)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	b, _ := NewDirBlobs(t.TempDir())
	w, _ := b.Writer("snap_d")
	io.WriteString(w, "x")
	w.Close()
	if err := b.Delete("snap_d"); err != nil {
		t.Fatal(err)
	}
	// Deleting again is fine, so cleanup paths do not have to check first.
	if err := b.Delete("snap_d"); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

func TestRejectsUnsafeIDs(t *testing.T) {
	b, _ := NewDirBlobs(t.TempDir())
	for _, id := range []string{"", "../escape", "a/b", "with.dot"} {
		if _, err := b.Writer(id); err == nil {
			t.Errorf("Writer(%q) accepted an unsafe id", id)
		}
		if _, err := b.Reader(id); err == nil {
			t.Errorf("Reader(%q) accepted an unsafe id", id)
		}
	}
}

func TestDoubleCloseIsSafe(t *testing.T) {
	b, _ := NewDirBlobs(t.TempDir())
	w, _ := b.Writer("snap_c")
	io.WriteString(w, "x")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	// The blob survives the second close.
	if _, err := b.Size("snap_c"); err != nil {
		t.Errorf("blob lost: %v", err)
	}
}

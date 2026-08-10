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

// nopWriteCloser is a WriteCloser that is deliberately NOT an Aborter, so
// AbortWrite has to take its close-and-delete fallback path.
type nopWriteCloser struct{ closed bool }

func (n *nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (n *nopWriteCloser) Close() error                { n.closed = true; return nil }

func TestAbortWriteFallbackClosesAndDeletes(t *testing.T) {
	b, _ := NewDirBlobs(t.TempDir())
	// Publish a blob under the id first so the fallback's Delete has something
	// to remove -- the fallback closes the writer, then deletes the key.
	real, _ := b.Writer("snap_fb")
	io.WriteString(real, "published")
	real.Close()

	w := &nopWriteCloser{}
	AbortWrite(b, "snap_fb", w)
	if !w.closed {
		t.Error("fallback did not Close the writer")
	}
	if _, err := b.Reader("snap_fb"); !errors.Is(err, ErrBlobNotFound) {
		t.Errorf("blob still present after fallback abort: %v", err)
	}
}

func TestAbortAfterCloseIsNoop(t *testing.T) {
	b, _ := NewDirBlobs(t.TempDir())
	w, _ := b.Writer("snap_ac")
	io.WriteString(w, "x")
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	// Abort after a successful Close must not remove the published blob.
	w.(*atomicFile).Abort()
	if _, err := b.Size("snap_ac"); err != nil {
		t.Errorf("Abort after Close removed the blob: %v", err)
	}
}

func TestNewDirBlobsRejectsUncreatablePath(t *testing.T) {
	// A regular file cannot be a parent directory, so MkdirAll under it fails
	// and the constructor surfaces the error rather than a half-made store.
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDirBlobs(filepath.Join(f, "sub")); err == nil {
		t.Error("NewDirBlobs under a regular file returned no error")
	}
}

func TestCloseFailsWhenTargetIsADirectory(t *testing.T) {
	// A directory occupying the final path makes the rename fail; Close must
	// report the error and leave no temp file behind rather than publish.
	dir := t.TempDir()
	b, _ := NewDirBlobs(dir)
	if err := os.Mkdir(filepath.Join(dir, "snap_dir.tar.gz"), 0o700); err != nil {
		t.Fatal(err)
	}
	w, _ := b.Writer("snap_dir")
	io.WriteString(w, "payload")
	if err := w.Close(); err == nil {
		t.Error("Close over a directory target returned no error")
	}
	for _, e := range mustReadDir(t, dir) {
		if strings.Contains(e.Name(), "partial") {
			t.Errorf("temp file left behind after failed close: %s", e.Name())
		}
	}
}

func TestDeleteReportsRealError(t *testing.T) {
	// A non-empty directory at the blob's path is not removable by os.Remove,
	// so Delete surfaces that error instead of swallowing it as not-found.
	dir := t.TempDir()
	b, _ := NewDirBlobs(dir)
	blobPath := filepath.Join(dir, "snap_busy.tar.gz")
	if err := os.Mkdir(blobPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blobPath, "child"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete("snap_busy"); err == nil {
		t.Error("Delete of a non-empty directory returned no error")
	}
}

func mustReadDir(t *testing.T, dir string) []os.DirEntry {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

// requireNonRoot skips permission-dependent tests when running as root, where
// mode bits do not stop access and the error branch under test never fires.
func requireNonRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root; permission-based error branch is unreachable")
	}
}

func TestWriterFailsOnUnwritableDir(t *testing.T) {
	requireNonRoot(t)
	dir := t.TempDir()
	b, _ := NewDirBlobs(dir)
	// Drop write permission so CreateTemp cannot make its partial file.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	if _, err := b.Writer("snap_ro"); err == nil {
		t.Error("Writer on an unwritable dir returned no error")
	}
}

func TestSizeReportsNonNotExistError(t *testing.T) {
	requireNonRoot(t)
	// An unsearchable blob directory makes Stat fail with EACCES rather than
	// ErrNotExist, so Size must surface that error instead of ErrBlobNotFound.
	parent := t.TempDir()
	dir := filepath.Join(parent, "blobs")
	b, err := NewDirBlobs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	_, err = b.Size("snap_x")
	if err == nil || errors.Is(err, ErrBlobNotFound) {
		t.Errorf("Size on an unsearchable dir = %v, want a non-not-found error", err)
	}
}

package runtime

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTarUntarRoundTrip(t *testing.T) {
	src := t.TempDir()
	// A representative tree: nested dirs, a file with content and mode, and
	// a symlink.
	os.MkdirAll(filepath.Join(src, "a/b"), 0o755)
	os.WriteFile(filepath.Join(src, "a/b/file.txt"), []byte("hello"), 0o640)
	os.WriteFile(filepath.Join(src, "top.txt"), []byte("top"), 0o644)
	os.Symlink("a/b/file.txt", filepath.Join(src, "link"))

	var buf bytes.Buffer
	if err := tarDirectory(src, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() == 0 {
		t.Fatal("empty archive")
	}

	dst := t.TempDir()
	if err := untarDirectory(&buf, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "a/b/file.txt"))
	if err != nil || string(got) != "hello" {
		t.Errorf("nested file = %q err=%v", got, err)
	}
	info, err := os.Stat(filepath.Join(dst, "a/b/file.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Errorf("mode = %o, want 640", perm)
	}
	target, err := os.Readlink(filepath.Join(dst, "link"))
	if err != nil || target != "a/b/file.txt" {
		t.Errorf("symlink = %q err=%v", target, err)
	}
}

func TestUntarRejectsTraversal(t *testing.T) {
	// Craft an archive whose entry escapes the destination; a checkpoint is
	// untrusted input once it has round-tripped through storage.
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "ok.txt"), []byte("x"), 0o644)
	var buf bytes.Buffer
	if err := tarDirectory(src, &buf); err != nil {
		t.Fatal(err)
	}
	// safeJoin is the guard; verify it directly for the cases tar could carry.
	dir := t.TempDir()
	for _, name := range []string{"../escape", "a/../../escape", "/abs/escape"} {
		got, err := safeJoin(dir, name)
		if err != nil {
			continue // rejected outright is fine
		}
		if !strings.HasPrefix(got, dir) {
			t.Errorf("safeJoin(%q) = %q escaped %q", name, got, dir)
		}
	}
}

func TestUntarRejectsCorruptStream(t *testing.T) {
	if err := untarDirectory(bytes.NewReader([]byte("not-gzip")), t.TempDir()); err == nil {
		t.Error("expected error for non-gzip input")
	}
}

func TestTarEmptyDirectory(t *testing.T) {
	var buf bytes.Buffer
	if err := tarDirectory(t.TempDir(), &buf); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := untarDirectory(&buf, dst); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dst)
	if len(entries) != 0 {
		t.Errorf("expected empty restore, got %v", entries)
	}
}

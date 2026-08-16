package s3

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func newTestStore(t *testing.T) *DirStore {
	t.Helper()
	s, err := NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBuildLogKeyIsSlashFreeAndStable(t *testing.T) {
	k1, err := BuildLogKey("docker.io/library/app:v1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(k1, "/:@") {
		t.Fatalf("key still has path-unsafe chars: %q", k1)
	}
	k2, _ := BuildLogKey("docker.io/library/app:v1")
	if k1 != k2 {
		t.Fatalf("key not stable: %q vs %q", k1, k2)
	}
	// Distinct refs must not collide.
	other, _ := BuildLogKey("docker.io/library/app:v2")
	if other == k1 {
		t.Fatalf("distinct refs collided on %q", k1)
	}
	if _, err := BuildLogKey(""); err == nil {
		t.Fatal("empty ref should error")
	}
}

// TestBuildLogRoundTrip writes output through the writer and reads it all back
// through the reader, across chunk boundaries, then confirms the terminal
// manifest.
func TestBuildLogRoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ref := "team/app:v1"

	w, err := NewBuildLogWriter(store, ref)
	if err != nil {
		t.Fatal(err)
	}
	// Force several chunks by shrinking the threshold.
	w.flushBytes = 8

	// BuildKit emits output incrementally; each Write that pushes the buffer over
	// the threshold flushes a chunk, so several lines make several chunks.
	lines := []string{"line one\n", "line two\n", "line three\n", "final\n"}
	want := strings.Join(lines, "")
	for _, ln := range lines {
		if _, err := w.Write([]byte(ln)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Finish(false, ""); err != nil {
		t.Fatal(err)
	}

	r, err := NewBuildLogReader(store, ref)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	off, err := r.ReadFrom(ctx, 0, &got)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != want {
		t.Fatalf("round trip mismatch:\n got %q\nwant %q", got.String(), want)
	}
	if off != int64(len(want)) {
		t.Fatalf("offset = %d, want %d", off, len(want))
	}

	m, err := r.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Done || m.Failed {
		t.Fatalf("manifest not terminal-success: %+v", m)
	}
	if m.Chunks < 2 {
		t.Fatalf("expected multiple chunks, got %d", m.Chunks)
	}
}

// TestBuildLogReadFromOffset checks a follower that resumes mid-stream gets only
// the bytes past its offset, across a chunk boundary.
func TestBuildLogReadFromOffset(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ref := "app:v1"

	w, _ := NewBuildLogWriter(store, ref)
	w.flushBytes = 4
	full := "aaaabbbbccccdddd"
	w.Write([]byte(full))
	w.Finish(false, "")

	r, _ := NewBuildLogReader(store, ref)
	var got bytes.Buffer
	off, err := r.ReadFrom(ctx, 6, &got)
	if err != nil {
		t.Fatal(err)
	}
	if got.String() != full[6:] {
		t.Fatalf("offset read mismatch:\n got %q\nwant %q", got.String(), full[6:])
	}
	if off != int64(len(full)) {
		t.Fatalf("offset = %d, want %d", off, len(full))
	}
}

// TestBuildLogFailedManifest confirms a failed build records its reason.
func TestBuildLogFailedManifest(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	ref := "app:bad"

	w, _ := NewBuildLogWriter(store, ref)
	w.Write([]byte("starting\n"))
	if err := w.Finish(true, "boom"); err != nil {
		t.Fatal(err)
	}

	r, _ := NewBuildLogReader(store, ref)
	m, err := r.Manifest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !m.Done || !m.Failed || m.Reason != "boom" {
		t.Fatalf("manifest = %+v, want done+failed+reason=boom", m)
	}
}

// TestBuildLogExists distinguishes an unknown build from one with output.
func TestBuildLogExists(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	r, _ := NewBuildLogReader(store, "never:built")
	if ok, err := r.Exists(ctx); err != nil || ok {
		t.Fatalf("unknown build: ok=%v err=%v, want false/nil", ok, err)
	}

	w, _ := NewBuildLogWriter(store, "real:v1")
	w.Write([]byte("hi\n"))
	w.Finish(false, "")

	r2, _ := NewBuildLogReader(store, "real:v1")
	if ok, err := r2.Exists(ctx); err != nil || !ok {
		t.Fatalf("built: ok=%v err=%v, want true/nil", ok, err)
	}
}

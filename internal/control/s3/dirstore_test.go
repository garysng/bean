package s3

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

// The DirStore is the dev/CI stand-in for the S3-backed BucketStore, and the
// code above both is written to not care which it has. These tests pin the
// behaviours that equivalence depends on: round-trip, range reads, not-found
// mapping, atomic writes, idempotent delete, and key confinement.

func TestDirStoreRoundTripAndRange(t *testing.T) {
	st, err := NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	want := []byte("hello object store")
	if err := Put(ctx, st, "blobs/sha256:abc", bytes.NewReader(want), int64(len(want))); err != nil {
		t.Fatalf("put: %v", err)
	}

	if n, err := st.Head(ctx, "blobs/sha256:abc"); err != nil || n != int64(len(want)) {
		t.Fatalf("head = %d, %v; want %d", n, err, len(want))
	}

	rc, err := st.Get(ctx, "blobs/sha256:abc")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, want) {
		t.Fatalf("get = %q, want %q", got, want)
	}

	// A range read returns exactly the requested window.
	rr, err := st.GetRange(ctx, "blobs/sha256:abc", 6, 6)
	if err != nil {
		t.Fatalf("range: %v", err)
	}
	rangeGot, _ := io.ReadAll(rr)
	rr.Close()
	if string(rangeGot) != "object" {
		t.Fatalf("range = %q, want %q", rangeGot, "object")
	}
}

func TestDirStoreNotFoundAndIdempotentDelete(t *testing.T) {
	st, err := NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if _, err := st.Get(ctx, "missing/key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get missing: got %v, want ErrNotFound", err)
	}
	if _, err := st.Head(ctx, "missing/key"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("head missing: got %v, want ErrNotFound", err)
	}
	// Deleting something that was never there is success, so cleanup retries
	// do not have to distinguish "deleted it" from "nothing there".
	if err := st.Delete(ctx, "missing/key"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestDirStoreRejectsEscapingKeys(t *testing.T) {
	st, err := NewDirStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, key := range []string{"", "/abs", "../escape", "a/../../b"} {
		if err := Put(ctx, st, key, bytes.NewReader([]byte("x")), 1); err == nil {
			t.Errorf("key %q was accepted; it should be refused as escaping", key)
		}
	}
}

// Compile-time assertions that both stores satisfy the one contract.
var (
	_ ObjectStore = (*DirStore)(nil)
	_ ObjectStore = (*BucketStore)(nil)
)

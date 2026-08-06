package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
)

// These tests run against a real S3-compatible server. They are the only
// check that proves the hand-rolled SigV4 produces signatures a server
// accepts — unit tests can only show the canonicalisation is self-consistent.
//
// Set BEAN_S3_ENDPOINT, BEAN_S3_ACCESS_KEY and BEAN_S3_SECRET_KEY to enable;
// otherwise they skip, so `go test ./...` stays green without infrastructure.
func testClient(t *testing.T) (*Client, string) {
	t.Helper()
	endpoint := os.Getenv("BEAN_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("BEAN_S3_ENDPOINT not set; skipping object-store integration test")
	}
	c, err := New(Config{
		Endpoint:  endpoint,
		Region:    envOrDefault("BEAN_S3_REGION", "us-east-1"),
		AccessKey: os.Getenv("BEAN_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("BEAN_S3_SECRET_KEY"),
		PathStyle: true,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	bucket := envOrDefault("BEAN_S3_TEST_BUCKET", "bean-test")
	if err := c.EnsureBucket(context.Background(), bucket); err != nil {
		t.Fatalf("ensure bucket %s: %v", bucket, err)
	}
	return c, bucket
}

func envOrDefault(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func TestObjectRoundTrip(t *testing.T) {
	c, bucket := testClient(t)
	ctx := context.Background()
	key := "roundtrip/" + t.Name()
	body := []byte("bean snapshot payload")

	if err := c.PutObject(ctx, bucket, key, body); err != nil {
		t.Fatalf("put: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteObject(ctx, bucket, key) })

	r, err := c.GetObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("read back %q, want %q", got, body)
	}

	size, err := c.HeadObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if size != int64(len(body)) {
		t.Errorf("head size = %d, want %d", size, len(body))
	}
}

// TestKeyWithColonRoundTrips signs and reads back a key shaped like an OCI
// digest. The colon has to reach the server as %3A in the canonical request but
// stay a colon in the stored key, since overlaybd asks for the digest verbatim.
// A unit test can only check the canonical form we produce; this checks it is
// the form the server agrees with.
func TestKeyWithColonRoundTrips(t *testing.T) {
	c, bucket := testClient(t)
	ctx := context.Background()
	key := "blobs/sha256:e2f5b9a1c3d47e8f"
	body := []byte("sealed overlaybd layer")

	if err := c.PutObject(ctx, bucket, key, body); err != nil {
		t.Fatalf("put: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteObject(ctx, bucket, key) })

	r, err := c.GetObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("read back %q, want %q", got, body)
	}

	// The key must be listed under its unencoded name, or the digest overlaybd
	// requests would not resolve.
	objs, err := c.ListObjects(ctx, bucket, "blobs/")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, o := range objs {
		if o.Key == key {
			found = true
		}
	}
	if !found {
		t.Errorf("listing does not contain %q: %v", key, objs)
	}
}

// TestGetRangeReadsSlice covers the read pattern lazy block loading depends
// on: fetching an interior slice rather than the whole object.
func TestGetRangeReadsSlice(t *testing.T) {
	c, bucket := testClient(t)
	ctx := context.Background()
	key := "range/" + t.Name()
	body := []byte("0123456789abcdef")

	if err := c.PutObject(ctx, bucket, key, body); err != nil {
		t.Fatalf("put: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteObject(ctx, bucket, key) })

	r, err := c.GetRange(ctx, bucket, key, 4, 6)
	if err != nil {
		t.Fatalf("range get: %v", err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "456789" {
		t.Errorf("range read = %q, want %q", got, "456789")
	}
}

func TestMissingObjectReportsNotFound(t *testing.T) {
	c, bucket := testClient(t)
	ctx := context.Background()

	if _, err := c.GetObject(ctx, bucket, "absent/nothing-here"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get missing = %v, want ErrNotFound", err)
	}
	if _, err := c.HeadObject(ctx, bucket, "absent/nothing-here"); !errors.Is(err, ErrNotFound) {
		t.Errorf("head missing = %v, want ErrNotFound", err)
	}
	// Deleting a missing object must succeed so cleanup paths are idempotent.
	if err := c.DeleteObject(ctx, bucket, "absent/nothing-here"); err != nil {
		t.Errorf("delete missing = %v, want nil", err)
	}
}

// TestMultipartUploadSpansParts writes more than one part so the part
// numbering, ETag collection and completion XML are all exercised. The part
// size is lowered to the 5 MiB minimum S3 allows for non-final parts.
func TestMultipartUploadSpansParts(t *testing.T) {
	c, bucket := testClient(t)
	ctx := context.Background()
	key := "multipart/" + t.Name()

	u, err := c.NewUploader(ctx, bucket, key)
	if err != nil {
		t.Fatalf("new uploader: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteObject(ctx, bucket, key) })
	u.partSize = 5 << 20

	// Two full parts plus a remainder, with a recognisable pattern so a
	// mis-ordered part shows up as a content mismatch rather than a size one.
	want := bytes.Repeat([]byte("bean"), (11<<20)/4)
	if n, err := u.Write(want); err != nil || n != len(want) {
		t.Fatalf("write: n=%d err=%v", n, err)
	}
	if err := u.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	size, err := c.HeadObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if size != int64(len(want)) {
		t.Fatalf("uploaded size = %d, want %d", size, len(want))
	}

	r, err := c.GetObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Error("multipart content differs from what was written")
	}
}

// TestAbortLeavesNoObject is the property snapshot correctness rests on: a
// failed checkpoint must not leave a readable partial blob.
func TestAbortLeavesNoObject(t *testing.T) {
	c, bucket := testClient(t)
	ctx := context.Background()
	key := "multipart/" + t.Name()

	u, err := c.NewUploader(ctx, bucket, key)
	if err != nil {
		t.Fatalf("new uploader: %v", err)
	}
	u.partSize = 5 << 20
	if _, err := u.Write(bytes.Repeat([]byte("x"), 6<<20)); err != nil {
		t.Fatalf("write: %v", err)
	}
	u.Abort()
	// Abort must be idempotent: cleanup paths can reach it more than once.
	u.Abort()

	if _, err := c.HeadObject(ctx, bucket, key); !errors.Is(err, ErrNotFound) {
		_ = c.DeleteObject(ctx, bucket, key)
		t.Errorf("after abort head = %v, want ErrNotFound", err)
	}
}

func TestEmptyUploadCompletesAsEmptyObject(t *testing.T) {
	c, bucket := testClient(t)
	ctx := context.Background()
	key := "multipart/" + t.Name()

	u, err := c.NewUploader(ctx, bucket, key)
	if err != nil {
		t.Fatalf("new uploader: %v", err)
	}
	t.Cleanup(func() { _ = c.DeleteObject(ctx, bucket, key) })
	if err := u.Close(); err != nil {
		t.Fatalf("close empty: %v", err)
	}
	size, err := c.HeadObject(ctx, bucket, key)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if size != 0 {
		t.Errorf("empty object size = %d, want 0", size)
	}
}

// TestListObjectsFiltersByPrefix also covers keys containing characters that
// must survive escaping consistently between the URL and the signature.
func TestListObjectsFiltersByPrefix(t *testing.T) {
	c, bucket := testClient(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("list-%s/", t.Name())
	keys := []string{prefix + "a", prefix + "nested/b", prefix + "with space"}

	for _, k := range keys {
		if err := c.PutObject(ctx, bucket, k, []byte(k)); err != nil {
			t.Fatalf("put %s: %v", k, err)
		}
		t.Cleanup(func() { _ = c.DeleteObject(ctx, bucket, k) })
	}

	got, err := c.ListObjects(ctx, bucket, prefix)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != len(keys) {
		t.Fatalf("listed %d objects, want %d: %+v", len(got), len(keys), got)
	}
	for _, obj := range got {
		if !strings.HasPrefix(obj.Key, prefix) {
			t.Errorf("key %q outside prefix", obj.Key)
		}
	}

	// A key with a space round-trips only if escaping matches the signature.
	r, err := c.GetObject(ctx, bucket, prefix+"with space")
	if err != nil {
		t.Fatalf("get spaced key: %v", err)
	}
	body, _ := io.ReadAll(r)
	r.Close()
	if string(body) != prefix+"with space" {
		t.Errorf("spaced key content = %q", body)
	}
}

package image

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakePutter stands in for the S3 client so the store's own logic is testable without
// a bucket. It records keys, because the key layout is the contract with overlaybd:
// the daemon builds "<repoBlobUrl>/<digest>" itself, so a key that does not match
// that shape produces a device whose reads 404 with the cause only in the daemon's log.
type fakePutter struct {
	objects   map[string][]byte
	buckets   []string
	putErr    error
	bucketErr error
}

func newFakePutter() *fakePutter {
	return &fakePutter{objects: map[string][]byte{}}
}

func (f *fakePutter) HeadObject(_ context.Context, bucket, key string) (int64, error) {
	b, ok := f.objects[bucket+"/"+key]
	if !ok {
		return 0, errors.New("not found")
	}
	return int64(len(b)), nil
}

func (f *fakePutter) PutObject(_ context.Context, bucket, key string, body []byte) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.objects[bucket+"/"+key] = body
	return nil
}

func (f *fakePutter) GetObject(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	b, ok := f.objects[bucket+"/"+key]
	if !ok {
		return nil, errors.New("not found")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakePutter) EnsureBucket(_ context.Context, bucket string) error {
	if f.bucketErr != nil {
		return f.bucketErr
	}
	f.buckets = append(f.buckets, bucket)
	return nil
}

// The URL and key together have to reconstruct exactly what overlaybd requests.
// Measured against MinIO: the daemon GETs "<repoBlobUrl>/<digest>" with the digest
// verbatim, colon included.
func TestBlobStoreURLAndKeyMatchWhatOverlaybdRequests(t *testing.T) {
	f := newFakePutter()
	store, err := NewS3BlobStore(f, "bean-obd", "blobs", "http://127.0.0.1:9000")
	if err != nil {
		t.Fatalf("NewS3BlobStore: %v", err)
	}

	digest := "sha256:5fcaec5cd6819987774cb5390089d7105ea20f0417f1c2421b628f05a9af3ab9"
	body := []byte("sealed layer bytes")
	if err := store.Put(context.Background(), digest, int64(len(body)), bytes.NewReader(body)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// What the daemon would fetch.
	wantURL := "http://127.0.0.1:9000/bean-obd/blobs"
	if got := store.BlobURL(); got != wantURL {
		t.Errorf("BlobURL() = %q, want %q", got, wantURL)
	}
	// What the store actually wrote. These must correspond, or reads 404.
	wantKey := "bean-obd/blobs/" + digest
	if _, ok := f.objects[wantKey]; !ok {
		keys := make([]string, 0, len(f.objects))
		for k := range f.objects {
			keys = append(keys, k)
		}
		t.Errorf("object not at %q; wrote %v", wantKey, keys)
	}

	// The digest keeps its colon: it is what the daemon appends to the URL, so
	// encoding it here would have to be undone there.
	if !strings.Contains(wantKey, "sha256:") {
		t.Error("the digest should be kept verbatim in the key")
	}
}

// Publication is keyed by digest, so a layer two images share is uploaded once. This
// is the same property that makes local conversion cheap on the second image.
//
// The size is asserted alongside presence because a create that decides to read a
// layer remotely references it by length: a right answer with a wrong length produces
// a device that reads past the end.
func TestBlobStoreStatReportsWhatWasPublished(t *testing.T) {
	f := newFakePutter()
	store, _ := NewS3BlobStore(f, "b", "blobs", "http://s3.example")
	ctx := context.Background()

	if _, ok, _ := store.Stat(ctx, "sha256:aaa"); ok {
		t.Error("Stat() reports a layer that was never published")
	}
	if err := store.Put(ctx, "sha256:aaa", 3, bytes.NewReader([]byte("abc"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	size, ok, err := store.Stat(ctx, "sha256:aaa")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !ok {
		t.Error("Stat() reports a layer that was just published as absent")
	}
	if size != 3 {
		t.Errorf("Stat() size = %d, want 3", size)
	}
}

// A zero-length blob cannot be a layer, so it is reported as absent and the caller
// converts. Referencing one instead yields a device whose reads fail with an error
// naming zfile structure, which says nothing about the empty upload behind it.
func TestBlobStoreStatTreatsAnEmptyBlobAsAbsent(t *testing.T) {
	f := newFakePutter()
	store, _ := NewS3BlobStore(f, "b", "blobs", "http://s3.example")
	ctx := context.Background()

	if err := store.Put(ctx, "sha256:empty", 0, bytes.NewReader(nil)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok, _ := store.Stat(ctx, "sha256:empty"); ok {
		t.Error("Stat() reports a zero-length blob as a usable layer")
	}
}

// A truncated upload would sit in the store under a digest claiming otherwise, and
// overlaybd would fail to open it with an error naming zfile structure rather than the
// upload. Caught here instead.
func TestBlobStoreRejectsAShortRead(t *testing.T) {
	f := newFakePutter()
	store, _ := NewS3BlobStore(f, "b", "blobs", "http://s3.example")

	err := store.Put(context.Background(), "sha256:aaa", 100, bytes.NewReader([]byte("short")))
	if err == nil {
		t.Fatal("Put accepted a body shorter than the declared size")
	}
	if !strings.Contains(err.Error(), "expected 100") {
		t.Errorf("error should name the mismatch: %v", err)
	}
	if len(f.objects) != 0 {
		t.Error("nothing should have been written")
	}
}

// The read URL is required rather than derived from the client's endpoint, because the
// daemon resolves it and the two can legitimately differ -- a node may write through an
// internal endpoint while the daemon reads through one the guest network can reach.
func TestBlobStoreRequiresTheReadURL(t *testing.T) {
	if _, err := NewS3BlobStore(newFakePutter(), "b", "blobs", ""); err == nil {
		t.Error("NewS3BlobStore accepted an empty read URL")
	}
	if _, err := NewS3BlobStore(nil, "b", "blobs", "http://s3"); err == nil {
		t.Error("NewS3BlobStore accepted a nil client")
	}
	if _, err := NewS3BlobStore(newFakePutter(), "", "blobs", "http://s3"); err == nil {
		t.Error("NewS3BlobStore accepted an empty bucket")
	}
}

// Trailing and leading slashes are the kind of thing that turns into a double slash in
// a URL the daemon then cannot resolve.
func TestBlobStoreNormalisesSlashes(t *testing.T) {
	f := newFakePutter()
	store, _ := NewS3BlobStore(f, "b", "/blobs/", "http://s3.example/")
	if got, want := store.BlobURL(), "http://s3.example/b/blobs"; got != want {
		t.Errorf("BlobURL() = %q, want %q", got, want)
	}
}

// The daemon reads the store anonymously -- registryfs knows nothing of SigV4. So a
// private bucket, which this process can write perfectly well, answers its reads with
// 403 and the create fails with ENOENT from the kernel. This is the check that turns
// that into something a deployment can act on.
func TestCheckReadableRejectsAStoreThatDemandsCredentials(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
			}))
			defer srv.Close()

			store, err := NewS3BlobStore(newFakePutter(), "bean-obd", "blobs", srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			err = store.CheckReadable(context.Background())
			if err == nil {
				t.Fatalf("CheckReadable accepted a store answering %d", code)
			}
			// The message has to name the fix, because the symptom it prevents names
			// nothing: overlaybd logs "connection failed" and the caller sees ENOENT.
			if !strings.Contains(err.Error(), "anonymous") {
				t.Errorf("error does not explain the anonymous-read requirement: %v", err)
			}
		})
	}
}

// An empty store is the normal state before the first prewarm, so a 404 for the probe
// key is not a failure: the store answered without demanding credentials, which is the
// property being checked.
func TestCheckReadableAcceptsAnEmptyButReadableStore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	store, err := NewS3BlobStore(newFakePutter(), "bean-obd", "blobs", srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CheckReadable(context.Background()); err != nil {
		t.Errorf("CheckReadable rejected a readable but empty store: %v", err)
	}
}

// An unreachable store is reported too, naming the URL: the read URL is configured
// separately from the write endpoint precisely because they can differ, and a typo in it
// otherwise surfaces only as a failed create.
func TestCheckReadableReportsAnUnreachableStore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	store, err := NewS3BlobStore(newFakePutter(), "bean-obd", "blobs", url)
	if err != nil {
		t.Fatal(err)
	}
	err = store.CheckReadable(context.Background())
	if err == nil {
		t.Fatal("CheckReadable accepted an unreachable store")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("error does not say the store was unreachable: %v", err)
	}
}

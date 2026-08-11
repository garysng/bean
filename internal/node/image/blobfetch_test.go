package image

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// rangeServer serves one blob over a range-capable endpoint, and can be told to misbehave
// in the specific ways a real registry does.
type rangeServer struct {
	data []byte
	// ignoreRange answers a Range request with the whole blob and a 200, which is what a
	// registry without range support does.
	ignoreRange bool
	// truncate returns fewer bytes than asked for.
	truncate bool
	requests []string
}

func (s *rangeServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(s.data)))
			w.WriteHeader(http.StatusOK)
			return
		}
		rangeHdr := r.Header.Get("Range")
		s.requests = append(s.requests, rangeHdr)

		if rangeHdr == "" || s.ignoreRange {
			w.Header().Set("Content-Length", strconv.Itoa(len(s.data)))
			w.WriteHeader(http.StatusOK)
			w.Write(s.data)
			return
		}

		var start, end int64
		if _, err := fmt.Sscanf(strings.TrimPrefix(rangeHdr, "bytes="), "%d-%d", &start, &end); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if end >= int64(len(s.data)) {
			end = int64(len(s.data)) - 1
		}
		body := s.data[start : end+1]
		if s.truncate && len(body) > 4 {
			body = body[:len(body)-4]
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(s.data)))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body)
	})
}

// newTestRegistry points a Registry at a local TLS server.
func newTestRegistry(t *testing.T, h http.Handler) (*Registry, Reference) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(nil)
	// The test server's certificate is self-signed, so the client has to accept it. Only
	// the transport is relaxed -- the request path, the auth handling and the range logic
	// under test are the production ones.
	reg.Client = srv.Client()
	if tr, ok := reg.Client.Transport.(*http.Transport); ok {
		tr.TLSClientConfig.InsecureSkipVerify = true
		_ = tls.VersionTLS12
	}
	return reg, Reference{Host: u.Host, Repository: "test/img"}
}

func TestRegistryRangeFetcherReadsExactRanges(t *testing.T) {
	data := patterned(64 << 10)
	s := &rangeServer{data: data}
	reg, ref := newTestRegistry(t, s.handler())

	f := newRegistryRangeFetcher(reg, ref, "sha256:abc", int64(len(data)))
	for _, tc := range []struct{ off, n int }{{0, 16}, {1000, 4096}, {len(data) - 8, 8}} {
		got := make([]byte, tc.n)
		if err := f.FetchRange(context.Background(), got, int64(tc.off)); err != nil {
			t.Errorf("fetch %d at %d: %v", tc.n, tc.off, err)
			continue
		}
		if !bytes.Equal(got, data[tc.off:tc.off+tc.n]) {
			t.Errorf("fetch %d at %d returned the wrong bytes", tc.n, tc.off)
		}
	}
}

// A closed-ended range is requested, not an open one.
//
// An open "bytes=N-" would stream the rest of the layer, which for a gigabyte blob is the
// opposite of lazy pull -- and it would look like unexplained slowness rather than a bug.
func TestRegistryRangeFetcherAsksForABoundedRange(t *testing.T) {
	data := patterned(1 << 20)
	s := &rangeServer{data: data}
	reg, ref := newTestRegistry(t, s.handler())

	f := newRegistryRangeFetcher(reg, ref, "sha256:abc", int64(len(data)))
	if err := f.FetchRange(context.Background(), make([]byte, 4096), 8192); err != nil {
		t.Fatal(err)
	}
	if len(s.requests) == 0 {
		t.Fatal("no request reached the server")
	}
	got := s.requests[len(s.requests)-1]
	if want := "bytes=8192-12287"; got != want {
		t.Errorf("Range header was %q, want %q", got, want)
	}
}

// A registry that ignores Range is refused, not worked around.
//
// It answers 200 with the whole blob from byte zero, so reading len(p) bytes off it returns
// the *start* of the layer for a request about its middle: the right length from the wrong
// place. Nothing downstream would catch that until the guest saw a corrupt filesystem, and
// silently reading-and-discarding the prefix instead would turn every read into a full
// transfer.
func TestRegistryRangeFetcherRefusesARangeIgnoringRegistry(t *testing.T) {
	data := patterned(64 << 10)
	s := &rangeServer{data: data, ignoreRange: true}
	reg, ref := newTestRegistry(t, s.handler())

	f := newRegistryRangeFetcher(reg, ref, "sha256:abc", int64(len(data)))
	err := f.FetchRange(context.Background(), make([]byte, 16), 4096)
	if err == nil {
		t.Fatal("a range-ignoring registry was accepted, so a read of the middle of a " +
			"layer would silently return its beginning")
	}
	if !strings.Contains(err.Error(), "ignored the Range header") {
		t.Errorf("the error does not name the cause, so an operator cannot act on it: %v", err)
	}
	if !strings.Contains(err.Error(), "prewarm") {
		t.Errorf("the error does not say what to do instead: %v", err)
	}
}

// A truncated response is an error, not a short read reported as data.
func TestRegistryRangeFetcherRefusesATruncatedResponse(t *testing.T) {
	data := patterned(64 << 10)
	s := &rangeServer{data: data, truncate: true}
	reg, ref := newTestRegistry(t, s.handler())

	f := newRegistryRangeFetcher(reg, ref, "sha256:abc", int64(len(data)))
	if err := f.FetchRange(context.Background(), make([]byte, 4096), 0); err == nil {
		t.Error("a truncated range response was accepted, so the tail of the buffer would " +
			"be zeros presented as layer content")
	}
}

// The size comes from the manifest when it is known, costing no request.
func TestRegistryRangeFetcherUsesTheKnownSize(t *testing.T) {
	data := patterned(4096)
	s := &rangeServer{data: data}
	reg, ref := newTestRegistry(t, s.handler())

	f := newRegistryRangeFetcher(reg, ref, "sha256:abc", int64(len(data)))
	got, err := f.Size(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != int64(len(data)) {
		t.Errorf("size = %d, want %d", got, len(data))
	}
	if len(s.requests) != 0 {
		t.Errorf("a known size still cost %d requests", len(s.requests))
	}
}

// Without a known size it is discovered with a HEAD.
func TestRegistryRangeFetcherDiscoversAnUnknownSize(t *testing.T) {
	data := patterned(4096)
	s := &rangeServer{data: data}
	reg, ref := newTestRegistry(t, s.handler())

	f := newRegistryRangeFetcher(reg, ref, "sha256:abc", 0)
	got, err := f.Size(context.Background())
	if err != nil {
		t.Fatalf("discover size: %v", err)
	}
	if got != int64(len(data)) {
		t.Errorf("size = %d, want %d", got, len(data))
	}
}

// The cache key is the digest, so the same layer from two mirrors is cached once.
func TestRegistryRangeFetcherKeysByDigest(t *testing.T) {
	reg, ref := newTestRegistry(t, (&rangeServer{data: []byte("x")}).handler())
	a := newRegistryRangeFetcher(reg, ref, "sha256:same", 1)
	b := newRegistryRangeFetcher(reg, Reference{Host: "other.example", Repository: "z"},
		"sha256:same", 1)
	if a.CacheKey() != b.CacheKey() {
		t.Errorf("the same digest from two hosts has different keys (%q vs %q), so an "+
			"identical layer is cached twice", a.CacheKey(), b.CacheKey())
	}
}

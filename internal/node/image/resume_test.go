package image

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// A layer blob is served through a CDN redirect where mid-transfer resets are
// common. These tests cover the resume path, which otherwise only shows itself
// on a flaky network — the failure mode being a conversion that dies partway
// through a large layer.

// truncatingRegistry serves a blob but cuts the connection after a while, then
// honours Range requests so the reader can resume.
type truncatingRegistry struct {
	content []byte
	// cutAfter bounds how much of the remaining blob each response delivers.
	cutAfter int

	mu       sync.Mutex
	requests []string // Range header of each blob request, for assertions
}

func (tr *truncatingRegistry) serve(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"t","expires_in":300}`)
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="http://%s/token"`, r.Host))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		rangeHeader := r.Header.Get("Range")
		tr.mu.Lock()
		tr.requests = append(tr.requests, rangeHeader)
		tr.mu.Unlock()

		offset := 0
		if rangeHeader != "" {
			// Only "bytes=N-" is produced by the client.
			spec := strings.TrimSuffix(strings.TrimPrefix(rangeHeader, "bytes="), "-")
			n, err := strconv.Atoi(spec)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			offset = n
		}
		if offset > len(tr.content) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}

		remaining := tr.content[offset:]
		send := remaining
		truncated := false
		if tr.cutAfter > 0 && len(send) > tr.cutAfter {
			send = send[:tr.cutAfter]
			truncated = true
		}

		if rangeHeader != "" {
			w.Header().Set("Content-Range",
				fmt.Sprintf("bytes %d-%d/%d", offset, offset+len(send)-1, len(tr.content)))
			w.WriteHeader(http.StatusPartialContent)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		w.Write(send)

		if truncated {
			// Break the connection mid-body, which is what a CDN reset looks
			// like to the client.
			if hijacker, ok := w.(http.Hijacker); ok {
				if conn, _, err := hijacker.Hijack(); err == nil {
					conn.Close()
				}
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestFetchBlobResumesAfterReset is the property that makes conversion reliable:
// a broken transfer must continue from where it stopped, and the assembled bytes
// must match the original exactly.
func TestFetchBlobResumesAfterReset(t *testing.T) {
	// A recognisable pattern, so a misplaced resume shows up as wrong content
	// rather than only a wrong length.
	content := []byte(strings.Repeat("0123456789abcdef", 4096)) // 64 KiB
	fake := &truncatingRegistry{content: content, cutAfter: 8 << 10}
	srv := fake.serve(t)
	ref, reg := hostRef(t, srv)

	body, err := reg.FetchBlob(context.Background(), ref, "sha256:layer")
	if err != nil {
		t.Fatalf("FetchBlob: %v", err)
	}
	defer body.Close()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("assembled %d bytes, want %d, and content must match exactly",
			len(got), len(content))
	}

	fake.mu.Lock()
	requests := append([]string(nil), fake.requests...)
	fake.mu.Unlock()

	if len(requests) < 2 {
		t.Fatalf("blob fetched in %d request(s); the reset was not resumed", len(requests))
	}
	// The first request asks for the whole blob; each retry resumes at an
	// offset, and that offset must advance or the reader is looping.
	if requests[0] != "" {
		t.Errorf("first request had Range %q, want none", requests[0])
	}
	var prev int
	for _, r := range requests[1:] {
		if !strings.HasPrefix(r, "bytes=") {
			t.Errorf("resume request had Range %q", r)
			continue
		}
		n, _ := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(r, "bytes="), "-"))
		if n <= prev {
			t.Errorf("resume offset did not advance: %d after %d", n, prev)
		}
		prev = n
	}
}

// TestFetchBlobRejectsRegistryIgnoringRange guards against silent corruption: a
// registry that restarts the blob instead of honouring the offset would splice
// the beginning of the layer into the middle.
func TestFetchBlobRejectsRegistryIgnoringRange(t *testing.T) {
	content := []byte(strings.Repeat("x", 32<<10))
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"t","expires_in":300}`)
	})
	var served int
	var mu sync.Mutex
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="http://%s/token"`, r.Host))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		served++
		mu.Unlock()
		// Always 200 from the start, even for a Range request.
		w.WriteHeader(http.StatusOK)
		w.Write(content[:8<<10])
		if hijacker, ok := w.(http.Hijacker); ok {
			if conn, _, err := hijacker.Hijack(); err == nil {
				conn.Close()
			}
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ref, reg := hostRef(t, srv)
	body, err := reg.FetchBlob(context.Background(), ref, "sha256:layer")
	if err != nil {
		t.Fatalf("FetchBlob: %v", err)
	}
	defer body.Close()

	_, err = io.ReadAll(body)
	if err == nil {
		t.Fatal("read succeeded against a registry that ignores Range; the layer would be corrupt")
	}
	if !strings.Contains(err.Error(), "resum") {
		t.Errorf("err = %v, want it to name the resume problem", err)
	}
}

// TestFetchBlobRetriesServerErrors covers the transient-5xx case, distinct from
// a mid-body reset.
func TestFetchBlobRetriesServerErrors(t *testing.T) {
	content := []byte("layer content")
	var attempts int
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"t","expires_in":300}`)
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="http://%s/token"`, r.Host))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write(content)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ref, reg := hostRef(t, srv)
	body, err := reg.FetchBlob(context.Background(), ref, "sha256:layer")
	if err != nil {
		t.Fatalf("FetchBlob: %v", err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content = %q", got)
	}
}

// TestFetchBlobDoesNotRetryClientErrors: a missing blob will not appear on
// retry, and spending five attempts on it delays a real error report.
func TestFetchBlobDoesNotRetryClientErrors(t *testing.T) {
	var attempts int
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"token":"t","expires_in":300}`)
	})
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.Header().Set("WWW-Authenticate",
				fmt.Sprintf(`Bearer realm="http://%s/token"`, r.Host))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ref, reg := hostRef(t, srv)
	if _, err := reg.FetchBlob(context.Background(), ref, "sha256:absent"); err == nil {
		t.Fatal("FetchBlob accepted a 404")
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Errorf("made %d attempts against a 404, want 1", attempts)
	}
}

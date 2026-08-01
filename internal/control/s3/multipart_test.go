package s3

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// multipartStub answers the initiate/upload/complete/abort sequence, recording
// what it was asked to do so tests can assert on protocol details a real
// server accepts silently.
type multipartStub struct {
	mu       sync.Mutex
	parts    map[int][]byte
	aborted  bool
	complete []byte

	initiateStatus int
	initiateBody   string
	partStatus     int
	completeStatus int
	completeBody   string
}

func newMultipartStub() *multipartStub {
	return &multipartStub{parts: map[int][]byte{}}
}

func (s *multipartStub) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		s.mu.Lock()
		defer s.mu.Unlock()

		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			if s.initiateStatus != 0 {
				w.WriteHeader(s.initiateStatus)
				_, _ = w.Write([]byte("refused"))
				return
			}
			body := s.initiateBody
			if body == "" {
				body = `<InitiateMultipartUploadResult><UploadId>up-1</UploadId>` +
					`</InitiateMultipartUploadResult>`
			}
			fmt.Fprint(w, body)

		case r.Method == http.MethodPut && q.Has("partNumber"):
			if q.Get("uploadId") != "up-1" {
				t.Errorf("part sent with uploadId %q", q.Get("uploadId"))
			}
			if s.partStatus != 0 {
				w.WriteHeader(s.partStatus)
				return
			}
			var n int
			_, _ = fmt.Sscanf(q.Get("partNumber"), "%d", &n)
			buf := new(bytes.Buffer)
			_, _ = buf.ReadFrom(r.Body)
			s.parts[n] = buf.Bytes()
			w.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, n))
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && q.Has("uploadId"):
			buf := new(bytes.Buffer)
			_, _ = buf.ReadFrom(r.Body)
			s.complete = buf.Bytes()
			if s.completeStatus != 0 {
				w.WriteHeader(s.completeStatus)
			}
			body := s.completeBody
			if body == "" {
				body = `<CompleteMultipartUploadResult></CompleteMultipartUploadResult>`
			}
			fmt.Fprint(w, body)

		case r.Method == http.MethodDelete && q.Has("uploadId"):
			s.aborted = true
			w.WriteHeader(http.StatusNoContent)

		case r.Method == http.MethodPut:
			// Plain PUT, used for the empty-object path.
			w.WriteHeader(http.StatusOK)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
			w.WriteHeader(http.StatusBadRequest)
		}
	}
}

func (s *multipartStub) wasAborted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aborted
}

// TestUploaderNumbersPartsInOrder checks the property a restore depends on:
// parts must be numbered so the server reassembles them in write order.
func TestUploaderNumbersPartsInOrder(t *testing.T) {
	stub := newMultipartStub()
	c := stubClient(t, stub.handler(t))

	u, err := c.NewUploader(context.Background(), "b", "k")
	if err != nil {
		t.Fatal(err)
	}
	u.partSize = 4

	// Write across part boundaries in uneven chunks: buffering must not
	// reorder or drop bytes.
	for _, chunk := range []string{"ab", "cde", "fghi", "jk"} {
		if n, err := u.Write([]byte(chunk)); err != nil || n != len(chunk) {
			t.Fatalf("write %q: n=%d err=%v", chunk, n, err)
		}
	}
	if err := u.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if got := string(stub.parts[1]); got != "abcd" {
		t.Errorf("part 1 = %q, want abcd", got)
	}
	if got := string(stub.parts[2]); got != "efgh" {
		t.Errorf("part 2 = %q, want efgh", got)
	}
	if got := string(stub.parts[3]); got != "ijk" {
		t.Errorf("part 3 (final, short) = %q, want ijk", got)
	}
	// The completion body must list every part with its ETag.
	for _, want := range []string{"etag-1", "etag-2", "etag-3", "<PartNumber>3</PartNumber>"} {
		if !strings.Contains(string(stub.complete), want) {
			t.Errorf("completion body missing %s:\n%s", want, stub.complete)
		}
	}
}

func TestUploaderRejectsWriteAfterClose(t *testing.T) {
	stub := newMultipartStub()
	c := stubClient(t, stub.handler(t))
	u, err := c.NewUploader(context.Background(), "b", "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := u.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := u.Write([]byte("more")); err == nil {
		t.Error("write after close accepted")
	}
	// Close is idempotent so deferred cleanup does not double-complete.
	if err := u.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// TestUploaderAbortsAfterPartFailure checks that a mid-stream failure cleans
// up its parts rather than leaving the caller paying for orphaned storage.
func TestUploaderAbortsAfterPartFailure(t *testing.T) {
	stub := newMultipartStub()
	stub.partStatus = http.StatusInternalServerError
	c := stubClient(t, stub.handler(t))

	u, err := c.NewUploader(context.Background(), "b", "k")
	if err != nil {
		t.Fatal(err)
	}
	u.partSize = 4

	_, werr := u.Write([]byte("abcdefgh"))
	if werr == nil {
		t.Fatal("write should fail when a part upload fails")
	}
	// The failure sticks: later writes report it rather than appearing to work.
	if _, err := u.Write([]byte("x")); err == nil {
		t.Error("write after failure accepted")
	}
	if err := u.Close(); err == nil {
		t.Error("Close should report the earlier failure")
	}
	if !stub.wasAborted() {
		t.Error("upload not aborted after a part failure")
	}
}

func TestUploaderReportsMissingETag(t *testing.T) {
	stub := newMultipartStub()
	c := stubClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && r.URL.Query().Has("partNumber") {
			w.WriteHeader(http.StatusOK) // no ETag header at all
			return
		}
		stub.handler(t)(w, r)
	})

	u, err := c.NewUploader(context.Background(), "b", "k")
	if err != nil {
		t.Fatal(err)
	}
	u.partSize = 2
	if _, err := u.Write([]byte("abcd")); err == nil {
		t.Error("a part response without an ETag should fail")
	}
}

func TestNewUploaderRejectsBadInitiate(t *testing.T) {
	for name, mutate := range map[string]func(*multipartStub){
		"error status":  func(s *multipartStub) { s.initiateStatus = http.StatusForbidden },
		"malformed xml": func(s *multipartStub) { s.initiateBody = "not xml" },
		"no upload id": func(s *multipartStub) {
			s.initiateBody = `<InitiateMultipartUploadResult></InitiateMultipartUploadResult>`
		},
	} {
		stub := newMultipartStub()
		mutate(stub)
		c := stubClient(t, stub.handler(t))
		if _, err := c.NewUploader(context.Background(), "b", "k"); err == nil {
			t.Errorf("%s: NewUploader accepted a bad initiate response", name)
		}
	}
}

// TestCompleteDetectsErrorInsideSuccessStatus covers S3's habit of reporting a
// completion failure inside a 200 response. Trusting the status would publish
// a snapshot that cannot be read back.
func TestCompleteDetectsErrorInsideSuccessStatus(t *testing.T) {
	stub := newMultipartStub()
	stub.completeBody = `<Error><Code>InternalError</Code></Error>`
	c := stubClient(t, stub.handler(t))

	u, err := c.NewUploader(context.Background(), "b", "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	if err := u.Close(); err == nil {
		t.Error("Close accepted an <Error> body inside a 200 response")
	}
}

func TestCompleteReportsErrorStatus(t *testing.T) {
	stub := newMultipartStub()
	stub.completeStatus = http.StatusInternalServerError
	stub.completeBody = "upstream failure"
	c := stubClient(t, stub.handler(t))

	u, err := c.NewUploader(context.Background(), "b", "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	err = u.Close()
	if err == nil || !strings.Contains(err.Error(), "upstream failure") {
		t.Errorf("Close error = %v, want the response body included", err)
	}
}

// TestAbortBeforeCloseSkipsCompletion checks that Abort wins: an aborted
// upload must not later publish itself.
func TestAbortBeforeCloseSkipsCompletion(t *testing.T) {
	stub := newMultipartStub()
	c := stubClient(t, stub.handler(t))

	u, err := c.NewUploader(context.Background(), "b", "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	u.Abort()
	if err := u.Close(); err != nil {
		t.Errorf("Close after Abort = %v, want nil", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if !stub.aborted {
		t.Error("Abort did not reach the server")
	}
	if stub.complete != nil {
		t.Error("aborted upload was completed anyway")
	}
}

// TestAbortSucceedsAfterContextCancel matters because cleanup runs on the
// failure path, where the request context is usually already cancelled.
func TestAbortSucceedsAfterContextCancel(t *testing.T) {
	stub := newMultipartStub()
	c := stubClient(t, stub.handler(t))

	ctx, cancel := context.WithCancel(context.Background())
	u, err := c.NewUploader(ctx, "b", "k")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	cancel()
	u.Abort()
	if !stub.wasAborted() {
		t.Error("abort did not reach the server after the context was cancelled")
	}
}

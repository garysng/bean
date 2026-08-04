package api

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The write path used to read the whole body into gateway memory before forwarding
// any of it, and capped uploads at 4 MiB as a consequence. Both are gone; these tests
// pin the properties that replaced them.
//
// The interesting one is not "a large file works" -- it is "the gateway does not hold
// the file", and that cannot be asserted by reading a response. So the test drives a
// body larger than any buffer the old code would have allowed and checks the bytes
// arrive, which is the observable half, plus a reader that would deadlock if anything
// tried to consume it all before sending.

// putFile uploads through the gateway, returning the response and decoded body.
func putFile(t *testing.T, e *testEnv, sandboxID, path string, body io.Reader) (*http.Response, string) {
	t.Helper()
	url := fmt.Sprintf("%s/v1/sandboxes/%s/files?path=%s&mkdirs=true",
		e.Server.URL, sandboxID, path)
	req, err := http.NewRequest(http.MethodPut, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

// TestWriteFileAcceptsMoreThanTheOldInlineCap is the user-visible half of the change.
//
// 4 MiB was small enough to reject an ordinary source tree, and the rejection told the
// caller to "use presigned flow" -- which did not exist anywhere in the codebase, so
// it was an instruction to do something impossible.
func TestWriteFileAcceptsMoreThanTheOldInlineCap(t *testing.T) {
	env := startEnv(t, envOpts{})
	sb := env.createSandbox(nil)
	id := sb["id"].(string)

	// Deliberately above the retired 4 MiB cap and not a multiple of the 1 MiB chunk,
	// so a final short frame is exercised too.
	const size = 5<<20 + 1234
	body := bytes.Repeat([]byte("x"), size)

	resp, out := putFile(t, env, id, "/tmp/big.bin", bytes.NewReader(body))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT files = %d %s; an upload above the old 4 MiB cap must now "+
			"succeed", resp.StatusCode, out)
	}
	if !strings.Contains(out, fmt.Sprint(size)) {
		t.Errorf("response %s does not report %d bytes written; a chunked upload that "+
			"loses a frame would still return 200", out, size)
	}
}

// The gateway no longer buffers an upload, and that property is deliberately NOT
// tested here, because it is not observable from outside.
//
// Two attempts were made. The first gated the body and released the gate on the first
// read, which passed against a buffering implementation because io.ReadAll consumed
// the gate too. The second blocked the body forever and polled for the file to appear
// on the node -- and that one cannot work either, for a better reason: the agent writes
// to a temp file and renames it atomically on completion (internal/beand/server.go),
// specifically so a mid-stream failure never leaves a truncated file. So no file can
// exist while the upload is in flight, by design.
//
// The property is real but internal: the handler holds one fileChunkBytes buffer
// instead of the whole body. What is left to assert from outside is the consequence
// that used to be visible -- the size cap the buffering forced -- which
// TestWriteFileAcceptsMoreThanTheOldInlineCap covers, and it does fail against the
// buffering implementation. Writing a third test that passes either way would be worse
// than admitting the limit.

// TestWriteFileHandlesAChunkBoundaryExactly covers the off-by-one the chunked loop
// could have: a body that is an exact multiple of the chunk size ends with a read
// returning (0, EOF), and an implementation that sent before checking the error would
// emit an empty final frame.
func TestWriteFileHandlesAChunkBoundaryExactly(t *testing.T) {
	env := startEnv(t, envOpts{})
	sb := env.createSandbox(nil)
	id := sb["id"].(string)

	const size = 2 << 20 // exactly two chunks
	resp, out := putFile(t, env, id, "/tmp/exact.bin",
		bytes.NewReader(bytes.Repeat([]byte("z"), size)))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT files = %d %s", resp.StatusCode, out)
	}
	if !strings.Contains(out, fmt.Sprint(size)) {
		t.Errorf("response %s does not report %d bytes", out, size)
	}
	if got := env.readFile(id, "/tmp/exact.bin"); len(got) != size {
		t.Errorf("read back %d bytes, want %d", len(got), size)
	}
}

// TestWriteFileRoundTripsContent checks the bytes are not merely counted but correct,
// which a chunking bug could break while still reporting the right total.
func TestWriteFileRoundTripsContent(t *testing.T) {
	env := startEnv(t, envOpts{})
	sb := env.createSandbox(nil)
	id := sb["id"].(string)

	// Distinguishable per chunk, so a loop that repeated or dropped one is visible in
	// the content rather than only in the length.
	var body bytes.Buffer
	for i := 0; i < 3; i++ {
		body.Write(bytes.Repeat([]byte{byte('A' + i)}, 1<<20))
	}
	want := body.String()

	resp, out := putFile(t, env, id, "/tmp/multi.bin", bytes.NewReader(body.Bytes()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT files = %d %s", resp.StatusCode, out)
	}
	if got := env.readFile(id, "/tmp/multi.bin"); got != want {
		t.Errorf("content differs: got %d bytes, want %d; first mismatch matters more "+
			"than the length, since a chunking bug can preserve the total",
			len(got), len(want))
	}
}

// TestWriteFileRejectsAMissingPath keeps the argument check ahead of the stream: a
// request with no path must fail before a node stream is opened, or the node is left
// with a stream nobody will complete.
func TestWriteFileRejectsAMissingPath(t *testing.T) {
	env := startEnv(t, envOpts{})
	sb := env.createSandbox(nil)
	id := sb["id"].(string)

	url := fmt.Sprintf("%s/v1/sandboxes/%s/files", env.Server.URL, id)
	req, err := http.NewRequest(http.MethodPut, url, strings.NewReader("data"))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("PUT with no path = %d, want 400", resp.StatusCode)
	}
}

package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// stubAPI serves the subset of bean-api the CLI uses.
func stubAPI(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	mux := http.NewServeMux()

	mux.HandleFunc("POST /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "create")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		img, _ := body["image"].(string)
		snap, _ := body["snapshot"].(string)
		// A create must name exactly one source; "reject-me" simulates a
		// server-side rejection.
		if (img == "" && snap == "") || img == "reject-me" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"code": "IMAGE_REF_INVALID", "message": "image rejected"}})
			return
		}
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{
			"sandbox": map[string]any{"id": "sbx_cli1", "state": "RUNNING",
				"image": body["image"], "labels": body["labels"], "createdAt": "2026-08-01T00:00:00Z"}})
	})
	mux.HandleFunc("GET /v1/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "list:"+r.URL.Query().Get("label"))
		json.NewEncoder(w).Encode(map[string]any{"sandboxes": []map[string]any{
			{"id": "sbx_cli1", "image": "busybox", "state": "RUNNING",
				"labels": map[string]string{"k": "v"}, "createdAt": "2026-08-01T00:00:00Z"}}})
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/exec", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "exec")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		cmd, _ := body["cmd"].([]any)
		parts := make([]string, 0, len(cmd))
		for _, c := range cmd {
			parts = append(parts, c.(string))
		}
		json.NewEncoder(w).Encode(map[string]any{
			"exitCode": 7, "stdout": strings.Join(parts, " "), "stderr": "err-stream",
			"truncated": true, "durationMs": 3})
	})
	mux.HandleFunc("DELETE /v1/sandboxes/{id}", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "delete:force="+r.URL.Query().Get("force"))
		w.WriteHeader(202)
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/pause", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "pause")
		w.WriteHeader(202)
	})
	mux.HandleFunc("POST /v1/sandboxes/{id}/resume", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "resume")
		w.WriteHeader(202)
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "logs:tail="+r.URL.Query().Get("tailLines"))
		w.Write([]byte("log-line-1\nlog-line-2\n"))
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "events")
		json.NewEncoder(w).Encode(map[string]any{"events": []map[string]any{
			{"type": "sandbox.lifecycle.created", "timestamp": "2026-08-01T00:00:00Z"},
			{"type": "sandbox.lifecycle.running", "timestamp": "2026-08-01T00:00:01Z"}}})
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "stream:sandbox="+r.URL.Query().Get("sandbox")+
			",label="+r.URL.Query().Get("label"))
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, ": connected\n\n")
		for _, typ := range []string{"sandbox.lifecycle.created", "sandbox.lifecycle.failed"} {
			data := map[string]any{"type": typ, "sandboxId": "sbx_cli1",
				"timestamp": "2026-08-01T00:00:00Z", "version": "v1"}
			if typ == "sandbox.lifecycle.failed" {
				data["data"] = map[string]string{"reason": "boom"}
			}
			b, _ := json.Marshal(data)
			io.WriteString(w, "event: "+typ+"\ndata: "+string(b)+"\n\n")
		}
		io.WriteString(w, ": keepalive\n\n")
	})
	mux.HandleFunc("PUT /v1/sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "upload:"+r.URL.Query().Get("path"))
		json.NewEncoder(w).Encode(map[string]any{"bytesWritten": 9})
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "download:"+r.URL.Query().Get("path"))
		w.Write([]byte("remote-data"))
	})

	mux.HandleFunc("POST /v1/sandboxes/{id}/snapshot", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, fmt.Sprintf("snapshot:name=%v,keepRunning=%v",
			body["name"], body["keepRunning"]))
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]any{
			"snapshotId": "snap_cli1",
			"snapshot":   map[string]any{"state": "READY", "sizeBytes": 4096},
		})
	})
	mux.HandleFunc("GET /v1/snapshots", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "snapshots:label="+r.URL.Query().Get("label"))
		json.NewEncoder(w).Encode(map[string]any{"snapshots": []map[string]any{{
			"id": "snap_cli1", "name": "after-setup", "state": "READY",
			"sandboxId": "sbx_cli1", "image": "busybox", "sizeBytes": 4096,
			"createdAt": "2026-08-01T00:00:00Z",
		}}})
	})
	mux.HandleFunc("DELETE /v1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "snapshot-rm")
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /v1/images", func(w http.ResponseWriter, r *http.Request) {
		// The source filter has to reach the server as a query param; the CLI
		// deciding locally would list images the caller cannot see.
		if source := r.URL.Query().Get("source"); source != "" {
			seen = append(seen, "images:source="+source)
		} else {
			seen = append(seen, "images")
		}
		json.NewEncoder(w).Encode(map[string]any{"images": []map[string]any{{
			"ref": "busybox:1.36", "state": "PENDING", "source": "imported",
			"cachedNodes": 0, "sizeBytes": 0,
		}}})
	})
	mux.HandleFunc("GET /v1/images/status", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "image-status:"+r.URL.Query().Get("ref"))
		json.NewEncoder(w).Encode(map[string]any{
			"ref": r.URL.Query().Get("ref"), "state": "PENDING", "format": "oci",
			"cachedNodes": 0, "sizeBytes": 0,
		})
	})
	mux.HandleFunc("POST /v1/images/prewarm", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, fmt.Sprintf("prewarm:nodes=%v", body["targetNodes"]))
		w.WriteHeader(202)
		json.NewEncoder(w).Encode(map[string]any{
			"jobId": "pw_cli1", "ready": map[string]int{"busybox:1.36": 2},
		})
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, &seen
}

func runCLI(t *testing.T, ts *httptest.Server, args ...string) (string, string, int) {
	t.Helper()
	t.Setenv("BEAN_BASE_URL", ts.URL)
	t.Setenv("BEAN_API_KEY", "test-key")
	t.Setenv("BEAN_TIMEOUT", "30s")
	var out, errBuf bytes.Buffer
	code := Run(args, &out, &errBuf)
	return out.String(), errBuf.String(), code
}

func TestCmdRun(t *testing.T) {
	ts, seen := stubAPI(t)
	out, errStr, code := runCLI(t, ts, "run", "--image", "busybox", "--label", "k=v",
		"--idle-timeout", "300s", "--on-idle", "kill")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errStr)
	}
	if !strings.Contains(out, "sbx_cli1") || !strings.Contains(out, "RUNNING") {
		t.Errorf("out = %q", out)
	}
	if len(*seen) != 1 || (*seen)[0] != "create" {
		t.Errorf("calls = %v", *seen)
	}
}

func TestCmdRunAPIError(t *testing.T) {
	ts, _ := stubAPI(t)
	// Server rejects this image; the CLI must surface the API error code
	// rather than a generic HTTP failure.
	_, errStr, code := runCLI(t, ts, "run", "--image", "reject-me")
	if code == 0 {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errStr, "IMAGE_REF_INVALID") {
		t.Errorf("stderr = %q", errStr)
	}
}

func TestCmdRunRequiresImageLocally(t *testing.T) {
	ts, seen := stubAPI(t)
	// Missing --image is caught client-side; no request should be sent.
	_, errStr, code := runCLI(t, ts, "run")
	if code != 125 || !strings.Contains(errStr, "--image or --snapshot") {
		t.Errorf("code=%d stderr=%q", code, errStr)
	}
	if len(*seen) != 0 {
		t.Errorf("unexpected server calls: %v", *seen)
	}
}

func TestCmdLs(t *testing.T) {
	ts, seen := stubAPI(t)
	out, errStr, code := runCLI(t, ts, "ls", "--label", "k=v")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errStr)
	}
	for _, want := range []string{"ID", "IMAGE", "STATE", "sbx_cli1", "busybox", "k=v"} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q: %q", want, out)
		}
	}
	if (*seen)[0] != "list:k=v" {
		t.Errorf("calls = %v", *seen)
	}
}

func TestCmdExecPassesThroughExitCode(t *testing.T) {
	ts, _ := stubAPI(t)
	out, errStr, code := runCLI(t, ts, "exec", "sbx_cli1", "--", "echo", "hello")
	if code != 7 {
		t.Errorf("code = %d, want 7 (remote exit code)", code)
	}
	if out != "echo hello" {
		t.Errorf("stdout = %q", out)
	}
	if !strings.Contains(errStr, "err-stream") {
		t.Errorf("stderr = %q", errStr)
	}
	if !strings.Contains(errStr, "truncated") {
		t.Errorf("expected truncation notice: %q", errStr)
	}
}

func TestCmdExecUsage(t *testing.T) {
	ts, _ := stubAPI(t)
	_, errStr, code := runCLI(t, ts, "exec")
	if code != 125 || !strings.Contains(errStr, "usage") {
		t.Errorf("code=%d stderr=%q", code, errStr)
	}
}

func TestCmdKillAndForce(t *testing.T) {
	ts, seen := stubAPI(t)
	if out, errStr, code := runCLI(t, ts, "kill", "sbx_cli1"); code != 0 ||
		!strings.Contains(out, "killed") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errStr)
	}
	if _, _, code := runCLI(t, ts, "kill", "sbx_cli1", "--force"); code != 0 {
		t.Fatalf("force kill code=%d", code)
	}
	if got := *seen; got[0] != "delete:force=" || got[1] != "delete:force=true" {
		t.Errorf("calls = %v", got)
	}
}

func TestCmdPauseResume(t *testing.T) {
	ts, seen := stubAPI(t)
	if _, e, code := runCLI(t, ts, "pause", "sbx_cli1"); code != 0 {
		t.Fatalf("pause code=%d err=%q", code, e)
	}
	if _, e, code := runCLI(t, ts, "resume", "sbx_cli1"); code != 0 {
		t.Fatalf("resume code=%d err=%q", code, e)
	}
	if got := *seen; len(got) != 2 || got[0] != "pause" || got[1] != "resume" {
		t.Errorf("calls = %v", got)
	}
	// missing id
	if _, errStr, code := runCLI(t, ts, "pause"); code != 125 || !strings.Contains(errStr, "usage") {
		t.Errorf("code=%d stderr=%q", code, errStr)
	}
}

func TestCmdLogs(t *testing.T) {
	ts, seen := stubAPI(t)
	out, errStr, code := runCLI(t, ts, "logs", "sbx_cli1", "--tail", "5")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errStr)
	}
	if !strings.Contains(out, "log-line-1") {
		t.Errorf("out = %q", out)
	}
	if (*seen)[0] != "logs:tail=5" {
		t.Errorf("calls = %v", *seen)
	}
}

func TestCmdEvents(t *testing.T) {
	ts, _ := stubAPI(t)
	out, errStr, code := runCLI(t, ts, "events", "sbx_cli1")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errStr)
	}
	if !strings.Contains(out, "sandbox.lifecycle.created") || !strings.Contains(out, "TIME") {
		t.Errorf("out = %q", out)
	}
}

func TestCmdCpUpload(t *testing.T) {
	ts, seen := stubAPI(t)
	local := filepath.Join(t.TempDir(), "up.txt")
	os.WriteFile(local, []byte("local-data"), 0o644)
	out, errStr, code := runCLI(t, ts, "cp", local, "sbx:sbx_cli1:/w/up.txt")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errStr)
	}
	if !strings.Contains(out, "copied") {
		t.Errorf("out = %q", out)
	}
	if (*seen)[0] != "upload:/w/up.txt" {
		t.Errorf("calls = %v", *seen)
	}
}

func TestCmdCpDownload(t *testing.T) {
	ts, seen := stubAPI(t)
	dst := filepath.Join(t.TempDir(), "down.txt")
	if _, errStr, code := runCLI(t, ts, "cp", "sbx:sbx_cli1:/w/f.txt", dst); code != 0 {
		t.Fatalf("code=%d err=%q", code, errStr)
	}
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != "remote-data" {
		t.Errorf("file = %q err=%v", got, err)
	}
	if (*seen)[0] != "download:/w/f.txt" {
		t.Errorf("calls = %v", *seen)
	}
}

func TestCmdCpValidation(t *testing.T) {
	ts, _ := stubAPI(t)
	for _, args := range [][]string{
		{"cp", "only-one"},
		{"cp", "local", "also-local"},
		{"cp", "sbx:bad-format", "dst"},
	} {
		if _, errStr, code := runCLI(t, ts, args...); code == 0 {
			t.Errorf("args %v: expected failure, stderr=%q", args, errStr)
		}
	}
}

func TestVersionAndEmptyArgs(t *testing.T) {
	ts, _ := stubAPI(t)
	if out, _, code := runCLI(t, ts, "version"); code != 0 || !strings.Contains(out, "bean") {
		t.Errorf("code=%d out=%q", code, out)
	}
	if _, errStr, code := runCLI(t, ts); code != 125 || !strings.Contains(errStr, "usage") {
		t.Errorf("code=%d stderr=%q", code, errStr)
	}
}

func TestCLIUnreachableServer(t *testing.T) {
	t.Setenv("BEAN_BASE_URL", "http://192.0.2.1:8080") // TEST-NET-1: unroutable
	t.Setenv("BEAN_API_KEY", "k")
	t.Setenv("BEAN_TIMEOUT", "1s") // fail fast instead of the 15m default
	var out, errBuf bytes.Buffer
	code := Run([]string{"ls"}, &out, &errBuf)
	if code == 0 {
		t.Error("expected failure against unreachable server")
	}
}

func TestClientTimeoutFromEnv(t *testing.T) {
	t.Setenv("BEAN_TIMEOUT", "42s")
	if got := NewClient("http://x", "k").HTTP.Timeout; got != 42*time.Second {
		t.Errorf("timeout = %s, want 42s", got)
	}
	t.Setenv("BEAN_TIMEOUT", "garbage")
	if got := NewClient("http://x", "k").HTTP.Timeout; got != 15*time.Minute {
		t.Errorf("invalid value should fall back to default, got %s", got)
	}
}

func TestCmdEventsFollow(t *testing.T) {
	ts, seen := stubAPI(t)
	out, errStr, code := runCLI(t, ts, "events", "-f", "sbx_cli1", "--label", "run=a")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errStr)
	}
	for _, want := range []string{"sbx_cli1", "sandbox.lifecycle.created", "sandbox.lifecycle.failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q: %q", want, out)
		}
	}
	// The reason from event data is surfaced inline.
	if !strings.Contains(out, "boom") {
		t.Errorf("expected failure reason in output: %q", out)
	}
	// Comment/keepalive lines must not leak into output.
	if strings.Contains(out, "connected") || strings.Contains(out, "keepalive") {
		t.Errorf("SSE comments leaked: %q", out)
	}
	if (*seen)[0] != "stream:sandbox=sbx_cli1,label=run=a" {
		t.Errorf("stream call = %q", (*seen)[0])
	}
}

func TestCmdEventsFollowClusterWide(t *testing.T) {
	ts, seen := stubAPI(t)
	// No sandbox id: follow everything.
	if _, errStr, code := runCLI(t, ts, "events", "--follow"); code != 0 {
		t.Fatalf("code=%d err=%q", code, errStr)
	}
	if (*seen)[0] != "stream:sandbox=,label=" {
		t.Errorf("stream call = %q", (*seen)[0])
	}
}

func TestCmdEventsRequiresIDWithoutFollow(t *testing.T) {
	ts, _ := stubAPI(t)
	_, errStr, code := runCLI(t, ts, "events")
	if code != 125 || !strings.Contains(errStr, "usage") {
		t.Errorf("code=%d stderr=%q", code, errStr)
	}
}

func TestCmdRunFromSnapshot(t *testing.T) {
	ts, seen := stubAPI(t)
	out, errStr, code := runCLI(t, ts, "run", "--snapshot", "snap_cli1")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errStr)
	}
	if !strings.Contains(out, "sbx_cli1") {
		t.Errorf("out = %q", out)
	}
	if (*seen)[0] != "create" {
		t.Errorf("calls = %v", *seen)
	}
}

func TestCmdRunRejectsBothSources(t *testing.T) {
	ts, seen := stubAPI(t)
	_, errStr, code := runCLI(t, ts, "run", "--image", "x", "--snapshot", "s")
	if code != 125 || !strings.Contains(errStr, "exactly one") {
		t.Errorf("code=%d stderr=%q", code, errStr)
	}
	if len(*seen) != 0 {
		t.Errorf("request sent despite invalid args: %v", *seen)
	}
}

func TestCmdSnapshotCreate(t *testing.T) {
	ts, seen := stubAPI(t)
	out, errStr, code := runCLI(t, ts, "snapshot", "create", "sbx_cli1", "--name", "after-setup")
	if code != 0 {
		t.Fatalf("code=%d err=%q", code, errStr)
	}
	for _, want := range []string{"snap_cli1", "READY", "4096"} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q: %q", want, out)
		}
	}
	// keepRunning is left to the server default unless asked otherwise.
	if got := (*seen)[0]; got != "snapshot:name=after-setup,keepRunning=<nil>" {
		t.Errorf("request = %q", got)
	}

	if _, _, code := runCLI(t, ts, "snapshot", "create", "sbx_cli1", "--no-keep-running"); code != 0 {
		t.Fatal("no-keep-running failed")
	}
	if got := (*seen)[1]; !strings.Contains(got, "keepRunning=false") {
		t.Errorf("request = %q", got)
	}
}

func TestCmdSnapshotListAndRemove(t *testing.T) {
	ts, seen := stubAPI(t)
	out, _, code := runCLI(t, ts, "snapshot", "ls", "--label", "kind=test")
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	for _, want := range []string{"ID", "snap_cli1", "after-setup", "READY", "busybox"} {
		if !strings.Contains(out, want) {
			t.Errorf("out missing %q: %q", want, out)
		}
	}
	if (*seen)[0] != "snapshots:label=kind=test" {
		t.Errorf("call = %q", (*seen)[0])
	}

	out, _, code = runCLI(t, ts, "snapshot", "rm", "snap_cli1")
	if code != 0 || !strings.Contains(out, "deleted") {
		t.Errorf("code=%d out=%q", code, out)
	}
}

func TestCmdSnapshotUsage(t *testing.T) {
	ts, _ := stubAPI(t)
	for _, args := range [][]string{
		{"snapshot"},
		{"snapshot", "create"},
		{"snapshot", "rm"},
		{"snapshot", "bogus"},
	} {
		if _, errStr, code := runCLI(t, ts, args...); code == 0 {
			t.Errorf("args %v: expected failure, stderr=%q", args, errStr)
		}
	}
}

func TestCmdImage(t *testing.T) {
	ts, seen := stubAPI(t)

	out, _, code := runCLI(t, ts, "image", "ls")
	if code != 0 || !strings.Contains(out, "busybox:1.36") {
		t.Errorf("ls: code=%d out=%q", code, out)
	}
	// Provenance is shown, because "is this ours or pulled from outside" is the
	// question someone looking at an unfamiliar ref has.
	if !strings.Contains(out, "imported") {
		t.Errorf("ls does not report source: %q", out)
	}

	out, _, code = runCLI(t, ts, "image", "ls", "--source", "built")
	if code != 0 {
		t.Fatalf("ls --source code = %d out=%q", code, out)
	}
	if !slices.Contains(*seen, "images:source=built") {
		t.Errorf("--source not passed to the server: %v", *seen)
	}

	out, _, code = runCLI(t, ts, "image", "status", "busybox:1.36")
	if code != 0 {
		t.Fatalf("status code = %d", code)
	}
	// format tells the user which tier can run it today.
	if !strings.Contains(out, "oci") || !strings.Contains(out, "PENDING") {
		t.Errorf("status out = %q", out)
	}

	out, _, code = runCLI(t, ts, "image", "prewarm", "busybox:1.36", "--replicas", "2")
	if code != 0 {
		t.Fatalf("prewarm code = %d", code)
	}
	// Readiness is what a caller acts on. How many machines hold the image is
	// not reported: it is placement, which they cannot influence and should not
	// come to depend on.
	if !strings.Contains(out, "pw_cli1") || !strings.Contains(out, "ready") {
		t.Errorf("prewarm out = %q", out)
	}
	if strings.Contains(out, "node") {
		t.Errorf("prewarm output mentions nodes: %q", out)
	}
	// Matched by content rather than position: an index breaks whenever an
	// unrelated request is added to this test, which says nothing about prewarm.
	if !slices.Contains(*seen, "prewarm:nodes=2") {
		t.Errorf("prewarm request not seen: %v", *seen)
	}
}

func TestCmdImagePrewarmRejectsANonNumericReplicaCount(t *testing.T) {
	ts, _ := stubAPI(t)
	// Silently ignoring the value would warm one copy while the caller believed
	// they had asked for many.
	_, errStr, code := runCLI(t, ts, "image", "prewarm", "busybox:1.36", "--replicas", "lots")
	if code == 0 {
		t.Error("accepted a non-numeric replica count")
	}
	if !strings.Contains(errStr, "replicas") {
		t.Errorf("stderr = %q, want it to name the flag", errStr)
	}
}

func TestCmdImageUsage(t *testing.T) {
	ts, _ := stubAPI(t)
	for _, args := range [][]string{{"image"}, {"image", "status"}, {"image", "prewarm"}, {"image", "bogus"}} {
		if _, errStr, code := runCLI(t, ts, args...); code == 0 {
			t.Errorf("args %v: expected failure, stderr=%q", args, errStr)
		}
	}
}

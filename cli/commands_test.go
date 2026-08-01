package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		if img, _ := body["image"].(string); img == "" || img == "reject-me" {
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
	mux.HandleFunc("PUT /v1/sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "upload:"+r.URL.Query().Get("path"))
		json.NewEncoder(w).Encode(map[string]any{"bytesWritten": 9})
	})
	mux.HandleFunc("GET /v1/sandboxes/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, "download:"+r.URL.Query().Get("path"))
		w.Write([]byte("remote-data"))
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
	if code != 125 || !strings.Contains(errStr, "--image required") {
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

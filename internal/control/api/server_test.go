package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node"
	"github.com/garysng/bean/internal/node/runtime"
)

const testKey = "bk_test_secret"

var agentBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bean-agent-bin")
	if err != nil {
		panic(err)
	}
	agentBin = filepath.Join(dir, "bean-agent")
	cmd := exec.Command("go", "build", "-o", agentBin, "github.com/garysng/bean/cmd/bean-agent")
	if out, err := cmd.CombinedOutput(); err != nil {
		panic("build agent: " + string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// startStack runs beand (localRuntime) + bean-api in-process.
func startStack(t *testing.T) *httptest.Server {
	t.Helper()
	mgr := node.NewManager(runtime.NewLocalRuntime(agentBin, t.TempDir()))
	t.Cleanup(mgr.Close)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gsrv := grpc.NewServer()
	nodev1.RegisterSandboxServiceServer(gsrv, node.NewGRPCServer(mgr))
	go gsrv.Serve(lis)
	t.Cleanup(gsrv.Stop)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	srv := NewServer(st, nodev1.NewSandboxServiceClient(conn), "node-test", testKey)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func doReq(t *testing.T, ts *httptest.Server, method, path string, body any) (*http.Response, map[string]any) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestAuthRequired(t *testing.T) {
	ts := startStack(t)
	resp, err := http.Get(ts.URL + "/v1/sandboxes")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestSandboxLifecycleViaREST(t *testing.T) {
	ts := startStack(t)

	// create
	resp, out := doReq(t, ts, "POST", "/v1/sandboxes", map[string]any{
		"image":  "python:3.12",
		"labels": map[string]string{"run": "t1"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d: %v", resp.StatusCode, out)
	}
	sb := out["sandbox"].(map[string]any)
	id := sb["id"].(string)
	if sb["state"] != "RUNNING" {
		t.Fatalf("state = %v", sb["state"])
	}

	// exec
	resp, out = doReq(t, ts, "POST", "/v1/sandboxes/"+id+"/exec", map[string]any{
		"cmd": []string{"sh", "-c", "echo hi"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec status = %d: %v", resp.StatusCode, out)
	}
	if strings.TrimSpace(out["stdout"].(string)) != "hi" {
		t.Errorf("stdout = %q", out["stdout"])
	}
	if out["exitCode"].(float64) != 0 {
		t.Errorf("exitCode = %v", out["exitCode"])
	}

	// list with label filter
	resp, out = doReq(t, ts, "GET", "/v1/sandboxes?label=run%3Dt1", nil)
	if n := len(out["sandboxes"].([]any)); n != 1 {
		t.Errorf("list count = %d", n)
	}
	resp, out = doReq(t, ts, "GET", "/v1/sandboxes?label=run%3Dother", nil)
	if n := len(out["sandboxes"].([]any)); n != 0 {
		t.Errorf("filtered list count = %d", n)
	}

	// events
	resp, out = doReq(t, ts, "GET", "/v1/sandboxes/"+id+"/events", nil)
	events := out["events"].([]any)
	if len(events) < 2 {
		t.Errorf("events = %v", events)
	}
	first := events[0].(map[string]any)
	if first["type"] != "sandbox.lifecycle.created" {
		t.Errorf("first event = %v", first["type"])
	}

	// delete
	resp, _ = doReq(t, ts, "DELETE", "/v1/sandboxes/"+id, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	resp, out = doReq(t, ts, "GET", "/v1/sandboxes/"+id, nil)
	if out["sandbox"].(map[string]any)["state"] != "STOPPED" {
		t.Errorf("state after delete = %v", out["sandbox"].(map[string]any)["state"])
	}
}

func TestFilesViaREST(t *testing.T) {
	ts := startStack(t)
	_, out := doReq(t, ts, "POST", "/v1/sandboxes", map[string]any{"image": "x"})
	id := out["sandbox"].(map[string]any)["id"].(string)

	// write
	req, _ := http.NewRequest("PUT", ts.URL+"/v1/sandboxes/"+id+"/files?path=/w/a.txt&mkdirs=true", strings.NewReader("hello file"))
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write status = %d", resp.StatusCode)
	}

	// read back
	req, _ = http.NewRequest("GET", ts.URL+"/v1/sandboxes/"+id+"/files?path=/w/a.txt", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data := new(bytes.Buffer)
	data.ReadFrom(resp.Body)
	resp.Body.Close()
	if data.String() != "hello file" {
		t.Errorf("read = %q", data.String())
	}

	// ls
	_, out = doReq(t, ts, "GET", "/v1/sandboxes/"+id+"/files/ls?path=/w", nil)
	entries := out["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["name"] != "a.txt" {
		t.Errorf("entries = %v", entries)
	}

	// delete file
	req, _ = http.NewRequest("DELETE", ts.URL+"/v1/sandboxes/"+id+"/files?path=/w/a.txt", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete file status = %d", resp.StatusCode)
	}
}

func TestPauseResumeAndWakeViaREST(t *testing.T) {
	ts := startStack(t)
	_, out := doReq(t, ts, "POST", "/v1/sandboxes", map[string]any{"image": "x"})
	id := out["sandbox"].(map[string]any)["id"].(string)

	resp, _ := doReq(t, ts, "POST", "/v1/sandboxes/"+id+"/pause", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pause status = %d", resp.StatusCode)
	}
	_, out = doReq(t, ts, "GET", "/v1/sandboxes/"+id, nil)
	if out["sandbox"].(map[string]any)["state"] != "PAUSED" {
		t.Fatalf("state = %v", out["sandbox"].(map[string]any)["state"])
	}

	// exec against PAUSED wakes it transparently
	resp, out = doReq(t, ts, "POST", "/v1/sandboxes/"+id+"/exec", map[string]any{
		"cmd": []string{"echo", "awake"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec on paused = %d: %v", resp.StatusCode, out)
	}
	_, out = doReq(t, ts, "GET", "/v1/sandboxes/"+id, nil)
	if out["sandbox"].(map[string]any)["state"] != "RUNNING" {
		t.Errorf("state after wake = %v", out["sandbox"].(map[string]any)["state"])
	}
}

func TestCreateValidation(t *testing.T) {
	ts := startStack(t)
	resp, out := doReq(t, ts, "POST", "/v1/sandboxes", map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d", resp.StatusCode)
	}
	resp, out = doReq(t, ts, "POST", "/v1/sandboxes", map[string]any{
		"image":     "x",
		"lifecycle": map[string]any{"idleTimeout": "nonsense"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad idleTimeout status = %d: %v", resp.StatusCode, out)
	}
	resp, out = doReq(t, ts, "POST", "/v1/sandboxes", map[string]any{
		"image":     "x",
		"lifecycle": map[string]any{"idleTimeout": "10s", "onIdle": "explode"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad onIdle status = %d: %v", resp.StatusCode, out)
	}
}

func TestNotFound(t *testing.T) {
	ts := startStack(t)
	resp, out := doReq(t, ts, "GET", "/v1/sandboxes/sbx_missing", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d: %v", resp.StatusCode, out)
	}
	errObj := out["error"].(map[string]any)
	if errObj["code"] != "SANDBOX_NOT_FOUND" {
		t.Errorf("code = %v", errObj["code"])
	}
}

func TestExecNonZeroExit(t *testing.T) {
	ts := startStack(t)
	_, out := doReq(t, ts, "POST", "/v1/sandboxes", map[string]any{"image": "x"})
	id := out["sandbox"].(map[string]any)["id"].(string)
	_, out = doReq(t, ts, "POST", fmt.Sprintf("/v1/sandboxes/%s/exec", id), map[string]any{
		"cmd": []string{"sh", "-c", "exit 42"},
	})
	if out["exitCode"].(float64) != 42 {
		t.Errorf("exitCode = %v", out["exitCode"])
	}
}

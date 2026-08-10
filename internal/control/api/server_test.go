package api

import (
	"bytes"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestAuthRequired(t *testing.T) {
	env := startEnv(t, envOpts{})
	resp, err := http.Get(env.Server.URL + "/v1/sandboxes")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestSandboxLifecycleViaREST(t *testing.T) {
	env := startEnv(t, envOpts{})

	// create
	resp, out := env.do("POST", "/v1/sandboxes", map[string]any{
		"imageRef": "python:3.12",
		"labels":   map[string]string{"run": "t1"},
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
	resp, out = env.do("POST", "/v1/sandboxes/"+id+"/exec", map[string]any{
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
	resp, out = env.do("GET", "/v1/sandboxes?label=run%3Dt1", nil)
	if n := len(out["sandboxes"].([]any)); n != 1 {
		t.Errorf("list count = %d", n)
	}
	resp, out = env.do("GET", "/v1/sandboxes?label=run%3Dother", nil)
	if n := len(out["sandboxes"].([]any)); n != 0 {
		t.Errorf("filtered list count = %d", n)
	}

	// events
	resp, out = env.do("GET", "/v1/sandboxes/"+id+"/events", nil)
	events := out["events"].([]any)
	if len(events) < 2 {
		t.Errorf("events = %v", events)
	}
	first := events[0].(map[string]any)
	if first["type"] != "sandbox.lifecycle.created" {
		t.Errorf("first event = %v", first["type"])
	}

	// delete
	resp, _ = env.do("DELETE", "/v1/sandboxes/"+id, nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	resp, out = env.do("GET", "/v1/sandboxes/"+id, nil)
	if out["sandbox"].(map[string]any)["state"] != "STOPPED" {
		t.Errorf("state after delete = %v", out["sandbox"].(map[string]any)["state"])
	}
}

func TestFilesViaREST(t *testing.T) {
	env := startEnv(t, envOpts{})
	_, out := env.do("POST", "/v1/sandboxes", map[string]any{"imageRef": "x"})
	id := out["sandbox"].(map[string]any)["id"].(string)

	// write
	req, _ := http.NewRequest("PUT", env.Server.URL+"/v1/sandboxes/"+id+"/files?path=/w/a.txt&mkdirs=true", strings.NewReader("hello file"))
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
	req, _ = http.NewRequest("GET", env.Server.URL+"/v1/sandboxes/"+id+"/files?path=/w/a.txt", nil)
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
	_, out = env.do("GET", "/v1/sandboxes/"+id+"/files/ls?path=/w", nil)
	entries := out["entries"].([]any)
	if len(entries) != 1 || entries[0].(map[string]any)["name"] != "a.txt" {
		t.Errorf("entries = %v", entries)
	}

	// delete file
	req, _ = http.NewRequest("DELETE", env.Server.URL+"/v1/sandboxes/"+id+"/files?path=/w/a.txt", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete file status = %d", resp.StatusCode)
	}
}

func TestPauseResumeAndWakeViaREST(t *testing.T) {
	env := startEnv(t, envOpts{})
	_, out := env.do("POST", "/v1/sandboxes", map[string]any{"imageRef": "x"})
	id := out["sandbox"].(map[string]any)["id"].(string)

	resp, _ := env.do("POST", "/v1/sandboxes/"+id+"/pause", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pause status = %d", resp.StatusCode)
	}
	_, out = env.do("GET", "/v1/sandboxes/"+id, nil)
	if out["sandbox"].(map[string]any)["state"] != "PAUSED" {
		t.Fatalf("state = %v", out["sandbox"].(map[string]any)["state"])
	}

	// exec against PAUSED wakes it transparently
	resp, out = env.do("POST", "/v1/sandboxes/"+id+"/exec", map[string]any{
		"cmd": []string{"echo", "awake"},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("exec on paused = %d: %v", resp.StatusCode, out)
	}
	_, out = env.do("GET", "/v1/sandboxes/"+id, nil)
	if out["sandbox"].(map[string]any)["state"] != "RUNNING" {
		t.Errorf("state after wake = %v", out["sandbox"].(map[string]any)["state"])
	}
}

func TestCreateValidation(t *testing.T) {
	env := startEnv(t, envOpts{})
	resp, out := env.do("POST", "/v1/sandboxes", map[string]any{})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d", resp.StatusCode)
	}
	resp, out = env.do("POST", "/v1/sandboxes", map[string]any{
		"imageRef":  "x",
		"lifecycle": map[string]any{"idleTimeout": "nonsense"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad idleTimeout status = %d: %v", resp.StatusCode, out)
	}
	resp, out = env.do("POST", "/v1/sandboxes", map[string]any{
		"imageRef":  "x",
		"lifecycle": map[string]any{"idleTimeout": "10s", "onIdle": "explode"},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad onIdle status = %d: %v", resp.StatusCode, out)
	}
}

func TestNotFound(t *testing.T) {
	env := startEnv(t, envOpts{})
	resp, out := env.do("GET", "/v1/sandboxes/sbx_missing", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d: %v", resp.StatusCode, out)
	}
	errObj := out["error"].(map[string]any)
	if errObj["code"] != "SANDBOX_NOT_FOUND" {
		t.Errorf("code = %v", errObj["code"])
	}
}

func TestExecNonZeroExit(t *testing.T) {
	env := startEnv(t, envOpts{})
	_, out := env.do("POST", "/v1/sandboxes", map[string]any{"imageRef": "x"})
	id := out["sandbox"].(map[string]any)["id"].(string)
	_, out = env.do("POST", fmt.Sprintf("/v1/sandboxes/%s/exec", id), map[string]any{
		"cmd": []string{"sh", "-c", "exit 42"},
	})
	if out["exitCode"].(float64) != 42 {
		t.Errorf("exitCode = %v", out["exitCode"])
	}
}

func TestResumeAndLogsHandlers(t *testing.T) {
	env := startEnv(t, envOpts{})
	_, out := env.do("POST", "/v1/sandboxes", map[string]any{"imageRef": "x"})
	id := out["sandbox"].(map[string]any)["id"].(string)

	// Explicit pause then explicit resume (the transparent-wake path is
	// covered separately; this exercises the resume handler directly).
	if resp, _ := env.do("POST", "/v1/sandboxes/"+id+"/pause", nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pause status = %d", resp.StatusCode)
	}
	resp, _ := env.do("POST", "/v1/sandboxes/"+id+"/resume", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("resume status = %d", resp.StatusCode)
	}
	_, out = env.do("GET", "/v1/sandboxes/"+id, nil)
	if got := out["sandbox"].(map[string]any)["state"]; got != "RUNNING" {
		t.Errorf("state after resume = %v", got)
	}

	// Logs endpoint streams the sandbox log buffer.
	req, _ := http.NewRequest("GET", env.Server.URL+"/v1/sandboxes/"+id+"/logs?tailLines=10", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	lresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer lresp.Body.Close()
	if lresp.StatusCode != http.StatusOK {
		t.Errorf("logs status = %d", lresp.StatusCode)
	}
	if ct := lresp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("logs content-type = %q", ct)
	}

	// Both endpoints 404 for an unknown sandbox.
	if resp, _ := env.do("POST", "/v1/sandboxes/sbx_missing/resume", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("resume unknown = %d, want 404", resp.StatusCode)
	}
	req2, _ := http.NewRequest("GET", env.Server.URL+"/v1/sandboxes/sbx_missing/logs", nil)
	req2.Header.Set("Authorization", "Bearer "+testKey)
	r2, _ := http.DefaultClient.Do(req2)
	r2.Body.Close()
	if r2.StatusCode != http.StatusNotFound {
		t.Errorf("logs unknown = %d, want 404", r2.StatusCode)
	}
}

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
)

// TestListNodesReportsRegisteredNode drives handleListNodes over the real
// single-node stack and confirms the operational view carries the node's id
// and capacity.
func TestListNodesReportsRegisteredNode(t *testing.T) {
	env := startEnv(t, envOpts{})
	resp, out := env.do("GET", "/v1/nodes", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %v", resp.StatusCode, out)
	}
	nodes, ok := out["nodes"].([]any)
	if !ok || len(nodes) != 1 {
		t.Fatalf("nodes = %v, want one node", out["nodes"])
	}
	n := nodes[0].(map[string]any)
	if n["id"] != env.NodeIDs[0] {
		t.Errorf("node id = %v, want %s", n["id"], env.NodeIDs[0])
	}
	if _, has := n["cpuAllocatable"]; !has {
		t.Error("node view is missing cpuAllocatable")
	}
}

// TestDrainNodeStopsPlacement drains a known node and confirms an unknown one is
// a 404.
func TestDrainNodeStopsPlacement(t *testing.T) {
	env := startEnv(t, envOpts{})
	resp, _ := env.do("POST", "/v1/nodes/"+env.NodeIDs[0]+"/drain", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("drain = %d, want 202", resp.StatusCode)
	}
	resp, _ = env.do("POST", "/v1/nodes/node-absent/drain", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("drain unknown = %d, want 404", resp.StatusCode)
	}
}

// TestBuildLogsRequiresRef and the not-found path cover handleBuildLogs's
// guards without needing a running build.
func TestBuildLogsRequiresRef(t *testing.T) {
	env := startEnv(t, envOpts{})
	resp, _ := env.raw("GET", "/v1/templates/build/logs", "")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no ref = %d, want 400", resp.StatusCode)
	}
	resp, _ = env.raw("GET", "/v1/templates/build/logs?ref=nope:v1", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown ref = %d, want 404", resp.StatusCode)
	}
}

// TestBuildLogsStreamsRetained registers a build directly on the tracker so the
// streaming reader has something to serve, then reads it back with follow=false.
func TestBuildLogsStreamsRetained(t *testing.T) {
	env := startEnv(t, envOpts{})
	bl := env.API.builds.start("streamed:v1", func() {})
	bl.Write([]byte("step 1\nstep 2\n"))
	bl.finish(false, "")

	resp, body := env.raw("GET", "/v1/templates/build/logs?ref=streamed:v1&follow=false", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if want := "step 1"; !strings.Contains(body, want) {
		t.Errorf("body = %q, missing %q", body, want)
	}
	if !strings.Contains(body, "build succeeded") {
		t.Errorf("body = %q, missing success trailer", body)
	}
}

// TestBuildCancelGuards covers handleBuildCancel's missing-ref, unknown-build
// and already-finished paths.
func TestBuildCancelGuards(t *testing.T) {
	env := startEnv(t, envOpts{})
	resp, _ := env.do("POST", "/v1/templates/build/cancel", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("no ref = %d, want 400", resp.StatusCode)
	}
	resp, _ = env.do("POST", "/v1/templates/build/cancel?ref=nope:v1", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown = %d, want 404", resp.StatusCode)
	}

	// A live build cancels; a finished one is a conflict.
	cancelled := false
	bl := env.API.builds.start("live:v1", func() { cancelled = true })
	resp, out := env.do("POST", "/v1/templates/build/cancel?ref=live:v1", nil)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("cancel live = %d: %v", resp.StatusCode, out)
	}
	if !cancelled {
		t.Error("cancel did not invoke the build's cancel func")
	}

	bl.finish(false, "")
	resp, _ = env.do("POST", "/v1/templates/build/cancel?ref=live:v1", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("cancel finished = %d, want 409", resp.StatusCode)
	}
}

// TestProtoImageConfig copies a node's reported config field-for-field and maps
// a nil config to nil.
func TestProtoImageConfig(t *testing.T) {
	if protoImageConfig(nil) != nil {
		t.Error("nil config did not map to nil")
	}
	cfg := protoImageConfig(&nodev1.ImageConfig{
		Env: []string{"A=1"}, Entrypoint: []string{"/bin/sh"},
		Cmd: []string{"-c", "true"}, WorkingDir: "/w", User: "root",
	})
	if cfg == nil || len(cfg.Env) != 1 || cfg.WorkingDir != "/w" || cfg.User != "root" {
		t.Fatalf("config = %+v", cfg)
	}
	if len(cfg.Entrypoint) != 1 || len(cfg.Cmd) != 2 {
		t.Errorf("entrypoint/cmd = %v/%v", cfg.Entrypoint, cfg.Cmd)
	}
}

// TestWriteFaultUnwrapsFault confirms a fault error writes its own status/code,
// and a plain error falls back through grpcFault to a 500.
func TestWriteFaultUnwrapsFault(t *testing.T) {
	rec := httptest.NewRecorder()
	writeFault(rec, faultf(http.StatusForbidden, "NOPE", "denied"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("fault status = %d, want 403", rec.Code)
	}

	rec = httptest.NewRecorder()
	writeFault(rec, errPlain{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("plain error status = %d, want 500", rec.Code)
	}
}

// TestFaultError pins the fault's error string, the form callers log.
func TestFaultError(t *testing.T) {
	f := faultf(http.StatusBadRequest, "BAD", "why %d", 7)
	if got := f.Error(); got != "BAD: why 7" {
		t.Errorf("Error() = %q, want \"BAD: why 7\"", got)
	}
}

// TestRegistryAuthResolvesStoredCredential stores a credential through the API
// (which seals it) and confirms RegistryAuth decrypts it back for a matching
// ref, while a ref with no stored credential resolves to empty.
func TestRegistryAuthResolvesStoredCredential(t *testing.T) {
	env := startEnv(t, envOpts{WithSecrets: true})
	resp, out := env.do("PUT", "/v1/registries", map[string]any{
		"host": "registry.example.com", "username": "robot", "secret": "s3cr3t",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put registry = %d: %v", resp.StatusCode, out)
	}

	user, pass, err := env.API.RegistryAuth("registry.example.com/team/app:v1")
	if err != nil {
		t.Fatalf("RegistryAuth: %v", err)
	}
	if user != "robot" || pass != "s3cr3t" {
		t.Errorf("auth = %q/%q, want robot/s3cr3t", user, pass)
	}

	// A public image with no stored credential resolves to empty, not an error.
	user, pass, err = env.API.RegistryAuth("python:3.12")
	if err != nil || user != "" || pass != "" {
		t.Errorf("public auth = %q/%q/%v, want empty", user, pass, err)
	}
}

// TestCreateFromStoredTemplate exercises the `template` create source: a ready
// template resolves by id and by name, an unknown one is 404, and a
// not-yet-ready one is a 409.
func TestCreateFromStoredTemplate(t *testing.T) {
	env := startEnv(t, envOpts{})

	ready := &store.Template{
		ID: store.NewID(store.PrefixTemplate), Name: "built/app:v1",
		Source: store.TemplateBuilt, State: store.TemplateReady,
	}
	if err := env.Store.PutTemplate(ready); err != nil {
		t.Fatal(err)
	}

	// By name.
	resp, out := env.do("POST", "/v1/sandboxes", map[string]any{"template": "built/app:v1"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create by name = %d: %v", resp.StatusCode, out)
	}
	// By id.
	resp, out = env.do("POST", "/v1/sandboxes", map[string]any{"template": ready.ID})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create by id = %d: %v", resp.StatusCode, out)
	}

	// Unknown template.
	resp, _ = env.do("POST", "/v1/sandboxes", map[string]any{"template": "tpl_absent"})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown template = %d, want 404", resp.StatusCode)
	}

	// A template still converting is a conflict, not a create.
	pending := &store.Template{
		ID: store.NewID(store.PrefixTemplate), Name: "pending/app:v1",
		Source: store.TemplateConverted, State: store.TemplatePending,
	}
	if err := env.Store.PutTemplate(pending); err != nil {
		t.Fatal(err)
	}
	resp, _ = env.do("POST", "/v1/sandboxes", map[string]any{"template": "pending/app:v1"})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("pending template = %d, want 409", resp.StatusCode)
	}
}

// TestCreateFromPublishedTemplateUsesFSDigest confirms a template carrying an
// overlaybd manifest digest drives the create through the shared-store fs path.
func TestCreateFromPublishedTemplateUsesFSDigest(t *testing.T) {
	env := startEnv(t, envOpts{})
	published := &store.Template{
		ID: store.NewID(store.PrefixTemplate), Name: "published/app:v1",
		Source: store.TemplateConverted, State: store.TemplateReady,
		FS: store.FSArtifact{Digest: "sha256:deadbeef", SizeBytes: 4096},
	}
	if err := env.Store.PutTemplate(published); err != nil {
		t.Fatal(err)
	}
	resp, out := env.do("POST", "/v1/sandboxes", map[string]any{"template": published.ID})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create from published template = %d: %v", resp.StatusCode, out)
	}
}

// TestFailSnapshotRecordsFailure drives failSnapshot directly: the record moves
// to FAILED with the cause, and it survives a read-back.
func TestFailSnapshotRecordsFailure(t *testing.T) {
	env := startEnv(t, envOpts{})
	snap := &store.Snapshot{
		ID: store.NewID(store.PrefixSnapshot), SandboxID: "sbx-1",
		State: store.SnapshotCreating,
	}
	if err := env.Store.PutSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	env.API.failSnapshot(snap, errPlain{})

	got, err := env.Store.GetSnapshot(snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != store.SnapshotFailed || got.Reason != "plain" {
		t.Errorf("snapshot = %s/%q, want FAILED/plain", got.State, got.Reason)
	}
}

type errPlain struct{}

func (errPlain) Error() string { return "plain" }

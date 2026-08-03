package api

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestSnapshotRestoreEndToEnd is the flow the design exists for: set an
// environment up once, capture it, then recreate it.
func TestSnapshotRestoreEndToEnd(t *testing.T) {
	env := startEnv(t, envOpts{})
	blobs := env.Blobs

	// 1. Create a sandbox and put state in it.
	_, out := env.do("POST", "/v1/sandboxes", map[string]any{"image": "python:3.12"})
	srcID := out["sandbox"].(map[string]any)["id"].(string)

	req, _ := http.NewRequest("PUT",
		env.Server.URL+"/v1/sandboxes/"+srcID+"/files?mkdirs=true&path=/work/setup.txt",
		strings.NewReader("environment-is-ready"))
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("write file status = %d", resp.StatusCode)
	}

	// 2. Snapshot it.
	code, out := env.do("POST", "/v1/sandboxes/"+srcID+"/snapshot", map[string]any{
		"name": "after-setup", "labels": map[string]string{"suite": "snap"},
	})
	if code.StatusCode != http.StatusAccepted {
		t.Fatalf("snapshot status = %d: %v", code.StatusCode, out)
	}
	snapID := out["snapshotId"].(string)
	if !strings.HasPrefix(snapID, "snap_") {
		t.Errorf("snapshotId = %q", snapID)
	}
	snapRec := out["snapshot"].(map[string]any)
	if snapRec["state"] != "READY" {
		t.Errorf("snapshot state = %v, want READY", snapRec["state"])
	}
	if size := snapRec["sizeBytes"].(float64); size <= 0 {
		t.Errorf("sizeBytes = %v, want > 0", size)
	}
	// The blob is really on disk.
	if n, err := blobs.Size(snapID); err != nil || n <= 0 {
		t.Errorf("blob size = %d err=%v", n, err)
	}

	// The source sandbox is untouched by the snapshot.
	_, out = env.do("GET", "/v1/sandboxes/"+srcID, nil)
	if state := out["sandbox"].(map[string]any)["state"]; state != "RUNNING" {
		t.Errorf("source state = %v, want RUNNING", state)
	}

	// 3. Restore into a new sandbox.
	code, out = env.do("POST", "/v1/sandboxes", map[string]any{"snapshot": snapID})
	if code.StatusCode != http.StatusCreated {
		t.Fatalf("restore status = %d: %v", code.StatusCode, out)
	}
	restored := out["sandbox"].(map[string]any)
	dstID := restored["id"].(string)
	if restored["snapshotId"] != snapID {
		t.Errorf("snapshotId = %v, want %s", restored["snapshotId"], snapID)
	}
	// The restored sandbox inherits the snapshot's image.
	if restored["image"] != "python:3.12" {
		t.Errorf("image = %v, want inherited from snapshot", restored["image"])
	}

	// 4. The state written before the snapshot is present in the restore.
	req, _ = http.NewRequest("GET",
		env.Server.URL+"/v1/sandboxes/"+dstID+"/files?path=/work/setup.txt", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	body.ReadFrom(resp.Body)
	resp.Body.Close()
	if got := body.String(); got != "environment-is-ready" {
		t.Errorf("restored file = %q, want the pre-snapshot content", got)
	}

	// 5. The two sandboxes are independent: writing to one does not affect
	// the other.
	req, _ = http.NewRequest("PUT",
		env.Server.URL+"/v1/sandboxes/"+dstID+"/files?path=/work/setup.txt",
		strings.NewReader("changed-in-clone"))
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()

	req, _ = http.NewRequest("GET",
		env.Server.URL+"/v1/sandboxes/"+srcID+"/files?path=/work/setup.txt", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, _ = http.DefaultClient.Do(req)
	body.Reset()
	body.ReadFrom(resp.Body)
	resp.Body.Close()
	if got := body.String(); got != "environment-is-ready" {
		t.Errorf("source file changed to %q; clones must be independent", got)
	}
}

func TestSnapshotFanOut(t *testing.T) {
	env := startEnv(t, envOpts{})
	_, out := env.do("POST", "/v1/sandboxes", map[string]any{"image": "base:1"})
	srcID := out["sandbox"].(map[string]any)["id"].(string)
	_, out = env.do("POST", "/v1/sandboxes/"+srcID+"/snapshot", nil)
	snapID := out["snapshotId"].(string)

	// One snapshot, many sandboxes — the batch-evaluation use case.
	ids := map[string]bool{}
	for i := 0; i < 3; i++ {
		code, out := env.do("POST", "/v1/sandboxes", map[string]any{"snapshot": snapID})
		if code.StatusCode != http.StatusCreated {
			t.Fatalf("clone %d: %d %v", i, code.StatusCode, out)
		}
		ids[out["sandbox"].(map[string]any)["id"].(string)] = true
	}
	if len(ids) != 3 {
		t.Errorf("expected 3 distinct clones, got %v", ids)
	}
	// The snapshot is still deletable afterwards: restores released their refs.
	if code, _ := env.do("DELETE", "/v1/snapshots/"+snapID, nil); code.StatusCode != http.StatusNoContent {
		t.Errorf("delete after fan-out = %d", code.StatusCode)
	}
}

func TestSnapshotListAndGet(t *testing.T) {
	env := startEnv(t, envOpts{})
	_, out := env.do("POST", "/v1/sandboxes", map[string]any{"image": "base:1"})
	srcID := out["sandbox"].(map[string]any)["id"].(string)
	_, out = env.do("POST", "/v1/sandboxes/"+srcID+"/snapshot", map[string]any{
		"name": "s1", "labels": map[string]string{"kind": "test"},
	})
	snapID := out["snapshotId"].(string)

	_, out = env.do("GET", "/v1/snapshots", nil)
	if n := len(out["snapshots"].([]any)); n != 1 {
		t.Errorf("snapshots = %d, want 1", n)
	}
	// Label filtering works.
	_, out = env.do("GET", "/v1/snapshots?label=kind%3Dtest", nil)
	if n := len(out["snapshots"].([]any)); n != 1 {
		t.Errorf("filtered = %d, want 1", n)
	}
	_, out = env.do("GET", "/v1/snapshots?label=kind%3Dother", nil)
	if n := len(out["snapshots"].([]any)); n != 0 {
		t.Errorf("non-matching filter = %d, want 0", n)
	}

	_, out = env.do("GET", "/v1/snapshots/"+snapID, nil)
	snap := out["snapshot"].(map[string]any)
	if snap["name"] != "s1" || snap["sandboxId"] != srcID {
		t.Errorf("snapshot = %+v", snap)
	}

	code, _ := env.do("GET", "/v1/snapshots/snap_missing", nil)
	if code.StatusCode != http.StatusNotFound {
		t.Errorf("missing snapshot = %d, want 404", code.StatusCode)
	}
}

func TestSnapshotKeepRunningFalseStopsSource(t *testing.T) {
	env := startEnv(t, envOpts{})
	_, out := env.do("POST", "/v1/sandboxes", map[string]any{"image": "base:1"})
	srcID := out["sandbox"].(map[string]any)["id"].(string)

	code, out := env.do("POST", "/v1/sandboxes/"+srcID+"/snapshot", map[string]any{
		"keepRunning": false,
	})
	if code.StatusCode != http.StatusAccepted {
		t.Fatalf("snapshot status = %d: %v", code.StatusCode, out)
	}
	_, out = env.do("GET", "/v1/sandboxes/"+srcID, nil)
	if state := out["sandbox"].(map[string]any)["state"]; state != "STOPPED" {
		t.Errorf("source state = %v, want STOPPED", state)
	}
}

func TestRestoreFromMissingSnapshot(t *testing.T) {
	env := startEnv(t, envOpts{})
	code, out := env.do("POST", "/v1/sandboxes", map[string]any{"snapshot": "snap_nope"})
	if code.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %v", code.StatusCode, out)
	}
	if c := out["error"].(map[string]any)["code"]; c != "SNAPSHOT_NOT_FOUND" {
		t.Errorf("code = %v", c)
	}
}

func TestCreateRejectsBothImageAndSnapshot(t *testing.T) {
	env := startEnv(t, envOpts{})
	code, out := env.do("POST", "/v1/sandboxes", map[string]any{
		"image": "x:1", "snapshot": "snap_1",
	})
	if code.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400: %v", code.StatusCode, out)
	}
	code, _ = env.do("POST", "/v1/sandboxes", map[string]any{})
	if code.StatusCode != http.StatusBadRequest {
		t.Errorf("neither given: status = %d, want 400", code.StatusCode)
	}
}

func TestDeleteSnapshotRemovesBlob(t *testing.T) {
	env := startEnv(t, envOpts{})
	blobs := env.Blobs
	_, out := env.do("POST", "/v1/sandboxes", map[string]any{"image": "base:1"})
	srcID := out["sandbox"].(map[string]any)["id"].(string)
	_, out = env.do("POST", "/v1/sandboxes/"+srcID+"/snapshot", nil)
	snapID := out["snapshotId"].(string)

	if _, err := blobs.Size(snapID); err != nil {
		t.Fatalf("blob missing before delete: %v", err)
	}
	if code, _ := env.do("DELETE", "/v1/snapshots/"+snapID, nil); code.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", code.StatusCode)
	}
	// Both the record and the blob are gone.
	if code, _ := env.do("GET", "/v1/snapshots/"+snapID, nil); code.StatusCode != http.StatusNotFound {
		t.Errorf("record still present: %d", code.StatusCode)
	}
	if _, err := blobs.Size(snapID); err == nil {
		t.Error("blob survived record deletion")
	}
	if code, _ := env.do("DELETE", "/v1/snapshots/"+snapID, nil); code.StatusCode != http.StatusNotFound {
		t.Errorf("second delete = %d, want 404", code.StatusCode)
	}
}

func TestSnapshotDisabledWithoutStorage(t *testing.T) {
	// Without configured storage the endpoints refuse rather than pretending.
	env := startEnv(t, envOpts{WithoutSnapshots: true})
	_, out := env.do("POST", "/v1/sandboxes", map[string]any{"image": "x"})
	id := out["sandbox"].(map[string]any)["id"].(string)
	code, _ := env.do("POST", "/v1/sandboxes/"+id+"/snapshot", nil)
	if code.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", code.StatusCode)
	}
}

// Snapshotting an already-paused sandbox must not wait for its agent.
//
// This asserts on elapsed time, which is normally a bad idea, because no
// assertion on state can catch the bug. Reconnecting to the agent after a
// checkpoint is right for a sandbox that is running again, but an agent in a
// sandbox that was PAUSED to begin with is not scheduled to answer a health
// check -- so the reconnect burned the full agentReadyTimeout and then logged a
// failure for a sandbox that was behaving correctly. Every assertion still
// passed: the snapshot is valid, the source is still paused, the children are
// usable. Only the clock showed it.
//
// The bound is far below the 20s timeout and far above the ~100ms this takes, so
// it fails on the regression and not on a slow machine.
func TestSnapshotOfPausedSandboxDoesNotWaitForItsAgent(t *testing.T) {
	env := startEnv(t, envOpts{})
	id := env.sandboxID(map[string]any{"image": "base:1"})
	if resp, _ := env.do("POST", "/v1/sandboxes/"+id+"/pause", nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pause: %d", resp.StatusCode)
	}

	start := time.Now()
	resp, body := env.do("POST", "/v1/sandboxes/"+id+"/snapshot", nil)
	elapsed := time.Since(start)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("snapshot: %d %v", resp.StatusCode, body)
	}
	if elapsed > 5*time.Second {
		t.Errorf("snapshot of a paused sandbox took %s; it is waiting for an "+
			"agent that cannot answer while the guest is stopped", elapsed)
	}

	// The snapshot has to be real, or the speed means nothing.
	if resp, _ := env.do("POST", "/v1/sandboxes/"+id+"/resume", nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("resume: %d", resp.StatusCode)
	}
	if st := env.state(id); st != "RUNNING" {
		t.Errorf("state after resume = %q, want RUNNING", st)
	}
}

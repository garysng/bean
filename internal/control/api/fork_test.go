package api

import (
	"bytes"
	"net/http"
	"strings"
	"testing"

	"github.com/garysng/bean/internal/control/store"
)

// writeFile puts content in a sandbox through the API, so a test's setup goes
// through the same path a caller would use.
func (e *testEnv) writeFile(id, path, content string) {
	e.T.Helper()
	req, err := http.NewRequest("PUT",
		e.Server.URL+"/v1/sandboxes/"+id+"/files?mkdirs=true&path="+path,
		strings.NewReader(content))
	if err != nil {
		e.T.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.T.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e.T.Fatalf("write %s to %s: status %d", path, id, resp.StatusCode)
	}
}

// readFile reads a file out of a sandbox through the API.
func (e *testEnv) readFile(id, path string) string {
	e.T.Helper()
	req, err := http.NewRequest("GET",
		e.Server.URL+"/v1/sandboxes/"+id+"/files?path="+path, nil)
	if err != nil {
		e.T.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.T.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	return buf.String()
}

// forkIDs forks a sandbox and returns the children's ids, requiring full success.
func (e *testEnv) forkIDs(id string, body map[string]any) []string {
	e.T.Helper()
	resp, out := e.do("POST", "/v1/sandboxes/"+id+"/fork", body)
	if resp.StatusCode != http.StatusCreated {
		e.T.Fatalf("fork %s: %d %v", id, resp.StatusCode, out)
	}
	raw, ok := out["forkIds"].([]any)
	if !ok {
		e.T.Fatalf("fork %s: no forkIds in %v", id, out)
	}
	ids := make([]string, len(raw))
	for i, v := range raw {
		ids[i] = v.(string)
	}
	return ids
}

func TestForkProducesUsableCopyAndLeavesSourceRunning(t *testing.T) {
	env := startEnv(t, envOpts{})
	srcID := env.sandboxID(map[string]any{"imageRef": "python:3.12"})
	env.writeFile(srcID, "/work/setup.txt", "environment-is-ready")

	resp, out := env.do("POST", "/v1/sandboxes/"+srcID+"/fork", nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("fork status = %d: %v", resp.StatusCode, out)
	}
	if out["sourceId"] != srcID {
		t.Errorf("sourceId = %v, want %s", out["sourceId"], srcID)
	}
	ids := out["forkIds"].([]any)
	if len(ids) != 1 {
		t.Fatalf("forkIds = %v, want one child by default", ids)
	}
	childID := ids[0].(string)
	if childID == srcID {
		t.Fatal("fork returned the source's own id")
	}

	// The child carries the state the source had when it was forked.
	if got := env.readFile(childID, "/work/setup.txt"); got != "environment-is-ready" {
		t.Errorf("child file = %q, want the source's content", got)
	}
	// The source is still running. A fork that stopped what it forked from would
	// be a move, and every use for this needs the original to survive.
	if st := env.state(srcID); st != "RUNNING" {
		t.Errorf("source state = %q, want RUNNING", st)
	}
	// The child names what it came from, which is the only surviving trace: the
	// checkpoint is gone by now.
	sb := out["sandboxes"].([]any)[0].(map[string]any)
	labels := sb["labels"].(map[string]any)
	if labels["bean.fork.source"] != srcID {
		t.Errorf("fork source label = %v, want %s", labels["bean.fork.source"], srcID)
	}
}

// TestForkTwiceYieldsIndependentChildren is the property the whole feature rests
// on: children share the immutable parts of a checkpoint but write privately, so
// one child's writes are invisible to its sibling and to the source.
func TestForkTwiceYieldsIndependentChildren(t *testing.T) {
	env := startEnv(t, envOpts{})
	srcID := env.sandboxID(map[string]any{"imageRef": "base:1"})
	env.writeFile(srcID, "/work/marker.txt", "original")

	// Two separate fork calls rather than one call for two, so the shared state
	// is a checkpoint taken twice from the same source -- the case where a stale
	// or shared writable layer would show up.
	firstID := env.forkIDs(srcID, nil)[0]
	secondID := env.forkIDs(srcID, nil)[0]
	if firstID == secondID {
		t.Fatal("two forks produced the same sandbox id")
	}

	// Each child starts from the same content.
	for _, id := range []string{firstID, secondID} {
		if got := env.readFile(id, "/work/marker.txt"); got != "original" {
			t.Fatalf("child %s starts at %q, want \"original\"", id, got)
		}
	}

	// Each child writes something different to the same path.
	env.writeFile(firstID, "/work/marker.txt", "first-child")
	env.writeFile(secondID, "/work/marker.txt", "second-child")

	// Nobody sees anybody else's write.
	if got := env.readFile(firstID, "/work/marker.txt"); got != "first-child" {
		t.Errorf("first child = %q, want its own write", got)
	}
	if got := env.readFile(secondID, "/work/marker.txt"); got != "second-child" {
		t.Errorf("second child = %q; siblings must not share a writable layer", got)
	}
	if got := env.readFile(srcID, "/work/marker.txt"); got != "original" {
		t.Errorf("source = %q; a fork must not be able to write back into its source", got)
	}

	// A file only one child creates does not appear in the other.
	env.writeFile(firstID, "/work/only-first.txt", "x")
	if got := env.readFile(secondID, "/work/only-first.txt"); got == "x" {
		t.Error("second child sees a file created only in the first")
	}
}

func TestForkCountProducesThatManyChildren(t *testing.T) {
	env := startEnv(t, envOpts{})
	srcID := env.sandboxID(map[string]any{"imageRef": "base:1"})
	env.writeFile(srcID, "/work/shared.txt", "prepared-once")

	ids := env.forkIDs(srcID, map[string]any{"count": 4})
	if len(ids) != 4 {
		t.Fatalf("forkIds = %v, want 4", ids)
	}
	distinct := map[string]bool{}
	for _, id := range ids {
		distinct[id] = true
	}
	if len(distinct) != 4 {
		t.Errorf("ids not distinct: %v", ids)
	}
	// Every child got the prepared state, from one checkpoint.
	for _, id := range ids {
		if got := env.readFile(id, "/work/shared.txt"); got != "prepared-once" {
			t.Errorf("child %s = %q, want the prepared content", id, got)
		}
	}
	if st := env.state(srcID); st != "RUNNING" {
		t.Errorf("source state = %q after fan-out, want RUNNING", st)
	}
}

// TestForkLeavesNoSnapshotBehind pins the decision that the intermediate
// checkpoint is internal. A caller who forks repeatedly must not accumulate
// snapshot records to reap.
func TestForkLeavesNoSnapshotBehind(t *testing.T) {
	env := startEnv(t, envOpts{})
	srcID := env.sandboxID(map[string]any{"imageRef": "base:1"})

	for i := 0; i < 3; i++ {
		env.forkIDs(srcID, nil)
	}

	_, out := env.do("GET", "/v1/snapshots", nil)
	if snaps := out["snapshots"].([]any); len(snaps) != 0 {
		t.Errorf("fork left %d snapshot record(s) behind: %v", len(snaps), snaps)
	}
	// The blobs are gone too, not just the records.
	all, err := env.Store.ListSnapshots("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Errorf("store still holds %d snapshot(s)", len(all))
	}
}

// TestForkDoesNotDisturbExplicitSnapshots checks the internal checkpoint's
// cleanup is targeted: a snapshot the caller asked for survives a later fork.
func TestForkDoesNotDisturbExplicitSnapshots(t *testing.T) {
	env := startEnv(t, envOpts{})
	srcID := env.sandboxID(map[string]any{"imageRef": "base:1"})

	_, out := env.do("POST", "/v1/sandboxes/"+srcID+"/snapshot",
		map[string]any{"name": "keep-me"})
	snapID := out["snapshotId"].(string)

	env.forkIDs(srcID, nil)

	resp, _ := env.do("GET", "/v1/snapshots/"+snapID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("explicit snapshot gone after fork: %d", resp.StatusCode)
	}
}

func TestForkRejectsStoppedSource(t *testing.T) {
	env := startEnv(t, envOpts{})
	srcID := env.sandboxID(map[string]any{"imageRef": "base:1"})
	if resp, _ := env.do("DELETE", "/v1/sandboxes/"+srcID, nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("kill source: %d", resp.StatusCode)
	}

	resp, out := env.do("POST", "/v1/sandboxes/"+srcID+"/fork", nil)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("fork of stopped sandbox = %d, want 409: %v", resp.StatusCode, out)
	}
	if code := out["error"].(map[string]any)["code"]; code != "SANDBOX_NOT_RUNNING" {
		t.Errorf("code = %v, want SANDBOX_NOT_RUNNING", code)
	}
}

func TestForkOfPausedSourceIsAllowed(t *testing.T) {
	// A paused guest's memory is intact, which is the whole input to a fork, so
	// pausing first is a legitimate way to branch from a quiescent instant.
	env := startEnv(t, envOpts{})
	srcID := env.sandboxID(map[string]any{"imageRef": "base:1"})
	env.writeFile(srcID, "/work/f.txt", "paused-state")
	if resp, _ := env.do("POST", "/v1/sandboxes/"+srcID+"/pause", nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("pause: %d", resp.StatusCode)
	}

	ids := env.forkIDs(srcID, nil)
	if got := env.readFile(ids[0], "/work/f.txt"); got != "paused-state" {
		t.Errorf("child of paused source = %q", got)
	}
	// Pausing is not undone by forking: the source is left as it was found.
	if st := env.state(srcID); st != "PAUSED" {
		t.Errorf("source state = %q, want PAUSED", st)
	}
}

func TestForkRejectsBadCount(t *testing.T) {
	env := startEnv(t, envOpts{})
	srcID := env.sandboxID(map[string]any{"imageRef": "base:1"})

	for _, count := range []int{-1, maxForkCount + 1} {
		resp, out := env.do("POST", "/v1/sandboxes/"+srcID+"/fork",
			map[string]any{"count": count})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("count=%d: status %d, want 400: %v", count, resp.StatusCode, out)
		}
	}
}

func TestForkOfMissingSandbox(t *testing.T) {
	env := startEnv(t, envOpts{})
	resp, out := env.do("POST", "/v1/sandboxes/sbx_nope/fork", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %v", resp.StatusCode, out)
	}
	if code := out["error"].(map[string]any)["code"]; code != "SANDBOX_NOT_FOUND" {
		t.Errorf("code = %v", code)
	}
}

func TestForkDisabledWithoutSnapshotStorage(t *testing.T) {
	// Forking needs somewhere to put the intermediate checkpoint, so without
	// storage it refuses rather than pretending.
	env := startEnv(t, envOpts{WithoutSnapshots: true})
	srcID := env.sandboxID(map[string]any{"imageRef": "base:1"})
	resp, _ := env.do("POST", "/v1/sandboxes/"+srcID+"/fork", nil)
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}

func TestForkChildInheritsResourcesAndLabels(t *testing.T) {
	env := startEnv(t, envOpts{})
	srcID := env.sandboxID(map[string]any{
		"imageRef":     "base:1",
		"resources": map[string]any{"cpu": 2, "memoryMiB": 1024},
		"labels":    map[string]string{"suite": "fork", "eval-run": "run-7"},
	})

	ids := env.forkIDs(srcID, map[string]any{
		"labels": map[string]string{"child": "yes"},
	})

	child, err := env.Store.GetSandbox(ids[0])
	if err != nil {
		t.Fatal(err)
	}
	// Resources are inherited, not defaulted: a child runs the same guest, so a
	// smaller allocation would place and then fail to hold the checkpoint.
	if child.CPU != 2 || child.MemoryMiB != 1024 {
		t.Errorf("child resources = cpu %v mem %v, want the source's 2/1024",
			child.CPU, child.MemoryMiB)
	}
	// Source labels carry over, and the request's labels are added on top. The
	// inherited "eval-run" is what the scheduler spreads on, so a caller who
	// groups a run keeps that grouping in its forks.
	if child.Labels["suite"] != "fork" || child.Labels["eval-run"] != "run-7" {
		t.Errorf("child labels = %v, want the source's inherited", child.Labels)
	}
	if child.Labels["child"] != "yes" {
		t.Errorf("request labels not applied: %v", child.Labels)
	}
}

// TestForkPartialFailureReportsWhichChildrenFailed pins the decision not to roll
// back: children that did start are usable and are kept.
func TestForkPartialFailureReportsWhichChildrenFailed(t *testing.T) {
	// Capacity for the source plus two children and no more, so a fork of four
	// partially succeeds.
	env := startEnv(t, envOpts{CPUPerNode: 3, MemoryPerNode: 3 * 512})
	srcID := env.sandboxID(map[string]any{"imageRef": "base:1"})

	resp, out := env.do("POST", "/v1/sandboxes/"+srcID+"/fork",
		map[string]any{"count": 4})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("status = %d, want 207: %v", resp.StatusCode, out)
	}
	started := out["forkIds"].([]any)
	failures := out["failures"].([]any)
	if len(started) == 0 {
		t.Fatal("no child started; expected a partial success")
	}
	if len(started)+len(failures) != 4 {
		t.Errorf("started %d + failed %d != 4", len(started), len(failures))
	}
	// The failures name the capacity problem rather than a generic error, and say
	// which child hit it.
	first := failures[0].(map[string]any)
	if first["code"] != "NO_CAPACITY" {
		t.Errorf("failure code = %v, want NO_CAPACITY", first["code"])
	}
	if _, ok := first["index"]; !ok {
		t.Error("failure does not say which child it was")
	}
	// The children that did start are real and were not rolled back.
	for _, raw := range started {
		id := raw.(string)
		if rec, err := env.Store.GetSandbox(id); err != nil || rec == nil {
			t.Errorf("started child %s not in the store (err=%v)", id, err)
		} else if store.IsTerminal(rec.State) {
			t.Errorf("started child %s was rolled back to %s", id, rec.State)
		}
	}
	// Even a partial fork cleans up its checkpoint.
	all, err := env.Store.ListSnapshots("", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Errorf("partial fork left %d snapshot(s)", len(all))
	}
}

// TestForkFailsWhenNoChildCanStart checks the all-failed case reports the
// children's own reason rather than burying it under a generic error.
func TestForkFailsWhenNoChildCanStart(t *testing.T) {
	// Room for the source only.
	env := startEnv(t, envOpts{CPUPerNode: 1, MemoryPerNode: 512})
	srcID := env.sandboxID(map[string]any{"imageRef": "base:1"})

	resp, out := env.do("POST", "/v1/sandboxes/"+srcID+"/fork", nil)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %v", resp.StatusCode, out)
	}
	e := out["error"].(map[string]any)
	if e["code"] != "NO_CAPACITY" {
		t.Errorf("code = %v, want the child's own NO_CAPACITY", e["code"])
	}
	// The source survives a fork that produced nothing.
	if st := env.state(srcID); st != "RUNNING" {
		t.Errorf("source state = %q after a failed fork, want RUNNING", st)
	}
}

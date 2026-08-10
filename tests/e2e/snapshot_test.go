//go:build e2e

package e2e

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// TestE2ESnapshotRestore exercises the flow the design exists for, against
// real bean-api and noded processes: set up an environment once, capture it,
// recreate it, and confirm the clone carries the state forward.
func TestE2ESnapshotRestore(t *testing.T) {
	// 1. Create and populate a sandbox.
	code, out := api(t, "POST", "/v1/sandboxes", map[string]any{
		"imageRef": "python:3.12",
		"labels":   map[string]string{"suite": "snapshot"},
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	srcID := out["sandbox"].(map[string]any)["id"].(string)

	if _, err := putFile(t, srcID, "/work/env.txt", "provisioned"); err != nil {
		t.Fatal(err)
	}

	// 2. Snapshot.
	code, out = api(t, "POST", "/v1/sandboxes/"+srcID+"/snapshot", map[string]any{
		"name": "e2e-after-setup",
	})
	if code != http.StatusAccepted {
		t.Fatalf("snapshot: %d %v", code, out)
	}
	snapID := out["snapshotId"].(string)
	if snap := out["snapshot"].(map[string]any); snap["state"] != "READY" {
		t.Fatalf("snapshot state = %v", snap["state"])
	}

	// 3. Restore into a new sandbox.
	code, out = api(t, "POST", "/v1/sandboxes", map[string]any{"snapshot": snapID})
	if code != http.StatusCreated {
		t.Fatalf("restore: %d %v", code, out)
	}
	dstID := out["sandbox"].(map[string]any)["id"].(string)

	// 4. The provisioned state is present, verified through exec so the
	// whole data path (gateway -> node -> in-sandbox daemon) is involved.
	code, out = api(t, "POST", "/v1/sandboxes/"+dstID+"/exec", map[string]any{
		"cmd": []string{"cat", "work/env.txt"},
	})
	if code != http.StatusOK {
		t.Fatalf("exec on restored sandbox: %d %v", code, out)
	}
	if got := strings.TrimSpace(out["stdout"].(string)); got != "provisioned" {
		t.Errorf("restored content = %q, want provisioned", got)
	}

	// 5. Snapshot metadata is queryable and deletable.
	code, out = api(t, "GET", "/v1/snapshots/"+snapID, nil)
	if code != http.StatusOK {
		t.Fatalf("get snapshot: %d %v", code, out)
	}
	if snap := out["snapshot"].(map[string]any); snap["sandboxId"] != srcID {
		t.Errorf("sandboxId = %v, want %s", snap["sandboxId"], srcID)
	}
	if code, _ := api(t, "DELETE", "/v1/snapshots/"+snapID, nil); code != http.StatusNoContent {
		t.Errorf("delete snapshot = %d", code)
	}

	// Clean up the sandboxes.
	api(t, "DELETE", "/v1/sandboxes/"+srcID, nil)
	api(t, "DELETE", "/v1/sandboxes/"+dstID, nil)
}

// TestE2EImageAndRegistry covers the image metadata surface end to end.
func TestE2EImageAndRegistry(t *testing.T) {
	// Creating a sandbox registers its image.
	code, out := api(t, "POST", "/v1/sandboxes", map[string]any{"imageRef": "busybox:1.36"})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	id := out["sandbox"].(map[string]any)["id"].(string)
	defer api(t, "DELETE", "/v1/sandboxes/"+id, nil)

	code, out = api(t, "GET", "/v1/images/status?ref=busybox%3A1.36", nil)
	if code != http.StatusOK {
		t.Fatalf("image status: %d %v", code, out)
	}
	if out["ref"] != "busybox:1.36" {
		t.Errorf("ref = %v", out["ref"])
	}
	// Nothing has been converted, so only the standard pull path applies.
	if out["format"] != "oci" {
		t.Errorf("format = %v, want oci", out["format"])
	}

	// Prewarm accepts a batch and reports a job.
	code, out = api(t, "POST", "/v1/images/prewarm", map[string]any{
		"refs": []string{"busybox:1.36", "alpine:3.20"}, "targetNodes": 1,
	})
	if code != http.StatusAccepted {
		t.Fatalf("prewarm: %d %v", code, out)
	}
	jobID := out["jobId"].(string)
	code, out = api(t, "GET", "/v1/images/prewarm/"+jobID, nil)
	if code != http.StatusOK {
		t.Fatalf("prewarm status: %d %v", code, out)
	}
	if refs := out["refs"].([]any); len(refs) != 2 {
		t.Errorf("refs = %v", refs)
	}
}

// putFile writes a file into a sandbox through the gateway.
func putFile(t *testing.T, sandboxID, path, content string) (int, error) {
	t.Helper()
	req, err := http.NewRequest("PUT",
		apiURL+"/v1/sandboxes/"+sandboxID+"/files?mkdirs=true&path="+path,
		strings.NewReader(content))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var body bytes.Buffer
	body.ReadFrom(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put file %s: %d %s", path, resp.StatusCode, body.String())
	}
	return resp.StatusCode, nil
}

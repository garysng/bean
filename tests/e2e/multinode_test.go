//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	mnAPIKey    = "bk_mn_key"
	mnNodeToken = "nt_mn_token"
	mnBootstrap = "boot_mn_token"
)

// TestE2EMultiNode runs a real multi-node stack: one bean-api in
// multi-node mode plus two noded processes that register themselves,
// then verifies placement spread and that exec reaches the right node.
func TestE2EMultiNode(t *testing.T) {
	dir, err := os.MkdirTemp("", "bean-mn")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)

	agentBin := filepath.Join(binDir, "beand")
	nodedBin := filepath.Join(binDir, "noded")
	apiBin := filepath.Join(binDir, "bean-api")
	for _, b := range []string{agentBin, nodedBin, apiBin} {
		if _, err := os.Stat(b); err != nil {
			t.Fatalf("binary missing (TestMain should have built it): %s", b)
		}
	}

	httpPort, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	nodeGRPCPort, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	apiURL := fmt.Sprintf("http://127.0.0.1:%d", httpPort)

	var procs []*exec.Cmd
	defer func() {
		for _, p := range procs {
			if p.Process != nil {
				_ = p.Process.Signal(syscall.SIGTERM)
			}
		}
		for _, p := range procs {
			done := make(chan struct{})
			go func(c *exec.Cmd) { c.Wait(); close(done) }(p)
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				_ = p.Process.Kill()
			}
		}
	}()

	api := exec.Command(apiBin,
		"--listen", fmt.Sprintf("127.0.0.1:%d", httpPort),
		"--node-grpc", fmt.Sprintf("127.0.0.1:%d", nodeGRPCPort),
		"--region", "r1",
		"--db", filepath.Join(dir, "mn.db"),
		"--api-key", mnAPIKey,
		"--node-token", mnNodeToken,
		"--bootstrap-token", mnBootstrap,
		"--runtime-tier", "local")
	api.Stdout, api.Stderr = os.Stderr, os.Stderr
	if err := api.Start(); err != nil {
		t.Fatal(err)
	}
	procs = append(procs, api)

	waitHealthy(t, apiURL, 15*time.Second)

	// Two nodes register themselves with the control plane.
	for i := 0; i < 2; i++ {
		port, err := freePort()
		if err != nil {
			t.Fatal(err)
		}
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		nd := exec.Command(nodedBin,
			"--listen", addr,
			"--runtime", "local",
			"--agent-bin", agentBin,
			"--base-dir", filepath.Join(dir, fmt.Sprintf("sbx-%d", i)),
			"--node-token", mnNodeToken,
			"--control-plane", fmt.Sprintf("127.0.0.1:%d", nodeGRPCPort),
			"--node-id", fmt.Sprintf("node-%d", i),
			"--region", "r1",
			"--bootstrap-token", mnBootstrap,
			"--advertise", addr,
			"--cpu", "2", "--memory-mib", "2048")
		nd.Stdout, nd.Stderr = os.Stderr, os.Stderr
		if err := nd.Start(); err != nil {
			t.Fatal(err)
		}
		procs = append(procs, nd)
	}

	// Wait until both nodes can actually host sandboxes.
	placed := map[string]string{} // sandboxID -> nodeID
	deadline := time.Now().Add(30 * time.Second)
	for len(placed) < 4 {
		code, out := mnReq(t, apiURL, "POST", "/v1/sandboxes", map[string]any{
			"image":     "img:1",
			"resources": map[string]any{"cpu": 1, "memoryMiB": 512},
			"labels":    map[string]string{"eval-run": "mn"},
		})
		if code == http.StatusCreated {
			sb := out["sandbox"].(map[string]any)
			placed[sb["id"].(string)] = sb["nodeId"].(string)
			continue
		}
		if time.Now().After(deadline) {
			t.Fatalf("could not place 4 sandboxes across 2 nodes (got %d): last=%d %v",
				len(placed), code, out)
		}
		time.Sleep(500 * time.Millisecond)
	}

	nodesUsed := map[string]int{}
	for _, n := range placed {
		nodesUsed[n]++
	}
	if len(nodesUsed) != 2 {
		t.Errorf("sandboxes landed on %d nodes, want 2: %v", len(nodesUsed), nodesUsed)
	}

	// Each sandbox must be reachable through the gateway, which proves the
	// router resolved the owning node for every one of them.
	for id := range placed {
		code, out := mnReq(t, apiURL, "POST", "/v1/sandboxes/"+id+"/exec", map[string]any{
			"cmd": []string{"echo", id},
		})
		if code != http.StatusOK {
			t.Fatalf("exec on %s (node %s): %d %v", id, placed[id], code, out)
		}
		if got := strings.TrimSpace(out["stdout"].(string)); got != id {
			t.Errorf("sandbox %s routed to wrong place, stdout=%q", id, got)
		}
	}

	// Capacity is exhausted (2 nodes x 2 CPU, 1 CPU each): expect 503.
	code, out := mnReq(t, apiURL, "POST", "/v1/sandboxes", map[string]any{
		"image": "img:1", "resources": map[string]any{"cpu": 1, "memoryMiB": 512},
	})
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 once both nodes are committed: %v", code, out)
	}

	// Freeing one sandbox admits another.
	var freed string
	for id := range placed {
		freed = id
		break
	}
	if code, _ := mnReq(t, apiURL, "DELETE", "/v1/sandboxes/"+freed, nil); code != http.StatusAccepted {
		t.Fatalf("delete status = %d", code)
	}
	if code, out := mnReq(t, apiURL, "POST", "/v1/sandboxes", map[string]any{
		"image": "img:1", "resources": map[string]any{"cpu": 1, "memoryMiB": 512},
	}); code != http.StatusCreated {
		t.Errorf("create after delete: %d %v", code, out)
	}
}

func waitHealthy(t *testing.T, baseURL string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s not healthy within %s", baseURL, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func mnReq(t *testing.T, baseURL, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, baseURL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+mnAPIKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

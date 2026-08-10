package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func TestMultiNodePlacementSpreadsAndExecs(t *testing.T) {
	env := startEnv(t, envOpts{Nodes: 3, CPUPerNode: 4, MemoryPerNode: 4096})
	sched, ids := env.Sched, env.NodeIDs

	placed := map[string]int{}
	var sandboxIDs []string
	for i := 0; i < 6; i++ {
		resp, out := env.do("POST", "/v1/sandboxes", map[string]any{
			"imageRef":     "img:1",
			"resources": map[string]any{"cpu": 1, "memoryMiB": 512},
			"labels":    map[string]string{"eval-run": "r1"},
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %d: %d %v", i, resp.StatusCode, out)
		}
		sb := out["sandbox"].(map[string]any)
		placed[sb["nodeId"].(string)]++
		sandboxIDs = append(sandboxIDs, sb["id"].(string))
	}
	if len(placed) != len(ids) {
		t.Errorf("sandboxes landed on %d nodes, want %d (spread): %v", len(placed), len(ids), placed)
	}

	// Every sandbox must be reachable through the gateway, which means the
	// router picked the right node for each one.
	for _, id := range sandboxIDs {
		_, out := env.do("POST", "/v1/sandboxes/"+id+"/exec", map[string]any{
			"cmd": []string{"echo", id},
		})
		if got := strings.TrimSpace(out["stdout"].(string)); got != id {
			t.Errorf("sandbox %s: exec routed wrong, stdout=%q", id, got)
		}
	}

	// Committed capacity is reflected in the durable accounting.
	nodes, err := sched.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	var totalCPU float64
	for _, n := range nodes {
		totalCPU += n.CPUCommitted
	}
	if totalCPU != 6 {
		t.Errorf("committed cpu = %.1f, want 6", totalCPU)
	}
}

func TestMultiNodeDeleteReleasesCapacity(t *testing.T) {
	env := startEnv(t, envOpts{Nodes: 1, CPUPerNode: 4, MemoryPerNode: 4096})
	sched := env.Sched

	// Fill the single node (4 CPU) exactly.
	var ids []string
	for i := 0; i < 4; i++ {
		resp, out := env.do("POST", "/v1/sandboxes", map[string]any{
			"imageRef": "img:1", "resources": map[string]any{"cpu": 1, "memoryMiB": 512},
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %d: %d %v", i, resp.StatusCode, out)
		}
		ids = append(ids, out["sandbox"].(map[string]any)["id"].(string))
	}

	// Node is committed to capacity: the next create must be rejected.
	resp, out := env.do("POST", "/v1/sandboxes", map[string]any{
		"imageRef": "img:1", "resources": map[string]any{"cpu": 1, "memoryMiB": 512},
	})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 NO_CAPACITY: %v", resp.StatusCode, out)
	}
	if code := out["error"].(map[string]any)["code"]; code != "NO_CAPACITY" {
		t.Errorf("code = %v", code)
	}

	// Deleting one frees capacity for another.
	if resp, _ := env.do("DELETE", "/v1/sandboxes/"+ids[0], nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	nodes, err := sched.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	if got := nodes[0].CPUCommitted; got != 3 {
		t.Errorf("committed after delete = %.1f, want 3", got)
	}
	if resp, out := env.do("POST", "/v1/sandboxes", map[string]any{
		"imageRef": "img:1", "resources": map[string]any{"cpu": 1, "memoryMiB": 512},
	}); resp.StatusCode != http.StatusCreated {
		t.Errorf("create after delete: %d %v", resp.StatusCode, out)
	}
}

func TestUnreachableNodeSurfacesAndReleasesCapacity(t *testing.T) {
	// A node the scheduler knows about but cannot be dialled must surface as
	// NODE_UNREACHABLE, and its reserved capacity must come back.
	env := startEnv(t, envOpts{Nodes: 1, CPUPerNode: 4, MemoryPerNode: 4096})
	env.resolver.mu.Lock()
	delete(env.resolver.addrs, env.NodeIDs[0])
	env.resolver.mu.Unlock()

	resp, out := env.do("POST", "/v1/sandboxes", map[string]any{"imageRef": "img:1"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %v", resp.StatusCode, out)
	}
	if code := out["error"].(map[string]any)["code"]; code != "NODE_UNREACHABLE" {
		t.Errorf("code = %v", code)
	}
	// The sandbox is recorded FAILED rather than left PENDING.
	_, list := env.do("GET", "/v1/sandboxes", nil)
	sbs := list["sandboxes"].([]any)
	if len(sbs) != 1 || sbs[0].(map[string]any)["state"] != "FAILED" {
		t.Errorf("sandboxes = %v", sbs)
	}
	// Capacity was released, so the node is not permanently poisoned.
	nodes, err := env.Sched.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	if got := nodes[0].CPUCommitted; got != 0 {
		t.Errorf("committed = %.1f, want 0 after a failed create", got)
	}
}

func TestNodeRouterReusesConnections(t *testing.T) {
	res := &mapResolver{addrs: map[string]string{"n1": startTestNode(t)}}
	r := NewNodeRouter(res, "")
	defer r.Close()

	c1, err := r.Client("n1")
	if err != nil {
		t.Fatal(err)
	}
	c2, err := r.Client("n1")
	if err != nil {
		t.Fatal(err)
	}
	if c1 == nil || c2 == nil {
		t.Fatal("nil client")
	}
	if got := len(r.conns); got != 1 {
		t.Errorf("pooled connections = %d, want 1", got)
	}

	if _, err := r.Client(""); err == nil {
		t.Error("empty node id should error")
	}
	if _, err := r.Client("unknown"); err == nil {
		t.Error("unknown node should error")
	}

	r.Evict("n1")
	if got := len(r.conns); got != 0 {
		t.Errorf("connections after evict = %d", got)
	}
	// Re-dialing after eviction works.
	if _, err := r.Client("n1"); err != nil {
		t.Errorf("client after evict: %v", err)
	}
}

func TestNodeRouterConcurrentClients(t *testing.T) {
	res := &mapResolver{addrs: map[string]string{"n1": startTestNode(t)}}
	r := NewNodeRouter(res, "")
	defer r.Close()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Client("n1"); err != nil {
				t.Errorf("client: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := len(r.conns); got != 1 {
		t.Errorf("pooled connections = %d, want 1 (no dial race)", got)
	}
}

// sandbox records must expose nodeId so clients can see placement.
func TestSandboxRecordIncludesNodeID(t *testing.T) {
	env := startEnv(t, envOpts{Nodes: 2})
	sb := env.createSandbox(nil)
	if sb["nodeId"] == nil || sb["nodeId"] == "" {
		b, _ := json.Marshal(sb)
		t.Errorf("nodeId missing from record: %s", b)
	}
}

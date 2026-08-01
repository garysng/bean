package api

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc"

	"github.com/garysng/bean/internal/control/scheduler"
	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node"
	"github.com/garysng/bean/internal/node/runtime"
)

// mapResolver is a static NodeResolver for tests.
type mapResolver struct {
	mu    sync.Mutex
	addrs map[string]string
}

func (m *mapResolver) NodeAddr(id string) (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.addrs[id]
	return a, ok
}

func (m *mapResolver) set(id, addr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.addrs[id] = addr
}

// startNode brings up one noded (local runtime) and returns its address.
func startNode(t *testing.T) string {
	t.Helper()
	mgr := node.NewManager(runtime.NewLocalRuntime(agentBin, t.TempDir()))
	t.Cleanup(mgr.Close)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	nodev1.RegisterSandboxServiceServer(srv, node.NewGRPCServer(mgr))
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

// startMultiNodeStack wires a gateway with a scheduler across n nodes.
func startMultiNodeStack(t *testing.T, n int) (*httptest.Server, *scheduler.Scheduler, []string) {
	t.Helper()
	res := &mapResolver{addrs: map[string]string{}}
	sched := scheduler.New(scheduler.DefaultWeights())
	var ids []string
	for i := 0; i < n; i++ {
		id := "node-" + string(rune('a'+i))
		res.set(id, startNode(t))
		sched.Register(&scheduler.Node{
			ID: id, Region: "r1", Runtimes: []string{"local"},
			CPUAllocatable: 4, MemoryMiBAllocate: 4096, DiskMiBAllocate: 1 << 20,
			CachedImages: map[string]int64{},
		})
		ids = append(ids, id)
	}

	st, err := store.Open(filepath.Join(t.TempDir(), "mn.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	router := NewNodeRouter(res, "")
	t.Cleanup(router.Close)
	srv := NewServerWithRouter(st, router, sched, "", "r1", testKey)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, sched, ids
}

func TestMultiNodePlacementSpreadsAndExecs(t *testing.T) {
	ts, sched, ids := startMultiNodeStack(t, 3)

	placed := map[string]int{}
	var sandboxIDs []string
	for i := 0; i < 6; i++ {
		resp, out := doReq(t, ts, "POST", "/v1/sandboxes", map[string]any{
			"image":     "img:1",
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
		_, out := doReq(t, ts, "POST", "/v1/sandboxes/"+id+"/exec", map[string]any{
			"cmd": []string{"echo", id},
		})
		if got := strings.TrimSpace(out["stdout"].(string)); got != id {
			t.Errorf("sandbox %s: exec routed wrong, stdout=%q", id, got)
		}
	}

	// Committed capacity is reflected in the scheduler.
	var totalCPU float64
	for _, n := range sched.Nodes() {
		totalCPU += n.CPUCommitted
	}
	if totalCPU != 6 {
		t.Errorf("committed cpu = %.1f, want 6", totalCPU)
	}
}

func TestMultiNodeDeleteReleasesCapacity(t *testing.T) {
	ts, sched, _ := startMultiNodeStack(t, 1)

	// Fill the single node (4 CPU) exactly.
	var ids []string
	for i := 0; i < 4; i++ {
		resp, out := doReq(t, ts, "POST", "/v1/sandboxes", map[string]any{
			"image": "img:1", "resources": map[string]any{"cpu": 1, "memoryMiB": 512},
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("create %d: %d %v", i, resp.StatusCode, out)
		}
		ids = append(ids, out["sandbox"].(map[string]any)["id"].(string))
	}

	// Node is committed to capacity: the next create must be rejected.
	resp, out := doReq(t, ts, "POST", "/v1/sandboxes", map[string]any{
		"image": "img:1", "resources": map[string]any{"cpu": 1, "memoryMiB": 512},
	})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 NO_CAPACITY: %v", resp.StatusCode, out)
	}
	if code := out["error"].(map[string]any)["code"]; code != "NO_CAPACITY" {
		t.Errorf("code = %v", code)
	}

	// Deleting one frees capacity for another.
	if resp, _ := doReq(t, ts, "DELETE", "/v1/sandboxes/"+ids[0], nil); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	if got := sched.Nodes()[0].CPUCommitted; got != 3 {
		t.Errorf("committed after delete = %.1f, want 3", got)
	}
	if resp, out := doReq(t, ts, "POST", "/v1/sandboxes", map[string]any{
		"image": "img:1", "resources": map[string]any{"cpu": 1, "memoryMiB": 512},
	}); resp.StatusCode != http.StatusCreated {
		t.Errorf("create after delete: %d %v", resp.StatusCode, out)
	}
}

func TestMultiNodeUnreachableNode(t *testing.T) {
	// A node the scheduler knows about but whose address is unknown must
	// surface as NODE_UNREACHABLE rather than a generic 500.
	res := &mapResolver{addrs: map[string]string{}}
	sched := scheduler.New(scheduler.DefaultWeights())
	sched.Register(&scheduler.Node{
		ID: "ghost", Region: "r1", Runtimes: []string{"local"},
		CPUAllocatable: 4, MemoryMiBAllocate: 4096, DiskMiBAllocate: 1 << 20,
	})
	st, err := store.Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	router := NewNodeRouter(res, "")
	t.Cleanup(router.Close)
	ts := httptest.NewServer(NewServerWithRouter(st, router, sched, "", "r1", testKey).Handler())
	t.Cleanup(ts.Close)

	resp, out := doReq(t, ts, "POST", "/v1/sandboxes", map[string]any{"image": "img:1"})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %v", resp.StatusCode, out)
	}
	if code := out["error"].(map[string]any)["code"]; code != "NODE_UNREACHABLE" {
		t.Errorf("code = %v", code)
	}
	// The failed sandbox is recorded as FAILED, not left PENDING.
	_, list := doReq(t, ts, "GET", "/v1/sandboxes", nil)
	sbs := list["sandboxes"].([]any)
	if len(sbs) != 1 || sbs[0].(map[string]any)["state"] != "FAILED" {
		t.Errorf("sandboxes = %v", sbs)
	}
	// Capacity was returned.
	if got := sched.Nodes()[0].CPUCommitted; got != 0 {
		t.Errorf("committed = %.1f, want 0 after failed create", got)
	}
}

func TestNodeRouterReusesConnections(t *testing.T) {
	res := &mapResolver{addrs: map[string]string{"n1": startNode(t)}}
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
	res := &mapResolver{addrs: map[string]string{"n1": startNode(t)}}
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
	ts, _, _ := startMultiNodeStack(t, 2)
	_, out := doReq(t, ts, "POST", "/v1/sandboxes", map[string]any{"image": "img:1"})
	sb := out["sandbox"].(map[string]any)
	if sb["nodeId"] == nil || sb["nodeId"] == "" {
		b, _ := json.Marshal(sb)
		t.Errorf("nodeId missing from record: %s", b)
	}
}

package node

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node/runtime"
)

// fakeControlPlane is a minimal NodeService that records what nodes send.
type fakeControlPlane struct {
	nodev1.UnimplementedNodeServiceServer

	mu         sync.Mutex
	registered []*nodev1.RegisterRequest
	heartbeats []*nodev1.HeartbeatRequest
	expected   []*nodev1.SandboxSpec
	syncCalls  int
	hbSignal   chan struct{}
}

func newFakeCP(expected []*nodev1.SandboxSpec) *fakeControlPlane {
	return &fakeControlPlane{expected: expected, hbSignal: make(chan struct{}, 8)}
}

func (f *fakeControlPlane) Register(_ context.Context, req *nodev1.RegisterRequest) (*nodev1.RegisterResponse, error) {
	f.mu.Lock()
	f.registered = append(f.registered, req)
	f.mu.Unlock()
	return &nodev1.RegisterResponse{NodeToken: "tok-1", HeartbeatIntervalSeconds: 1}, nil
}

func (f *fakeControlPlane) SyncState(_ context.Context, req *nodev1.SyncStateRequest) (*nodev1.SyncStateResponse, error) {
	f.mu.Lock()
	f.syncCalls++
	f.mu.Unlock()
	return &nodev1.SyncStateResponse{Expected: f.expected}, nil
}

func (f *fakeControlPlane) Heartbeat(stream nodev1.NodeService_HeartbeatServer) error {
	for {
		req, err := stream.Recv()
		if err != nil {
			return nil
		}
		f.mu.Lock()
		f.heartbeats = append(f.heartbeats, req)
		f.mu.Unlock()
		select {
		case f.hbSignal <- struct{}{}:
		default:
		}
		if err := stream.Send(&nodev1.HeartbeatResponse{LeaseOk: true}); err != nil {
			return err
		}
	}
}

func (f *fakeControlPlane) snapshot() ([]*nodev1.RegisterRequest, []*nodev1.HeartbeatRequest, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*nodev1.RegisterRequest(nil), f.registered...),
		append([]*nodev1.HeartbeatRequest(nil), f.heartbeats...), f.syncCalls
}

func startFakeCP(t *testing.T, cp *fakeControlPlane) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	nodev1.RegisterNodeServiceServer(srv, cp)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func testRegistrar(mgr *Manager, addr string) *Registrar {
	return NewRegistrar(mgr, addr, "node-a", "r1", "boot-tok",
		map[string]string{"pool": "nvme"}, []string{"fc"},
		&nodev1.NodeResources{CpuAllocatable: 8, MemoryAllocatableMib: 8192, DiskSandboxesMib: 1000})
}

func TestRegistrarRegistersAndHeartbeats(t *testing.T) {
	mgr := newTestManager(t)
	if _, err := mgr.Create(context.Background(), spec("hb1")); err != nil {
		t.Fatal(err)
	}
	// Expected set includes the running sandbox, so nothing is reaped.
	cp := newFakeCP([]*nodev1.SandboxSpec{{SandboxId: "hb1"}})
	addr := startFakeCP(t, cp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go testRegistrar(mgr, addr).Run(ctx)

	select {
	case <-cp.hbSignal:
	case <-time.After(5 * time.Second):
		t.Fatal("no heartbeat received")
	}

	regs, hbs, syncs := cp.snapshot()
	if len(regs) != 1 {
		t.Fatalf("register calls = %d", len(regs))
	}
	r := regs[0]
	if r.NodeId != "node-a" || r.Region != "r1" || r.BootstrapToken != "boot-tok" {
		t.Errorf("register req = %+v", r)
	}
	if r.Capabilities.GetRuntimes()[0] != "fc" || r.Labels["pool"] != "nvme" {
		t.Errorf("capabilities/labels = %+v %+v", r.Capabilities, r.Labels)
	}
	if r.Resources.CpuAllocatable != 8 {
		t.Errorf("resources = %+v", r.Resources)
	}
	if syncs != 1 {
		t.Errorf("SyncState calls = %d, want 1", syncs)
	}
	if len(hbs) == 0 {
		t.Fatal("no heartbeat recorded")
	}
	hb := hbs[0]
	if hb.NodeToken != "tok-1" {
		t.Errorf("heartbeat token = %q, want the one issued by Register", hb.NodeToken)
	}
	if len(hb.Sandboxes) != 1 || hb.Sandboxes[0].SandboxId != "hb1" {
		t.Errorf("heartbeat sandboxes = %+v", hb.Sandboxes)
	}
	if hb.Usage == nil || hb.Usage.CpuCommitted != 1 {
		t.Errorf("usage = %+v, want cpu=1 from the running sandbox", hb.Usage)
	}
	// The sandbox is still alive: it was in the expected set.
	if mgr.StateOf("hb1") != runtime.StateRunning {
		t.Errorf("state = %s", mgr.StateOf("hb1"))
	}
}

func TestRegistrarReconcileDestroysOrphan(t *testing.T) {
	mgr := newTestManager(t)
	if _, err := mgr.Create(context.Background(), spec("orphan")); err != nil {
		t.Fatal(err)
	}
	// Control plane knows nothing about this sandbox -> it must be reaped.
	cp := newFakeCP(nil)
	addr := startFakeCP(t, cp)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go testRegistrar(mgr, addr).Run(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for mgr.Get("orphan") != nil {
		if time.Now().After(deadline) {
			t.Fatal("orphan sandbox was not reconciled away")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRegistrarRetriesAfterControlPlaneRestart(t *testing.T) {
	mgr := newTestManager(t)
	cp := newFakeCP(nil)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := lis.Addr().String()
	srv := grpc.NewServer()
	nodev1.RegisterNodeServiceServer(srv, cp)
	go srv.Serve(lis)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go testRegistrar(mgr, addr).Run(ctx)

	select {
	case <-cp.hbSignal:
	case <-time.After(5 * time.Second):
		t.Fatal("no initial heartbeat")
	}

	// Drop the control plane, then bring it back on the same address.
	srv.Stop()
	lis2, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("cannot rebind %s: %v", addr, err)
	}
	srv2 := grpc.NewServer()
	nodev1.RegisterNodeServiceServer(srv2, cp)
	go srv2.Serve(lis2)
	defer srv2.Stop()

	// Registrar must re-register and resume heartbeating.
	deadline := time.Now().Add(20 * time.Second)
	for {
		if regs, _, _ := cp.snapshot(); len(regs) >= 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("registrar did not re-register after control-plane restart")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func TestRegistrarRunFailsOnBadAddress(t *testing.T) {
	mgr := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Unroutable address: Run keeps retrying until ctx expires.
	err := testRegistrar(mgr, "192.0.2.1:7443").Run(ctx)
	if err == nil {
		t.Error("expected ctx error")
	}
}

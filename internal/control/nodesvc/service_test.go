package nodesvc

import (
	"context"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/garysng/bean/internal/control/scheduler"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
)

type stubLister struct{ specs []*nodev1.SandboxSpec }

func (s stubLister) ExpectedForNode(string) []*nodev1.SandboxSpec { return s.specs }

func start(t *testing.T, opts Options) (nodev1.NodeServiceClient, *scheduler.Scheduler, *Service) {
	t.Helper()
	sched := scheduler.New(scheduler.DefaultWeights())
	svc := New(sched, opts)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	nodev1.RegisterNodeServiceServer(srv, svc)
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return nodev1.NewNodeServiceClient(conn), sched, svc
}

func regReq(nodeID string) *nodev1.RegisterRequest {
	return &nodev1.RegisterRequest{
		BootstrapToken: "boot-tok",
		NodeId:         nodeID,
		Region:         "r1",
		Labels:         map[string]string{"pool": "nvme"},
		Capabilities:   &nodev1.NodeCapabilities{Runtimes: []string{"fc"}},
		Resources: &nodev1.NodeResources{
			CpuAllocatable: 8, MemoryAllocatableMib: 8192, DiskSandboxesMib: 100000,
		},
	}
}

func TestRegisterAddsNodeToScheduler(t *testing.T) {
	c, sched, _ := start(t, Options{BootstrapToken: "boot-tok", Lister: stubLister{}})
	resp, err := c.Register(context.Background(), regReq("n1"))
	if err != nil {
		t.Fatal(err)
	}
	if resp.NodeToken == "" {
		t.Error("no node token issued")
	}
	if resp.HeartbeatIntervalSeconds <= 0 {
		t.Error("heartbeat interval not advertised")
	}
	nodes := sched.Nodes()
	if len(nodes) != 1 || nodes[0].ID != "n1" || nodes[0].Region != "r1" {
		t.Fatalf("scheduler nodes = %+v", nodes)
	}
	if nodes[0].Labels["pool"] != "nvme" || nodes[0].CPUAllocatable != 8 {
		t.Errorf("node = %+v", nodes[0])
	}
	// The registered node is immediately schedulable.
	if _, err := sched.Schedule(&scheduler.Request{
		SandboxID: "s1", Region: "r1", Image: "i", CPU: 1, MemoryMiB: 512, Runtime: "fc",
	}); err != nil {
		t.Errorf("schedule after register: %v", err)
	}
}

func TestRegisterRejectsBadToken(t *testing.T) {
	c, _, _ := start(t, Options{BootstrapToken: "boot-tok"})
	req := regReq("n1")
	req.BootstrapToken = "wrong"
	if _, err := c.Register(context.Background(), req); status.Code(err) != codes.Unauthenticated {
		t.Errorf("err = %v, want Unauthenticated", err)
	}
}

func TestRegisterValidation(t *testing.T) {
	c, _, _ := start(t, Options{})
	ctx := context.Background()
	bad := []*nodev1.RegisterRequest{
		{Region: "r1", Resources: &nodev1.NodeResources{CpuAllocatable: 1, MemoryAllocatableMib: 1}},
		{NodeId: "n", Resources: &nodev1.NodeResources{CpuAllocatable: 1, MemoryAllocatableMib: 1}},
		{NodeId: "n", Region: "r1"},
		{NodeId: "n", Region: "r1", Resources: &nodev1.NodeResources{}},
	}
	for i, req := range bad {
		if _, err := c.Register(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("case %d: err = %v, want InvalidArgument", i, err)
		}
	}
}

func TestHeartbeatRenewsLease(t *testing.T) {
	c, sched, _ := start(t, Options{BootstrapToken: "boot-tok"})
	ctx := context.Background()
	reg, err := c.Register(ctx, regReq("n1"))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := c.Heartbeat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(&nodev1.HeartbeatRequest{NodeId: "n1", NodeToken: reg.NodeToken}); err != nil {
		t.Fatal(err)
	}
	resp, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if !resp.LeaseOk {
		t.Error("lease not confirmed")
	}
	if got := sched.Nodes()[0].State; got != scheduler.NodeReady {
		t.Errorf("state = %s", got)
	}
}

func TestHeartbeatRejectsBadToken(t *testing.T) {
	c, _, _ := start(t, Options{BootstrapToken: "boot-tok"})
	ctx := context.Background()
	if _, err := c.Register(ctx, regReq("n1")); err != nil {
		t.Fatal(err)
	}
	stream, err := c.Heartbeat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stream.Send(&nodev1.HeartbeatRequest{NodeId: "n1", NodeToken: "forged"})
	if _, err := stream.Recv(); status.Code(err) != codes.Unauthenticated {
		t.Errorf("err = %v, want Unauthenticated", err)
	}
}

func TestHeartbeatUnregisteredNode(t *testing.T) {
	c, _, _ := start(t, Options{})
	stream, err := c.Heartbeat(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stream.Send(&nodev1.HeartbeatRequest{NodeId: "ghost", NodeToken: "x"})
	if _, err := stream.Recv(); status.Code(err) != codes.Unauthenticated {
		t.Errorf("err = %v, want Unauthenticated", err)
	}
}

func TestSyncStateReturnsExpected(t *testing.T) {
	specs := []*nodev1.SandboxSpec{{SandboxId: "s1"}, {SandboxId: "s2"}}
	c, _, _ := start(t, Options{BootstrapToken: "boot-tok", Lister: stubLister{specs: specs}})
	ctx := context.Background()
	reg, err := c.Register(ctx, regReq("n1"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.SyncState(ctx, &nodev1.SyncStateRequest{NodeId: "n1", NodeToken: reg.NodeToken})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Expected) != 2 {
		t.Errorf("expected = %+v", resp.Expected)
	}
	// Auth is enforced here too.
	if _, err := c.SyncState(ctx, &nodev1.SyncStateRequest{NodeId: "n1", NodeToken: "bad"}); status.Code(err) != codes.Unauthenticated {
		t.Errorf("err = %v, want Unauthenticated", err)
	}
}

func TestLivenessSweepNotifiesLost(t *testing.T) {
	now := time.Now()
	sched := scheduler.New(scheduler.DefaultWeights())
	sched.SetClock(func() time.Time { return now })
	lostCh := make(chan string, 1)
	svc := New(sched, Options{OnLost: func(id string) { lostCh <- id }})

	if _, err := svc.Register(context.Background(), regReq("n1")); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute) // past the lost threshold

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.RunLivenessSweep(ctx, 10*time.Millisecond)

	select {
	case id := <-lostCh:
		if id != "n1" {
			t.Errorf("lost node = %s", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lost handler not called")
	}
}

func TestNodeAddrRegistry(t *testing.T) {
	_, _, svc := start(t, Options{})
	if _, ok := svc.NodeAddr("n1"); ok {
		t.Error("unknown node should not resolve")
	}
	svc.SetNodeAddr("n1", "10.0.0.5:7443")
	got, ok := svc.NodeAddr("n1")
	if !ok || got != "10.0.0.5:7443" {
		t.Errorf("addr = %q ok=%v", got, ok)
	}
}

func TestRegisterWithoutBootstrapTokenAllowsAny(t *testing.T) {
	// Empty configured token disables enforcement (dev mode).
	c, _, _ := start(t, Options{})
	req := regReq("n1")
	req.BootstrapToken = ""
	if _, err := c.Register(context.Background(), req); err != nil {
		t.Errorf("err = %v", err)
	}
}

package nodesvc

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/garysng/bean/internal/control/scheduler"
	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node"
)

type stubLister struct{ specs []*nodev1.SandboxSpec }

func (s stubLister) ExpectedForNode(string) []*nodev1.SandboxSpec { return s.specs }

func start(t *testing.T, opts Options) (nodev1.NodeServiceClient, *store.Store, *Service) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "nodesvc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sched := scheduler.New(st, scheduler.DefaultWeights())
	svc := New(st, sched, opts)
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
	return nodev1.NewNodeServiceClient(conn), st, svc
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

func TestRegisterPersistsNode(t *testing.T) {
	c, st, _ := start(t, Options{BootstrapToken: "boot-tok", Lister: stubLister{}})
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
	// The node is persisted, so any replica can see and route to it.
	nodes, err := st.LoadNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].ID != "n1" || nodes[0].Region != "r1" {
		t.Fatalf("persisted nodes = %+v", nodes)
	}
	if nodes[0].Labels["pool"] != "nvme" || nodes[0].CPUAllocatable != 8 {
		t.Errorf("node = %+v", nodes[0])
	}
	// It is immediately schedulable by a scheduler that never saw the
	// registration, which is the property replicas depend on.
	fresh := scheduler.New(st, scheduler.DefaultWeights())
	if _, err := fresh.Schedule(&scheduler.Request{
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
	c, st, _ := start(t, Options{BootstrapToken: "boot-tok"})
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
	nodes, _ := st.LoadNodes()
	if got := nodes[0].State; got != scheduler.NodeReady {
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
	st, err := store.Open(filepath.Join(t.TempDir(), "sweep.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sched := scheduler.New(st, scheduler.DefaultWeights())
	sched.SetClock(func() time.Time { return now })
	lostCh := make(chan string, 1)
	svc := New(st, sched, Options{OnLost: func(id string) { lostCh <- id }})

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

func TestNodeAddrComesFromRegistration(t *testing.T) {
	c, _, svc := start(t, Options{})
	if _, ok := svc.NodeAddr("n1"); ok {
		t.Error("unknown node should not resolve")
	}
	// The advertise address travels in registration labels, so a replica
	// that never handled the registration can still route to the node.
	req := regReq("n1")
	req.Labels[node.LabelAdvertiseAddr] = "10.0.0.5:7443"
	if _, err := c.Register(context.Background(), req); err != nil {
		t.Fatal(err)
	}
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

// nodeImages reads back what the control plane recorded for a node.
func nodeImages(t *testing.T, st *store.Store, nodeID string) map[string]store.CachedImage {
	t.Helper()
	nodes, err := st.LoadNodes()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID == nodeID {
			return n.CachedImages
		}
	}
	t.Fatalf("node %s not found", nodeID)
	return nil
}

func TestUpdateNodeStatusRecordsImagesWithDigests(t *testing.T) {
	c, st, _ := start(t, Options{BootstrapToken: "boot-tok"})
	ctx := context.Background()
	reg, err := c.Register(ctx, regReq("n1"))
	if err != nil {
		t.Fatal(err)
	}

	const digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	if _, err := c.UpdateNodeStatus(ctx, &nodev1.UpdateNodeStatusRequest{
		NodeId:    "n1",
		NodeToken: reg.NodeToken,
		Images: &nodev1.ImageInventory{Images: map[string]*nodev1.CachedImage{
			"python:3.12": {SizeBytes: 1 << 30, Digest: digest},
		}},
	}); err != nil {
		t.Fatalf("UpdateNodeStatus: %v", err)
	}

	got := nodeImages(t, st, "n1")
	img, ok := got["python:3.12"]
	if !ok {
		t.Fatalf("image not recorded: %v", got)
	}
	if img.SizeBytes != 1<<30 {
		t.Errorf("size = %d, want %d", img.SizeBytes, int64(1)<<30)
	}
	// The digest is the whole reason this RPC carries more than a size: a warm
	// snapshot keyed on a tag would serve an environment captured from whatever the
	// tag used to name.
	if img.Digest != digest {
		t.Errorf("digest = %q, want %q", img.Digest, digest)
	}
}

// TestUpdateNodeStatusWithNoCategoryLeavesImagesAlone is the property the
// optional fields exist for. A node reporting some other category on its own must
// not read as a node that has dropped every image -- that would clear affinity
// intermittently, which is the hardest kind of scheduling bug to attribute.
func TestUpdateNodeStatusWithNoCategoryLeavesImagesAlone(t *testing.T) {
	c, st, _ := start(t, Options{BootstrapToken: "boot-tok"})
	ctx := context.Background()
	reg, err := c.Register(ctx, regReq("n1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateNodeStatus(ctx, &nodev1.UpdateNodeStatusRequest{
		NodeId:    "n1",
		NodeToken: reg.NodeToken,
		Images: &nodev1.ImageInventory{Images: map[string]*nodev1.CachedImage{
			"keepme:1": {SizeBytes: 42},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	// Images absent entirely, which is what a report about another category looks
	// like.
	if _, err := c.UpdateNodeStatus(ctx, &nodev1.UpdateNodeStatusRequest{
		NodeId:    "n1",
		NodeToken: reg.NodeToken,
	}); err != nil {
		t.Fatal(err)
	}

	if got := nodeImages(t, st, "n1"); len(got) != 1 || got["keepme:1"].SizeBytes != 42 {
		t.Errorf("a report with no image category changed the inventory: %v", got)
	}
}

// TestUpdateNodeStatusWithEmptyInventoryClearsImages is the other half. An empty
// inventory is a node saying it holds nothing, which is different from saying
// nothing -- and it has to be honoured or a node that evicted everything would
// keep attracting work it has to pull for.
func TestUpdateNodeStatusWithEmptyInventoryClearsImages(t *testing.T) {
	c, st, _ := start(t, Options{BootstrapToken: "boot-tok"})
	ctx := context.Background()
	reg, err := c.Register(ctx, regReq("n1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateNodeStatus(ctx, &nodev1.UpdateNodeStatusRequest{
		NodeId:    "n1",
		NodeToken: reg.NodeToken,
		Images: &nodev1.ImageInventory{Images: map[string]*nodev1.CachedImage{
			"gone:1": {SizeBytes: 42},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateNodeStatus(ctx, &nodev1.UpdateNodeStatusRequest{
		NodeId:    "n1",
		NodeToken: reg.NodeToken,
		Images:    &nodev1.ImageInventory{},
	}); err != nil {
		t.Fatal(err)
	}
	if got := nodeImages(t, st, "n1"); len(got) != 0 {
		t.Errorf("an empty inventory did not clear the images: %v", got)
	}
}

func TestUpdateNodeStatusRejectsBadToken(t *testing.T) {
	c, _, _ := start(t, Options{BootstrapToken: "boot-tok"})
	ctx := context.Background()
	if _, err := c.Register(ctx, regReq("n1")); err != nil {
		t.Fatal(err)
	}
	_, err := c.UpdateNodeStatus(ctx, &nodev1.UpdateNodeStatusRequest{
		NodeId:    "n1",
		NodeToken: "wrong",
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("err = %v, want Unauthenticated", err)
	}
}

// TestHeartbeatDoesNotClearTheInventory guards the split itself. The heartbeat no
// longer carries images, and the failure to avoid is it writing an empty map over
// the inventory every few seconds -- which would leave affinity working only in
// the window between a status report and the next heartbeat.
func TestHeartbeatDoesNotClearTheInventory(t *testing.T) {
	c, st, _ := start(t, Options{BootstrapToken: "boot-tok"})
	ctx := context.Background()
	reg, err := c.Register(ctx, regReq("n1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdateNodeStatus(ctx, &nodev1.UpdateNodeStatusRequest{
		NodeId:    "n1",
		NodeToken: reg.NodeToken,
		Images: &nodev1.ImageInventory{Images: map[string]*nodev1.CachedImage{
			"survives:1": {SizeBytes: 7},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	stream, err := c.Heartbeat(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := stream.Send(&nodev1.HeartbeatRequest{
			NodeId: "n1", NodeToken: reg.NodeToken,
			Usage: &nodev1.NodeUsage{DiskUsedMib: 10},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Recv(); err != nil {
			t.Fatal(err)
		}
	}

	if got := nodeImages(t, st, "n1"); len(got) != 1 || got["survives:1"].SizeBytes != 7 {
		t.Errorf("heartbeats clobbered the inventory: %v", got)
	}
}

package scheduler

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func node(id string, cpu float64, memMiB int64, mut ...func(*Node)) *Node {
	n := &Node{
		ID: id, Region: "r1", Runtimes: []string{"fc"},
		CPUAllocatable: cpu, MemoryMiBAllocate: memMiB,
		DiskMiBAllocate: 1 << 20, GPUCount: 0,
		CachedImages: map[string]int64{},
	}
	for _, f := range mut {
		f(n)
	}
	return n
}

func req(id string, cpu float64, memMiB int64, mut ...func(*Request)) *Request {
	r := &Request{
		SandboxID: id, Region: "r1", Image: "img:1",
		CPU: cpu, MemoryMiB: memMiB, DiskMiB: 1024, Runtime: "fc",
	}
	for _, f := range mut {
		f(r)
	}
	return r
}

func newSched(t *testing.T, nodes ...*Node) *Scheduler {
	t.Helper()
	s := New(DefaultWeights())
	for _, n := range nodes {
		s.Register(n)
	}
	return s
}

func TestSchedulePicksFeasibleNode(t *testing.T) {
	s := newSched(t, node("n1", 4, 4096))
	got, err := s.Schedule(req("s1", 2, 2048))
	if err != nil {
		t.Fatal(err)
	}
	if got != "n1" {
		t.Errorf("node = %s", got)
	}
	ns := s.Nodes()
	if ns[0].CPUCommitted != 2 || ns[0].MemoryMiBCommit != 2048 {
		t.Errorf("committed = %.1f/%d", ns[0].CPUCommitted, ns[0].MemoryMiBCommit)
	}
	if ns[0].CreateInFlight != 1 {
		t.Errorf("in-flight = %d", ns[0].CreateInFlight)
	}
}

func TestScheduleRegionRequired(t *testing.T) {
	s := newSched(t, node("n1", 4, 4096))
	if _, err := s.Schedule(req("s1", 1, 512, func(r *Request) { r.Region = "" })); err == nil {
		t.Error("expected error when region is empty")
	}
}

func TestScheduleFiltersHardConstraints(t *testing.T) {
	s := newSched(t,
		node("wrong-region", 8, 8192, func(n *Node) { n.Region = "r2" }),
		node("wrong-runtime", 8, 8192, func(n *Node) { n.Runtimes = []string{"runc"} }),
		node("no-label", 8, 8192),
		node("labelled", 8, 8192, func(n *Node) { n.Labels = map[string]string{"pool": "nvme"} }),
	)
	got, err := s.Schedule(req("s1", 1, 512, func(r *Request) {
		r.NodeSelector = map[string]string{"pool": "nvme"}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if got != "labelled" {
		t.Errorf("node = %s, want labelled", got)
	}
}

func TestScheduleNoCapacity(t *testing.T) {
	s := newSched(t, node("small", 1, 512))
	_, err := s.Schedule(req("big", 4, 4096))
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity", err)
	}
	// GPU request with no GPU nodes
	if _, err := s.Schedule(req("gpu", 1, 256, func(r *Request) { r.GPU = 1 })); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("gpu err = %v, want ErrNoCapacity", err)
	}
}

func TestCommitmentAccountingPreventsOversell(t *testing.T) {
	s := newSched(t, node("n1", 4, 4096))
	for i := 0; i < 4; i++ {
		if _, err := s.Schedule(req("s", 1, 1024)); err != nil {
			t.Fatalf("placement %d: %v", i, err)
		}
	}
	// Node is now fully committed even though nothing actually runs.
	if _, err := s.Schedule(req("overflow", 1, 1024)); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity", err)
	}
}

func TestReleaseReturnsCapacity(t *testing.T) {
	s := newSched(t, node("n1", 2, 2048))
	r := req("s1", 2, 2048)
	if _, err := s.Schedule(r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Schedule(req("s2", 1, 512)); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected full node, got %v", err)
	}
	s.Release("n1", r)
	if _, err := s.Schedule(req("s2", 1, 512)); err != nil {
		t.Errorf("after release: %v", err)
	}
	// Release never goes negative.
	s.Release("n1", req("x", 999, 999999))
	ns := s.Nodes()
	if ns[0].CPUCommitted < 0 || ns[0].MemoryMiBCommit < 0 {
		t.Errorf("negative commitment: %+v", ns[0])
	}
	// Release of an unknown node is a no-op.
	s.Release("ghost", r)
}

func TestImageAffinityWins(t *testing.T) {
	s := newSched(t,
		node("cold", 8, 8192),
		node("warm", 8, 8192, func(n *Node) { n.CachedImages = map[string]int64{"img:1": 1 << 30} }),
	)
	got, err := s.Schedule(req("s1", 1, 512))
	if err != nil {
		t.Fatal(err)
	}
	if got != "warm" {
		t.Errorf("node = %s, want warm (image affinity)", got)
	}
}

func TestNVMePreferredForColdImage(t *testing.T) {
	s := newSched(t,
		node("spinning", 8, 8192),
		node("nvme", 8, 8192, func(n *Node) { n.NVMeCache = true }),
	)
	got, err := s.Schedule(req("cold", 1, 512, func(r *Request) { r.Image = "uncached:1" }))
	if err != nil {
		t.Fatal(err)
	}
	if got != "nvme" {
		t.Errorf("node = %s, want nvme for a cold image", got)
	}
}

func TestSpreadAcrossNodes(t *testing.T) {
	s := newSched(t, node("a", 16, 16384), node("b", 16, 16384), node("c", 16, 16384))
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		got, err := s.Schedule(req("s", 1, 512, func(r *Request) { r.SpreadKey = "run-1" }))
		if err != nil {
			t.Fatal(err)
		}
		seen[got]++
	}
	if len(seen) != 3 {
		t.Errorf("placements landed on %d nodes, want 3: %v", len(seen), seen)
	}
	for id, n := range seen {
		if n != 2 {
			t.Errorf("node %s got %d, want even spread", id, n)
		}
	}
}

func TestSpreadReleasedOnRelease(t *testing.T) {
	s := newSched(t, node("a", 16, 16384))
	r := req("s1", 1, 512, func(r *Request) { r.SpreadKey = "run-x" })
	if _, err := s.Schedule(r); err != nil {
		t.Fatal(err)
	}
	s.Release("a", r)
	// After release the spread penalty is gone, so scoring is back to base.
	s2 := newSched(t, node("a", 16, 16384))
	base := s2.scoreLocked(s2.nodes["a"], r)
	after := s.scoreLocked(s.nodes["a"], r)
	if base != after {
		t.Errorf("score after release = %.2f, want base %.2f", after, base)
	}
}

func TestMaxCreatesBoundsBurst(t *testing.T) {
	s := newSched(t, node("n1", 100, 1<<20, func(n *Node) { n.MaxCreates = 2 }))
	for i := 0; i < 2; i++ {
		if _, err := s.Schedule(req("s", 1, 256)); err != nil {
			t.Fatalf("placement %d: %v", i, err)
		}
	}
	if _, err := s.Schedule(req("third", 1, 256)); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity when create slots are full", err)
	}
	s.ReleaseCreate("n1")
	if _, err := s.Schedule(req("third", 1, 256)); err != nil {
		t.Errorf("after ReleaseCreate: %v", err)
	}
	// ReleaseCreate never underflows or panics on unknown nodes.
	for i := 0; i < 5; i++ {
		s.ReleaseCreate("n1")
	}
	s.ReleaseCreate("ghost")
	if got := s.Nodes()[0].CreateInFlight; got != 0 {
		t.Errorf("in-flight = %d, want 0", got)
	}
}

func TestScheduleBatchPartialSuccess(t *testing.T) {
	s := newSched(t, node("n1", 2, 2048))
	reqs := []*Request{
		req("ok1", 1, 1024),
		req("ok2", 1, 1024),
		req("too-big", 4, 4096),
	}
	nodes, errs := s.ScheduleBatch(reqs)
	if nodes[0] != "n1" || errs[0] != nil {
		t.Errorf("item0 = %q/%v", nodes[0], errs[0])
	}
	if nodes[1] != "n1" || errs[1] != nil {
		t.Errorf("item1 = %q/%v", nodes[1], errs[1])
	}
	if !errors.Is(errs[2], ErrNoCapacity) {
		t.Errorf("item2 err = %v, want ErrNoCapacity", errs[2])
	}
}

func TestDrainStopsPlacement(t *testing.T) {
	s := newSched(t, node("n1", 8, 8192), node("n2", 8, 8192))
	if err := s.Drain("n1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		got, err := s.Schedule(req("s", 1, 512))
		if err != nil {
			t.Fatal(err)
		}
		if got == "n1" {
			t.Fatal("placed on draining node")
		}
	}
	if err := s.Drain("ghost"); err == nil {
		t.Error("draining unknown node should error")
	}
}

func TestHeartbeatLivenessTransitions(t *testing.T) {
	now := time.Now()
	s := New(DefaultWeights())
	s.SetClock(func() time.Time { return now })
	s.Register(node("n1", 4, 4096))

	if lost := s.SweepLiveness(); len(lost) != 0 {
		t.Fatalf("fresh node reported lost: %v", lost)
	}

	// Past the suspect threshold: no new placements should be blocked yet,
	// but state reflects the doubt.
	now = now.Add(20 * time.Second)
	s.SweepLiveness()
	if got := s.Nodes()[0].State; got != NodeSuspect {
		t.Errorf("state = %s, want SUSPECT", got)
	}
	if _, err := s.Schedule(req("s1", 1, 512)); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("SUSPECT node accepted placement: %v", err)
	}

	// Past the lost threshold: reported once, not repeatedly.
	now = now.Add(60 * time.Second)
	lost := s.SweepLiveness()
	if len(lost) != 1 || lost[0] != "n1" {
		t.Fatalf("lost = %v", lost)
	}
	if again := s.SweepLiveness(); len(again) != 0 {
		t.Errorf("lost reported twice: %v", again)
	}

	// A heartbeat brings it back.
	if err := s.Heartbeat("n1", map[string]int64{"img:1": 1}); err != nil {
		t.Fatal(err)
	}
	if got := s.Nodes()[0].State; got != NodeReady {
		t.Errorf("state = %s, want READY", got)
	}
	if _, err := s.Schedule(req("s1", 1, 512)); err != nil {
		t.Errorf("recovered node rejected placement: %v", err)
	}
	if err := s.Heartbeat("ghost", nil); err == nil {
		t.Error("heartbeat for unknown node should error")
	}
}

func TestDrainingNodeNotSweptToLost(t *testing.T) {
	now := time.Now()
	s := New(DefaultWeights())
	s.SetClock(func() time.Time { return now })
	s.Register(node("n1", 4, 4096))
	s.Drain("n1")
	now = now.Add(10 * time.Minute)
	if lost := s.SweepLiveness(); len(lost) != 0 {
		t.Errorf("draining node reported lost: %v", lost)
	}
	if got := s.Nodes()[0].State; got != NodeDraining {
		t.Errorf("state = %s, want DRAINING", got)
	}
}

func TestReRegisterPreservesCommitments(t *testing.T) {
	s := newSched(t, node("n1", 4, 4096))
	if _, err := s.Schedule(req("s1", 2, 2048)); err != nil {
		t.Fatal(err)
	}
	// noded restarts and re-registers: accounting must survive, otherwise
	// the node would be oversold.
	s.Register(node("n1", 4, 4096))
	ns := s.Nodes()
	if ns[0].CPUCommitted != 2 || ns[0].MemoryMiBCommit != 2048 {
		t.Errorf("commitments lost on re-register: %+v", ns[0])
	}
	if ns[0].State != NodeReady {
		t.Errorf("state = %s", ns[0].State)
	}
}

func TestDeterministicTieBreak(t *testing.T) {
	// Identical nodes must produce a stable choice across runs.
	for i := 0; i < 20; i++ {
		s := newSched(t, node("b", 8, 8192), node("a", 8, 8192), node("c", 8, 8192))
		got, err := s.Schedule(req("s1", 1, 512))
		if err != nil {
			t.Fatal(err)
		}
		if got != "a" {
			t.Fatalf("iteration %d picked %s, want deterministic 'a'", i, got)
		}
	}
}

func TestConcurrentScheduleNoOversell(t *testing.T) {
	// 8 CPU total, 1 CPU each, 32 concurrent attempts: exactly 8 succeed.
	s := newSched(t, node("n1", 8, 1<<20, func(n *Node) { n.MaxCreates = 1000 }))
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Schedule(req("s", 1, 1)); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 8 {
		t.Errorf("successful placements = %d, want 8", ok)
	}
	if got := s.Nodes()[0].CPUCommitted; got != 8 {
		t.Errorf("committed = %.1f, want 8", got)
	}
}

func TestNodesSnapshotIsCopy(t *testing.T) {
	s := newSched(t, node("n1", 4, 4096, func(n *Node) {
		n.Labels = map[string]string{"pool": "a"}
		n.CachedImages = map[string]int64{"img:1": 5}
	}))
	snap := s.Nodes()
	snap[0].Labels["pool"] = "mutated"
	snap[0].CachedImages["img:1"] = 999
	snap[0].Runtimes[0] = "mutated"

	fresh := s.Nodes()
	if fresh[0].Labels["pool"] != "a" || fresh[0].CachedImages["img:1"] != 5 ||
		fresh[0].Runtimes[0] != "fc" {
		t.Errorf("snapshot mutation leaked into scheduler state: %+v", fresh[0])
	}
}

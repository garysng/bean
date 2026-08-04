package scheduler

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/garysng/bean/internal/control/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sched.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func node(id string, cpu float64, memMiB int64, mut ...func(*store.NodeRecord)) *store.NodeRecord {
	n := &store.NodeRecord{
		ID: id, Region: "r1", Runtimes: []string{"fc"},
		CPUAllocatable: cpu, MemoryAllocateMiB: memMiB,
		DiskAllocateMiB: 1 << 20, GPUCount: 0,
		CachedImages: map[string]store.CachedImage{},
		State:        NodeReady, LastHeartbeat: time.Now(),
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

func newSched(t *testing.T, nodes ...*store.NodeRecord) (*Scheduler, *store.Store) {
	t.Helper()
	st := newTestStore(t)
	for _, n := range nodes {
		if err := st.UpsertNode(n); err != nil {
			t.Fatal(err)
		}
	}
	return New(st, DefaultWeights()), st
}

func TestSchedulePlacesAndReserves(t *testing.T) {
	s, st := newSched(t, node("n1", 4, 4096))
	got, err := s.Schedule(req("s1", 2, 2048))
	if err != nil {
		t.Fatal(err)
	}
	if got != "n1" {
		t.Errorf("node = %s", got)
	}
	// The reservation is durable, not just in this process.
	n, err := st.GetNode("n1")
	if err != nil {
		t.Fatal(err)
	}
	if n.CPUCommitted != 2 || n.MemoryCommitMiB != 2048 {
		t.Errorf("committed = %.1f/%d", n.CPUCommitted, n.MemoryCommitMiB)
	}
	if n.CreateInFlight != 1 {
		t.Errorf("in-flight = %d", n.CreateInFlight)
	}
}

func TestAccountingSurvivesSchedulerRestart(t *testing.T) {
	// The point of durable accounting: a fresh scheduler over the same store
	// must see existing reservations and not oversell.
	s, st := newSched(t, node("n1", 2, 2048))
	if _, err := s.Schedule(req("s1", 2, 2048)); err != nil {
		t.Fatal(err)
	}

	fresh := New(st, DefaultWeights())
	if _, err := fresh.Schedule(req("s2", 1, 512)); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity from a restarted scheduler", err)
	}
	// After releasing, the fresh scheduler can place again.
	if err := fresh.Release("s1"); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Schedule(req("s2", 1, 512)); err != nil {
		t.Errorf("after release: %v", err)
	}
}

func TestConcurrentSchedulersDoNotOversell(t *testing.T) {
	// Two schedulers over one store stand in for two gateway replicas.
	// 8 CPU total, 1 CPU each, 32 concurrent attempts split across both:
	// exactly 8 succeed, because the store arbitrates.
	st := newTestStore(t)
	if err := st.UpsertNode(node("n1", 8, 1<<20, func(n *store.NodeRecord) {
		n.MaxCreates = 1000
	})); err != nil {
		t.Fatal(err)
	}
	replicas := []*Scheduler{New(st, DefaultWeights()), New(st, DefaultWeights())}

	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := replicas[i%len(replicas)]
			if _, err := s.Schedule(req(store.NewID("sbx"), 1, 1)); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if ok != 8 {
		t.Errorf("successful placements = %d, want exactly 8", ok)
	}
	n, _ := st.GetNode("n1")
	if n.CPUCommitted != 8 {
		t.Errorf("committed = %.1f, want 8", n.CPUCommitted)
	}
}

func TestScheduleRegionRequired(t *testing.T) {
	s, _ := newSched(t, node("n1", 4, 4096))
	if _, err := s.Schedule(req("s1", 1, 512, func(r *Request) { r.Region = "" })); err == nil {
		t.Error("expected error when region is empty")
	}
}

func TestScheduleFiltersHardConstraints(t *testing.T) {
	s, _ := newSched(t,
		node("wrong-region", 8, 8192, func(n *store.NodeRecord) { n.Region = "r2" }),
		node("wrong-runtime", 8, 8192, func(n *store.NodeRecord) { n.Runtimes = []string{"runc"} }),
		node("no-label", 8, 8192),
		node("labelled", 8, 8192, func(n *store.NodeRecord) {
			n.Labels = map[string]string{"pool": "nvme"}
		}),
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
	s, _ := newSched(t, node("small", 1, 512))
	if _, err := s.Schedule(req("big", 4, 4096)); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity", err)
	}
	if _, err := s.Schedule(req("gpu", 1, 256, func(r *Request) { r.GPU = 1 })); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("gpu err = %v, want ErrNoCapacity", err)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	s, st := newSched(t, node("n1", 2, 2048))
	if _, err := s.Schedule(req("s1", 2, 2048)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Release("s1"); err != nil {
			t.Fatalf("release %d: %v", i, err)
		}
	}
	n, _ := st.GetNode("n1")
	if n.CPUCommitted != 0 || n.MemoryCommitMiB != 0 {
		t.Errorf("committed = %.1f/%d, want zero", n.CPUCommitted, n.MemoryCommitMiB)
	}
	// Releasing something never reserved is also fine.
	if err := s.Release("never-existed"); err != nil {
		t.Errorf("release unknown: %v", err)
	}
}

func TestImageAffinityWins(t *testing.T) {
	s, _ := newSched(t,
		node("cold", 8, 8192),
		node("warm", 8, 8192, func(n *store.NodeRecord) {
			n.CachedImages = map[string]store.CachedImage{"img:1": {SizeBytes: 1 << 30}}
		}),
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
	s, _ := newSched(t,
		node("spinning", 8, 8192),
		node("nvme", 8, 8192, func(n *store.NodeRecord) { n.NVMeCache = true }),
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
	s, _ := newSched(t, node("a", 16, 16384), node("b", 16, 16384), node("c", 16, 16384))
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		got, err := s.Schedule(req(store.NewID("sbx"), 1, 512, func(r *Request) {
			r.SpreadKey = "run-1"
		}))
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
			t.Errorf("node %s got %d, want an even spread", id, n)
		}
	}
}

func TestMaxCreatesBoundsBurst(t *testing.T) {
	s, _ := newSched(t, node("n1", 100, 1<<20, func(n *store.NodeRecord) { n.MaxCreates = 2 }))
	for i := 0; i < 2; i++ {
		if _, err := s.Schedule(req(store.NewID("sbx"), 1, 256)); err != nil {
			t.Fatalf("placement %d: %v", i, err)
		}
	}
	if _, err := s.Schedule(req("third", 1, 256)); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("err = %v, want ErrNoCapacity when create slots are full", err)
	}
	if err := s.FinishCreate("n1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Schedule(req("third", 1, 256)); err != nil {
		t.Errorf("after FinishCreate: %v", err)
	}
	// FinishCreate never underflows.
	for i := 0; i < 5; i++ {
		if err := s.FinishCreate("n1"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestScheduleBatchPartialSuccess(t *testing.T) {
	s, _ := newSched(t, node("n1", 2, 2048))
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
	s, _ := newSched(t, node("n1", 8, 8192), node("n2", 8, 8192))
	if err := s.Drain("n1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		got, err := s.Schedule(req(store.NewID("sbx"), 1, 512))
		if err != nil {
			t.Fatal(err)
		}
		if got == "n1" {
			t.Fatal("placed on a draining node")
		}
	}
	if err := s.Drain("ghost"); err == nil {
		t.Error("draining an unknown node should error")
	}
}

func TestLivenessTransitions(t *testing.T) {
	now := time.Now()
	st := newTestStore(t)
	n := node("n1", 4, 4096)
	n.LastHeartbeat = now
	if err := st.UpsertNode(n); err != nil {
		t.Fatal(err)
	}
	s := New(st, DefaultWeights())
	s.SetClock(func() time.Time { return now })

	if lost, err := s.SweepLiveness(); err != nil || len(lost) != 0 {
		t.Fatalf("fresh node reported lost: %v %v", lost, err)
	}

	// Past the suspect threshold the node stops receiving placements while
	// we wait to see whether it comes back.
	now = now.Add(20 * time.Second)
	if _, err := s.SweepLiveness(); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetNode("n1")
	if got.State != NodeSuspect {
		t.Errorf("state = %s, want SUSPECT", got.State)
	}
	if _, err := s.Schedule(req("s1", 1, 512)); !errors.Is(err, ErrNoCapacity) {
		t.Errorf("SUSPECT node accepted a placement: %v", err)
	}

	// Past the lost threshold it is reported once, not repeatedly.
	now = now.Add(60 * time.Second)
	lost, err := s.SweepLiveness()
	if err != nil {
		t.Fatal(err)
	}
	if len(lost) != 1 || lost[0] != "n1" {
		t.Fatalf("lost = %v", lost)
	}
	if again, _ := s.SweepLiveness(); len(again) != 0 {
		t.Errorf("lost reported twice: %v", again)
	}

	// A heartbeat brings it back.
	if err := st.TouchNode("n1", 0); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetNode("n1")
	if got.State != NodeReady {
		t.Errorf("state = %s, want READY", got.State)
	}
	if _, err := s.Schedule(req("s1", 1, 512)); err != nil {
		t.Errorf("recovered node rejected a placement: %v", err)
	}
}

func TestLostReportedOnceAcrossReplicas(t *testing.T) {
	// Several replicas sweep concurrently; a node must be reported lost
	// exactly once so its sandboxes are not marked lost twice.
	now := time.Now()
	st := newTestStore(t)
	n := node("n1", 4, 4096)
	n.LastHeartbeat = now.Add(-10 * time.Minute)
	if err := st.UpsertNode(n); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s := New(st, DefaultWeights())
			s.SetClock(func() time.Time { return now })
			lost, err := s.SweepLiveness()
			if err != nil {
				t.Errorf("sweep: %v", err)
				return
			}
			mu.Lock()
			total += len(lost)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if total != 1 {
		t.Errorf("node reported lost %d times, want exactly 1", total)
	}
}

func TestDrainingNodeNotSweptToLost(t *testing.T) {
	now := time.Now()
	st := newTestStore(t)
	n := node("n1", 4, 4096)
	n.LastHeartbeat = now
	st.UpsertNode(n)
	s := New(st, DefaultWeights())
	s.SetClock(func() time.Time { return now })
	if err := s.Drain("n1"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(10 * time.Minute)
	if lost, _ := s.SweepLiveness(); len(lost) != 0 {
		t.Errorf("draining node reported lost: %v", lost)
	}
	got, _ := st.GetNode("n1")
	if got.State != NodeDraining {
		t.Errorf("state = %s, want DRAINING", got.State)
	}
}

func TestReRegisterPreservesCommitments(t *testing.T) {
	s, st := newSched(t, node("n1", 4, 4096))
	if _, err := s.Schedule(req("s1", 2, 2048)); err != nil {
		t.Fatal(err)
	}
	// A node restarting and re-registering must not appear empty, or the
	// scheduler would oversell it.
	if err := st.UpsertNode(node("n1", 4, 4096)); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetNode("n1")
	if got.CPUCommitted != 2 || got.MemoryCommitMiB != 2048 {
		t.Errorf("commitments lost on re-register: %+v", got)
	}
}

func TestDeterministicTieBreak(t *testing.T) {
	for i := 0; i < 10; i++ {
		s, _ := newSched(t, node("b", 8, 8192), node("a", 8, 8192), node("c", 8, 8192))
		got, err := s.Schedule(req("s1", 1, 512))
		if err != nil {
			t.Fatal(err)
		}
		if got != "a" {
			t.Fatalf("iteration %d picked %s, want a deterministic 'a'", i, got)
		}
	}
}

func TestReclaimOrphanReservations(t *testing.T) {
	// A gateway that dies mid-create leaves a reservation with no sandbox.
	// Without reclamation that node's capacity would leak permanently.
	s, st := newSched(t, node("n1", 4, 4096))
	if _, err := s.Schedule(req("orphaned", 4, 4096)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Schedule(req("next", 1, 512)); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected a full node, got %v", err)
	}

	n, err := s.ReclaimOrphanReservations()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("reclaimed %d, want 1", n)
	}
	if _, err := s.Schedule(req("next", 1, 512)); err != nil {
		t.Errorf("after reclaim: %v", err)
	}

	// A reservation whose sandbox is alive is left alone.
	st.PutSandbox(&store.Sandbox{ID: "next", State: store.SandboxRunning, NodeID: "n1"})
	if n, err := s.ReclaimOrphanReservations(); err != nil || n != 0 {
		t.Errorf("reclaimed %d (err %v), want 0 for a live sandbox", n, err)
	}

	// A terminal sandbox's reservation is reclaimed.
	st.PutSandbox(&store.Sandbox{ID: "next", State: store.SandboxStopped, NodeID: "n1"})
	if n, err := s.ReclaimOrphanReservations(); err != nil || n != 1 {
		t.Errorf("reclaimed %d (err %v), want 1 for a stopped sandbox", n, err)
	}
}

func TestUtilisation(t *testing.T) {
	s, _ := newSched(t, node("n1", 4, 4000), node("n2", 4, 4000))
	if _, err := s.Schedule(req("s1", 2, 2000)); err != nil {
		t.Fatal(err)
	}
	cpu, mem, err := s.Utilisation("r1")
	if err != nil {
		t.Fatal(err)
	}
	// 2 of 8 CPU, 2000 of 8000 MiB.
	if cpu != 0.25 || mem != 0.25 {
		t.Errorf("utilisation = %.2f/%.2f, want 0.25/0.25", cpu, mem)
	}
	// An unknown region is empty, not an error.
	if cpu, mem, err = s.Utilisation("nowhere"); err != nil || cpu != 0 || mem != 0 {
		t.Errorf("unknown region = %.2f/%.2f err=%v", cpu, mem, err)
	}
}

func TestNodesSnapshot(t *testing.T) {
	s, _ := newSched(t, node("n1", 4, 4096, func(n *store.NodeRecord) {
		n.Labels = map[string]string{"pool": "a"}
	}))
	nodes, err := s.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].Labels["pool"] != "a" {
		t.Errorf("nodes = %+v", nodes)
	}
}

// Warm snapshots are the only scoring term denominated in host CPU rather than
// latency, and CPU is what bounds a node's create rate. So the tests below are
// about the ordering holding and about a cold node staying placeable -- the second
// matters more, because a cluster where a new CPU generation became unschedulable
// until somebody prewarmed it would be worse than any slow create.

// warmImage marks an image as cached and warm on a node.
func warmImage(ref string) func(*store.NodeRecord) {
	return func(n *store.NodeRecord) {
		n.CachedImages[ref] = store.CachedImage{SizeBytes: 1 << 30, Warm: true}
	}
}

// coldImage marks an image as cached but not warm.
func coldImage(ref string) func(*store.NodeRecord) {
	return func(n *store.NodeRecord) {
		n.CachedImages[ref] = store.CachedImage{SizeBytes: 1 << 30}
	}
}

func TestWarmNodeWinsOverACachedButColdOne(t *testing.T) {
	s, _ := newSched(t,
		node("cold", 8, 8192, coldImage("img:1")),
		node("warm", 8, 8192, warmImage("img:1")),
	)
	// Repeated because a single placement could be right by accident: both nodes are
	// feasible and identical apart from the warm flag, so a scorer ignoring it would
	// pick by map iteration order and pass roughly half the time.
	for i := 0; i < 8; i++ {
		got, err := s.Schedule(req(store.NewID("sbx"), 1, 512))
		if err != nil {
			t.Fatal(err)
		}
		if got != "warm" {
			t.Fatalf("placed on %s; a create there boots and costs about five times "+
				"the host CPU of one that restores", got)
		}
		if err := s.Release(""); err != nil {
			_ = err
		}
	}
}

// TestWarmScoresOnTopOfImageAffinity is the property that keeps the two terms from
// being confused. A warm node necessarily has the image, so if warm replaced the
// affinity term rather than adding to it, warm and cold-but-cached would tie.
func TestWarmScoresOnTopOfImageAffinity(t *testing.T) {
	warm := node("warm", 8, 8192, warmImage("img:1"))
	cold := node("cold", 8, 8192, coldImage("img:1"))
	bare := node("bare", 8, 8192)

	s, _ := newSched(t, warm, cold, bare)
	spread := map[string]int{}
	r := req("sbx-score", 1, 512)

	warmScore := s.score(warm, spread, r)
	coldScore := s.score(cold, spread, r)
	bareScore := s.score(bare, spread, r)

	if !(warmScore > coldScore && coldScore > bareScore) {
		t.Errorf("scores are warm=%v cold=%v bare=%v; want strictly decreasing, so "+
			"that warm beats cached-but-cold and cached beats nothing",
			warmScore, coldScore, bareScore)
	}

	// The ordering above is necessary but not sufficient, and an earlier version of
	// this test stopped there. Replacing `score +=` with `score =` for the warm term
	// still leaves warm above cold whenever the warm weight exceeds the affinity
	// weight, so the ordering assertion passed against exactly the implementation it
	// was written to reject.
	//
	// Asserted as an arithmetic identity instead: the gap warm opens over cold must be
	// the warm weight itself. That holds only if the term is added to everything else
	// the node scored, which is the property in question.
	gap := warmScore - coldScore
	if diff := gap - s.weights.WarmSnapshot; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("warm scored %v above cold, want exactly the warm weight %v; a "+
			"difference means the warm term replaced part of the score instead of "+
			"adding to it, which loses whatever else the node had earned",
			gap, s.weights.WarmSnapshot)
	}
}

// TestAColdNodeIsStillPlaceable is the safety property. A node with no warm
// snapshot -- a new CPU generation, or one nobody has prewarmed -- must take work.
func TestAColdNodeIsStillPlaceable(t *testing.T) {
	s, _ := newSched(t, node("only-cold", 8, 8192, coldImage("img:1")))
	got, err := s.Schedule(req("sbx-cold", 1, 512))
	if err != nil {
		t.Fatalf("a cluster with no warm node refused to place: %v", err)
	}
	if got != "only-cold" {
		t.Errorf("placed on %q", got)
	}
}

// TestWarmForADifferentImageDoesNotCount guards the lookup being by reference. A
// node warm for something else is no better than cold for this request, and scoring
// it as warm would send work to a node that boots.
func TestWarmForADifferentImageDoesNotCount(t *testing.T) {
	other := node("warm-elsewhere", 8, 8192, warmImage("other:1"), coldImage("img:1"))
	mine := node("warm-here", 8, 8192, warmImage("img:1"))
	s, _ := newSched(t, other, mine)

	spread := map[string]int{}
	r := req("sbx-x", 1, 512)
	if s.score(other, spread, r) >= s.score(mine, spread, r) {
		t.Errorf("a node warm for another image scored at least as high as one warm "+
			"for this request: %v vs %v",
			s.score(other, spread, r), s.score(mine, spread, r))
	}
}

// TestWarmDoesNotOverrideTheCPUConstraint keeps an optimisation from breaking a
// correctness rule. A restore of a memory snapshot only works on a compatible CPU,
// and no amount of warm-snapshot score may place one where the guest would
// misbehave rather than fail cleanly.
func TestWarmDoesNotOverrideTheCPUConstraint(t *testing.T) {
	wrongCPU := node("warm-wrong-cpu", 8, 8192, warmImage("img:1"),
		func(n *store.NodeRecord) {
			n.CPUVendor, n.CPUFamily = "GenuineIntel", 6
		})
	s, _ := newSched(t, wrongCPU)

	_, err := s.Schedule(req("sbx-cpu", 1, 512, func(r *Request) {
		r.CPUConstraint = CPUConstraint{Vendor: "AuthenticAMD", Family: 23}
	}))
	if !errors.Is(err, ErrIncompatibleCPU) {
		t.Errorf("err = %v, want ErrIncompatibleCPU; a warm snapshot must not make "+
			"an incompatible node feasible", err)
	}
}

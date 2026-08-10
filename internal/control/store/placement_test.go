package store

import (
	"testing"
	"time"
)

// node builds a minimally-valid, ready node record for placement tests.
func node(id, region string) *NodeRecord {
	return &NodeRecord{
		ID: id, Region: region, State: string(NodeReady),
		CPUAllocatable: 8, MemoryAllocateMiB: 8192, DiskAllocateMiB: 65536, GPUCount: 0,
		MaxCreates: 4, LastHeartbeat: time.Now(),
	}
}

func TestLoadNodesReturnsEveryRegisteredNode(t *testing.T) {
	st := openTestStore(t)
	for _, n := range []*NodeRecord{node("node-a", "r1"), node("node-b", "r2")} {
		if err := st.UpsertNode(n); err != nil {
			t.Fatal(err)
		}
	}
	nodes, err := st.LoadNodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 2 {
		t.Fatalf("loaded %d nodes, want 2", len(nodes))
	}
	// Ordered by id, so node-a comes first and its fields round-trip.
	if nodes[0].ID != "node-a" || nodes[0].Region != "r1" {
		t.Errorf("node[0] = %+v, want node-a/r1", nodes[0])
	}
	if nodes[0].CPUAllocatable != 8 || nodes[0].MemoryAllocateMiB != 8192 {
		t.Errorf("allocatable did not round-trip: %+v", nodes[0])
	}
}

func TestPutNodeImagesRoundTrips(t *testing.T) {
	st := openTestStore(t)
	if err := st.UpsertNode(node("node-i", "r1")); err != nil {
		t.Fatal(err)
	}
	imgs := map[string]CachedImage{
		"python:3.12": {SizeBytes: 1234, Digest: "sha256:abc", Warm: true},
	}
	if err := st.PutNodeImages("node-i", imgs); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetNode("node-i")
	if err != nil {
		t.Fatal(err)
	}
	ci, ok := got.CachedImages["python:3.12"]
	if !ok {
		t.Fatalf("cached image not persisted: %+v", got.CachedImages)
	}
	if ci.SizeBytes != 1234 || ci.Digest != "sha256:abc" || !ci.Warm {
		t.Errorf("cached image = %+v, want size 1234 / sha256:abc / warm", ci)
	}
}

func TestTouchNodeClearsSuspectAndRecordsDisk(t *testing.T) {
	st := openTestStore(t)
	if err := st.UpsertNode(node("node-t", "r1")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetNodeState("node-t", "SUSPECT"); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchNode("node-t", 4096); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetNode("node-t")
	if err != nil {
		t.Fatal(err)
	}
	// A heartbeat from a SUSPECT node returns it to READY and records the disk
	// figure from the same beat.
	if got.State != "READY" {
		t.Errorf("state = %q, want READY after heartbeat", got.State)
	}
	if got.DiskUsedMiB != 4096 {
		t.Errorf("disk used = %d, want 4096", got.DiskUsedMiB)
	}
}

func TestStaleNodesRespectsCutoffAndExclusions(t *testing.T) {
	st := openTestStore(t)
	old := node("node-old", "r1")
	old.LastHeartbeat = time.Now().Add(-time.Hour)
	fresh := node("node-fresh", "r1")
	fresh.LastHeartbeat = time.Now()
	lostOld := node("node-lost", "r1")
	lostOld.LastHeartbeat = time.Now().Add(-time.Hour)
	lostOld.State = "LOST"
	for _, n := range []*NodeRecord{old, fresh, lostOld} {
		if err := st.UpsertNode(n); err != nil {
			t.Fatal(err)
		}
	}
	cutoff := time.Now().Add(-time.Minute)
	stale, err := st.StaleNodes(cutoff, "LOST")
	if err != nil {
		t.Fatal(err)
	}
	// node-old is stale and not excluded; node-fresh is recent; node-lost is
	// stale but excluded by state.
	if len(stale) != 1 || stale[0].ID != "node-old" {
		t.Fatalf("stale = %v, want [node-old]", stale)
	}
}

func TestSpreadCountsGroupsReservationsByNode(t *testing.T) {
	st := openTestStore(t)
	if err := st.UpsertNode(node("node-s", "r1")); err != nil {
		t.Fatal(err)
	}
	// An empty spread key is not a query: nothing groups.
	if got, err := st.SpreadCounts(""); err != nil || got != nil {
		t.Fatalf("SpreadCounts(\"\") = %v, %v; want nil, nil", got, err)
	}
	for _, sbx := range []string{"sbx-1", "sbx-2"} {
		if err := st.Reserve("node-s", &Reservation{
			SandboxID: sbx, CPU: 1, MemoryMiB: 512, DiskMiB: 4096, SpreadKey: "run-x",
		}); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := st.SpreadCounts("run-x")
	if err != nil {
		t.Fatal(err)
	}
	if counts["node-s"] != 2 {
		t.Errorf("spread count = %v, want node-s:2", counts)
	}
}

func TestOrphanReservationsFindsReservationsWithoutLiveSandbox(t *testing.T) {
	st := openTestStore(t)
	if err := st.UpsertNode(node("node-o", "r1")); err != nil {
		t.Fatal(err)
	}
	// One reservation whose sandbox never got a row (a gateway that died
	// mid-create), one whose sandbox is running and must not be reclaimed.
	if err := st.Reserve("node-o", &Reservation{SandboxID: "sbx-orphan", CPU: 1, MemoryMiB: 512, DiskMiB: 4096}); err != nil {
		t.Fatal(err)
	}
	if err := st.Reserve("node-o", &Reservation{SandboxID: "sbx-live", CPU: 1, MemoryMiB: 512, DiskMiB: 4096}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSandbox(rec("sbx-live", SandboxRunning, nil)); err != nil {
		t.Fatal(err)
	}
	orphans, err := st.OrphanReservations()
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0] != "sbx-orphan" {
		t.Fatalf("orphans = %v, want [sbx-orphan]", orphans)
	}
}

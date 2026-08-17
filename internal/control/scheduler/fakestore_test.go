package scheduler

import (
	"testing"
	"time"

	"github.com/garysng/bean/internal/control/store"
)

// The interface's purpose, demonstrated: a fake covering the 11 methods the scheduler
// uses, rather than the 39 the concrete store has.
//
// Before this, a scheduler test either used a real SQLite file or was not written. The
// count is the argument -- 11 against 39 is the difference between a fake somebody
// writes and one they work around.
type fakeStore struct {
	nodes    []*store.NodeRecord
	reserved map[string]string
	spread   map[string]int
	failWith error
}

func newFakeStore(nodes ...*store.NodeRecord) *fakeStore {
	return &fakeStore{nodes: nodes, reserved: map[string]string{}, spread: map[string]int{}}
}

func (f *fakeStore) Reserve(nodeID string, res *store.Reservation) error {
	if f.failWith != nil {
		return f.failWith
	}
	f.reserved[res.SandboxID] = nodeID
	return nil
}
func (f *fakeStore) Release(sandboxID string) error      { delete(f.reserved, sandboxID); return nil }
func (f *fakeStore) FinishCreate(sandboxID string) error { return nil }
func (f *fakeStore) OrphanReservations() ([]string, error) {
	return nil, nil
}
func (f *fakeStore) SpreadCounts(key string) (map[string]int, error) { return f.spread, nil }

func (f *fakeStore) GetNode(id string) (*store.NodeRecord, error) {
	for _, n := range f.nodes {
		if n.ID == id {
			return n, nil
		}
	}
	return nil, nil
}
func (f *fakeStore) UpsertNode(n *store.NodeRecord) error               { f.nodes = append(f.nodes, n); return nil }
func (f *fakeStore) LoadNodes() ([]*store.NodeRecord, error)            { return f.nodes, nil }
func (f *fakeStore) SetNodeState(id, state string) (bool, error)        { return true, nil }
func (f *fakeStore) RenewLease(id string) error                         { return nil }
func (f *fakeStore) SetNodeDiskUsed(id string, diskUsedMiB int64) error { return nil }
func (f *fakeStore) StaleNodes(olderThan time.Time, exclude ...string) ([]*store.NodeRecord, error) {
	return nil, nil
}
func (f *fakeStore) PutNodeImages(id string, images map[string]store.CachedImage) error {
	return nil
}

var _ Store = (*fakeStore)(nil)

func TestSchedulesAgainstAFakeStore(t *testing.T) {
	f := newFakeStore(&store.NodeRecord{
		ID: "node-1", Region: "local", State: "READY",
		Runtimes:          []string{"fc"},
		CPUAllocatable:    8,
		MemoryAllocateMiB: 8192,
		DiskAllocateMiB:   100000,
		LastHeartbeat:     time.Now(),
	})
	s := New(f, DefaultWeights())

	node, err := s.Schedule(&Request{
		SandboxID: "sbx-1", Region: "local", Runtime: "fc",
		CPU: 1, MemoryMiB: 512, DiskMiB: 2048,
	})
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if node != "node-1" {
		t.Fatalf("placed on %q, want node-1", node)
	}
	if f.reserved["sbx-1"] != "node-1" {
		t.Fatalf("reservation recorded as %q; the scheduler must commit through the "+
			"store rather than only returning a choice", f.reserved["sbx-1"])
	}
}

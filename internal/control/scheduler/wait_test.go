package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/garysng/bean/internal/control/store"
)

// waitFixture builds a scheduler over one node with the given capacity, which is
// enough to exercise the queue: the decision under test is per-node.
func waitFixture(t *testing.T, n *store.NodeRecord) (*Scheduler, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/bean.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	n.LastHeartbeat = time.Now()
	if n.State == "" {
		n.State = NodeReady
	}
	if err := st.UpsertNode(n); err != nil {
		t.Fatal(err)
	}
	return New(st, Weights{}), st
}

func smallRequest(id string) *Request {
	return &Request{
		SandboxID: id, Region: "local", Runtime: "fc",
		CPU: 1, MemoryMiB: 512, DiskMiB: 1024,
	}
}

func TestScheduleWaitPlacesImmediatelyWhenThereIsRoom(t *testing.T) {
	s, _ := waitFixture(t, &store.NodeRecord{
		ID: "n1", Region: "local", Runtimes: []string{"fc"},
		CPUAllocatable: 8, MemoryAllocateMiB: 8192, DiskAllocateMiB: 102400,
		MaxCreates: 4,
	})
	node, err := s.ScheduleWait(context.Background(), smallRequest("sbx_1"),
		WaitOptions{Timeout: time.Second})
	if err != nil {
		t.Fatalf("expected placement: %v", err)
	}
	if node != "n1" {
		t.Errorf("placed on %q, want n1", node)
	}
}

// The distinction this whole mechanism rests on: a request short of memory will
// still be short of memory later, because nothing frees it. Waiting would return
// the same rejection having also held the caller.
func TestScheduleWaitDoesNotWaitOnLifetimeCommitments(t *testing.T) {
	s, _ := waitFixture(t, &store.NodeRecord{
		ID: "n1", Region: "local", Runtimes: []string{"fc"},
		CPUAllocatable: 8, MemoryAllocateMiB: 256, DiskAllocateMiB: 102400,
		MaxCreates: 4,
	})
	start := time.Now()
	_, err := s.ScheduleWait(context.Background(), smallRequest("sbx_1"),
		WaitOptions{Timeout: 3 * time.Second, Poll: 10 * time.Millisecond})
	elapsed := time.Since(start)

	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected ErrNoCapacity, got %v", err)
	}
	if errors.Is(err, ErrQueueTimeout) {
		t.Error("a memory shortfall must not be reported as a queue timeout: " +
			"waiting cannot fix it")
	}
	if elapsed > time.Second {
		t.Errorf("waited %v on a shortfall that will never clear; should refuse at once",
			elapsed)
	}
}

func TestScheduleWaitWaitsWhenOnlyCreateConcurrencyBlocks(t *testing.T) {
	s, st := waitFixture(t, &store.NodeRecord{
		ID: "n1", Region: "local", Runtimes: []string{"fc"},
		CPUAllocatable: 8, MemoryAllocateMiB: 8192, DiskAllocateMiB: 102400,
		MaxCreates: 1,
	})
	// Saturate create concurrency without consuming meaningful CPU or memory, so
	// concurrency is the sole blocker.
	if err := st.Reserve("n1", &store.Reservation{
		SandboxID: "sbx_holder", CPU: 1, MemoryMiB: 512, DiskMiB: 1024,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Schedule(smallRequest("sbx_blocked")); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected the node to be at its create limit, got %v", err)
	}

	// The slot frees while the request waits, which is exactly the burst shape:
	// the 14 rejected creates in the stress run would have been placed.
	go func() {
		time.Sleep(80 * time.Millisecond)
		_ = st.FinishCreate("n1")
	}()

	node, err := s.ScheduleWait(context.Background(), smallRequest("sbx_waiter"),
		WaitOptions{Timeout: 3 * time.Second, Poll: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("expected the wait to be placed once a slot freed: %v", err)
	}
	if node != "n1" {
		t.Errorf("placed on %q, want n1", node)
	}
}

func TestScheduleWaitReportsAQueueTimeoutDistinctFromNoCapacity(t *testing.T) {
	s, st := waitFixture(t, &store.NodeRecord{
		ID: "n1", Region: "local", Runtimes: []string{"fc"},
		CPUAllocatable: 8, MemoryAllocateMiB: 8192, DiskAllocateMiB: 102400,
		MaxCreates: 1,
	})
	if err := st.Reserve("n1", &store.Reservation{
		SandboxID: "sbx_holder", CPU: 1, MemoryMiB: 512, DiskMiB: 1024,
	}); err != nil {
		t.Fatal(err)
	}

	_, err := s.ScheduleWait(context.Background(), smallRequest("sbx_waiter"),
		WaitOptions{Timeout: 120 * time.Millisecond, Poll: 10 * time.Millisecond})
	if !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("expected ErrQueueTimeout so the gateway can answer 504 rather "+
			"than 503, got %v", err)
	}
}

func TestScheduleWaitStopsWhenTheCallerHangsUp(t *testing.T) {
	s, st := waitFixture(t, &store.NodeRecord{
		ID: "n1", Region: "local", Runtimes: []string{"fc"},
		CPUAllocatable: 8, MemoryAllocateMiB: 8192, DiskAllocateMiB: 102400,
		MaxCreates: 1,
	})
	if err := st.Reserve("n1", &store.Reservation{
		SandboxID: "sbx_holder", CPU: 1, MemoryMiB: 512, DiskMiB: 1024,
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(40 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err := s.ScheduleWait(ctx, smallRequest("sbx_waiter"),
		WaitOptions{Timeout: 10 * time.Second, Poll: 10 * time.Millisecond})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled request is not a capacity problem; want context.Canceled, "+
			"got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("kept waiting %v after the caller hung up", elapsed)
	}
}

// Found on hardware, not in a unit test: 6 of 30 creates were refused with
// "createConcurrency blocked 1/1" — the exact case that should have queued.
//
// The cause was treating "no node is blocked any more" as a reason to stop
// waiting. worthWaiting runs after a failed Schedule, and under a burst the
// in-flight count routinely drops in between, so a node that has become feasible
// must mean "retry", not "refuse".
func TestScheduleWaitRetriesWhenTheNodeBecameFeasibleAfterTheFailure(t *testing.T) {
	s, _ := waitFixture(t, &store.NodeRecord{
		ID: "n1", Region: "local", Runtimes: []string{"fc"},
		CPUAllocatable: 8, MemoryAllocateMiB: 8192, DiskAllocateMiB: 102400,
		MaxCreates: 1,
	})
	// A node with nothing blocking it is exactly the racy read: the first Schedule
	// lost a slot to a peer, and by the time the question is asked the slot is back.
	if !s.worthWaiting(smallRequest("sbx_1")) {
		t.Fatal("a feasible node must be treated as worth retrying; refusing here " +
			"throws away a request that was a moment from being placed")
	}
}

func TestScheduleWaitWithZeroTimeoutBehavesLikeSchedule(t *testing.T) {
	s, st := waitFixture(t, &store.NodeRecord{
		ID: "n1", Region: "local", Runtimes: []string{"fc"},
		CPUAllocatable: 8, MemoryAllocateMiB: 8192, DiskAllocateMiB: 102400,
		MaxCreates: 1,
	})
	if err := st.Reserve("n1", &store.Reservation{
		SandboxID: "sbx_holder", CPU: 1, MemoryMiB: 512, DiskMiB: 1024,
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := s.ScheduleWait(context.Background(), smallRequest("sbx_waiter"),
		WaitOptions{})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("expected an immediate ErrNoCapacity, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("zero timeout must not wait, took %v", elapsed)
	}
}

// Attribution: the resources masquerade as each other, which is what made a
// 16-success burst look like the create limit when it was the core count.
func TestRejectionNamesTheResourceThatRanOut(t *testing.T) {
	for _, tc := range []struct {
		name string
		node *store.NodeRecord
		want string
	}{
		{
			name: "disk",
			node: &store.NodeRecord{ID: "n1", Region: "local", Runtimes: []string{"fc"},
				CPUAllocatable: 8, MemoryAllocateMiB: 8192, DiskAllocateMiB: 512,
				MaxCreates: 4},
			want: "disk",
		},
		{
			name: "memory",
			node: &store.NodeRecord{ID: "n1", Region: "local", Runtimes: []string{"fc"},
				CPUAllocatable: 8, MemoryAllocateMiB: 128, DiskAllocateMiB: 102400,
				MaxCreates: 4},
			want: "memory",
		},
		{
			name: "cpu",
			node: &store.NodeRecord{ID: "n1", Region: "local", Runtimes: []string{"fc"},
				CPUAllocatable: 0.5, MemoryAllocateMiB: 8192, DiskAllocateMiB: 102400,
				MaxCreates: 4},
			want: "cpu",
		},
		{
			name: "runtime",
			node: &store.NodeRecord{ID: "n1", Region: "local", Runtimes: []string{"local"},
				CPUAllocatable: 8, MemoryAllocateMiB: 8192, DiskAllocateMiB: 102400,
				MaxCreates: 4},
			want: "runtime",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := waitFixture(t, tc.node)
			_, err := s.Schedule(smallRequest("sbx_1"))
			if err == nil {
				t.Fatal("expected a rejection")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("rejection does not name %q, so an operator cannot tell "+
					"which limit to raise: %v", tc.want, err)
			}
		})
	}
}

func TestRejectionNamesEveryBlockingResourceNotJustTheFirst(t *testing.T) {
	// A node short on two resources is not fixed by raising one, so reporting only
	// the first sends an operator round the loop twice.
	s, _ := waitFixture(t, &store.NodeRecord{
		ID: "n1", Region: "local", Runtimes: []string{"fc"},
		CPUAllocatable: 0.5, MemoryAllocateMiB: 128, DiskAllocateMiB: 512,
		MaxCreates: 4,
	})
	_, err := s.Schedule(smallRequest("sbx_1"))
	if err == nil {
		t.Fatal("expected a rejection")
	}
	for _, want := range []string{"cpu", "memory", "disk"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("rejection omits %q: %v", want, err)
		}
	}
}

func TestRejectionReportsTheShortfall(t *testing.T) {
	s, _ := waitFixture(t, &store.NodeRecord{
		ID: "n1", Region: "local", Runtimes: []string{"fc"},
		CPUAllocatable: 8, MemoryAllocateMiB: 8192, DiskAllocateMiB: 512,
		MaxCreates: 4,
	})
	// Requesting 1024 MiB against 512 allocatable is 512 short, which is the
	// number that tells an operator how much to add.
	_, err := s.Schedule(smallRequest("sbx_1"))
	if err == nil {
		t.Fatal("expected a rejection")
	}
	if !strings.Contains(err.Error(), "512 MiB") {
		t.Errorf("rejection does not quantify the shortfall: %v", err)
	}
}

func TestRejectionSaysSoWhenTheRegionIsEmpty(t *testing.T) {
	s, _ := waitFixture(t, &store.NodeRecord{
		ID: "n1", Region: "elsewhere", Runtimes: []string{"fc"},
		CPUAllocatable: 8, MemoryAllocateMiB: 8192, DiskAllocateMiB: 102400,
		MaxCreates: 4,
	})
	_, err := s.Schedule(smallRequest("sbx_1"))
	if err == nil {
		t.Fatal("expected a rejection")
	}
	// "cpu blocked 0/0" would be nonsense; an empty region is a configuration
	// problem, not a capacity one.
	if !strings.Contains(err.Error(), "no node is registered in region local") {
		t.Errorf("an empty region should be reported as such: %v", err)
	}
}

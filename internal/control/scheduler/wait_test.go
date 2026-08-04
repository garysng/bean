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

// bigRequest wants more memory than a fixture node has left once smallRequest's
// worth is held, so it is refused for a reason that waiting cannot fix.
func bigRequest(id string) *Request {
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

// TestScheduleWaitNoLongerNeedsToWaitOnConcurrency replaces
// TestScheduleWaitWaitsWhenOnlyCreateConcurrencyBlocks, which tested the case this
// file was originally written for: a node at its create limit, where waiting was the
// right answer because the limit drains on its own.
//
// That case cannot occur now. Create concurrency is a placement score rather than an
// admission check, so a saturated pipeline never produces a rejection to wait on --
// the placement simply happens, on that node or a quieter one. What is asserted here
// is the consequence: saturating concurrency and asking again must succeed
// immediately, with no queueing involved at all.
func TestScheduleWaitNoLongerNeedsToWaitOnConcurrency(t *testing.T) {
	s, st := waitFixture(t, &store.NodeRecord{
		ID: "n1", Region: "local", Runtimes: []string{"fc"},
		CPUAllocatable: 64, MemoryAllocateMiB: 65536, DiskAllocateMiB: 1 << 20,
		MaxCreates: 1,
	})
	if err := st.Reserve("n1", &store.Reservation{
		SandboxID: "sbx_holder", CPU: 1, MemoryMiB: 512, DiskMiB: 1024,
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	got, err := s.ScheduleWait(context.Background(), smallRequest("sbx_next"),
		WaitOptions{Timeout: 3 * time.Second, Poll: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("refused or queued a create while the node was over its preferred "+
			"concurrency: %v", err)
	}
	if got != "n1" {
		t.Errorf("placed on %q", got)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("waited %v for concurrency that no longer blocks", elapsed)
	}
}

// TestScheduleWaitRefusesAtOnceWhenWaitingCannotHelp is what the queue-timeout test
// became.
//
// It used to saturate create concurrency and expect ErrQueueTimeout. Concurrency no
// longer refuses, so the only rejections left are lifetime commitments -- CPU, memory,
// disk -- and those do not free themselves while a caller waits. The right answer for
// them is an immediate refusal, and holding the caller for the full timeout would be
// worse than the old behaviour rather than better.
//
// ErrQueueTimeout is still reachable, but only for genuine contention: a node that
// reads as feasible every round while every Reserve loses the race to a peer. That
// needs concurrent writers and is covered by the store's own tests, not here.
func TestScheduleWaitRefusesAtOnceWhenWaitingCannotHelp(t *testing.T) {
	s, st := waitFixture(t, &store.NodeRecord{
		ID: "n1", Region: "local", Runtimes: []string{"fc"},
		CPUAllocatable: 8, MemoryAllocateMiB: 768, DiskAllocateMiB: 102400,
		MaxCreates: 8,
	})
	if err := st.Reserve("n1", &store.Reservation{
		SandboxID: "sbx_holder", CPU: 1, MemoryMiB: 512, DiskMiB: 1024,
	}); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err := s.ScheduleWait(context.Background(), bigRequest("sbx_waiter"),
		WaitOptions{Timeout: 5 * time.Second, Poll: 10 * time.Millisecond})
	if !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("want ErrNoCapacity for a shortfall that cannot clear, got %v", err)
	}
	if errors.Is(err, ErrQueueTimeout) {
		t.Error("reported a queue timeout for a memory shortfall: waiting cannot fix " +
			"it, and the two need different responses from a caller")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("held the caller %v on a shortfall that will never clear", elapsed)
	}
}

// TestScheduleWaitStopsWhenTheCallerHangsUp keeps the cancellation contract. The wait
// is driven by a node that stays feasible on every read, which is the only shape that
// still loops.
func TestScheduleWaitStopsWhenTheCallerHangsUp(t *testing.T) {
	s, _ := waitFixture(t, &store.NodeRecord{
		ID: "n1", Region: "local", Runtimes: []string{"fc"},
		CPUAllocatable: 8, MemoryAllocateMiB: 8192, DiskAllocateMiB: 102400,
		MaxCreates: 8,
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Cancelled before the call, so the result cannot depend on winning a race with
	// a sleeping goroutine -- an earlier version cancelled after 40ms and would have
	// passed for the wrong reason if placement had simply been fast.
	_, err := s.ScheduleWait(ctx, bigRequest("sbx_waiter"),
		WaitOptions{Timeout: 10 * time.Second, Poll: 10 * time.Millisecond})
	if err == nil {
		t.Fatal("placed a sandbox for a caller that had already hung up")
	}
	if errors.Is(err, ErrQueueTimeout) {
		t.Errorf("a cancelled request is not a capacity problem: %v", err)
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
		CPUAllocatable: 8, MemoryAllocateMiB: 768, DiskAllocateMiB: 102400,
		MaxCreates: 8,
	})
	if err := st.Reserve("n1", &store.Reservation{
		SandboxID: "sbx_holder", CPU: 1, MemoryMiB: 512, DiskMiB: 1024,
	}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err := s.ScheduleWait(context.Background(), bigRequest("sbx_waiter"),
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

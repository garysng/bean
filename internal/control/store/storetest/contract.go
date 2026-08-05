// Package storetest is the conformance suite for the store interfaces.
//
// The compile-time assertions in the store package check shape: the right method names
// with the right signatures. They cannot check the property those interfaces exist for
// -- that each operation's atomicity belongs to the database rather than to the caller's
// process. An implementation that reads a row, decides in Go, and then writes satisfies
// every assertion and silently loses concurrent updates.
//
// That is not hypothetical. It is what the SQLite store did until recently, and it was
// invisible because a process-local mutex made the loss impossible to reproduce through
// a single handle. Measured after removing it: two connections each doing an unguarded
// read-then-write to one row lost 194 of 200 updates.
//
// So the contract is runnable rather than written down. A second engine calls Run and
// gets the same scrutiny, instead of writing its own concurrency tests -- which would
// test what their author already believed.
//
// It lives in its own package so importing it cannot pull test-only code into a
// production build, and so a Postgres implementation in another package can use it
// without an import cycle.
package storetest

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/garysng/bean/internal/control/store"
)

// Store is the slice of the store this suite exercises. Narrow on purpose: these are
// the operations whose atomicity is load-bearing, and an implementation should be able
// to run the suite before the rest of it exists.
type Store interface {
	store.Snapshots
	store.Placement
	store.Nodes
}

// OpenFunc returns a handle to the same underlying database each time it is called.
//
// Separate handles are the whole point: a process-local lock inside one handle would
// serialise every caller and make the races below unreproducible. Two handles are what
// two control-plane replicas are, from the database's point of view.
type OpenFunc func(t *testing.T) Store

// Run executes the conformance suite.
func Run(t *testing.T, open OpenFunc) {
	t.Run("AcquireCountsEveryReference", func(t *testing.T) {
		acquireCountsEveryReference(t, open)
	})
	t.Run("AcquireRefusesUnready", func(t *testing.T) {
		acquireRefusesUnready(t, open)
	})
	t.Run("DeleteRefusesWhileReferenced", func(t *testing.T) {
		deleteRefusesWhileReferenced(t, open)
	})
	t.Run("DeleteRefusesWithDescendants", func(t *testing.T) {
		deleteRefusesWithDescendants(t, open)
	})
	t.Run("ReserveDoesNotOversell", func(t *testing.T) {
		reserveDoesNotOversell(t, open)
	})
	t.Run("SetNodeStateReportsChange", func(t *testing.T) {
		setNodeStateReportsChange(t, open)
	})
}

// acquireCountsEveryReference is requirement 1: N successful acquires leave ref_count
// at N, under concurrency, across connections.
//
// A lost increment means a restore holds a reference the database does not know about,
// so the snapshot becomes deletable while it is being read.
func acquireCountsEveryReference(t *testing.T, open OpenFunc) {
	const handles = 4
	stores := make([]Store, handles)
	for i := range stores {
		stores[i] = open(t)
	}

	for round := 0; round < 20; round++ {
		id := fmt.Sprintf("snap-acq-%d", round)
		if err := stores[0].PutSnapshot(&store.Snapshot{
			ID: id, SandboxID: "sbx", State: store.SnapshotReady,
		}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}

		var wg sync.WaitGroup
		var mu sync.Mutex
		succeeded := 0
		start := make(chan struct{})
		for _, st := range stores {
			s := st
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := s.AcquireSnapshot(id); err == nil {
					mu.Lock()
					succeeded++
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()

		snap, err := stores[0].GetSnapshot(id)
		if err != nil {
			t.Fatalf("read back %s: %v", id, err)
		}
		if snap == nil {
			t.Fatalf("%s vanished", id)
		}
		if snap.RefCount != succeeded {
			t.Fatalf("%s: %d acquires reported success but ref_count is %d. A restore "+
				"holds a reference the database does not know about, so this snapshot "+
				"can be deleted while it is being read. The check and the increment "+
				"have to be one statement", id, succeeded, snap.RefCount)
		}
	}
}

// acquireRefusesUnready is requirement 2.
func acquireRefusesUnready(t *testing.T, open OpenFunc) {
	s := open(t)
	if err := s.PutSnapshot(&store.Snapshot{
		ID: "creating", SandboxID: "sbx", State: store.SnapshotCreating,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireSnapshot("creating"); err == nil {
		t.Fatal("acquired a snapshot that is not ready")
	}
	if _, err := s.AcquireSnapshot("absent"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("acquiring an absent snapshot gave %v, want ErrNotFound so the API "+
			"answers 404 rather than 409", err)
	}
}

// deleteRefusesWhileReferenced is requirement 3, held across two connections so a
// process-local lock cannot be what enforces it.
func deleteRefusesWhileReferenced(t *testing.T, open OpenFunc) {
	a, b := open(t), open(t)
	if err := a.PutSnapshot(&store.Snapshot{
		ID: "held", SandboxID: "sbx", State: store.SnapshotReady,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AcquireSnapshot("held"); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	err := b.DeleteSnapshot("held")
	if err == nil {
		t.Fatal("deleted a snapshot another connection holds a reference to")
	}
	if !errors.Is(err, store.ErrInUse) {
		t.Fatalf("got %v, want ErrInUse so the API answers 409", err)
	}

	// Released, it becomes deletable -- otherwise the refusal is permanent and a
	// snapshot could never be reclaimed.
	if err := a.ReleaseSnapshot("held"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := b.DeleteSnapshot("held"); err != nil {
		t.Fatalf("delete after release: %v", err)
	}
}

// deleteRefusesWithDescendants is the other half of requirement 3.
//
// An incremental snapshot holds only what changed since its base, so deleting the base
// leaves it unrestorable -- and the failure would surface at merge time, long after the
// deletion that caused it, on a different machine.
func deleteRefusesWithDescendants(t *testing.T, open OpenFunc) {
	s := open(t)
	if err := s.PutSnapshot(&store.Snapshot{
		ID: "base", SandboxID: "sbx", State: store.SnapshotReady,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSnapshot(&store.Snapshot{
		ID: "diff", SandboxID: "sbx", State: store.SnapshotReady, BaseID: "base",
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteSnapshot("base"); !errors.Is(err, store.ErrInUse) {
		t.Fatalf("deleting a base with a descendant gave %v, want ErrInUse", err)
	}
	if err := s.DeleteSnapshot("diff"); err != nil {
		t.Fatalf("delete the descendant: %v", err)
	}
	if err := s.DeleteSnapshot("base"); err != nil {
		t.Fatalf("delete the base once it has no descendants: %v", err)
	}
}

// reserveDoesNotOversell is requirement 4, and the one with the worst consequence:
// overselling memory kills a guest, overselling disk destroys a copy-on-write layer.
//
// The assertion is the invariant rather than a success count, and that distinction was
// learned by getting it wrong. An earlier version launched four concurrent reservations
// at a node with room for two and checked that at most two were granted. It passed
// against a store with every capacity guard deleted -- because SQLite's write lock
// refused three of the four with SQLITE_BUSY before they reached the check. The test was
// measuring the engine's locking, not this package's correctness.
//
// So reservations are issued serially, and what is asserted is that committed never
// exceeds allocatable. A store that checks capacity outside the committing statement
// fails this the moment it grants one too many, without needing the failure to be a
// race.
func reserveDoesNotOversell(t *testing.T, open OpenFunc) {
	s := open(t)

	// Room for exactly two of the reservations below.
	if err := s.UpsertNode(&store.NodeRecord{
		ID: "node-1", Region: "local", State: "READY",
		Runtimes:          []string{"fc"},
		CPUAllocatable:    2,
		MemoryAllocateMiB: 1024,
		DiskAllocateMiB:   4096,
		LastHeartbeat:     time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	granted := 0
	for i := 0; i < 4; i++ {
		err := s.Reserve("node-1", &store.Reservation{
			SandboxID: fmt.Sprintf("sbx-%d", i),
			CPU:       1,
			MemoryMiB: 512,
			DiskMiB:   2048,
		})
		if err == nil {
			granted++
			continue
		}
		if !errors.Is(err, store.ErrCapacityChanged) {
			t.Fatalf("reserve %d failed for an unexpected reason: %v", i, err)
		}
	}

	node, err := s.GetNode("node-1")
	if err != nil {
		t.Fatal(err)
	}
	if node.MemoryCommitMiB > node.MemoryAllocateMiB {
		t.Errorf("committed memory %d exceeds allocatable %d after %d grants; capacity "+
			"was checked outside the statement that committed it",
			node.MemoryCommitMiB, node.MemoryAllocateMiB, granted)
	}
	if node.DiskCommitMiB > node.DiskAllocateMiB {
		t.Errorf("committed disk %d exceeds allocatable %d after %d grants",
			node.DiskCommitMiB, node.DiskAllocateMiB, granted)
	}
	if node.CPUCommitted > node.CPUAllocatable {
		t.Errorf("committed CPU %.1f exceeds allocatable %.1f after %d grants",
			node.CPUCommitted, node.CPUAllocatable, granted)
	}
	if granted != 2 {
		t.Errorf("granted %d reservations, want exactly 2: the node had room for two "+
			"and the third must be refused", granted)
	}
}

// setNodeStateReportsChange is requirement 5. Without it a health sweep cannot tell a
// node it just marked lost from one that was already lost, and logs the transition on
// every pass.
func setNodeStateReportsChange(t *testing.T, open OpenFunc) {
	s := open(t)
	if err := s.UpsertNode(&store.NodeRecord{
		ID: "node-1", Region: "local", State: "READY", LastHeartbeat: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	changed, err := s.SetNodeState("node-1", "LOST")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("the first transition to LOST reported no change")
	}

	changed, err = s.SetNodeState("node-1", "LOST")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatal("setting a node to the state it already holds reported a change; a " +
			"health sweep would announce the same node as newly lost on every pass")
	}
}

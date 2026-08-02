package store

import (
	"errors"
	"testing"
	"time"
)

// putChainLink stores a ready snapshot pointing at base.
func putChainLink(t *testing.T, s *Store, id, base string, depth int) {
	t.Helper()
	mem := true
	if err := s.PutSnapshot(&Snapshot{
		ID: id, State: SnapshotReady, SandboxID: "sbx_1", Image: "alpine:3.20",
		BaseID: base, ChainDepth: depth, IncludeMemory: &mem,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSnapshotChainIsOrderedBaseFirst is the property a restore depends on. A
// diff's pages overwrite its base's, so a chain replayed in the wrong order
// produces a coherent-looking memory image built from stale pages — which nothing
// downstream can detect, making the ordering this function's responsibility alone.
func TestSnapshotChainIsOrderedBaseFirst(t *testing.T) {
	s := openTestStore(t)
	putChainLink(t, s, "snap_root", "", 0)
	putChainLink(t, s, "snap_mid", "snap_root", 1)
	putChainLink(t, s, "snap_leaf", "snap_mid", 2)

	chain, err := s.SnapshotChain("snap_leaf")
	if err != nil {
		t.Fatalf("SnapshotChain: %v", err)
	}
	want := []string{"snap_root", "snap_mid", "snap_leaf"}
	if len(chain) != len(want) {
		t.Fatalf("chain has %d links, want %d", len(chain), len(want))
	}
	for i, id := range want {
		if chain[i].ID != id {
			t.Errorf("chain[%d] = %s, want %s", i, chain[i].ID, id)
		}
	}
}

// TestSnapshotChainOfSelfContainedIsItself keeps the common case free of special
// handling: a full snapshot is a chain of one, so restore has a single path.
func TestSnapshotChainOfSelfContainedIsItself(t *testing.T) {
	s := openTestStore(t)
	putChainLink(t, s, "snap_solo", "", 0)

	chain, err := s.SnapshotChain("snap_solo")
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 1 || chain[0].ID != "snap_solo" {
		t.Errorf("chain = %v, want just snap_solo", chain)
	}
}

// TestSnapshotChainReportsAMissingBase covers a chain whose ancestor is gone.
// DeleteSnapshot prevents this, so reaching it means the records were damaged some
// other way — and a restore has to fail rather than reconstruct a guest from a
// partial chain, which would give it memory full of holes.
func TestSnapshotChainReportsAMissingBase(t *testing.T) {
	s := openTestStore(t)
	putChainLink(t, s, "snap_orphan", "snap_vanished", 1)

	_, err := s.SnapshotChain("snap_orphan")
	if err == nil {
		t.Fatal("built a chain with a missing base")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error %v is not ErrNotFound", err)
	}
}

// TestDeleteSnapshotRefusesABaseWithDescendants is what keeps the case above from
// happening. The failure it prevents is remote in both time and place: the delete
// succeeds now, and the restore breaks later on another machine, with nothing to
// connect them.
func TestDeleteSnapshotRefusesABaseWithDescendants(t *testing.T) {
	s := openTestStore(t)
	putChainLink(t, s, "snap_base", "", 0)
	putChainLink(t, s, "snap_child", "snap_base", 1)

	err := s.DeleteSnapshot("snap_base")
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("DeleteSnapshot error = %v, want ErrInUse", err)
	}

	// The leaf goes freely, and its base becomes deletable once it has.
	if err := s.DeleteSnapshot("snap_child"); err != nil {
		t.Fatalf("deleting the leaf: %v", err)
	}
	if err := s.DeleteSnapshot("snap_base"); err != nil {
		t.Fatalf("deleting the base after its child: %v", err)
	}
}

// TestSnapshotChainSurvivesACycle guards against hanging on damaged records. The
// write path cannot create a cycle, so this is about failing rather than looping
// if one ever exists.
func TestSnapshotChainSurvivesACycle(t *testing.T) {
	s := openTestStore(t)
	putChainLink(t, s, "snap_a", "snap_b", 1)
	putChainLink(t, s, "snap_b", "snap_a", 1)

	done := make(chan error, 1)
	go func() {
		_, err := s.SnapshotChain("snap_a")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cyclic chain was accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SnapshotChain hung on a cyclic chain")
	}
}

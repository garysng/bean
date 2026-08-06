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
	"bytes"
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
	store.RegistryCredentials
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
	t.Run("CiphertextSurvivesRoundTrip", func(t *testing.T) {
		ciphertextSurvivesRoundTrip(t, open)
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

	// GPUs get their own node because the dimension has to be the only thing that can
	// refuse. Reusing node-1 above would let an exhausted CPU or disk guard produce the
	// refusal and report a pass for a GPU check that does not exist -- which is what
	// happened: the statement had no GPU condition at all for as long as this suite has
	// existed, and every requirement passed.
	//
	// Deliberately generous on every other dimension for the same reason.
	if err := s.UpsertNode(&store.NodeRecord{
		ID: "node-gpu", Region: "local", State: "READY",
		Runtimes:          []string{"fc"},
		CPUAllocatable:    64,
		MemoryAllocateMiB: 262144,
		DiskAllocateMiB:   1048576,
		GPUCount:          1,
		LastHeartbeat:     time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	gpuGranted := 0
	for i := 0; i < 3; i++ {
		err := s.Reserve("node-gpu", &store.Reservation{
			SandboxID: fmt.Sprintf("sbx-gpu-%d", i),
			CPU:       1,
			MemoryMiB: 512,
			DiskMiB:   2048,
			GPU:       1,
		})
		if err == nil {
			gpuGranted++
			continue
		}
		if !errors.Is(err, store.ErrCapacityChanged) {
			t.Fatalf("gpu reserve %d failed for an unexpected reason: %v", i, err)
		}
	}

	gpuNode, err := s.GetNode("node-gpu")
	if err != nil {
		t.Fatal(err)
	}
	if gpuNode.GPUCommitted > gpuNode.GPUCount {
		t.Errorf("committed %d GPUs on a node with %d after %d grants; the same physical "+
			"device is handed to two guests, and the failure surfaces inside one of them "+
			"as a device already in use",
			gpuNode.GPUCommitted, gpuNode.GPUCount, gpuGranted)
	}
	if gpuGranted != 1 {
		t.Errorf("granted %d GPU reservations on a single-GPU node, want exactly 1",
			gpuGranted)
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

// ciphertextSurvivesRoundTrip requires that opaque bytes come back byte-identical.
//
// This requirement exists because of a real failure. The dialect layer was sized by
// counting placeholders and grepping for AUTOINCREMENT, which found 103 binds and two DDL
// constructs; it missed `secret BLOB NOT NULL`, and Postgres rejects the whole schema with
// `type "blob" does not exist`. The suite could not have caught it earlier: no requirement
// touched the credentials table, so the column's type was unconstrained on any engine but
// the one it was written for.
//
// The payload is deliberately not text. A column declared TEXT instead of BYTEA would pass
// a round trip of printable ASCII and corrupt real ciphertext, so the bytes here include a
// zero, a 0xff, and an invalid UTF-8 sequence -- the values an encoding conversion would
// mangle. Storing plaintext would also pass; what is being checked is the transport, since
// the store never decrypts anything.
func ciphertextSurvivesRoundTrip(t *testing.T, open OpenFunc) {
	s := open(t)

	ciphertext := []byte{0x00, 0x01, 0xff, 0xfe, 0x80, 0xc3, 0x28, 0x7f, 0x00, 0xab}
	host := "registry.conformance.invalid"

	if err := s.PutRegistryCredential(&store.RegistryCredential{
		Host:             host,
		Username:         "robot",
		SecretCiphertext: ciphertext,
	}); err != nil {
		t.Fatalf("put credential: %v", err)
	}

	got, err := s.GetRegistryCredential(host)
	if err != nil {
		t.Fatalf("get credential: %v", err)
	}
	if got == nil {
		t.Fatal("credential is missing after a successful put; a pull of a private " +
			"image would fall back to anonymous and fail with a permission error " +
			"that points at the registry rather than at the store")
	}
	if !bytes.Equal(got.SecretCiphertext, ciphertext) {
		t.Errorf("ciphertext changed in the database: stored %x, read back %x; "+
			"decryption will fail and the error will name the cipher, not the column",
			ciphertext, got.SecretCiphertext)
	}

	// Overwriting matters as much as inserting: PutRegistryCredential is an upsert, and
	// rotating a credential is the common path. An ON CONFLICT clause that updates the
	// wrong column would leave the old ciphertext in place and read as a stale secret.
	rotated := []byte{0xde, 0xad, 0x00, 0xbe, 0xef}
	if err := s.PutRegistryCredential(&store.RegistryCredential{
		Host:             host,
		Username:         "robot",
		SecretCiphertext: rotated,
	}); err != nil {
		t.Fatalf("rotate credential: %v", err)
	}
	got, err = s.GetRegistryCredential(host)
	if err != nil {
		t.Fatalf("get rotated credential: %v", err)
	}
	if !bytes.Equal(got.SecretCiphertext, rotated) {
		t.Errorf("rotation did not take: read back %x, want %x", got.SecretCiphertext, rotated)
	}

	// ListRegistryCredentials is documented to omit the ciphertext so its result is safe
	// to serialise into an API response. Checked here because the only thing standing
	// between that comment and a leaked secret is the column list in one SELECT.
	list, err := s.ListRegistryCredentials()
	if err != nil {
		t.Fatalf("list credentials: %v", err)
	}
	for _, c := range list {
		if len(c.SecretCiphertext) != 0 {
			t.Errorf("ListRegistryCredentials returned ciphertext for %q; this result "+
				"is serialised into an API response", c.Host)
		}
	}

	if err := s.DeleteRegistryCredential(host); err != nil {
		t.Fatalf("delete credential: %v", err)
	}
	got, err = s.GetRegistryCredential(host)
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if got != nil {
		t.Error("credential survived deletion, so revoking access does not revoke it")
	}
}

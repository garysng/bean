package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func rec(id string, state SandboxState, labels map[string]string) *Sandbox {
	return &Sandbox{
		ID: id, Image: "img:1", State: state, NodeID: "node-0",
		CPU: 1, MemoryMiB: 512, DiskMiB: 20480, Labels: labels,
		CreatedAt: time.Now(), LastActivity: time.Now(),
	}
}

func TestPutGetSandbox(t *testing.T) {
	st := openTestStore(t)
	in := rec("sbx_a", SandboxRunning, map[string]string{"run": "r1"})
	idle := int64(300)
	in.IdleTimeout = &idle
	in.OnIdle = "pause"
	if err := st.PutSandbox(in); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetSandbox("sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("record not found")
	}
	if got.ID != in.ID || got.State != SandboxRunning || got.Labels["run"] != "r1" {
		t.Errorf("got = %+v", got)
	}
	if got.IdleTimeout == nil || *got.IdleTimeout != 300 || got.OnIdle != "pause" {
		t.Errorf("lifecycle not round-tripped: %+v", got)
	}
}

func TestGetSandboxMissingReturnsNil(t *testing.T) {
	st := openTestStore(t)
	got, err := st.GetSandbox("nope")
	if err != nil {
		t.Fatalf("err = %v, want nil for missing record", err)
	}
	if got != nil {
		t.Errorf("got = %+v, want nil", got)
	}
}

func TestPutSandboxUpserts(t *testing.T) {
	st := openTestStore(t)
	r := rec("sbx_b", SandboxPending, nil)
	if err := st.PutSandbox(r); err != nil {
		t.Fatal(err)
	}
	r.State = SandboxStopped
	r.Reason = "done"
	if err := st.PutSandbox(r); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetSandbox("sbx_b")
	if got.State != SandboxStopped || got.Reason != "done" {
		t.Errorf("upsert failed: %+v", got)
	}
	all, _ := st.ListSandboxes("", "", "")
	if len(all) != 1 {
		t.Errorf("expected 1 record after upsert, got %d", len(all))
	}
}

func TestListSandboxesFilters(t *testing.T) {
	st := openTestStore(t)
	st.PutSandbox(rec("s1", SandboxRunning, map[string]string{"run": "a"}))
	st.PutSandbox(rec("s2", SandboxStopped, map[string]string{"run": "a"}))
	st.PutSandbox(rec("s3", SandboxRunning, map[string]string{"run": "b"}))

	cases := []struct {
		name     string
		key, val string
		state    SandboxState
		want     int
	}{
		{"no filter", "", "", "", 3},
		{"by state", "", "", SandboxRunning, 2},
		{"by label", "run", "a", "", 2},
		{"label and state", "run", "a", SandboxRunning, 1},
		{"label miss", "run", "zzz", "", 0},
		{"state miss", "", "", SandboxFailed, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := st.ListSandboxes(tc.key, tc.val, tc.state)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tc.want {
				t.Errorf("count = %d, want %d", len(got), tc.want)
			}
		})
	}
}

func TestDeleteSandbox(t *testing.T) {
	st := openTestStore(t)
	st.PutSandbox(rec("gone", SandboxRunning, nil))
	if err := st.DeleteSandbox("gone"); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetSandbox("gone")
	if got != nil {
		t.Error("record still present after delete")
	}
	// Deleting a missing record is not an error.
	if err := st.DeleteSandbox("gone"); err != nil {
		t.Errorf("second delete: %v", err)
	}
}

func TestEventsAppendAndListChronological(t *testing.T) {
	st := openTestStore(t)
	base := time.Now()
	for i, typ := range []string{"created", "running", "paused"} {
		if err := st.AppendEvent(&Event{
			Type:      "sandbox.lifecycle." + typ,
			Timestamp: base.Add(time.Duration(i) * time.Second),
			SandboxID: "sbx_e",
			Data:      map[string]string{"i": typ},
			Version:   "v1",
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Event for a different sandbox must not leak into the list.
	st.AppendEvent(&Event{Type: "sandbox.lifecycle.created", Timestamp: base,
		SandboxID: "other", Version: "v1"})

	got, err := st.ListEvents("sbx_e", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("count = %d, want 3", len(got))
	}
	if got[0].Type != "sandbox.lifecycle.created" || got[2].Type != "sandbox.lifecycle.paused" {
		t.Errorf("not chronological: %v -> %v", got[0].Type, got[2].Type)
	}
	if got[0].Data["i"] != "created" {
		t.Errorf("data not round-tripped: %+v", got[0].Data)
	}
	if got[0].Version != "v1" {
		t.Errorf("version = %q", got[0].Version)
	}
}

func TestListEventsLimit(t *testing.T) {
	st := openTestStore(t)
	for i := 0; i < 10; i++ {
		st.AppendEvent(&Event{Type: "e", Timestamp: time.Now(), SandboxID: "x"})
	}
	got, err := st.ListEvents("x", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("count = %d, want 3", len(got))
	}
	// Oversized/zero limits fall back to the 100 default.
	if got, _ := st.ListEvents("x", 99999); len(got) != 10 {
		t.Errorf("count = %d, want 10", len(got))
	}
}

func TestListEventsEmpty(t *testing.T) {
	st := openTestStore(t)
	got, err := st.ListEvents("nobody", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d events", len(got))
	}
}

func TestOpenInvalidPath(t *testing.T) {
	if _, err := Open("/nonexistent-dir-xyz/sub/test.db"); err == nil {
		t.Error("expected error opening store in a missing directory")
	}
}

func TestOpenReadOnlyWorksWhileTheWriterHoldsTheFile(t *testing.T) {
	// The case bean-proxy needs: two processes, one writing and one reading. Calling
	// Open from the reader failed with "database is locked (SQLITE_BUSY)" and the
	// proxy never started, because Open runs migrate() -- DDL against a file another
	// process owns.
	path := filepath.Join(t.TempDir(), "bean.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer writer.Close()

	if err := writer.PutSandbox(&Sandbox{ID: "sbx_ro", State: SandboxRunning,
		NodeID: "node-1"}); err != nil {
		t.Fatalf("write: %v", err)
	}

	reader, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("OpenReadOnly while the writer is open: %v", err)
	}
	defer reader.Close()

	got, err := reader.GetSandbox("sbx_ro")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got == nil || got.NodeID != "node-1" {
		t.Fatalf("read back %+v, want the record the writer stored", got)
	}

	// And a write through the read-only handle must fail, rather than making the
	// proxy a second writer to the placement ledger.
	if err := reader.PutSandbox(&Sandbox{ID: "sbx_forbidden"}); err == nil {
		t.Fatal("a write succeeded through the read-only handle")
	}

	// The writer keeps working with a reader attached, which is what WAL buys.
	if err := writer.PutSandbox(&Sandbox{ID: "sbx_after", State: SandboxRunning}); err != nil {
		t.Fatalf("writer blocked while a reader was attached: %v", err)
	}
}

func TestOpenSetsWALSoAReaderDoesNotBlockTheWriter(t *testing.T) {
	// OpenReadOnly refuses a database that is not in WAL, so this pins the writer's
	// side of that contract: without it the refusal would be correct and every
	// deployment would hit it.
	path := filepath.Join(t.TempDir(), "bean.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	var mode string
	if err := st.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("read journal_mode: %v", err)
	}
	if !strings.EqualFold(mode, "wal") {
		t.Fatalf("journal_mode is %q, want wal: in rollback-journal mode a reader and "+
			"the writer lock each other out, which presents as bean-proxy stalling "+
			"whenever a sandbox is created", mode)
	}
}

// TestConcurrentAcquiresCountEveryReference is the test a process-local mutex made
// impossible to write honestly.
//
// A mutex serialises every caller inside one Store, so no arrangement of goroutines
// through a single handle could fail -- and a concurrency test that cannot fail proves
// nothing. Two Store instances over one file are two callers the mutex cannot see,
// which is exactly what two bean-api replicas are.
//
// Measured first, so the test is known to discriminate: two connections each doing an
// unguarded read-then-write to the same row lost 194 of 200 updates. SQLite does not
// serialise this for us.
//
// The invariant: N successful acquires must leave ref_count at N. A lost increment
// means a restore holds a reference the database does not know about, and the snapshot
// becomes deletable while it is being read.
func TestConcurrentAcquiresCountEveryReference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acquire.db")
	stores := make([]*Store, 4)
	for i := range stores {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		defer st.Close()
		stores[i] = st
	}

	const rounds = 20
	for r := 0; r < rounds; r++ {
		id := fmt.Sprintf("snap-acq-%d", r)
		if err := stores[0].PutSnapshot(&Snapshot{
			ID: id, SandboxID: "sbx", State: SnapshotReady,
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}

		var wg sync.WaitGroup
		var mu sync.Mutex
		ok := 0
		start := make(chan struct{})
		for _, st := range stores {
			s := st
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				if _, err := s.AcquireSnapshot(id); err == nil {
					mu.Lock()
					ok++
					mu.Unlock()
				}
			}()
		}
		close(start)
		wg.Wait()

		snap, err := stores[0].GetSnapshot(id)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if snap == nil {
			t.Fatalf("%s vanished", id)
		}
		if snap.RefCount != ok {
			t.Fatalf("%s: %d acquires reported success but ref_count is %d; a restore "+
				"holds a reference the database does not know about, so the snapshot "+
				"can be deleted while it is being read", id, ok, snap.RefCount)
		}
	}
}

// TestAcquireRefusesWhatDeleteRemoved pins the pair from the other side: once a
// snapshot is gone, an acquire must fail rather than resurrect a reference to it.
func TestAcquireRefusesWhatDeleteRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pair.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	if err := a.PutSnapshot(&Snapshot{ID: "s1", SandboxID: "sbx", State: SnapshotReady}); err != nil {
		t.Fatal(err)
	}
	if err := b.DeleteSnapshot("s1"); err != nil {
		t.Fatalf("delete through the second store: %v", err)
	}
	if _, err := a.AcquireSnapshot("s1"); err == nil {
		t.Fatal("acquired a snapshot the other store deleted")
	}

	// And the converse: a held reference must block the delete, across stores.
	if err := a.PutSnapshot(&Snapshot{ID: "s2", SandboxID: "sbx", State: SnapshotReady}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.AcquireSnapshot("s2"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := b.DeleteSnapshot("s2"); err == nil {
		t.Fatal("deleted a snapshot another store holds a reference to")
	} else if !errors.Is(err, ErrInUse) {
		t.Fatalf("got %v, want ErrInUse so the API answers 409", err)
	}
}

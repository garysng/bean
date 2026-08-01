package store

import (
	"path/filepath"
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

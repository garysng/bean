package store

import (
	"encoding/json"
	"testing"
)

// TestHasMemoryTreatsAbsentAsPresent covers snapshots written before the field
// existed. Every one of those captured guest memory, so decoding a missing value
// as false would claim they carry none — and the CPU constraint that keeps them
// off an incompatible host is only applied when memory is present. The bug would
// therefore silently remove a safety check from exactly the old snapshots that
// still need it.
func TestHasMemoryTreatsAbsentAsPresent(t *testing.T) {
	var old Snapshot
	if err := json.Unmarshal([]byte(`{"id":"snap_old","cpuVendor":"AuthenticAMD"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.IncludeMemory != nil {
		t.Errorf("absent field decoded to %v, want nil", *old.IncludeMemory)
	}
	if !old.HasMemory() {
		t.Error("a snapshot predating the field must be assumed to carry memory")
	}
}

func TestHasMemoryRespectsExplicitValues(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want bool
	}{
		{`{"includeMemory":true}`, true},
		{`{"includeMemory":false}`, false},
	} {
		var s Snapshot
		if err := json.Unmarshal([]byte(tc.raw), &s); err != nil {
			t.Fatal(err)
		}
		if got := s.HasMemory(); got != tc.want {
			t.Errorf("HasMemory() for %s = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// TestSnapshotRoundTripsThroughStore makes sure the field survives persistence.
// The record is stored as a JSON blob, so a pointer that marshals wrongly would
// come back as nil and read as "assume memory" — safe, but it would erase a
// caller's explicit choice of a filesystem-only snapshot.
func TestSnapshotRoundTripsThroughStore(t *testing.T) {
	no := false
	raw, err := json.Marshal(&Snapshot{ID: "snap_x", IncludeMemory: &no})
	if err != nil {
		t.Fatal(err)
	}
	var back Snapshot
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.HasMemory() {
		t.Errorf("explicit includeMemory=false did not survive: %s", raw)
	}
}

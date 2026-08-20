package node

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
)

// TestLoadAdmissionMissingFileDisables is the fresh-node case: no file means
// admission is off, not an error, and the disk Path is still supplied so a later
// push can enable the floor without restating it.
func TestLoadAdmissionMissingFileDisables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admission.json")
	disk, mem, found, err := LoadAdmission(path, "/data")
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if found {
		t.Error("found = true for a missing file")
	}
	if disk.Enabled() || mem.Enabled() {
		t.Errorf("guards enabled from a missing file: disk=%+v mem=%+v", disk, mem)
	}
	if disk.Path != "/data" {
		t.Errorf("disk path = %q, want the supplied /data", disk.Path)
	}
}

// TestSaveLoadAdmissionRoundTrip is the property persistence exists for: what a
// retune writes is exactly what the next startup reads back, with the disk Path
// re-supplied rather than stored.
func TestSaveLoadAdmissionRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "admission.json")
	in := DiskGuard{Path: "/ignored-on-save", MinFreeBytes: 2048 << 20, MinFreePercent: 5}
	inMem := MemGuard{MaxUsedPercent: 82}
	if err := SaveAdmission(path, in, inMem); err != nil {
		t.Fatalf("save: %v", err)
	}

	disk, mem, found, err := LoadAdmission(path, "/data")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !found {
		t.Fatal("found = false after a save")
	}
	if disk.MinFreeBytes != 2048<<20 || disk.MinFreePercent != 5 {
		t.Errorf("disk round-trip lost values: %+v", disk)
	}
	if disk.Path != "/data" {
		t.Errorf("disk path = %q, want the load-time /data not the saved one", disk.Path)
	}
	if mem.MaxUsedPercent != 82 {
		t.Errorf("mem round-trip = %g, want 82", mem.MaxUsedPercent)
	}
}

// TestLoadAdmissionRejectsInvalid pins that a file describing an out-of-range
// threshold stops startup rather than being silently ignored: a bad policy an
// operator wrote is a mistake worth surfacing.
func TestLoadAdmissionRejectsInvalid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admission.json")
	if err := os.WriteFile(path, []byte(`{"maxMemPercent": 150}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := LoadAdmission(path, "/data"); err == nil {
		t.Error("an out-of-range ceiling in the file must fail the load")
	}
}

// TestLoadAdmissionRejectsGarbage covers a corrupt (unparsable) file: the same
// stop-don't-ignore rule as an invalid threshold.
func TestLoadAdmissionRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admission.json")
	if err := os.WriteFile(path, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := LoadAdmission(path, "/data"); err == nil {
		t.Error("an unparsable file must fail the load")
	}
}

// TestConfigureAdmissionPersistsAndSurvivesReload is the end-to-end promise: a
// push through ConfigureAdmission writes the file, and a fresh LoadAdmission
// reads the pushed value back -- the restart-survival the file exists for.
func TestConfigureAdmissionPersistsAndSurvivesReload(t *testing.T) {
	m := newTestManager(t)
	m.SetAdmission(DiskGuard{Path: "/data"}, MemGuard{MaxUsedPercent: 80})
	path := filepath.Join(t.TempDir(), "admission.json")

	s := NewGRPCServer(m, nil)
	s.SetAdmissionPersister(func(disk DiskGuard, mem MemGuard) error {
		return SaveAdmission(path, disk, mem)
	})

	if _, err := s.ConfigureAdmission(context.Background(), &nodev1.ConfigureAdmissionRequest{
		Config: &nodev1.AdmissionConfig{MaxMemPercent: f64p(85)},
	}); err != nil {
		t.Fatal(err)
	}

	// The file the next startup would read carries the pushed ceiling.
	_, mem, found, err := LoadAdmission(path, "/data")
	if err != nil || !found {
		t.Fatalf("reload after push: found=%v err=%v", found, err)
	}
	if mem.MaxUsedPercent != 85 {
		t.Errorf("reloaded ceiling = %g, want the pushed 85", mem.MaxUsedPercent)
	}
}

// TestConfigureAdmissionKeepsGuardsWhenPersistFails is the no-drift rule: if the
// write fails the node must not adopt the new threshold in memory either, so its
// live guard and its file never disagree.
func TestConfigureAdmissionKeepsGuardsWhenPersistFails(t *testing.T) {
	m := newTestManager(t)
	m.SetAdmission(DiskGuard{Path: "/data"}, MemGuard{MaxUsedPercent: 80})

	s := NewGRPCServer(m, nil)
	s.SetAdmissionPersister(func(DiskGuard, MemGuard) error {
		return os.ErrPermission
	})

	_, err := s.ConfigureAdmission(context.Background(), &nodev1.ConfigureAdmissionRequest{
		Config: &nodev1.AdmissionConfig{MaxMemPercent: f64p(85)},
	})
	if err == nil {
		t.Fatal("a failed persist must fail the RPC")
	}
	if _, mem := m.Admission(); mem.MaxUsedPercent != 80 {
		t.Errorf("guard changed despite a failed persist: %g, want the previous 80",
			mem.MaxUsedPercent)
	}
}

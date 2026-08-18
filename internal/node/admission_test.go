package node

import (
	"context"
	"sync"
	"testing"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
)

func i64(v int64) *int64      { return &v }
func f64p(v float64) *float64 { return &v }

// TestConfigureAdmissionPartialLeavesUnsetFieldsAlone is the property the optional
// fields exist for: pushing only the memory ceiling must not wipe the disk floor.
func TestConfigureAdmissionPartialLeavesUnsetFieldsAlone(t *testing.T) {
	m := newTestManager(t)
	m.SetAdmission(
		DiskGuard{Path: "/data", MinFreeBytes: 1 << 30, MinFreePercent: 10},
		MemGuard{MaxUsedPercent: 70},
	)
	s := NewGRPCServer(m, nil)

	// Push only the memory ceiling.
	if _, err := s.ConfigureAdmission(context.Background(), &nodev1.ConfigureAdmissionRequest{
		Config: &nodev1.AdmissionConfig{MaxMemPercent: f64p(85)},
	}); err != nil {
		t.Fatal(err)
	}

	disk, mem := m.Admission()
	if mem.MaxUsedPercent != 85 {
		t.Errorf("mem ceiling = %g, want 85", mem.MaxUsedPercent)
	}
	// The disk floor must be untouched.
	if disk.MinFreeBytes != 1<<30 || disk.MinFreePercent != 10 || disk.Path != "/data" {
		t.Errorf("disk floor changed by a mem-only push: %+v", disk)
	}
}

func TestConfigureAdmissionUpdatesDiskFloor(t *testing.T) {
	m := newTestManager(t)
	m.SetAdmission(DiskGuard{Path: "/data"}, MemGuard{})
	s := NewGRPCServer(m, nil)

	if _, err := s.ConfigureAdmission(context.Background(), &nodev1.ConfigureAdmissionRequest{
		Config: &nodev1.AdmissionConfig{MinFreeDiskMib: i64(2048), MinFreeDiskPercent: f64p(5)},
	}); err != nil {
		t.Fatal(err)
	}
	disk, _ := m.Admission()
	if disk.MinFreeBytes != 2048<<20 {
		t.Errorf("min free bytes = %d, want %d", disk.MinFreeBytes, int64(2048)<<20)
	}
	if disk.MinFreePercent != 5 {
		t.Errorf("min free percent = %g, want 5", disk.MinFreePercent)
	}
}

func TestConfigureAdmissionRejectsInvalidAndKeepsPrevious(t *testing.T) {
	m := newTestManager(t)
	m.SetAdmission(DiskGuard{Path: "/data"}, MemGuard{MaxUsedPercent: 80})
	s := NewGRPCServer(m, nil)

	// 100% is out of range: the push must be rejected and the previous ceiling kept.
	_, err := s.ConfigureAdmission(context.Background(), &nodev1.ConfigureAdmissionRequest{
		Config: &nodev1.AdmissionConfig{MaxMemPercent: f64p(100)},
	})
	if err == nil {
		t.Fatal("expected an out-of-range ceiling to be rejected")
	}
	if _, mem := m.Admission(); mem.MaxUsedPercent != 80 {
		t.Errorf("a rejected config must leave the previous threshold: got %g, want 80",
			mem.MaxUsedPercent)
	}
}

func TestConfigureAdmissionNilConfigIsNoOp(t *testing.T) {
	m := newTestManager(t)
	m.SetAdmission(DiskGuard{Path: "/data"}, MemGuard{MaxUsedPercent: 80})
	s := NewGRPCServer(m, nil)
	if _, err := s.ConfigureAdmission(context.Background(),
		&nodev1.ConfigureAdmissionRequest{}); err != nil {
		t.Fatalf("a nil config must be a no-op, not an error: %v", err)
	}
	if _, mem := m.Admission(); mem.MaxUsedPercent != 80 {
		t.Errorf("a nil config changed the ceiling: %g", mem.MaxUsedPercent)
	}
}

// TestAdmissionConcurrentReadWrite is the race the mutex exists for: SetAdmission
// (ConfigureAdmission) writing while diskGuard/memGuard (Create) read. Run under
// -race to catch an unsynchronised access.
func TestAdmissionConcurrentReadWrite(t *testing.T) {
	m := newTestManager(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
				m.SetAdmission(
					DiskGuard{Path: "/data", MinFreeBytes: int64(i) << 20},
					MemGuard{MaxUsedPercent: float64(i%90) + 1},
				)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = m.diskGuard()
				_ = m.memGuard()
				_, _ = m.Admission()
			}
		}
	}()

	// A brief overlap is enough for the race detector.
	for i := 0; i < 10000; i++ {
		_, _ = m.Admission()
	}
	close(stop)
	wg.Wait()
}

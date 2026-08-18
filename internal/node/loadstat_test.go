package node

import (
	"os"
	"path/filepath"
	"testing"
)

func writeProcStat(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stat")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadProcStatCPUSumsFieldsAndIdle(t *testing.T) {
	// user nice system idle iowait irq softirq steal guest guest_nice
	// idle = idle(400) + iowait(50) = 450; total = sum of first 8 = 100+20+30+
	// 400+50+5+5+0 = 610. guest/guest_nice are excluded (already in user/nice).
	p := writeProcStat(t, "cpu  100 20 30 400 50 5 5 0 999 888\n"+
		"cpu0 1 2 3 4 5 6 7 8\nintr 12345\n")
	idle, total, err := readProcStatCPU(p)
	if err != nil {
		t.Fatal(err)
	}
	if idle != 450 {
		t.Errorf("idle = %d, want 450", idle)
	}
	if total != 610 {
		t.Errorf("total = %d, want 610", total)
	}
}

func TestCPUSamplerFirstCallIsZero(t *testing.T) {
	s := newCPUSampler(writeProcStat(t, "cpu  100 0 0 400 0 0 0 0\n"))
	if got := s.Percent(); got != 0 {
		t.Errorf("first sample = %g, want 0 (no interval yet)", got)
	}
}

func TestCPUSamplerComputesBusyFractionOverInterval(t *testing.T) {
	p := writeProcStat(t, "cpu  100 0 0 400 0 0 0 0\n") // idle 400, total 500
	s := newCPUSampler(p)
	if got := s.Percent(); got != 0 {
		t.Fatalf("baseline sample = %g, want 0", got)
	}
	// Second read: +100 busy (user), +0 idle. Interval total = 100, all busy => 100%.
	if err := os.WriteFile(p, []byte("cpu  200 0 0 400 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.Percent(); got < 99.9 || got > 100.1 {
		t.Errorf("busy fraction = %g, want ~100", got)
	}
}

func TestCPUSamplerHalfBusy(t *testing.T) {
	p := writeProcStat(t, "cpu  100 0 0 400 0 0 0 0\n")
	s := newCPUSampler(p)
	s.Percent() // baseline
	// +50 busy, +50 idle => interval total 100, half busy => 50%.
	if err := os.WriteFile(p, []byte("cpu  150 0 0 450 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.Percent(); got < 49.9 || got > 50.1 {
		t.Errorf("busy fraction = %g, want ~50", got)
	}
}

func TestCPUSamplerZeroIntervalReportsZero(t *testing.T) {
	p := writeProcStat(t, "cpu  100 0 0 400 0 0 0 0\n")
	s := newCPUSampler(p)
	s.Percent() // baseline
	// Identical read: no jiffies elapsed. Must report 0, not divide by zero.
	if got := s.Percent(); got != 0 {
		t.Errorf("zero interval = %g, want 0", got)
	}
}

func TestCPUSamplerBackwardCounterReportsZero(t *testing.T) {
	p := writeProcStat(t, "cpu  100 0 0 400 0 0 0 0\n")
	s := newCPUSampler(p)
	s.Percent() // baseline
	// Counters reset lower (a reboot between reads): must not underflow into a
	// huge unsigned delta, must report 0.
	if err := os.WriteFile(p, []byte("cpu  10 0 0 40 0 0 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.Percent(); got != 0 {
		t.Errorf("backward counter = %g, want 0", got)
	}
}

func TestCPUSamplerUnreadableReportsZero(t *testing.T) {
	s := newCPUSampler("/definitely/not/a/real/proc/stat")
	if got := s.Percent(); got != 0 {
		t.Errorf("unreadable stat = %g, want 0", got)
	}
}

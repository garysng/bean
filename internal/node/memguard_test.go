package node

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeMeminfo drops a meminfo-shaped file and returns its path, so the guard's
// admission logic is exercised without a real kernel (and on non-Linux dev hosts).
func writeMeminfo(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMemGuardValidateRejectsCeilingsThatCannotHold(t *testing.T) {
	for _, tc := range []struct {
		name  string
		guard MemGuard
		ok    bool
	}{
		{"zero value admits everything", MemGuard{}, true},
		{"a normal ceiling", MemGuard{MaxUsedPercent: 80}, true},
		{"negative percent", MemGuard{MaxUsedPercent: -1}, false},
		// 100% would only refuse a machine with literally zero available memory,
		// which never happens in time to matter -- a typo, not a policy.
		{"percent at 100", MemGuard{MaxUsedPercent: 100}, false},
		{"percent above 100", MemGuard{MaxUsedPercent: 150}, false},
	} {
		err := tc.guard.Validate()
		if tc.ok && err != nil {
			t.Errorf("%s: expected acceptance, got %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: expected rejection, got none", tc.name)
		}
	}
}

func TestMemGuardIsDisabledAtZero(t *testing.T) {
	g := MemGuard{}
	if g.Enabled() {
		t.Fatal("a zero ceiling must be disabled")
	}
	if err := g.Admit(); err != nil {
		t.Errorf("a disabled guard must admit: %v", err)
	}
}

func TestMemStatParsesKiBToBytes(t *testing.T) {
	// 16 GiB total, 4 GiB available, in kibibytes as meminfo reports them. MemFree
	// and the rest are present to prove the parser picks the right two lines.
	g := MemGuard{Path: writeMeminfo(t, "MemTotal:       16777216 kB\n"+
		"MemFree:         1048576 kB\n"+
		"MemAvailable:    4194304 kB\n"+
		"Buffers:          123456 kB\n")}
	stats, err := g.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if stats.TotalBytes != 16<<30 {
		t.Errorf("total = %d, want %d", stats.TotalBytes, int64(16)<<30)
	}
	if stats.AvailableBytes != 4<<30 {
		t.Errorf("available = %d, want %d", stats.AvailableBytes, int64(4)<<30)
	}
	// 12 of 16 GiB unavailable = 75%.
	if got := stats.UsedPercent(); got < 74.9 || got > 75.1 {
		t.Errorf("used percent = %g, want ~75", got)
	}
}

func TestMemGuardAdmitsBelowTheCeiling(t *testing.T) {
	// 75% used, ceiling 80% -> room to spare.
	g := MemGuard{MaxUsedPercent: 80, Path: writeMeminfo(t,
		"MemTotal:       16777216 kB\nMemAvailable:    4194304 kB\n")}
	if err := g.Admit(); err != nil {
		t.Errorf("expected admission at 75%% used under an 80%% ceiling: %v", err)
	}
}

func TestMemGuardRefusesAtOrAboveTheCeiling(t *testing.T) {
	// 90% used against an 80% ceiling.
	g := MemGuard{MaxUsedPercent: 80, Path: writeMeminfo(t,
		"MemTotal:       16777216 kB\nMemAvailable:    1677722 kB\n")}
	err := g.Admit()
	if err == nil {
		t.Fatal("expected refusal at 90% used under an 80% ceiling")
	}
	var pressure *ErrMemPressure
	if !errors.As(err, &pressure) {
		t.Fatalf("refusal must be an *ErrMemPressure so the gRPC layer can map it to "+
			"ResourceExhausted rather than Internal, got %T", err)
	}
	if pressure.MaxPercent != 80 {
		t.Errorf("ceiling not reported: got %g", pressure.MaxPercent)
	}
	if pressure.UsedPercent < 89 || pressure.UsedPercent > 91 {
		t.Errorf("used percent = %g, want ~90", pressure.UsedPercent)
	}
}

func TestMemGuardRefusesExactlyAtTheCeiling(t *testing.T) {
	// Exactly 80% used: the ceiling is inclusive, so this refuses. 20% of 16 GiB
	// available = 80% used.
	g := MemGuard{MaxUsedPercent: 80, Path: writeMeminfo(t,
		"MemTotal:       16777216 kB\nMemAvailable:    3355443 kB\n")}
	if err := g.Admit(); err == nil {
		t.Error("expected refusal at exactly the ceiling; the bound is inclusive")
	}
}

func TestMemGuardAdmitsWhenItCannotMeasure(t *testing.T) {
	// Same margin-not-gate stance as DiskGuard: a node that cannot read meminfo has
	// a monitoring problem, and refusing all work would make it an outage.
	g := MemGuard{MaxUsedPercent: 80, Path: "/definitely/not/a/real/meminfo"}
	if err := g.Admit(); err != nil {
		t.Errorf("an unreadable meminfo must admit rather than refuse: %v", err)
	}
}

func TestMemGuardTreatsMissingMemAvailableAsError(t *testing.T) {
	// A kernel old enough to lack MemAvailable cannot run firecracker anyway;
	// substituting MemFree would report a tighter node than reality. Stat errors,
	// and Admit (which swallows Stat errors) then admits.
	g := MemGuard{MaxUsedPercent: 80, Path: writeMeminfo(t,
		"MemTotal:       16777216 kB\nMemFree:         1048576 kB\n")}
	if _, err := g.Stat(); err == nil {
		t.Error("expected an error when MemAvailable is absent")
	}
	if err := g.Admit(); err != nil {
		t.Errorf("a failed measurement must admit: %v", err)
	}
}

func TestErrMemPressureExplainsWhyRefusingIsCheaper(t *testing.T) {
	err := &ErrMemPressure{UsedPercent: 92.5, MaxPercent: 80}
	msg := err.Error()
	for _, want := range []string{"92.5", "80.0", "OOM"} {
		if !contains(msg, want) {
			t.Errorf("message is missing %q: %s", want, msg)
		}
	}
}

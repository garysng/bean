package node

import (
	"errors"
	"testing"
)

func TestDiskGuardValidateRejectsFloorsThatCannotHold(t *testing.T) {
	for _, tc := range []struct {
		name  string
		guard DiskGuard
		ok    bool
	}{
		{"zero value admits everything", DiskGuard{}, true},
		{"bytes only", DiskGuard{MinFreeBytes: 1 << 30}, true},
		{"percent only", DiskGuard{MinFreePercent: 10}, true},
		{"negative bytes", DiskGuard{MinFreeBytes: -1}, false},
		{"negative percent", DiskGuard{MinFreePercent: -1}, false},
		// 100% free means no sandbox may ever be admitted, which is a typo rather
		// than a policy anyone wants.
		{"percent at 100", DiskGuard{MinFreePercent: 100}, false},
		{"percent above 100", DiskGuard{MinFreePercent: 150}, false},
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

func TestDiskGuardIsDisabledWithoutAPath(t *testing.T) {
	// A floor with nothing to measure would either refuse everything or admit
	// everything depending on how statfs failed; it has to be off.
	g := DiskGuard{MinFreeBytes: 1 << 40}
	if g.Enabled() {
		t.Fatal("a guard with no path must be disabled")
	}
	if err := g.Admit(); err != nil {
		t.Errorf("a disabled guard must admit: %v", err)
	}
}

func TestDiskGuardAdmitsWhenThereIsRoom(t *testing.T) {
	// A 1 KiB floor against a real temp directory: any usable filesystem clears it.
	g := DiskGuard{Path: t.TempDir(), MinFreeBytes: 1 << 10}
	if err := g.Admit(); err != nil {
		t.Errorf("expected admission on a filesystem with free space: %v", err)
	}
}

func TestDiskGuardRefusesWhenBelowTheFloor(t *testing.T) {
	// A floor of 1 EiB cannot be met by any real volume, which is what makes this
	// deterministic without filling a disk.
	g := DiskGuard{Path: t.TempDir(), MinFreeBytes: 1 << 60}
	err := g.Admit()
	if err == nil {
		t.Fatal("expected refusal against an unmeetable floor")
	}
	var pressure *ErrDiskPressure
	if !errors.As(err, &pressure) {
		t.Fatalf("refusal must be an *ErrDiskPressure so the gRPC layer can map it "+
			"to ResourceExhausted rather than Internal, got %T", err)
	}
	if pressure.FloorBytes != 1<<60 {
		t.Errorf("floor not reported: got %d", pressure.FloorBytes)
	}
	if pressure.Path == "" {
		t.Error("the path is what tells an operator which volume to look at")
	}
}

func TestDiskGuardPercentFloorAlsoRefuses(t *testing.T) {
	// 99.99% free is unmeetable on a volume holding anything at all.
	g := DiskGuard{Path: t.TempDir(), MinFreePercent: 99.99}
	if err := g.Admit(); err == nil {
		t.Error("expected refusal against a 99.99% free floor")
	}
}

func TestDiskGuardTakesTheLargerOfTheTwoFloors(t *testing.T) {
	// Adding a percentage to an existing byte count must be a tightening. If the
	// smaller won, an operator setting a modest percentage would silently lower a
	// carefully chosen byte floor.
	g := DiskGuard{MinFreeBytes: 100, MinFreePercent: 50}
	if got := g.floorFor(1000); got != 500 {
		t.Errorf("percent should win when larger: got %d, want 500", got)
	}
	g = DiskGuard{MinFreeBytes: 900, MinFreePercent: 10}
	if got := g.floorFor(1000); got != 900 {
		t.Errorf("bytes should win when larger: got %d, want 900", got)
	}
}

func TestDiskGuardAdmitsWhenItCannotMeasure(t *testing.T) {
	// The guard is a margin on top of the scheduler's accounting. A node that
	// cannot stat its own filesystem has a monitoring problem; refusing all work
	// would turn that into an outage.
	g := DiskGuard{Path: "/definitely/not/a/real/path", MinFreeBytes: 1 << 60}
	if err := g.Admit(); err != nil {
		t.Errorf("an unmeasurable filesystem must admit rather than refuse: %v", err)
	}
}

func TestDiskStatReportsPlausibleOccupancy(t *testing.T) {
	g := DiskGuard{Path: t.TempDir()}
	stats, err := g.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if stats.TotalBytes <= 0 {
		t.Fatalf("total must be positive, got %d", stats.TotalBytes)
	}
	if stats.FreeBytes < 0 || stats.FreeBytes > stats.TotalBytes {
		t.Errorf("free (%d) must be within total (%d)", stats.FreeBytes, stats.TotalBytes)
	}
	if stats.UsedBytes < 0 || stats.UsedBytes > stats.TotalBytes {
		t.Errorf("used (%d) must be within total (%d)", stats.UsedBytes, stats.TotalBytes)
	}
	// Free comes from Bavail and used from Bfree, so they need not sum to total —
	// the difference is the root reserve. Used plus available should not exceed it.
	if stats.UsedBytes+stats.FreeBytes > stats.TotalBytes {
		t.Errorf("used (%d) + free (%d) exceeds total (%d)",
			stats.UsedBytes, stats.FreeBytes, stats.TotalBytes)
	}
}

func TestErrDiskPressureExplainsWhyRefusingIsCheaper(t *testing.T) {
	err := &ErrDiskPressure{FreeBytes: 100, FloorBytes: 200, Path: "/var/lib/bean/sandboxes"}
	msg := err.Error()
	// The message is what an operator sees first, so it has to carry the numbers
	// and the reason. A bare "low on disk" sends them looking for the wrong thing.
	for _, want := range []string{"/var/lib/bean/sandboxes", "100", "200", "not recoverable"} {
		if !contains(msg, want) {
			t.Errorf("message is missing %q: %s", want, msg)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

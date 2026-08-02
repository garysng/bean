package node

import (
	"math"
	"strings"
	"testing"
)

// TestOvercommitScalesReportedCapacity is the point of the feature: a node
// reports a multiple of what it was configured with, so bursty sandboxes that
// mostly idle do not each hold their full declared share.
func TestOvercommitScalesReportedCapacity(t *testing.T) {
	o := Overcommit{CPU: 3.0, Memory: 1.5}
	if got := o.ApplyCPU(8); got != 24 {
		t.Errorf("ApplyCPU(8) = %v, want 24", got)
	}
	if got := o.ApplyMemory(16384); got != 24576 {
		t.Errorf("ApplyMemory(16384) = %d, want 24576", got)
	}
}

// TestDefaultOvercommitIsOneToOne keeps the default honest: a node that was not
// told to oversubscribe must report exactly what it has, so upgrading to a build
// that has this feature does not silently change what a cluster admits.
func TestDefaultOvercommitIsOneToOne(t *testing.T) {
	o := DefaultOvercommit()
	if got := o.ApplyCPU(8); got != 8 {
		t.Errorf("default ApplyCPU(8) = %v, want 8", got)
	}
	if got := o.ApplyMemory(16384); got != 16384 {
		t.Errorf("default ApplyMemory(16384) = %d, want 16384", got)
	}
	if o.Enabled() {
		t.Error("the default reports as oversubscribed")
	}
}

// TestOvercommitRejectsBelowOne covers the value most likely to be typed by
// mistake. Someone reaching for "hold back a quarter of this node" writes 0.75,
// and clamping it to 1.0 would ignore them while accepting it would report less
// capacity than the node has — with nothing in the logs to say why placements
// stopped fitting.
func TestOvercommitRejectsBelowOne(t *testing.T) {
	err := Overcommit{CPU: 0.75, Memory: 1.0}.Validate()
	if err == nil {
		t.Fatal("accepted a factor below 1.0")
	}
	// The error has to name the alternative, since refusing without saying what
	// to do instead just moves the confusion.
	if !strings.Contains(err.Error(), "allocatable") {
		t.Errorf("error %q does not point at lowering the allocatable amount", err)
	}
}

// TestOvercommitRejectsTypos catches a slipped decimal point. 30 instead of 3.0
// admits ten times the work, and the symptom is unexplained timeouts rather than
// anything that points back at configuration.
func TestOvercommitRejectsTypos(t *testing.T) {
	if err := (Overcommit{CPU: 30, Memory: 1.0}).Validate(); err == nil {
		t.Error("accepted cpu=30, which is almost certainly a typo for 3.0")
	}
	for _, bad := range []float64{math.NaN(), math.Inf(1)} {
		if err := (Overcommit{CPU: bad, Memory: 1.0}).Validate(); err == nil {
			t.Errorf("accepted cpu=%v", bad)
		}
	}
}

// TestOvercommitMemoryTruncates keeps the reported figure at or below what the
// factor allows. Rounding up would report memory the operator did not authorise,
// and for memory that difference is a killed process rather than a slow one.
func TestOvercommitMemoryTruncates(t *testing.T) {
	// 1001 * 1.5 = 1501.5, which must not become 1502.
	if got := (Overcommit{CPU: 1, Memory: 1.5}).ApplyMemory(1001); got != 1501 {
		t.Errorf("ApplyMemory(1001) at 1.5 = %d, want 1501", got)
	}
}

// TestOvercommitZeroIsInert guards the zero value. An Overcommit that was never
// initialised must behave as one-to-one rather than reporting a node with no
// capacity at all, which would take it out of scheduling entirely.
func TestOvercommitZeroIsInert(t *testing.T) {
	var o Overcommit
	if got := o.ApplyCPU(8); got != 8 {
		t.Errorf("zero-value ApplyCPU(8) = %v, want 8 — a node reporting 0 CPU is unschedulable", got)
	}
	if got := o.ApplyMemory(1024); got != 1024 {
		t.Errorf("zero-value ApplyMemory(1024) = %d, want 1024", got)
	}
}

// TestOvercommitEnabledOnEitherDimension makes sure the startup warning fires
// when only one dimension is raised, since either one changes what the node
// admits.
func TestOvercommitEnabledOnEitherDimension(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    Overcommit
		want bool
	}{
		{"neither", Overcommit{CPU: 1, Memory: 1}, false},
		{"cpu only", Overcommit{CPU: 2, Memory: 1}, true},
		{"memory only", Overcommit{CPU: 1, Memory: 1.2}, true},
		{"both", Overcommit{CPU: 3, Memory: 2}, true},
	} {
		if got := tc.o.Enabled(); got != tc.want {
			t.Errorf("%s: Enabled() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

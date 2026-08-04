package main

import "testing"

// The fixed default of 16 was the binding constraint on a large host long before its
// CPU was: a 128-core machine refused a 17th concurrent create while 87% of its cores
// were idle. These tests pin the shape of the replacement rather than the constant,
// because the constant is a starting point and the measurement that would refine it
// (GitHub #29) has not been done.

func TestDerivedMaxCreatesScalesWithCores(t *testing.T) {
	small := derivedMaxCreates(8)
	large := derivedMaxCreates(128)
	if large <= small {
		t.Fatalf("128 cores derived %d and 8 cores derived %d; the whole point is "+
			"that a larger host admits more concurrent boots", large, small)
	}
	// Proportional, not merely monotonic: a derivation that grew by a constant would
	// also pass the check above while leaving a large host nearly as limited.
	if got, want := large, small*16; got < want/2 {
		t.Errorf("128 cores derived %d, which is far below the %d that scaling with "+
			"cores implies from the 8-core figure %d", got, want, small)
	}
}

// TestDerivedMaxCreatesHasAFloor covers the small host. Four per core on a
// single-core machine is already more than it can boot, and going below the old
// default would serialise creates on hosts that used to handle 16.
func TestDerivedMaxCreatesHasAFloor(t *testing.T) {
	for _, cores := range []int{1, 2, 4} {
		if got := derivedMaxCreates(cores); got < 16 {
			t.Errorf("%d cores derived %d, below the 16 such a host had before",
				cores, got)
		}
	}
}

// TestDerivedMaxCreatesIsPositiveOnAnUnknownCoreCount guards the degenerate input.
// runtime.NumCPU never returns zero, but a value that did would otherwise produce a
// limit of zero, which the store reads as "no limit" -- the opposite of safe.
func TestDerivedMaxCreatesIsPositiveOnAnUnknownCoreCount(t *testing.T) {
	for _, cores := range []int{0, -1} {
		if got := derivedMaxCreates(cores); got <= 0 {
			t.Errorf("cores=%d derived %d; zero is read downstream as unlimited",
				cores, got)
		}
	}
}

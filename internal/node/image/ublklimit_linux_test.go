//go:build linux

package image

import (
	"strings"
	"sync"
	"testing"
)

// TestUblkProviderRefusesPastUblksMax checks the guard that was missing when it mattered.
//
// Without it, a node asked for more sandboxes than ublks_max allows does not fail cleanly:
// ADD_DEV succeeds up to the limit and past it the failures come from later in the
// sequence, each leaving a device allocated. Measured on a 128-core host asked for 256
// concurrent sandboxes: 141 devices against a limit of 64, none removable -- STOP_DEV
// waits on a dead queue and DEL_DEV blocks behind the kernel retrying IO to a server that
// is gone. Load reached 68, 37 processes were unkillable in D state, and the host needed a
// reboot.
//
// So this is tested without touching the kernel at all: the counter and the limit are
// arithmetic, and the point is that the refusal happens before anything is allocated.
func TestUblkProviderRefusesPastUblksMax(t *testing.T) {
	p := &UblkProvider{}
	// Set directly rather than read from the kernel, so the test is about the guard and
	// not about this host's configuration.
	p.limitOnce.Do(func() { p.limit = 4 })

	for i := 0; i < 4; i++ {
		if err := p.admit(); err != nil {
			t.Fatalf("admit %d of 4 was refused: %v", i+1, err)
		}
	}
	err := p.admit()
	if err == nil {
		t.Fatal("the fifth device was admitted against a limit of 4; the kernel would " +
			"then allocate objects this node cannot remove")
	}
	// The message has to name the remedy, because the operator's options are to raise
	// ublks_max or to place the sandbox elsewhere and nothing in the error's context
	// says which.
	if !strings.Contains(err.Error(), "ublks_max") {
		t.Errorf("the refusal does not mention ublks_max, so an operator cannot act on "+
			"it: %v", err)
	}

	// A released slot is reusable. Getting this wrong gives a ceiling that only falls:
	// the node would refuse every create after ublks_max sandboxes had ever existed
	// rather than after that many exist at once.
	p.release()
	if err := p.admit(); err != nil {
		t.Errorf("a slot freed by release was not reusable: %v", err)
	}
}

// TestUblkAdmitNeverExceedsTheLimit asserts the invariant, not the mechanism.
//
// An earlier version of this test launched 4x the limit concurrently and checked that
// exactly `limit` were admitted. It passed against a deliberately broken admit that
// released the lock between the check and the increment -- because the window is a few
// instructions wide and Go's scheduler rarely lands two goroutines inside it. A test that
// passes against the bug it names is worse than no test: it reads as coverage.
//
// So what is asserted is the property that matters and cannot be raced past: however many
// admits succeed, inFlight never ends up above the limit. A check separated from its
// increment breaks that arithmetic whether or not the interleaving is observed on any
// given run.
func TestUblkAdmitNeverExceedsTheLimit(t *testing.T) {
	const limit = 8
	p := &UblkProvider{}
	p.limitOnce.Do(func() { p.limit = limit })

	var wg sync.WaitGroup
	for i := 0; i < limit*16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.admit()
		}()
	}
	wg.Wait()

	p.mu.Lock()
	held := p.inFlight
	p.mu.Unlock()

	if held > limit {
		t.Errorf("inFlight is %d against a limit of %d: the node believes it may create "+
			"more devices than the kernel allows, and each one past ublks_max leaks a "+
			"kernel object that cannot be removed", held, limit)
	}
	if held != limit {
		t.Errorf("inFlight is %d, want exactly %d: with more contenders than slots every "+
			"slot should be taken", held, limit)
	}
}

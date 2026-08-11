package image

import (
	"context"
	"time"
)

// ObservePhase records how long one step inside the image layer took. Nil discards,
// which is what every test wants.
//
// A package-level variable rather than a field on a provider because a rootfs release
// is a closure captured at Prepare time, and there is one provider per node either
// way. A function rather than a metrics-registry import because this package has no
// dependency on the metrics layer and should not grow one to time two calls.
//
// It exists because Release was measured at 4.414s of a 4.761s destroy under 128-way
// concurrency while every component of it is fast in isolation -- the four configfs
// removals cost 92ms serially and 15ms each when 41 run concurrently, and the sandbox
// directory holds one sparse file whose removal takes 2ms. Without splitting the call
// the attribution could not go further than "somewhere in here".
//
// Declared in a portable file so a non-linux build, where the overlaybd provider does
// not exist, still compiles the manager that sets it.
var ObservePhase func(ctx context.Context, phase string, d time.Duration)

func obsPhase(ctx context.Context, phase string, d time.Duration) {
	if ObservePhase == nil {
		return
	}
	ObservePhase(ctx, phase, d)
}

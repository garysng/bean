package image

import (
	"context"
	"sync"
)

// layerFlight collapses concurrent work on the same key into one execution and
// shares its result.
//
// The overlaybd path needs this at layer granularity, which is finer than the
// per-reference dedup PullingProvider does. Two different images sharing a base
// are two different references, so that dedup does not see them as related --
// but they name the same layer digests, and without this each converts the
// shared layers independently: fetch the same blob, run apply and commit on the
// same bytes, and race to rename the same path.
//
// The rename makes that race safe rather than cheap. Both builds produce
// identical bytes and one overwrites the other, so nothing is corrupted; the
// cost is the duplicated fetch and conversion, which for a base layer is the
// most expensive thing this package does. Measured at 2.15-2.32s of CPU for one
// layer's conversion, so a fan-out of concurrent creates on a fresh node
// multiplies it.
type layerFlight struct {
	mu       sync.Mutex
	inflight map[string]*layerBuild
}

type layerBuild struct {
	done chan struct{}
	path string
	err  error
}

// do runs fn for key unless an equivalent call is already running, in which case
// it waits for that one instead.
//
// Waiters share the leader's outcome but keep their own cancellation: a client
// that gave up is not held by work someone else still wants. The leader runs
// with the cancellation stripped for the same reason -- others depend on it, so
// the first caller walking away must not abandon the conversion the rest are
// waiting for.
func (f *layerFlight) do(ctx context.Context, key string, fn func(context.Context) (string, error)) (string, error) {
	f.mu.Lock()
	if f.inflight == nil {
		f.inflight = map[string]*layerBuild{}
	}
	if existing, ok := f.inflight[key]; ok {
		f.mu.Unlock()
		select {
		case <-existing.done:
			return existing.path, existing.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	build := &layerBuild{done: make(chan struct{})}
	f.inflight[key] = build
	f.mu.Unlock()

	build.path, build.err = fn(context.WithoutCancel(ctx))

	// Deleted before the waiters are released so a caller arriving after a
	// failure starts a fresh attempt rather than joining a finished one and
	// inheriting its error. A transient registry failure should not be sticky.
	f.mu.Lock()
	delete(f.inflight, key)
	f.mu.Unlock()
	close(build.done)

	return build.path, build.err
}

package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// A batch of evaluations arrives as a burst by construction: a run fans out
// hundreds of sandboxes from one command. Measured on a 16-core node, 30
// concurrent creates produced 16 successes and 14 immediate rejections, and the
// caller's only recourse was to retry — which arrives as another burst.
//
// The two kinds of "full" deserve different answers, and the distinction is
// duration rather than severity:
//
//   - Create concurrency is transient. Those 16 creates drained in about seven
//     seconds, so a caller that waited would have been placed. Rejecting is
//     throwing away a request that was moments from succeeding.
//   - CPU, memory and disk commitments are held for a sandbox's whole life. A
//     request that does not fit now will not fit in ten seconds either, because
//     nothing is scheduled to free them.
//
// So waiting is only offered for the transient one. Waiting on a lifetime
// commitment would convert a fast, clear rejection into a slow, identical one —
// worse on both counts.
//
// This does not raise throughput. Throughput is bounded by boot cost: each
// firecracker burns about 5 CPU-seconds reaching a reachable agent, so a node
// sustains roughly cores/5 creates per second no matter how deep the queue. What
// queueing buys is a predictable answer instead of a retry storm.

// ErrQueueTimeout is returned when a request waited for create capacity and it
// did not appear. It is distinct from ErrNoCapacity: the request was admissible
// and the node was simply busy for longer than the caller was willing to wait.
var ErrQueueTimeout = errors.New("timed out waiting for create capacity")

// WaitOptions bounds how long Schedule may wait for transient capacity.
type WaitOptions struct {
	// Timeout is how long to keep trying. Zero disables waiting, which is the
	// previous behaviour: reject immediately.
	Timeout time.Duration
	// Poll is the interval between attempts. Zero uses a default derived from
	// measured create latency.
	Poll time.Duration
}

// defaultPoll is chosen against measured create latency rather than picked for
// roundness. A single create takes ~950ms and a saturated one ~6s, so polling
// much faster than this only adds load to the store while the node is already the
// bottleneck; polling much slower adds latency after a slot frees.
const defaultPoll = 250 * time.Millisecond

func (o WaitOptions) poll() time.Duration {
	if o.Poll > 0 {
		return o.Poll
	}
	return defaultPoll
}

// ScheduleWait places a request, waiting for create concurrency to drain if that
// is the only thing in the way.
//
// It returns immediately — without waiting — when the request cannot fit for a
// reason that waiting will not change. Distinguishing the two is the whole point:
// see the note at the top of this file.
func (s *Scheduler) ScheduleWait(ctx context.Context, req *Request, opts WaitOptions) (string, error) {
	node, err := s.Schedule(req)
	if err == nil || opts.Timeout <= 0 {
		return node, err
	}
	if !errors.Is(err, ErrNoCapacity) {
		return "", err
	}

	// Only wait when some node could admit this once transient capacity frees. A
	// node short on memory as well would still be short after the wait, and a
	// request that waited on it would get the same rejection later — having also
	// held a client for the duration.
	if !s.worthWaiting(req) {
		return "", err
	}

	deadline := s.now().Add(opts.Timeout)
	ticker := time.NewTicker(opts.poll())
	defer ticker.Stop()

	waited := err
	for {
		select {
		case <-ctx.Done():
			// The caller gave up. Its own error is more useful than ours: a
			// cancelled request is not a capacity problem.
			return "", ctx.Err()
		case <-ticker.C:
		}

		node, err := s.Schedule(req)
		if err == nil {
			return node, nil
		}
		if !errors.Is(err, ErrNoCapacity) {
			return "", err
		}
		waited = err

		// Re-checked every round rather than once: a node can fill up with
		// long-lived commitments while this request waits, at which point waiting
		// has stopped being useful.
		if !s.worthWaiting(req) {
			return "", err
		}
		if !s.now().Before(deadline) {
			return "", fmt.Errorf("%w after %s: %v", ErrQueueTimeout, opts.Timeout, waited)
		}
	}
}

// worthWaiting reports whether some node could admit this request once transient
// capacity frees.
//
// A node qualifies when its only blocker is create concurrency, or when it has no
// blocker at all. The second case is not redundant: this runs after a failed
// Schedule, and under a burst the node's in-flight count routinely drops in
// between. Treating "nothing is blocking any more" as a reason to stop waiting
// rejected requests that were a moment from being placed — observed as 6 of 30
// refused with `createConcurrency blocked 1/1`, which is precisely the case that
// should have queued.
func (s *Scheduler) worthWaiting(req *Request) bool {
	nodes, err := s.store.LoadNodes()
	if err != nil {
		// Cannot tell, so do not hold the caller. An unreadable store is a
		// different problem and waiting would only obscure it.
		return false
	}
	for _, n := range nodes {
		if n.Region != req.Region {
			continue
		}
		reasons := blockers(n, req)
		if len(reasons) == 0 {
			// Feasible as of this read: the earlier failure was a race, so retrying
			// is the right answer rather than refusing.
			return true
		}
		if len(reasons) == 1 && reasons[0] == constraintCreates {
			return true
		}
	}
	return false
}

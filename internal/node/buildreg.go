package node

import (
	"context"
	"sync"
	"time"

	"github.com/garysng/bean/internal/node/runtime"
)

// The node owns a build's lifetime, not whoever opened the BuildImage stream.
// Because the log now goes to shared storage (the node uploads it), a build is
// followable and cancellable from any control-plane replica -- so the node keeps
// a registry of in-flight builds keyed by tag and cancels the right one when a
// CancelBuild arrives, rather than relying on the originating stream's context
// being the only handle. This is what makes a build survive the control plane
// restarting mid-build (docs/build-logs-s3.md §8).
//
// One build per tag is guaranteed upstream: the control plane claims the tag in
// its store before starting a build and tags are immutable, so the registry
// never holds two live entries for one tag.

// buildPhase is where a build is in its lifecycle, as the registry sees it. It
// mirrors the proto BuildPhase but keeps the registry free of a proto import;
// grpc.go maps between the two.
type buildPhase int

const (
	buildUnknown buildPhase = iota // no entry for the tag
	buildRunning                   // registered, still building
	buildSucceeded                 // finished with a result
	buildFailed                    // finished with an error
)

// buildOutcomeRetention is how long a finished build's outcome stays readable
// after completion. The control plane polls GetBuildStatus every second and
// stops on the first terminal phase, so it only needs the outcome to survive
// the gap between completion and the next poll -- but a control-plane restart
// mid-build must still find the outcome when its reconciler resumes polling, so
// this is generous. The map holds one small struct per recent build; cost is
// trivial and a lazy sweep (see setOutcome) bounds it.
const buildOutcomeRetention = time.Hour

// buildOutcomeEntry is a finished build's terminal result, cached so
// GetBuildStatus can answer after the build goroutine has returned. (Named to
// avoid colliding with the metrics helper buildOutcome in manager.go.)
type buildOutcomeEntry struct {
	phase      buildPhase // buildSucceeded or buildFailed
	result     runtime.BuildResult
	reason     string
	finishedAt time.Time
}

// buildRegistry tracks in-flight builds so CancelBuild can reach them and
// GetBuildStatus can report them. A build is "running" while it has a cancel
// entry and "terminal" once it has an outcome; the two maps are keyed by tag and
// the outcome outlives the cancel entry (the build goroutine records the outcome
// before releasing the cancel registration), so a poll racing completion never
// sees a build vanish between running and terminal.
type buildRegistry struct {
	mu       sync.Mutex
	cancel   map[string]context.CancelFunc
	outcomes map[string]buildOutcomeEntry
}

func newBuildRegistry() *buildRegistry {
	return &buildRegistry{
		cancel:   map[string]context.CancelFunc{},
		outcomes: map[string]buildOutcomeEntry{},
	}
}

// add derives a cancellable context for tag's build and registers its cancel.
// The returned done releases the registration; the caller defers it so a
// finished build leaves nothing behind. A build already registered for the tag
// is cancelled and replaced, which cannot normally happen (one build per tag)
// but keeps a stale entry from stranding a cancel.
func (r *buildRegistry) add(parent context.Context, tag string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	r.mu.Lock()
	if prev := r.cancel[tag]; prev != nil {
		prev()
	}
	r.cancel[tag] = cancel
	// A fresh build supersedes any cached outcome for the tag: a rebuild of the
	// same tag must report RUNNING, not the previous run's terminal phase.
	delete(r.outcomes, tag)
	r.mu.Unlock()
	return ctx, func() {
		r.mu.Lock()
		// Only remove our own entry: a replacement registration (see above) must
		// not be deleted by the goroutine it replaced.
		if r.cancel[tag] != nil {
			delete(r.cancel, tag)
		}
		r.mu.Unlock()
		cancel()
	}
}

// cancel stops the build for tag if one is registered, reporting whether it
// found it. Cancelling an unknown tag is a no-op and returns false, so a
// double-cancel or a cancel for a build that already finished is harmless.
func (r *buildRegistry) cancelBuild(tag string) bool {
	r.mu.Lock()
	cancel := r.cancel[tag]
	r.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// setOutcome records a finished build's terminal result, keyed by tag. A nil
// err means success (res carries the coordinates); a non-nil err means failure
// (its message becomes the reason). The build goroutine calls this before
// releasing its registration (the done from add), so status never reports a
// build as gone between running and terminal. Each call also lazily sweeps
// outcomes older than the retention window, bounding the map without a timer.
func (r *buildRegistry) setOutcome(tag string, res runtime.BuildResult, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	oc := buildOutcomeEntry{finishedAt: now}
	if err != nil {
		oc.phase = buildFailed
		oc.reason = err.Error()
	} else {
		oc.phase = buildSucceeded
		oc.result = res
	}
	r.outcomes[tag] = oc
	for k, v := range r.outcomes {
		if now.Sub(v.finishedAt) > buildOutcomeRetention {
			delete(r.outcomes, k)
		}
	}
}

// status reports where tag's build is. A terminal outcome wins over a live
// cancel entry: setOutcome runs before the registration is released, so during
// that overlap the build is really finished. With neither, the tag is unknown
// (never started here, or aged out) -- not an error, so a poll that races
// registration or a restart just keeps polling.
func (r *buildRegistry) status(tag string) (buildPhase, runtime.BuildResult, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if oc, ok := r.outcomes[tag]; ok {
		return oc.phase, oc.result, oc.reason
	}
	if r.cancel[tag] != nil {
		return buildRunning, runtime.BuildResult{}, ""
	}
	return buildUnknown, runtime.BuildResult{}, ""
}

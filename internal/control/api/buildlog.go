package api

import (
	"context"
	"sync"
	"time"
)

// Where build logs live, and why not on the event bus.
//
// A build is started with 202 and runs in the background, so whoever wants its
// log almost never arrives before the first line is produced: a CLI has to
// receive the accepted response before it can ask, and a person deciding to look
// arrives minutes late. That rules out the event bus (events.go) as the vehicle
// on its own -- it is a live fan-out that holds no history and drops what a slow
// subscriber cannot keep up with, which is right for lifecycle events and wrong
// for a log where the dropped lines are the ones naming the failing step.
//
// So the log is retained per build, and readers address it by byte offset. That
// makes a late reader and a reconnecting reader the same case, which is what
// keeps the HTTP side small: no resumption protocol, no per-subscriber state.
//
// The retention is deliberately in memory and deliberately bounded. Build logs
// on the way to durable storage belong with the build artifacts (the S3 work),
// and putting them in the database first would create a second home to migrate
// away from. Until then a log outlives its build by buildLogRetention, which
// covers "the build failed, go and look" and nothing longer.

const (
	// maxBuildLogBytes bounds one build's retained log. Past it the oldest bytes
	// are dropped rather than the newest: a reader following live has already
	// been sent them, and a reader arriving late wants the end, where the
	// failure is.
	maxBuildLogBytes = 4 << 20
	// buildLogRetention is how long a finished build's log is kept. Long enough
	// to investigate a failure someone noticed; short enough that a busy
	// cluster's builds do not accumulate.
	buildLogRetention = 30 * time.Minute
)

// buildTracker holds the live and recently finished builds.
type buildTracker struct {
	mu     sync.Mutex
	builds map[string]*buildLog
}

func newBuildTracker() *buildTracker {
	return &buildTracker{builds: map[string]*buildLog{}}
}

// buildLog is one build's output plus the handle that stops it.
type buildLog struct {
	// cancel stops the build. It is the only cancellation mechanism there is:
	// the node runs buildctl under the gRPC call's context, so aborting the call
	// kills the build (see internal/node/grpc.go BuildImage).
	cancel context.CancelFunc

	mu sync.Mutex
	// buf holds the retained window; start is the absolute offset of buf[0], so
	// an offset stays meaningful after the window slides.
	buf   []byte
	start int64

	done     bool
	failed   bool
	reason   string
	finished time.Time
	// changed is closed and replaced on every append and on completion, which is
	// how a follower waits without polling.
	changed chan struct{}
}

// start registers a build, replacing any finished entry for the same tag.
//
// Two live builds for one tag cannot happen: handleBuild claims the tag in the
// store first, and tags are immutable. A finished entry for the tag can exist
// only after that record was removed, so replacing it is the correct reading.
func (t *buildTracker) start(ref string, cancel context.CancelFunc) *buildLog {
	bl := &buildLog{cancel: cancel, changed: make(chan struct{})}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	t.builds[ref] = bl
	return bl
}

// get returns a build's log, or nil when nothing is retained for that tag.
func (t *buildTracker) get(ref string) *buildLog {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pruneLocked()
	return t.builds[ref]
}

// pruneLocked drops finished builds past their retention. It runs on lookups
// rather than on a timer: the map is small, and a sweeper goroutine would be a
// lifecycle to own for no benefit.
func (t *buildTracker) pruneLocked() {
	cutoff := time.Now().Add(-buildLogRetention)
	for ref, bl := range t.builds {
		bl.mu.Lock()
		expired := bl.done && bl.finished.Before(cutoff)
		bl.mu.Unlock()
		if expired {
			delete(t.builds, ref)
		}
	}
}

// Write appends output. It satisfies io.Writer so the gRPC receive loop can copy
// into it, and never fails: losing a log line must not fail a build.
func (b *buildLog) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if excess := len(b.buf) - maxBuildLogBytes; excess > 0 {
		b.buf = append(b.buf[:0], b.buf[excess:]...)
		b.start += int64(excess)
	}
	b.notifyLocked()
	return len(p), nil
}

// finish marks the build complete. reason is empty on success.
func (b *buildLog) finish(failed bool, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.done {
		return
	}
	b.done, b.failed, b.reason = true, failed, reason
	b.finished = time.Now()
	b.notifyLocked()
}

func (b *buildLog) notifyLocked() {
	close(b.changed)
	b.changed = make(chan struct{})
}

// read returns the retained bytes from offset onward, the offset they start at,
// and whether the build has finished.
//
// The returned offset can be higher than the one asked for, when the window has
// slid past it. Reporting it rather than silently starting elsewhere is what
// lets a caller say that something was skipped.
func (b *buildLog) read(since int64) (data []byte, at int64, done bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if since < b.start {
		since = b.start
	}
	end := b.start + int64(len(b.buf))
	if since > end {
		since = end
	}
	out := make([]byte, end-since)
	copy(out, b.buf[since-b.start:])
	return out, since, b.done
}

// wait blocks until there is something past offset, the build finishes, or ctx
// ends. It reports false only when ctx ended, which is a reader hanging up.
func (b *buildLog) wait(ctx context.Context, offset int64) bool {
	b.mu.Lock()
	if b.done || b.start+int64(len(b.buf)) > offset {
		b.mu.Unlock()
		return true
	}
	changed := b.changed
	b.mu.Unlock()

	select {
	case <-changed:
		return true
	case <-ctx.Done():
		return false
	}
}

// status reports completion for the trailer a reader sees at the end.
func (b *buildLog) status() (done, failed bool, reason string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.done, b.failed, b.reason
}

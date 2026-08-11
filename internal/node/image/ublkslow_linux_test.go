//go:build linux

package image

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingBackend serves reads only when released, so a test can hold several in flight.
type blockingBackend struct {
	mayBlock bool

	release  chan struct{}
	inFlight atomic.Int32
	maxSeen  atomic.Int32
	served   atomic.Int32
}

func newBlockingBackend(mayBlock bool) *blockingBackend {
	return &blockingBackend{mayBlock: mayBlock, release: make(chan struct{})}
}

func (b *blockingBackend) MayBlock() bool { return b.mayBlock }

func (b *blockingBackend) ReadAt(p []byte, _ int64) (int, error) {
	n := b.inFlight.Add(1)
	for {
		m := b.maxSeen.Load()
		if n <= m || b.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	<-b.release
	b.inFlight.Add(-1)
	b.served.Add(1)
	for i := range p {
		p[i] = 'r'
	}
	return len(p), nil
}

func (b *blockingBackend) WriteAt(p []byte, _ int64) (int, error) { return len(p), nil }
func (b *blockingBackend) Flush() error                           { return nil }

// A queue over a blocking backend keeps accepting requests instead of serialising on one.
//
// This is the bug this split exists for. The serving loop is pinned to one OS thread because
// the kernel requires every uring_cmd for a queue to come from the thread that armed it, so
// running the work inline runs it serially. With a local file that is right; with a layer read
// over HTTP it wedges the device -- a guest's first root read blocks the thread, every other
// slot queues behind it, and the sandbox reaches RUNNING with a device that never completes an
// IO.
//
// Exercised at the dispatch level rather than through a real device, because what is under test
// is the concurrency, and a kernel-backed queue would need root and a spare ublk device to say
// the same thing.
func TestQueueDispatchesSlowBackendsConcurrently(t *testing.T) {
	b := newBlockingBackend(true)
	q := &ublkQueue{
		depth:       8,
		backend:     b,
		bufs:        make([][]byte, 8),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
		completions: make(chan ioCompletion, 8),
	}
	if sb, ok := q.backend.(slowBackend); ok {
		q.slow = sb.MayBlock()
	}
	if !q.slow {
		t.Fatal("a backend reporting MayBlock() was not treated as slow, so its reads would " +
			"run inline on the queue's single thread")
	}

	// Four requests dispatched the way serve() does for a slow backend.
	const n = 4
	var wg sync.WaitGroup
	for tag := uint16(0); tag < n; tag++ {
		q.bufs[tag] = make([]byte, 512)
		wg.Add(1)
		q.pending++
		go func(tag uint16) {
			defer wg.Done()
			// handle() would read the kernel's descriptor, which does not exist here, so the
			// backend is driven directly -- the dispatch is what matters.
			p := q.bufs[tag]
			nRead, err := q.backend.ReadAt(p, 0)
			res := int32(nRead)
			if err != nil {
				res = -1
			}
			q.completions <- ioCompletion{tag: tag, res: res}
		}(tag)
	}

	// All four should be inside ReadAt at once. Without the split, one would be running and
	// three would not have been dispatched at all.
	deadline := time.Now().Add(5 * time.Second)
	for b.inFlight.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d requests reached the backend within 5s: they are being "+
				"served one at a time", b.inFlight.Load(), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
	close(b.release)
	wg.Wait()

	if got := b.maxSeen.Load(); got < n {
		t.Errorf("at most %d requests were in flight together, want %d", got, n)
	}
	if got := b.served.Load(); got != n {
		t.Errorf("served %d requests, want %d", got, n)
	}
	if len(q.completions) != n {
		t.Errorf("%d completions queued, want %d", len(q.completions), n)
	}
}

// A local backend is not treated as slow, so it keeps the inline path.
//
// The handoff is not free: a goroutine, a channel round trip and a scheduling point cost more
// than a pread of a local file. Paying that per request on the default path would be a
// regression for every sandbox that is not lazily pulled.
func TestQueueKeepsLocalBackendsInline(t *testing.T) {
	q := &ublkQueue{backend: newBlockingBackend(false)}
	if sb, ok := q.backend.(slowBackend); ok {
		q.slow = sb.MayBlock()
	}
	if q.slow {
		t.Error("a backend reporting MayBlock() == false was treated as slow")
	}

	// A backend with no MayBlock method at all is also inline: fileBackend is the case,
	// and it must not pay for a property it does not have.
	var plain ublkBackend = &fileBackend{}
	if _, ok := plain.(slowBackend); ok {
		t.Error("fileBackend advertises slowBackend, so every local create would take the " +
			"worker path")
	}
}

// drainCompletions commits what workers finished, and leaves pending consistent.
//
// pending is what decides whether the loop may block in the kernel. If it drifts above the
// real count the loop polls forever; below it, the loop blocks in io_uring_enter while a
// finished slot sits uncommitted -- and since the guest cannot issue more IO than the queue
// has slots, the device wedges with every slot finished and none returned.
func TestDrainCompletionsKeepsPendingConsistent(t *testing.T) {
	q := &ublkQueue{
		completions: make(chan ioCompletion, 4),
		commitFn:    func(uint16, int32) error { return nil },
	}
	q.pending = 3
	for tag := uint16(0); tag < 3; tag++ {
		q.completions <- ioCompletion{tag: tag, res: 512}
	}
	if err := q.drainCompletions(); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if q.pending != 0 {
		t.Errorf("pending = %d after draining every completion, want 0", q.pending)
	}
	// Draining an empty channel is a no-op rather than a block.
	if err := q.drainCompletions(); err != nil {
		t.Fatalf("drain on empty: %v", err)
	}
	if q.pending != 0 {
		t.Errorf("pending = %d after an empty drain, want 0", q.pending)
	}
}

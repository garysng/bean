package image

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The property under test is not correctness of the output -- concurrent builds of
// one digest already produced identical bytes, since the layer is immutable and
// published by rename. It is that the expensive work happens once. Without the
// flight these tests still pass their result assertions and fail only on the call
// count, which is the whole point of asserting on it.

func TestFlightRunsOnceForConcurrentCallers(t *testing.T) {
	var f layerFlight
	var calls atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{})

	// The leader is started alone and confirmed to be inside fn, so the others are
	// genuinely arriving at an in-flight key rather than racing to create it.
	go func() {
		_, _ = f.do(context.Background(), "sha256:aaa", func(context.Context) (string, error) {
			calls.Add(1)
			close(entered)
			<-release
			return "/layers/aaa.obd", nil
		})
	}()
	<-entered

	const waiters = 7
	var wg sync.WaitGroup
	paths := make([]string, waiters)
	errs := make([]error, waiters)
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			paths[i], errs[i] = f.do(context.Background(), "sha256:aaa", func(context.Context) (string, error) {
				// A distinct path, so a waiter that ran its own conversion is visible
				// in the result and not only in the counter.
				calls.Add(1)
				<-release
				return "/layers/own.obd", nil
			})
		}(i)
	}

	// Waiting for an absence rather than for the waiters to arrive, because "has
	// registered as a waiter" is not observable from outside -- a waiter blocks on the
	// leader's channel without touching the map. A waiter that instead started its own
	// conversion increments the counter immediately, so polling for that is what makes
	// the check meaningful. An earlier version released the leader as soon as it was
	// running, which let the others arrive after the flight was already gone and start
	// their own; it passed only because they usually did not.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if calls.Load() > 1 {
			t.Fatalf("conversion ran %d times; concurrent callers are not sharing one flight", calls.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}

	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Errorf("conversion ran %d times, want 1", got)
	}
	for i := 0; i < waiters; i++ {
		if errs[i] != nil {
			t.Errorf("waiter %d: %v", i, errs[i])
		}
		if paths[i] != "/layers/aaa.obd" {
			t.Errorf("waiter %d got %q, want the leader's path", i, paths[i])
		}
	}
}

// Different digests must not share a flight, or an image would wait on a layer it
// does not contain.
func TestFlightKeysAreIndependent(t *testing.T) {
	var f layerFlight
	started := make(chan string, 2)
	release := make(chan struct{})
	done := make(chan struct{}, 2)

	for _, digest := range []string{"sha256:aaa", "sha256:bbb"} {
		go func(digest string) {
			_, _ = f.do(context.Background(), digest, func(context.Context) (string, error) {
				started <- digest
				<-release
				return digest, nil
			})
			done <- struct{}{}
		}(digest)
	}

	// Both must be able to start while neither has finished. If they shared a
	// flight the second would block and this read would time out.
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case d := <-started:
			seen[d] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("only %v started; distinct digests are sharing a flight", seen)
		}
	}
	close(release)
	for i := 0; i < 2; i++ {
		<-done
	}
}

// A caller that gives up must not take the conversion with it: the others are
// waiting on it, and on the real path so is a layer file other images will want.
func TestFlightLeaderSurvivesCallerCancellation(t *testing.T) {
	var f layerFlight
	ctx, cancel := context.WithCancel(context.Background())
	running := make(chan struct{})
	finished := make(chan string, 1)

	go func() {
		path, _ := f.do(ctx, "sha256:aaa", func(inner context.Context) (string, error) {
			close(running)
			// Fails if the leader inherited the caller's cancellation.
			select {
			case <-inner.Done():
				return "", errors.New("conversion was cancelled with its caller")
			case <-time.After(200 * time.Millisecond):
			}
			return "/layers/aaa.obd", nil
		})
		finished <- path
	}()

	<-running
	cancel()
	select {
	case path := <-finished:
		if path != "/layers/aaa.obd" {
			t.Errorf("leader returned %q; the conversion did not complete", path)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("leader never finished")
	}
}

// A waiter, unlike the leader, does get to leave -- it has nothing others depend
// on.
func TestFlightWaiterHonoursItsOwnCancellation(t *testing.T) {
	var f layerFlight
	leaderRunning := make(chan struct{})
	release := make(chan struct{})
	defer close(release)

	go func() {
		_, _ = f.do(context.Background(), "sha256:aaa", func(context.Context) (string, error) {
			close(leaderRunning)
			<-release
			return "/layers/aaa.obd", nil
		})
	}()
	<-leaderRunning

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.do(ctx, "sha256:aaa", func(context.Context) (string, error) {
		t.Error("waiter started its own conversion")
		return "", nil
	}); !errors.Is(err, context.Canceled) {
		t.Errorf("waiter error = %v, want context.Canceled", err)
	}
}

// A failed conversion must not be remembered. Registry failures are usually
// transient, and a create arriving after one should retry rather than be handed
// the previous attempt's error.
func TestFlightDoesNotCacheFailures(t *testing.T) {
	var f layerFlight
	var calls atomic.Int32
	boom := errors.New("registry unreachable")

	if _, err := f.do(context.Background(), "sha256:aaa", func(context.Context) (string, error) {
		calls.Add(1)
		return "", boom
	}); !errors.Is(err, boom) {
		t.Fatalf("first attempt err = %v, want %v", err, boom)
	}

	path, err := f.do(context.Background(), "sha256:aaa", func(context.Context) (string, error) {
		calls.Add(1)
		return "/layers/aaa.obd", nil
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if path != "/layers/aaa.obd" {
		t.Errorf("retry returned %q", path)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("ran %d times, want 2: the failure was cached", got)
	}
}

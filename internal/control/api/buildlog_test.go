package api

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestBuildLogReadTracksOffsets writes in two goes and reads from an offset,
// exercising the byte-addressed reader a late follower relies on.
func TestBuildLogReadTracksOffsets(t *testing.T) {
	tr := newBuildTracker()
	bl := tr.start("app:v1", func() {})

	if _, err := bl.Write([]byte("hello ")); err != nil {
		t.Fatal(err)
	}
	if _, err := bl.Write([]byte("world")); err != nil {
		t.Fatal(err)
	}
	// A zero-length write is a no-op, not an error.
	if n, err := bl.Write(nil); n != 0 || err != nil {
		t.Errorf("Write(nil) = %d, %v; want 0, nil", n, err)
	}

	data, at, done := bl.read(0)
	if string(data) != "hello world" || at != 0 || done {
		t.Fatalf("read(0) = %q, %d, %v", data, at, done)
	}
	// Reading from mid-stream returns only the tail.
	data, at, _ = bl.read(6)
	if string(data) != "world" || at != 6 {
		t.Errorf("read(6) = %q at %d, want world at 6", data, at)
	}
	// Reading past the end clamps rather than panicking.
	data, at, _ = bl.read(100)
	if len(data) != 0 || at != 11 {
		t.Errorf("read(100) = %q at %d, want empty at 11", data, at)
	}
}

// TestBuildLogWindowSlides drives the retained window past its cap and confirms
// the start offset advances so an old offset still resolves.
func TestBuildLogWindowSlides(t *testing.T) {
	tr := newBuildTracker()
	bl := tr.start("big:v1", func() {})
	chunk := strings.Repeat("x", 1<<20)
	// Five MiB into a 4 MiB window: the oldest megabyte is dropped.
	for i := 0; i < 5; i++ {
		bl.Write([]byte(chunk))
	}
	data, at, _ := bl.read(0)
	if at == 0 {
		t.Error("window did not slide: start offset still 0")
	}
	if len(data) > maxBuildLogBytes {
		t.Errorf("retained %d bytes, want <= %d", len(data), maxBuildLogBytes)
	}
}

// TestBuildLogWaitWakesOnAppend blocks a follower and then appends, which must
// release the wait without the context ending.
func TestBuildLogWaitWakesOnAppend(t *testing.T) {
	tr := newBuildTracker()
	bl := tr.start("wait:v1", func() {})
	woke := make(chan bool, 1)
	go func() { woke <- bl.wait(context.Background(), 0) }()
	// Give the goroutine a moment to park on the changed channel.
	time.Sleep(20 * time.Millisecond)
	bl.Write([]byte("data"))
	select {
	case ok := <-woke:
		if !ok {
			t.Error("wait returned false on an append")
		}
	case <-time.After(time.Second):
		t.Fatal("wait did not wake on append")
	}
}

// TestBuildLogWaitReturnsOnCancel confirms a hung-up reader is reported as false.
func TestBuildLogWaitReturnsOnCancel(t *testing.T) {
	tr := newBuildTracker()
	bl := tr.start("cancel:v1", func() {})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if bl.wait(ctx, 0) {
		t.Error("wait returned true for a cancelled context")
	}
}

// TestBuildLogWaitReturnsWhenDone confirms a finished build never blocks a
// follower, even one asking about an offset that has data.
func TestBuildLogWaitReturnsWhenDone(t *testing.T) {
	tr := newBuildTracker()
	bl := tr.start("done:v1", func() {})
	bl.finish(false, "")
	if !bl.wait(context.Background(), 0) {
		t.Error("wait blocked on a finished build")
	}
}

// TestBuildLogFinishIsOnceAndReported pins the completion trailer and the
// once-only guard finish carries.
func TestBuildLogFinishIsOnceAndReported(t *testing.T) {
	tr := newBuildTracker()
	bl := tr.start("fin:v1", func() {})
	bl.finish(true, "boom")
	if done, failed, reason := bl.status(); !done || !failed || reason != "boom" {
		t.Fatalf("status = %v/%v/%q, want done/failed/boom", done, failed, reason)
	}
	// A second finish must not overwrite the first outcome.
	bl.finish(false, "")
	if _, failed, reason := bl.status(); !failed || reason != "boom" {
		t.Errorf("second finish changed outcome: failed=%v reason=%q", failed, reason)
	}
}

// TestBuildTrackerGetAndPrune confirms get returns a live entry and drops one
// past its retention.
func TestBuildTrackerGetAndPrune(t *testing.T) {
	tr := newBuildTracker()
	bl := tr.start("live:v1", func() {})
	if tr.get("live:v1") != bl {
		t.Error("get did not return the live build")
	}
	if tr.get("absent:v1") != nil {
		t.Error("get returned a build for an unknown tag")
	}

	// A finished build older than the retention is pruned on the next lookup.
	bl.finish(false, "")
	bl.mu.Lock()
	bl.finished = time.Now().Add(-buildLogRetention - time.Minute)
	bl.mu.Unlock()
	if tr.get("live:v1") != nil {
		t.Error("expired build was not pruned")
	}
}

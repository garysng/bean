//go:build linux

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// writeEntry stands in for merging a chain: it drops the two reusable members
// into the directory the cache offers.
//
// An empty dir is how the cache tells a caller that another restore is already
// building the shared entry, so this returns nothing to publish — matching the
// real callback, which in that case only drains its own stream.
func writeEntry(t *testing.T, dir string) (snapEntry, error) {
	t.Helper()
	if dir == "" {
		return snapEntry{}, nil
	}
	var entry snapEntry
	for _, name := range []string{snapshotStateFile, snapshotMemFile} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(name+" contents"), 0o600); err != nil {
			return snapEntry{}, err
		}
		switch name {
		case snapshotStateFile:
			entry.StatePath = p
		case snapshotMemFile:
			entry.MemPath = p
		}
	}
	return entry, nil
}

func TestSnapCacheFillThenLookup(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))

	if _, ok := c.Lookup("snap_1"); ok {
		t.Fatal("reported a hit before anything was cached")
	}

	entry, err := c.Fill("snap_1", func(dir string) (snapEntry, error) {
		return writeEntry(t, dir)
	})
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	for _, p := range []string{entry.StatePath, entry.MemPath} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("Fill returned %s which does not exist: %v", p, err)
		}
	}

	got, ok := c.Lookup("snap_1")
	if !ok {
		t.Fatal("no hit after Fill")
	}
	if got != entry {
		t.Errorf("Lookup = %+v, Fill returned %+v", got, entry)
	}
}

// TestSnapCacheFillIsDoneOnceUnderConcurrency covers the case the cache exists
// for: a batch fanning out from one checkpoint restores it many times at once.
// Unpacking per restore would write the same hundreds of megabytes repeatedly,
// and worse, several writers would be producing the same file at once.
func TestSnapCacheFillIsDoneOnceUnderConcurrency(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))

	var mu sync.Mutex
	// Counts real builds, not callback calls. Every caller's callback runs — each
	// has its own stream to drain — but only one is offered a directory to build
	// the shared entry in.
	builds := 0
	calls := 0

	var wg sync.WaitGroup
	entries := make([]snapEntry, 8)
	errs := make([]error, 8)
	for i := range entries {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			entries[i], errs[i] = c.Fill("snap_hot", func(dir string) (snapEntry, error) {
				mu.Lock()
				calls++
				if dir != "" {
					builds++
				}
				mu.Unlock()
				return writeEntry(t, dir)
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("restore %d: %v", i, err)
		}
	}
	if builds != 1 {
		t.Errorf("built the shared entry %d times, want 1", builds)
	}
	if calls != len(entries) {
		t.Errorf("callback ran %d times for %d restores; each restore has its own "+
			"stream to drain, so every one must be called", calls, len(entries))
	}
	for i, e := range entries {
		if e != entries[0] {
			t.Errorf("restore %d got %+v, restore 0 got %+v", i, e, entries[0])
		}
	}
}

// TestSnapCacheDoesNotPublishAFailedUnpack guards the property that makes the
// cache safe: a torn entry would be found by every later restore and loaded into
// a VM that then faults against a truncated image.
func TestSnapCacheDoesNotPublishAFailedUnpack(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))

	want := errors.New("bundle truncated")
	if _, err := c.Fill("snap_bad", func(dir string) (snapEntry, error) {
		// Write one member, then fail: this is what an interrupted transfer
		// looks like.
		if err := os.WriteFile(filepath.Join(dir, snapshotStateFile),
			[]byte("partial"), 0o600); err != nil {
			return snapEntry{}, err
		}
		return snapEntry{}, want
	}); !errors.Is(err, want) {
		t.Fatalf("Fill error = %v, want %v", err, want)
	}

	if _, ok := c.Lookup("snap_bad"); ok {
		t.Error("a failed unpack was published to the cache")
	}
	// Nothing may be left behind under the snapshot's name either.
	if _, err := os.Stat(filepath.Join(c.dir, "snap_bad")); !os.IsNotExist(err) {
		t.Errorf("stat of failed entry = %v, want not-exist", err)
	}
}

// TestSnapCacheRejectsAnIncompleteUnpack covers an unpack that reports success
// but produced only one of the two members.
func TestSnapCacheRejectsAnIncompleteUnpack(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))
	_, err := c.Fill("snap_partial", func(dir string) (snapEntry, error) {
		p := filepath.Join(dir, snapshotStateFile)
		if err := os.WriteFile(p, []byte("state"), 0o600); err != nil {
			return snapEntry{}, err
		}
		return snapEntry{StatePath: p}, nil
	})
	if err == nil {
		t.Fatal("accepted a bundle with no memory image")
	}
	if !strings.Contains(err.Error(), "memory") {
		t.Errorf("error %q does not say what was missing", err)
	}
	if _, ok := c.Lookup("snap_partial"); ok {
		t.Error("an incomplete unpack was published")
	}
}

// TestSnapCacheLookupIgnoresAnEmptyMember covers a zero-length file left by a
// crash. It exists on disk, so a name check alone would treat it as usable.
func TestSnapCacheLookupIgnoresAnEmptyMember(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))
	dir := filepath.Join(c.dir, "snap_empty")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, snapshotStateFile),
		[]byte("state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, snapshotMemFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Lookup("snap_empty"); ok {
		t.Error("reported a hit for a zero-length memory image")
	}
}

func TestSnapCacheRequiresAnID(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))
	if _, ok := c.Lookup(""); ok {
		t.Error("an empty id reported a hit")
	}
	if _, err := c.Fill("", func(string) (snapEntry, error) {
		t.Error("unpacked despite having no cache key")
		return snapEntry{}, nil
	}); err == nil {
		t.Error("Fill accepted an empty id")
	}
}

// TestSnapCacheReusesAnEntryWithoutRebuilding is the payoff: a second restore of
// the same snapshot reuses the shared entry instead of rebuilding it.
//
// The callback still runs, with an empty dir. That is not a wasted call — each
// restore has its own stream to drain and its own writable layer to stage, and a
// caller that skipped it would leave its sender blocked on a stream nobody reads
// and hand its sandbox the base image instead of the snapshot's filesystem.
// So the assertion is "was told not to rebuild", not "was not called".
func TestSnapCacheReusesAnEntryWithoutRebuilding(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))
	if _, err := c.Fill("snap_reuse", func(dir string) (snapEntry, error) {
		if dir == "" {
			t.Error("first Fill was told not to build, but nothing was cached yet")
		}
		return writeEntry(t, dir)
	}); err != nil {
		t.Fatal(err)
	}

	called := false
	entry, err := c.Fill("snap_reuse", func(dir string) (snapEntry, error) {
		called = true
		if dir != "" {
			t.Errorf("second Fill offered dir %q; the entry is already cached", dir)
		}
		return snapEntry{}, nil
	})
	if err != nil {
		t.Fatalf("second Fill: %v", err)
	}
	if !called {
		t.Error("second Fill skipped the callback; its stream would never be drained")
	}
	// The cached entry is what comes back, not the callback's empty one.
	if entry.MemPath == "" || entry.StatePath == "" {
		t.Errorf("second Fill returned %+v, want the cached entry", entry)
	}
}

// TestFillTreatsMemorylessBundleAsUncacheable covers the checkpoint that has no
// guest memory. The cache exists to avoid unpacking a memory image twice, so a
// bundle without one has nothing to cache — and an earlier version of this code
// rejected it as a corrupt bundle, which would have made filesystem-only
// snapshots unrestorable.
func TestFillTreatsMemorylessBundleAsUncacheable(t *testing.T) {
	c := newSnapCache(t.TempDir())
	entry, err := c.Fill("snap-nomem",
		func(dir string) (snapEntry, error) {
			// Only a rootfs member, which lands on the sandbox's own device and
			// so is not reported as a cached path.
			return snapEntry{}, nil
		})
	if err != nil {
		t.Fatalf("memoryless bundle rejected: %v", err)
	}
	if entry.MemPath != "" || entry.StatePath != "" {
		t.Errorf("entry = %+v, want empty so the caller boots instead of loading", entry)
	}
}

// TestFillRejectsHalfBundle keeps the tolerance above from swallowing a real
// defect: one of vmstate and memory without the other is corruption, and
// loading it would leave the guest faulting on nothing.
func TestFillRejectsHalfBundle(t *testing.T) {
	c := newSnapCache(t.TempDir())
	_, err := c.Fill("snap-half",
		func(dir string) (snapEntry, error) {
			path := filepath.Join(dir, snapshotStateFile)
			if err := os.WriteFile(path, []byte("state"), 0o600); err != nil {
				return snapEntry{}, err
			}
			return snapEntry{StatePath: path}, nil
		})
	if err == nil {
		t.Error("a bundle with vmstate but no memory was accepted")
	}
}

// TestSnapCacheFillDoesNotRebuildAfterPublication pins the window CI caught.
//
// The sibling test above fans out eight goroutines and hopes the interleaving
// happens. It does, on a loaded shared runner -- but 400 race-enabled runs on two
// pinned cores did not reproduce it locally, so relying on chance means the fix is
// justified by a failure nobody can show, and a regression would look like flake.
//
// So this one opens the window deliberately: every caller but the first is held
// between observing the entry absent and taking the lock, which is exactly the
// ordering that lets the first builder publish and remove its pending marker before
// the others look for it. Measured against the unfixed code, this fails with the
// same message CI reported -- "built the shared entry 2 times, want 1".
func TestSnapCacheFillDoesNotRebuildAfterPublication(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))

	var misses atomic.Int32
	orig := afterCacheMiss
	// Only callers after the first are delayed. Delaying all of them makes everyone
	// arrive early, which is the ordering that already works.
	afterCacheMiss = func() {
		if misses.Add(1) > 1 {
			time.Sleep(60 * time.Millisecond)
		}
	}
	t.Cleanup(func() { afterCacheMiss = orig })

	var mu sync.Mutex
	builds := 0

	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.Fill("snap_race", func(dir string) (snapEntry, error) {
				if dir != "" {
					mu.Lock()
					builds++
					mu.Unlock()
				}
				return writeEntry(t, dir)
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("restore %d: %v", i, err)
		}
	}
	if builds != 1 {
		t.Errorf("built the shared entry %d times, want 1. A caller that missed the "+
			"lookup before publication and took the lock after it finds no pending "+
			"marker, so it unpacks hundreds of megabytes that are already on disk",
			builds)
	}
}

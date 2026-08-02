//go:build linux

package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// writeEntry stands in for unpacking a bundle: it drops the two reusable members
// into the directory the cache offers.
func writeEntry(t *testing.T, dir string) (map[string]string, error) {
	t.Helper()
	paths := map[string]string{}
	for _, name := range []string{snapshotStateFile, snapshotMemFile} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(name+" contents"), 0o600); err != nil {
			return nil, err
		}
		paths[name] = p
	}
	return paths, nil
}

func TestSnapCacheFillThenLookup(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))

	if _, ok := c.Lookup("snap_1"); ok {
		t.Fatal("reported a hit before anything was cached")
	}

	entry, err := c.Fill("snap_1", nil, func(dir string) (map[string]string, error) {
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
	unpacks := 0

	var wg sync.WaitGroup
	entries := make([]snapEntry, 8)
	errs := make([]error, 8)
	for i := range entries {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			entries[i], errs[i] = c.Fill("snap_hot", nil,
				func(dir string) (map[string]string, error) {
					mu.Lock()
					unpacks++
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
	if unpacks != 1 {
		t.Errorf("unpacked %d times, want 1", unpacks)
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
	if _, err := c.Fill("snap_bad", nil, func(dir string) (map[string]string, error) {
		// Write one member, then fail: this is what an interrupted transfer
		// looks like.
		if err := os.WriteFile(filepath.Join(dir, snapshotStateFile),
			[]byte("partial"), 0o600); err != nil {
			return nil, err
		}
		return nil, want
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
	_, err := c.Fill("snap_partial", nil, func(dir string) (map[string]string, error) {
		p := filepath.Join(dir, snapshotStateFile)
		if err := os.WriteFile(p, []byte("state"), 0o600); err != nil {
			return nil, err
		}
		return map[string]string{snapshotStateFile: p}, nil
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
	if _, err := c.Fill("", nil, func(string) (map[string]string, error) {
		t.Error("unpacked despite having no cache key")
		return nil, nil
	}); err == nil {
		t.Error("Fill accepted an empty id")
	}
}

// TestSnapCacheReusesAnEntryWithoutUnpacking is the payoff: the second restore
// of a snapshot must not run the unpack function at all.
func TestSnapCacheReusesAnEntryWithoutUnpacking(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))
	if _, err := c.Fill("snap_reuse", nil, func(dir string) (map[string]string, error) {
		return writeEntry(t, dir)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fill("snap_reuse", nil, func(string) (map[string]string, error) {
		t.Error("unpacked again for a snapshot already in the cache")
		return nil, errors.New("should not be called")
	}); err != nil {
		t.Fatalf("second Fill: %v", err)
	}
}

//go:build linux

package runtime

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// seedEntry publishes a cache entry of a given size with a given last-use time.
// It writes through the real filesystem rather than faking the cache's view of
// it, because the thing under test is block accounting and mtime ordering — both
// of which a fake would define away.
func seedEntry(t *testing.T, c *snapCache, id string, bytes int64, age time.Duration) {
	t.Helper()
	dir := filepath.Join(c.dir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("seed %s: %v", id, err)
	}
	for _, name := range []string{snapshotStateFile, snapshotMemFile} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, bytes/2), 0o600); err != nil {
			t.Fatalf("seed %s/%s: %v", id, name, err)
		}
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(dir, when, when); err != nil {
		t.Fatalf("age %s: %v", id, err)
	}
}

func TestEvictionPolicyRejectsMarksThatCannotBound(t *testing.T) {
	if err := (EvictionPolicy{}).Validate(); err != nil {
		t.Fatalf("the zero policy disables eviction and must be valid: %v", err)
	}
	for _, tc := range []struct {
		name   string
		policy EvictionPolicy
	}{
		{"low above high", EvictionPolicy{HighBytes: 100, LowBytes: 200}},
		{"low equal to high", EvictionPolicy{HighBytes: 100, LowBytes: 100}},
		{"high without low", EvictionPolicy{HighBytes: 100}},
		{"low without high", EvictionPolicy{LowBytes: 100}},
	} {
		if err := (tc.policy).Validate(); err == nil {
			t.Errorf("%s: expected rejection, got none", tc.name)
		}
	}
}

func TestUsageCountsAllocatedBlocksNotApparentSize(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))
	if err := os.MkdirAll(filepath.Join(c.dir, "snap_sparse"), 0o700); err != nil {
		t.Fatal(err)
	}
	// A merged memory image is sparse where no ancestor wrote. Charging the cache
	// for its apparent size would evict entries to reclaim nothing.
	f, err := os.Create(filepath.Join(c.dir, "snap_sparse", snapshotMemFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(512 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()

	used, err := c.Usage()
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	if used > 1<<20 {
		t.Errorf("a wholly sparse 512 MiB file should be charged near zero, got %d bytes", used)
	}
}

func TestEvictReclaimsOldestDownToTheLowMark(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))
	// Four entries of 64 KiB each. A sweep at 200 KiB down to 100 KiB has to
	// remove two, oldest first.
	seedEntry(t, c, "snap_oldest", 64<<10, 4*time.Hour)
	seedEntry(t, c, "snap_older", 64<<10, 3*time.Hour)
	seedEntry(t, c, "snap_newer", 64<<10, 2*time.Hour)
	seedEntry(t, c, "snap_newest", 64<<10, time.Hour)

	freed, err := c.Evict(EvictionPolicy{HighBytes: 200 << 10, LowBytes: 100 << 10})
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	if freed == 0 {
		t.Fatal("expected the sweep to reclaim something")
	}
	for _, id := range []string{"snap_oldest", "snap_older"} {
		if _, ok := c.Lookup(id); ok {
			t.Errorf("%s is the coldest and should have been reclaimed", id)
		}
	}
	if _, ok := c.Lookup("snap_newest"); !ok {
		t.Error("the hottest entry must survive a sweep")
	}
}

func TestEvictDoesNothingBelowTheHighMark(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))
	seedEntry(t, c, "snap_a", 64<<10, time.Hour)

	freed, err := c.Evict(EvictionPolicy{HighBytes: 10 << 20, LowBytes: 8 << 20})
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	if freed != 0 {
		t.Errorf("nothing should be reclaimed below the trigger, freed %d", freed)
	}
	if _, ok := c.Lookup("snap_a"); !ok {
		t.Error("entry removed while the cache was under its high mark")
	}
}

func TestEvictIsDisabledByTheZeroPolicy(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))
	seedEntry(t, c, "snap_a", 1<<20, time.Hour)

	if _, err := c.Evict(EvictionPolicy{}); err != nil {
		t.Fatalf("evict: %v", err)
	}
	if _, ok := c.Lookup("snap_a"); !ok {
		t.Error("the zero policy must leave the cache alone")
	}
}

// A pinned entry is one a restore has looked up but not yet opened. Reclaiming it
// there turns the later open into ENOENT with the restore's stream already
// consumed, so there is nothing left to rebuild from.
func TestEvictSkipsAPinnedEntryEvenWhenItIsColdest(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))
	seedEntry(t, c, "snap_pinned", 64<<10, 4*time.Hour)
	seedEntry(t, c, "snap_cold", 64<<10, 3*time.Hour)
	seedEntry(t, c, "snap_warm", 64<<10, time.Hour)

	unpin := c.Pin("snap_pinned")

	if _, err := c.Evict(EvictionPolicy{HighBytes: 100 << 10, LowBytes: 1 << 10}); err != nil {
		t.Fatalf("evict: %v", err)
	}
	if _, ok := c.Lookup("snap_pinned"); !ok {
		t.Fatal("a pinned entry was reclaimed; a restore holding it would fail to " +
			"open its memory image with nothing left to rebuild from")
	}

	// Once released it becomes reclaimable like anything else. The high mark drops
	// below its size because the earlier sweep already took everything else: at
	// the original mark the cache is now under the trigger, and declining to sweep
	// there is correct.
	unpin()
	if _, err := c.Evict(EvictionPolicy{HighBytes: 8 << 10, LowBytes: 1 << 10}); err != nil {
		t.Fatalf("evict after unpin: %v", err)
	}
	if _, ok := c.Lookup("snap_pinned"); ok {
		t.Error("an unpinned cold entry should be reclaimable")
	}
}

func TestPinIsCountedSoConcurrentRestoresBothProtectTheEntry(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))
	seedEntry(t, c, "snap_shared", 64<<10, 4*time.Hour)

	first := c.Pin("snap_shared")
	second := c.Pin("snap_shared")

	// One restore finishing must not expose the entry while the other still holds
	// only a path to it.
	first()
	if !c.inUse("snap_shared") {
		t.Fatal("entry became evictable while a second restore still held it")
	}
	second()
	if c.inUse("snap_shared") {
		t.Error("entry stayed pinned after every holder released it")
	}
}

func TestUnpinIsIdempotent(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))
	seedEntry(t, c, "snap_a", 1<<10, time.Hour)

	unpin := c.Pin("snap_a")
	other := c.Pin("snap_a")
	unpin()
	// stageSnapshot's Close can run on more than one path — an error return and a
	// deferred cleanup — so a double release must not drop somebody else's pin.
	unpin()
	if !c.inUse("snap_a") {
		t.Fatal("a repeated release dropped another holder's pin")
	}
	other()
	if c.inUse("snap_a") {
		t.Error("entry stayed pinned after the last holder released it")
	}
}

func TestEvictSkipsAnEntryBeingUnpacked(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))
	seedEntry(t, c, "snap_building", 64<<10, 4*time.Hour)
	seedEntry(t, c, "snap_cold", 64<<10, 3*time.Hour)

	// An in-flight unpack means waiters are parked on this id; removing the entry
	// under them would have them return a path to nothing.
	c.mu.Lock()
	c.pending["snap_building"] = &snapUnpack{done: make(chan struct{})}
	c.mu.Unlock()

	if _, err := c.Evict(EvictionPolicy{HighBytes: 64 << 10, LowBytes: 1 << 10}); err != nil {
		t.Fatalf("evict: %v", err)
	}
	if _, ok := c.Lookup("snap_building"); !ok {
		t.Error("an entry with an unpack in flight was reclaimed")
	}
}

// Removal renames before deleting so a Lookup never sees a half-destroyed entry:
// deleting in place passes through a state where the machine state is gone but
// the memory image is not, and a load against that faults on nothing.
func TestRemoveNeverExposesAHalfDestroyedEntry(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))
	seedEntry(t, c, "snap_a", 8<<10, time.Hour)

	if _, ok := c.Lookup("snap_a"); !ok {
		t.Fatal("seeded entry is not visible")
	}
	if err := c.remove("snap_a"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := c.Lookup("snap_a"); ok {
		t.Error("entry still visible after removal")
	}
	// The staging directory used for the rename must not survive as a stray entry
	// that a later scan would try to account for.
	dirents, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirents) != 0 {
		t.Errorf("removal left %d entries behind: %v", len(dirents), dirents)
	}
}

func TestScanIgnoresInProgressUnpackDirectories(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// unpackInto builds here and publishes by rename. Counting it as an entry
	// would let a sweep delete a restore that is still writing.
	tmp, err := os.MkdirTemp(c.dir, ".unpack-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, snapshotMemFile), make([]byte, 64<<10), 0o600); err != nil {
		t.Fatal(err)
	}

	entries, err := c.scan()
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("scan counted an in-progress unpack as a cache entry: %v", entries)
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Errorf("in-progress unpack directory was disturbed: %v", err)
	}
}

func TestTouchMakesAColdEntryOutliveAnUntouchedOne(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "snapshots"))
	seedEntry(t, c, "snap_reused", 64<<10, 4*time.Hour)
	seedEntry(t, c, "snap_forgotten", 64<<10, time.Hour)

	// A fan-out hammering one leaf must keep it, even though it was unpacked long
	// before the entry nobody has asked for since.
	if err := c.Touch("snap_reused"); err != nil {
		t.Fatalf("touch: %v", err)
	}

	if _, err := c.Evict(EvictionPolicy{HighBytes: 100 << 10, LowBytes: 70 << 10}); err != nil {
		t.Fatalf("evict: %v", err)
	}
	if _, ok := c.Lookup("snap_reused"); !ok {
		t.Error("a recently reused entry was reclaimed ahead of an untouched one")
	}
	if _, ok := c.Lookup("snap_forgotten"); ok {
		t.Error("the untouched entry should have been reclaimed first")
	}
}

func TestUsageOnANodeThatHasRestoredNothing(t *testing.T) {
	c := newSnapCache(filepath.Join(t.TempDir(), "never-created"))
	used, err := c.Usage()
	if err != nil {
		t.Fatalf("a missing cache directory is not an error: %v", err)
	}
	if used != 0 {
		t.Errorf("expected zero usage, got %d", used)
	}
}

//go:build linux

package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Eviction has one failure that reports nothing: removing a bundle a restore is
// reading. On Linux the unlink succeeds and the reader keeps its open file, so the
// restore finishes normally and the node is quietly cold for that image afterwards.
// Nothing logs it, no assertion about that restore fails, and the next create just
// boots. That is the test worth having here.

// seedWarm writes a bundle of size bytes with a given last-used time.
func seedWarm(t *testing.T, s *warmStore, digest string, size int, age time.Duration) warmKey {
	t.Helper()
	k := warmKey{Digest: digest, Vendor: "AuthenticAMD", Family: 23}
	f, commit, err := s.Create(k)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(make([]byte, size)); err != nil {
		t.Fatal(err)
	}
	if err := commit(); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(s.Path(k), when, when); err != nil {
		t.Fatal(err)
	}
	return k
}

// names lists the bundles present.
func names(t *testing.T, s *warmStore) []string {
	t.Helper()
	got, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(got))
	for n := range got {
		out = append(out, n)
	}
	return out
}

func has(list []string, name string) bool {
	for _, n := range list {
		if n == name {
			return true
		}
	}
	return false
}

// TestSweepIsOffWithoutAPolicy keeps a node that has not opted in behaving as it did
// before eviction existed.
func TestSweepIsOffWithoutAPolicy(t *testing.T) {
	s := newWarmStore(filepath.Join(t.TempDir(), "warm"))
	k := seedWarm(t, s, digestA, 4096, 0)

	freed, evicted, err := s.Sweep(EvictionPolicy{}, s.inUse, "")
	if err != nil {
		t.Fatal(err)
	}
	if freed != 0 || evicted != 0 {
		t.Errorf("swept with no policy: freed=%d evicted=%d", freed, evicted)
	}
	if _, ok := s.Lookup(k); !ok {
		t.Error("removed a bundle with eviction disabled")
	}
}

// TestSweepDoesNothingBelowTheHighMark is the watermark's point: eviction is an
// occasional batch, not something every prewarm pays for.
func TestSweepDoesNothingBelowTheHighMark(t *testing.T) {
	s := newWarmStore(filepath.Join(t.TempDir(), "warm"))
	seedWarm(t, s, digestA, 4096, 0)

	_, evicted, err := s.Sweep(EvictionPolicy{HighBytes: 1 << 30, LowBytes: 1 << 29}, s.inUse, "")
	if err != nil {
		t.Fatal(err)
	}
	if evicted != 0 {
		t.Errorf("evicted %d entries while under the high mark", evicted)
	}
}

// TestSweepEvictsTheLeastRecentlyRestored is the ordering property, and the reason
// this sweeper does not reuse the snapshot cache's: a warm bundle is written once and
// read for weeks, so ordering by age-since-creation would evict the busiest one on a
// node as soon as it became the oldest.
func TestSweepEvictsTheLeastRecentlyRestored(t *testing.T) {
	s := newWarmStore(filepath.Join(t.TempDir(), "warm"))
	// Ages deliberately run *against* filename order. digestA sorts first and digestC
	// last, so the coldest entry is given the last-sorting digest: an implementation
	// that fell back to sorting by name would evict digestA and this test would
	// notice, where an earlier version assigned ages in the same order as the names
	// and passed against exactly that bug.
	hot := seedWarm(t, s, digestA, 64<<10, 1*time.Minute)
	mid := seedWarm(t, s, digestB, 64<<10, 1*time.Hour)
	cold := seedWarm(t, s, digestC, 64<<10, 48*time.Hour)

	// High mark below the total, low mark leaving room for two.
	_, evicted, err := s.Sweep(EvictionPolicy{HighBytes: 100 << 10, LowBytes: 96 << 10}, s.inUse, "")
	if err != nil {
		t.Fatal(err)
	}
	if evicted == 0 {
		t.Fatal("evicted nothing while over the high mark")
	}
	left := names(t, s)
	if has(left, cold.filename()) {
		t.Errorf("kept the least recently restored bundle and evicted something "+
			"warmer; remaining: %v", left)
	}
	if !has(left, hot.filename()) {
		t.Errorf("evicted the most recently restored bundle; remaining: %v", left)
	}
	_ = mid
}

// TestSweepStopsAtTheLowMark checks it reclaims to the low mark rather than emptying
// the store, which would make the next N creates all boot.
func TestSweepStopsAtTheLowMark(t *testing.T) {
	s := newWarmStore(filepath.Join(t.TempDir(), "warm"))
	for i, d := range []string{digestA, digestB, digestC, digestD} {
		seedWarm(t, s, d, 64<<10, time.Duration(4-i)*time.Hour)
	}
	before, err := s.Usage()
	if err != nil {
		t.Fatal(err)
	}

	// Low mark at roughly half, so a sweep that emptied the store is distinguishable
	// from one that stopped.
	_, evicted, err := s.Sweep(EvictionPolicy{HighBytes: before, LowBytes: before / 2}, s.inUse, "")
	if err != nil {
		t.Fatal(err)
	}
	if evicted == 0 {
		t.Fatal("evicted nothing")
	}
	if left := names(t, s); len(left) == 0 {
		t.Error("emptied the store instead of reclaiming to the low mark; every " +
			"create would boot until something is prewarmed again")
	}
	after, err := s.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if after > before/2 {
		t.Errorf("usage after sweep = %d, want at or below the low mark %d",
			after, before/2)
	}
}

// TestSweepSkipsABundleBeingRead is the one that matters. Unlinking a file another
// reader has open succeeds on Linux, so without this the restore would work, the
// bundle would vanish, and nothing would report it.
func TestSweepSkipsABundleBeingRead(t *testing.T) {
	s := newWarmStore(filepath.Join(t.TempDir(), "warm"))
	// The coldest entry, so ordering alone would pick it first.
	busy := seedWarm(t, s, digestA, 64<<10, 72*time.Hour)
	seedWarm(t, s, digestB, 64<<10, 1*time.Hour)

	release := s.hold(busy.filename())
	defer release()

	before, err := s.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Sweep(EvictionPolicy{HighBytes: before, LowBytes: 1}, s.inUse, ""); err != nil {
		t.Fatal(err)
	}

	if _, ok := s.Lookup(busy); !ok {
		t.Error("evicted a bundle that was being read. The restore reading it would " +
			"still have succeeded, and the node would be cold for that image " +
			"afterwards with nothing reporting why")
	}
}

// TestHoldIsACounterNotAFlag covers the fan-out case: several creates restore from
// one warm bundle at the same time, so one of them finishing must not make the
// bundle evictable while the others are still reading.
func TestHoldIsACounterNotAFlag(t *testing.T) {
	s := newWarmStore(filepath.Join(t.TempDir(), "warm"))
	k := seedWarm(t, s, digestA, 4096, 0)
	name := k.filename()

	first := s.hold(name)
	second := s.hold(name)
	first()
	if !s.inUse(name) {
		t.Fatal("one reader finishing marked the bundle free while another still " +
			"holds it")
	}
	second()
	if s.inUse(name) {
		t.Error("the bundle is still marked in use after every reader finished")
	}
}

// TestHoldReleaseIsIdempotent guards a double release from underflowing the count,
// which would leave a bundle permanently unevictable or permanently evictable.
func TestHoldReleaseIsIdempotent(t *testing.T) {
	s := newWarmStore(filepath.Join(t.TempDir(), "warm"))
	name := "x.warm"
	release := s.hold(name)
	release()
	release()
	if s.inUse(name) {
		t.Error("bundle still in use after release")
	}
	// A second holder must still work, which a negative count would break.
	release2 := s.hold(name)
	if !s.inUse(name) {
		t.Error("a fresh hold after a double release did not register")
	}
	release2()
}

// TestTouchMakesABundleTheMostRecentlyUsed pins the mechanism eviction orders by.
func TestTouchMakesABundleTheMostRecentlyUsed(t *testing.T) {
	s := newWarmStore(filepath.Join(t.TempDir(), "warm"))
	old := seedWarm(t, s, digestA, 64<<10, 72*time.Hour)
	seedWarm(t, s, digestB, 64<<10, 1*time.Hour)

	// A restore of the old bundle happens, which must save it from being the coldest.
	s.Touch(old)

	before, err := s.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Sweep(EvictionPolicy{HighBytes: before, LowBytes: 70 << 10}, s.inUse, ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Lookup(old); !ok {
		t.Error("evicted a bundle that had just been restored from; Touch is not " +
			"feeding the eviction order, so a hot image can be evicted for being old")
	}
}

// TestUsageOnAnAbsentStore covers a node that has never warmed anything.
func TestUsageOnAnAbsentStore(t *testing.T) {
	s := newWarmStore(filepath.Join(t.TempDir(), "never"))
	got, err := s.Usage()
	if err != nil {
		t.Errorf("Usage on a missing directory: %v", err)
	}
	if got != 0 {
		t.Errorf("Usage = %d", got)
	}
}

// TestSweepIgnoresTemporaries keeps a partial bundle from being counted or evicted:
// it is not serving anything, and Clean owns it.
func TestSweepIgnoresTemporaries(t *testing.T) {
	s := newWarmStore(filepath.Join(t.TempDir(), "warm"))
	seedWarm(t, s, digestA, 64<<10, 0)

	tmp, _, err := s.Create(warmKey{Digest: digestB, Vendor: "V", Family: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tmp.Write(make([]byte, 1<<20)); err != nil {
		t.Fatal(err)
	}
	tmp.Close()

	usage, err := s.Usage()
	if err != nil {
		t.Fatal(err)
	}
	if usage > 512<<10 {
		t.Errorf("Usage = %d; a temporary is being counted against the bound, so a "+
			"partial bundle could trigger eviction of working ones", usage)
	}
	if _, err := os.Stat(tmp.Name()); err != nil {
		t.Errorf("the temporary was removed by a sweep rather than by Clean: %v", err)
	}
	for _, n := range names(t, s) {
		if strings.Contains(n, ".tmp.") {
			t.Errorf("List reported a temporary: %s", n)
		}
	}
}

// TestSweepKeepsTheBundleJustWritten covers a defect found on real hardware rather
// than in review. With a 10 MiB low mark and 15 MB bundles, a prewarm stored its
// snapshot and the sweep that followed removed it along with everything else: the
// store ended empty and the prewarm logged success, so the node was cold for an image
// it had just been asked to warm.
//
// The condition is not exotic. Any low mark below one bundle's size reaches it, and an
// operator has no way to know a bundle's size before the first one exists.
func TestSweepKeepsTheBundleJustWritten(t *testing.T) {
	s := newWarmStore(filepath.Join(t.TempDir(), "warm"))
	// Deliberately the coldest, so ordering alone would evict it first.
	fresh := seedWarm(t, s, digestA, 64<<10, 99*time.Hour)
	seedWarm(t, s, digestB, 64<<10, 1*time.Hour)

	before, err := s.Usage()
	if err != nil {
		t.Fatal(err)
	}
	// A low mark of 1 byte cannot be reached without removing everything, which is
	// the shape of the real failure.
	if _, _, err := s.Sweep(EvictionPolicy{HighBytes: before, LowBytes: 1},
		s.inUse, fresh.filename()); err != nil {
		t.Fatal(err)
	}

	if _, ok := s.Lookup(fresh); !ok {
		t.Error("evicted the bundle passed as keep. A prewarm would report success " +
			"against a store that no longer holds what it just wrote, and every " +
			"create of that image would boot")
	}
}

// TestSweepStillEvictsOthersWhileKeepingOne checks the guard protects one entry rather
// than disabling the sweep.
func TestSweepStillEvictsOthersWhileKeepingOne(t *testing.T) {
	s := newWarmStore(filepath.Join(t.TempDir(), "warm"))
	fresh := seedWarm(t, s, digestA, 64<<10, 1*time.Minute)
	old := seedWarm(t, s, digestB, 64<<10, 99*time.Hour)

	before, err := s.Usage()
	if err != nil {
		t.Fatal(err)
	}
	_, evicted, err := s.Sweep(EvictionPolicy{HighBytes: before, LowBytes: 1},
		s.inUse, fresh.filename())
	if err != nil {
		t.Fatal(err)
	}
	if evicted != 1 {
		t.Errorf("evicted %d, want 1: keeping one entry must not stop the sweep", evicted)
	}
	if _, ok := s.Lookup(old); ok {
		t.Error("kept the cold bundle")
	}
}

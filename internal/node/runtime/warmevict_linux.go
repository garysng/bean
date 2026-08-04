//go:build linux

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// Bounding the warm store.
//
// Warm snapshots were unbounded when they were added, which is the only reason
// --fc-warm-snapshots is off by default: each bundle is a full memory image, they
// accumulate per (image, CPU generation), and nothing deleted them. A node prewarmed
// against an eval-scale image set fills its disk.
//
// The watermark pair and most of the reasoning are borrowed from the snapshot cache
// next door (snapevict_linux.go), deliberately, because the shape of the problem is
// the same. What is *not* the same is the cost of being wrong, and that difference
// is why this is a separate sweeper rather than a second caller of that one:
//
//	a snapshot-cache entry  is a derived copy. Evicting it costs one unpack, and
//	                        the bytes can be rebuilt from the control plane's blob.
//	a warm bundle           is the only copy of itself. Evicting it costs a boot --
//	                        about 5 CPU-seconds -- and it can only be rebuilt by
//	                        prewarming the image again.
//
// So the two must not share a budget: a burst of restores filling the snapshot cache
// must not evict the warm bundles that make creates cheap, and vice versa. They also
// order candidates differently, which the next comment explains.

// warmEntryInfo is one warm bundle's footprint and last use.
type warmEntryInfo struct {
	name string
	// bytes is allocated blocks rather than apparent size, for the same reason the
	// snapshot cache uses them: a bundle written from a sparse memory image holds
	// holes, and charging for bytes the filesystem never allocated would evict
	// entries to reclaim nothing.
	bytes int64
	// lastUsed is when a create last restored from this bundle, not when it was
	// written.
	//
	// This is the substantive difference from the snapshot cache's ordering. A warm
	// bundle is written once and then read by every create of its image, possibly
	// for weeks, so age-since-creation says nothing about whether it is earning its
	// space -- ordering by it would evict the most-used bundle on a node as soon as
	// it became the oldest. Touch updates this on every hit.
	lastUsed int64
}

// Usage reports the warm store's total allocated size.
func (s *warmStore) Usage() (int64, error) {
	entries, err := s.scan()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		total += e.bytes
	}
	return total, nil
}

// scan lists warm bundles with their footprint and last use.
//
// A missing directory is not an error: a node that has never warmed anything has no
// store. Temporaries are skipped -- a partial bundle is not a candidate for eviction
// because it is not yet serving anything, and Clean owns them.
func (s *warmStore) scan() ([]warmEntryInfo, error) {
	dirents, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fc: read warm dir: %w", err)
	}
	var out []warmEntryInfo
	for _, d := range dirents {
		if d.IsDir() || filepath.Ext(d.Name()) != warmSuffix {
			continue
		}
		info, err := d.Info()
		if err != nil {
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		out = append(out, warmEntryInfo{
			name: d.Name(),
			// st_blocks is in 512-byte units regardless of the filesystem's block
			// size, which is the one thing about this field that is portable.
			bytes:    st.Blocks * 512,
			lastUsed: info.ModTime().Unix(),
		})
	}
	return out, nil
}

// Touch records that a bundle was just used, so eviction can order by last use.
//
// Failure is ignored on purpose. A bundle whose mtime could not be updated is
// mis-ordered for eviction, which is a worse eviction choice and not a broken
// restore; failing the create over it would turn a bookkeeping problem into an
// outage. The one that matters is being unable to update it *at all*, which shows up
// as the whole store looking equally cold and evicting arbitrarily -- visible in the
// eviction log rather than silently.
func (s *warmStore) Touch(k warmKey) {
	now := time.Now()
	_ = os.Chtimes(s.Path(k), now, now)
}

// Sweep evicts least-recently-used bundles until the store is under policy.LowBytes.
//
// Called after a bundle is added rather than before, and after a hit, so the store is
// bounded shortly after crossing the high mark rather than being checked on a path
// that would then have to fail. A prewarm that pushes the store over its bound is not
// refused: it evicts something colder and keeps going, because refusing would mean
// the first N images a node ever prewarms are the only ones it can ever have.
//
// inUse names bundles a restore is currently reading, which must not be removed: on
// Linux the unlink would succeed and the reader would keep its open file, so the
// restore would work and the bundle would vanish -- leaving the node quietly cold for
// that image with nothing reporting why.
//
// keep names one bundle that must survive regardless, and is how the caller protects
// the entry it has just written. Without it a sweep can evict what it was just asked
// to create, which is not a corner case: it happens whenever the low mark is below a
// single bundle's size. Measured on a node with a 10 MiB low mark and 15 MB bundles,
// a prewarm stored its snapshot and the sweep immediately removed it along with
// everything else, leaving an empty store and a prewarm that logged success.
func (s *warmStore) Sweep(policy EvictionPolicy, inUse func(name string) bool,
	keep string) (freed int64, evicted int, err error) {

	if !policy.Enabled() {
		return 0, 0, nil
	}
	entries, err := s.scan()
	if err != nil {
		return 0, 0, err
	}
	var total int64
	for _, e := range entries {
		total += e.bytes
	}
	if total < policy.HighBytes {
		return 0, 0, nil
	}

	// Coldest first. Ties broken by name so a sweep is deterministic, which matters
	// only for tests but costs nothing.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].lastUsed != entries[j].lastUsed {
			return entries[i].lastUsed < entries[j].lastUsed
		}
		return entries[i].name < entries[j].name
	})

	for _, e := range entries {
		if total <= policy.LowBytes {
			break
		}
		if e.name == keep {
			continue
		}
		if inUse != nil && inUse(e.name) {
			continue
		}
		if rmErr := os.Remove(filepath.Join(s.dir, e.name)); rmErr != nil {
			if os.IsNotExist(rmErr) {
				// Another sweep got it. Its bytes are gone either way.
				total -= e.bytes
				freed += e.bytes
				continue
			}
			// Reported rather than fatal: one undeletable bundle should not stop the
			// sweep from reclaiming the rest, and the alternative is a store that
			// stays over its bound because of a single file.
			err = fmt.Errorf("fc: evict warm bundle %s: %w", e.name, rmErr)
			continue
		}
		total -= e.bytes
		freed += e.bytes
		evicted++
	}
	return freed, evicted, err
}

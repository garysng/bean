//go:build linux

package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
)

// The unpacked snapshot cache grows once per distinct snapshot restored on a
// node and, until this file existed, never shrank: a measured 4.6 GB across nine
// entries, each roughly one guest's memory. That growth is worse than it sounds
// because the cache is invisible to the scheduler — it consumes no commitment,
// so a node can fill its disk while placement still believes it has room.
//
// The cache itself is worth keeping. Merging a diff chain happens once per node
// per leaf and every later restore of that leaf skips it, which is exactly the
// fan-out shape this platform is for. So the fix is a bound, not a removal.
//
// The watermark pair is borrowed from kubelet's image garbage collection: it
// reclaims at a high mark down to a low one rather than reclaiming at a single
// threshold, because a single threshold makes every subsequent admission pay for
// one eviction. The gap is what turns eviction into an occasional batch.

// cacheEntryInfo is one cached snapshot's footprint and last use.
type cacheEntryInfo struct {
	id string
	// bytes is allocated blocks rather than apparent size. A merged memory image
	// can hold holes where no ancestor wrote, and charging the cache for bytes
	// the filesystem never allocated would evict entries to reclaim nothing.
	bytes int64
	// lastUsed comes from the entry directory's mtime, which Touch sets on every
	// cache hit. Access times are not usable for this: relatime only updates
	// atime once a day for a file read repeatedly, which would order a hot entry
	// as though it were cold.
	lastUsed int64
}

// Usage reports the cache's total allocated size, for the heartbeat. A cache the
// scheduler cannot see is a cache that fills the disk behind its back.
func (c *snapCache) Usage() (int64, error) {
	entries, err := c.scan()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		total += e.bytes
	}
	return total, nil
}

// scan lists cached entries with their footprint. A missing cache directory is
// not an error: a node that has never restored anything has no cache.
func (c *snapCache) scan() ([]cacheEntryInfo, error) {
	dirents, err := os.ReadDir(c.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("fc: read snapshot cache: %w", err)
	}

	var out []cacheEntryInfo
	for _, d := range dirents {
		if !d.IsDir() || d.Name()[0] == '.' {
			// A dot-prefixed directory is an unpack in progress. It is not a cache
			// entry yet — unpackInto publishes by rename — and removing one would
			// destroy a restore that is still running.
			continue
		}
		path := filepath.Join(c.dir, d.Name())
		st, err := os.Stat(path)
		if err != nil {
			// Raced with another sweep or a failed unpack's cleanup. Nothing to
			// reclaim and nothing to report.
			continue
		}
		size, err := dirAllocated(path)
		if err != nil {
			continue
		}
		out = append(out, cacheEntryInfo{
			id: d.Name(), bytes: size, lastUsed: st.ModTime().UnixNano(),
		})
	}
	return out, nil
}

// dirAllocated sums the blocks allocated to an entry's files.
func dirAllocated(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if st, ok := info.Sys().(*syscall.Stat_t); ok {
			// st_blocks is always in 512-byte units regardless of the
			// filesystem's block size.
			total += st.Blocks * 512
			return nil
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// Touch records a cache hit, so eviction can tell a hot entry from a cold one.
//
// Failure is ignored by callers: an un-touched entry is ordered as though it were
// older than it is, which costs a re-unpack at worst. Failing a restore over it
// would be the wrong trade.
func (c *snapCache) Touch(id string) error {
	return os.Chtimes(filepath.Join(c.dir, id), timeNow(), timeNow())
}

// Pin marks an entry as in use, so a sweep cannot remove it.
//
// The window this closes is narrow but real: a restore looks the entry up, then
// opens its memory image a moment later. Unlinking between those two points
// turns the open into ENOENT, and by then the restore's stream is consumed —
// there is nothing left to rebuild from, so the restore fails outright.
//
// Once the image is open a sweep is harmless: an unlinked file's inode lives
// until the last mapping goes away, verified by mmapping a file, unlinking it and
// reading every byte back. So the pin only has to span lookup-to-open, not the
// VM's lifetime.
func (c *snapCache) Pin(id string) func() {
	if id == "" {
		return func() {}
	}
	c.mu.Lock()
	if c.pins == nil {
		c.pins = map[string]int{}
	}
	c.pins[id]++
	c.mu.Unlock()

	var once bool
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if once {
			return
		}
		once = true
		if c.pins[id] <= 1 {
			delete(c.pins, id)
			return
		}
		c.pins[id]--
	}
}

// Evict reclaims least-recently-used entries once the cache is over its high
// watermark, down to the low one. It returns the number of bytes reclaimed.
//
// Entries that are pinned, or that have an unpack in flight, are skipped: both
// mean a restore is depending on them right now. Skipping can leave the cache
// above the low mark, which is correct — the alternative is breaking a running
// restore to hit a number.
func (c *snapCache) Evict(policy EvictionPolicy) (int64, error) {
	if !policy.Enabled() {
		return 0, nil
	}
	entries, err := c.scan()
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		total += e.bytes
	}
	if total < policy.HighBytes {
		return 0, nil
	}

	// Oldest first. Ties broken by id so a sweep is deterministic, which is what
	// makes the behaviour testable.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].lastUsed != entries[j].lastUsed {
			return entries[i].lastUsed < entries[j].lastUsed
		}
		return entries[i].id < entries[j].id
	})

	var freed int64
	for _, e := range entries {
		if total-freed <= policy.LowBytes {
			break
		}
		if c.inUse(e.id) {
			continue
		}
		if err := c.remove(e.id); err != nil {
			continue
		}
		freed += e.bytes
	}
	return freed, nil
}

// inUse reports whether a restore is currently depending on an entry.
func (c *snapCache) inUse(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pins[id] > 0 {
		return true
	}
	_, building := c.pending[id]
	return building
}

// remove deletes an entry, renaming it out of the way first.
//
// The rename is what makes removal atomic from a concurrent Lookup's point of
// view: a directory tree deleted in place passes through a state where the
// machine state is gone but the memory image is not, and a Lookup landing there
// would report the entry present while it is half destroyed.
func (c *snapCache) remove(id string) error {
	live := filepath.Join(c.dir, id)
	doomed, err := os.MkdirTemp(c.dir, ".evict-*")
	if err != nil {
		return err
	}
	dest := filepath.Join(doomed, id)
	if err := os.Rename(live, dest); err != nil {
		os.RemoveAll(doomed)
		return err
	}
	return os.RemoveAll(doomed)
}

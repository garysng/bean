package node

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// A node's memory commitment ledger — what the scheduler summed from sandbox
// requests — is not what the node is actually using. A sandbox asks for 2 GiB and
// touches 200 MiB; the base image page cache, the agent, buildkit, and noded
// itself all consume RAM the ledger never counted. So a node can look
// under-committed to the scheduler while real memory is nearly gone, and the next
// create pushes it into reclaim and then the OOM killer — which does not
// distinguish a new sandbox from a running one, so admitting under real pressure
// risks the work already on the node.
//
// This mirrors DiskGuard (diskguard.go): the promise is the thing shown not to
// correspond to reality, so measure the machine instead. The defence is upstream —
// stop admitting new sandboxes while real memory is tight — because once the OOM
// killer runs the loss has already happened and there is nothing to recover.

// MemGuard refuses new sandboxes while real memory usage is above a ceiling.
//
// It reads MemAvailable from the kernel rather than summing what sandboxes were
// promised, so everything sharing the host — page cache that cannot be reclaimed,
// the agent, buildkit — is counted automatically.
type MemGuard struct {
	// MaxUsedPercent is the ceiling: a create is refused when real memory usage is
	// at or above this. Zero disables the guard, which is the historical behaviour —
	// a node that has never been near full has no reason to think about this.
	MaxUsedPercent float64
	// Path is the meminfo file to read. Empty means /proc/meminfo; overridable so
	// the admission logic is testable without a real kernel.
	Path string
}

// Validate rejects a ceiling that cannot hold.
func (g MemGuard) Validate() error {
	if g.MaxUsedPercent < 0 || g.MaxUsedPercent >= 100 {
		return fmt.Errorf("maximum memory used percent must be in [0,100), got %g",
			g.MaxUsedPercent)
	}
	return nil
}

// Enabled reports whether the guard will refuse anything.
func (g MemGuard) Enabled() bool {
	return g.MaxUsedPercent > 0
}

func (g MemGuard) path() string {
	if g.Path != "" {
		return g.Path
	}
	return "/proc/meminfo"
}

// MemStats is the host's real memory occupancy, in bytes.
type MemStats struct {
	TotalBytes int64
	// AvailableBytes is the kernel's MemAvailable: an estimate of how much can be
	// handed out without swapping, counting reclaimable page cache as available.
	// This is the right figure rather than MemFree, which excludes cache the kernel
	// would happily drop and so understates what a new sandbox can use.
	AvailableBytes int64
}

// UsedPercent is the fraction of total memory that is not available.
func (s MemStats) UsedPercent() float64 {
	if s.TotalBytes <= 0 {
		return 0
	}
	return float64(s.TotalBytes-s.AvailableBytes) / float64(s.TotalBytes) * 100
}

// Stat reads MemTotal and MemAvailable from meminfo.
//
// meminfo reports kibibytes ("MemTotal: 16384000 kB"); the values are scaled to
// bytes so MemStats matches DiskStats' units.
func (g MemGuard) Stat() (MemStats, error) {
	f, err := os.Open(g.path())
	if err != nil {
		return MemStats{}, fmt.Errorf("open %s: %w", g.path(), err)
	}
	defer f.Close()

	var total, avail int64
	haveTotal, haveAvail := false, false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total, err = meminfoKiBToBytes(line)
			haveTotal = true
		case strings.HasPrefix(line, "MemAvailable:"):
			avail, err = meminfoKiBToBytes(line)
			haveAvail = true
		}
		if err != nil {
			return MemStats{}, err
		}
		if haveTotal && haveAvail {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return MemStats{}, fmt.Errorf("read %s: %w", g.path(), err)
	}
	// MemAvailable has been in the kernel since 3.14; a node old enough to lack it
	// cannot run firecracker anyway. Treat its absence as a parse error rather than
	// silently substituting MemFree, which would report a tighter node than reality
	// and refuse creates a healthy host should accept.
	if !haveTotal || !haveAvail {
		return MemStats{}, fmt.Errorf("%s missing MemTotal/MemAvailable", g.path())
	}
	return MemStats{TotalBytes: total, AvailableBytes: avail}, nil
}

// meminfoKiBToBytes parses a "Key: 12345 kB" meminfo line into bytes.
func meminfoKiBToBytes(line string) (int64, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed meminfo line %q", line)
	}
	kib, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse meminfo line %q: %w", line, err)
	}
	return kib * 1024, nil
}

// ErrMemPressure is returned when a node declines work to protect the sandboxes
// it already has.
type ErrMemPressure struct {
	UsedPercent float64
	MaxPercent  float64
}

func (e *ErrMemPressure) Error() string {
	return fmt.Sprintf("node is low on memory: %.1f%% used, at or above the %.1f%% "+
		"ceiling. Admitting a sandbox here risks the OOM killer reaping the sandboxes "+
		"already running", e.UsedPercent, e.MaxPercent)
}

// Admit reports whether a new sandbox may be created.
//
// A failed measurement admits rather than refuses, exactly as DiskGuard does: the
// guard is a safety margin on top of the scheduler's accounting, and letting an
// unreadable meminfo stop a node from doing any work would turn a monitoring
// problem into an outage.
func (g MemGuard) Admit() error {
	if !g.Enabled() {
		return nil
	}
	stats, err := g.Stat()
	if err != nil {
		return nil
	}
	used := stats.UsedPercent()
	if used < g.MaxUsedPercent {
		return nil
	}
	return &ErrMemPressure{UsedPercent: used, MaxPercent: g.MaxUsedPercent}
}

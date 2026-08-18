package node

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// The scheduler scores a node's REAL load as a soft preference, so the node has
// to measure and report it. Disk it already measures (DiskGuard.Stat) and memory
// it can (MemGuard.Stat); CPU has no existing source, which is what this file
// adds.
//
// CPU utilisation is not a snapshot — /proc/stat reports cumulative jiffies since
// boot, so a single read says nothing. Utilisation over an interval is the
// fraction of jiffies in that interval that were not idle, which needs two reads.
// cpuSampler holds the previous read and returns the busy fraction since it; the
// first call has no prior sample and reports 0.

// cpuSampler computes CPU utilisation from successive /proc/stat reads.
//
// Not safe for concurrent use by construction — it is called only from the single
// status-report loop. A mutex guards it anyway so a future second caller cannot
// silently corrupt the stored sample and skew every subsequent reading.
type cpuSampler struct {
	// path is the stat file, /proc/stat in production and overridable in tests.
	path string

	mu       sync.Mutex
	haveLast bool
	lastIdle uint64
	lastAll  uint64
}

func newCPUSampler(path string) *cpuSampler {
	if path == "" {
		path = "/proc/stat"
	}
	return &cpuSampler{path: path}
}

// Percent returns CPU utilisation since the previous call, in [0,100].
//
// The first call establishes the baseline and returns 0: there is no interval to
// average over yet. A read error also returns 0 — like the guards, a measurement
// gap must not be mistaken for a specific load, and the scheduler treats the
// figure as advisory, so 0 reads as "not reported".
func (s *cpuSampler) Percent() float64 {
	idle, all, err := readProcStatCPU(s.path)
	if err != nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.haveLast {
		s.haveLast, s.lastIdle, s.lastAll = true, idle, all
		return 0
	}
	prevIdle, prevAll := s.lastIdle, s.lastAll
	s.lastIdle, s.lastAll = idle, all
	// A backward or empty interval — two reads within the same jiffy, or counters
	// that reset — has no meaningful ratio. Checked before subtracting because the
	// jiffy fields are unsigned and would wrap. Report 0 rather than divide by it.
	if all <= prevAll || idle < prevIdle {
		return 0
	}
	dAll := all - prevAll
	dIdle := idle - prevIdle
	busy := float64(dAll-dIdle) / float64(dAll) * 100
	if busy < 0 {
		busy = 0
	}
	if busy > 100 {
		busy = 100
	}
	return busy
}

// readProcStatCPU parses the aggregate "cpu" line of /proc/stat into (idle jiffies,
// total jiffies). The line is:
//
//	cpu  user nice system idle iowait irq softirq steal guest guest_nice
//
// idle is idle+iowait (iowait is time the CPU was idle waiting on IO); total is
// the sum of every field. guest and guest_nice are already counted inside user
// and nice, so they are not re-added.
func readProcStatCPU(path string) (idle, total uint64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)[1:] // drop the "cpu" label
		var sum uint64
		for i, f := range fields {
			// guest (8) and guest_nice (9) are already included in user/nice.
			if i >= 8 {
				break
			}
			v, perr := strconv.ParseUint(f, 10, 64)
			if perr != nil {
				return 0, 0, fmt.Errorf("parse %s field %d %q: %w", path, i, f, perr)
			}
			sum += v
			// idle is field 3; iowait is field 4.
			if i == 3 || i == 4 {
				idle += v
			}
		}
		return idle, sum, nil
	}
	if err := sc.Err(); err != nil {
		return 0, 0, fmt.Errorf("read %s: %w", path, err)
	}
	return 0, 0, fmt.Errorf("%s has no aggregate cpu line", path)
}

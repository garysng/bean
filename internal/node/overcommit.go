package node

import (
	"fmt"
	"math"
)

// Overcommit scales what a node reports as allocatable above what it physically
// has.
//
// Evaluation workloads are bursty: a sandbox spends most of its life waiting on
// IO or idle, so admitting only as much as the hardware nominally holds leaves
// the node mostly empty. Reporting a multiple of it lets more work in.
//
// The factor is applied here, on the node, rather than in the scheduler. Two
// reasons: the right value depends on what the node is (a CPU-bound pool wants
// 1.0 while a general pool wants more), and the scheduler already treats the
// reported figure as final — `NodeRecord.CPUAllocatable` is documented as
// already including it, so there is exactly one place that decides.
type Overcommit struct {
	// CPU multiplies allocatable CPU. Oversubscribing CPU degrades gracefully:
	// the kernel time-slices, so the cost of being wrong is slower sandboxes.
	CPU float64
	// Memory multiplies allocatable memory. Oversubscribing memory does not
	// degrade gracefully — the cost of being wrong is a killed process — so this
	// defaults to 1.0 even though the microVM tier has real headroom here:
	// Firecracker faults guest pages in on demand, so a guest's actual footprint
	// is well below what it declares.
	//
	// That headroom is not yet measured. The other prerequisite -- a cgroup around
	// the VMM processes, so that the host has a kernel-enforced ceiling per sandbox
	// rather than only the scheduler's ledger -- now exists, but it is off unless
	// the node runs with --fc-cgroups. On a node without it nothing has changed and
	// this must stay at 1.0.
	Memory float64
}

// DefaultOvercommit is one-to-one accounting: what the node reports is what it
// was configured with. Bursty workloads leave capacity on the table under this,
// but nothing is oversold.
func DefaultOvercommit() Overcommit {
	return Overcommit{CPU: 1.0, Memory: 1.0}
}

// Validate rejects factors that would misreport capacity.
//
// Below 1.0 is refused rather than clamped: it is a plausible thing to type when
// the intent was to hold back capacity, and silently accepting it would report
// less than the node has with nothing to explain why. Reserving capacity is a
// different operation, and should look like one.
func (o Overcommit) Validate() error {
	for _, f := range []struct {
		name  string
		value float64
	}{{"cpu", o.CPU}, {"memory", o.Memory}} {
		switch {
		case math.IsNaN(f.value) || math.IsInf(f.value, 0):
			return fmt.Errorf("overcommit %s must be a finite number", f.name)
		case f.value < 1.0:
			return fmt.Errorf("overcommit %s is %g: below 1.0 would report less "+
				"capacity than the node has; to hold capacity back, lower the "+
				"allocatable amount instead", f.name, f.value)
		case f.value > maxOvercommit:
			return fmt.Errorf("overcommit %s is %g, above the %g ceiling",
				f.name, f.value, maxOvercommit)
		}
	}
	return nil
}

// maxOvercommit caps the factor. There is no principled value here, so it is set
// where a typo becomes obvious: a slipped decimal point turning 3.0 into 30
// would otherwise admit ten times the work and surface as unexplained timeouts
// rather than as a configuration error.
const maxOvercommit = 20.0

// ApplyCPU scales an allocatable CPU count.
func (o Overcommit) ApplyCPU(cpu float64) float64 {
	if o.CPU <= 0 {
		return cpu
	}
	return cpu * o.CPU
}

// ApplyMemory scales an allocatable memory figure.
//
// The result is truncated rather than rounded, so the reported figure is never
// above what the factor allows.
func (o Overcommit) ApplyMemory(mib int64) int64 {
	if o.Memory <= 0 {
		return mib
	}
	return int64(float64(mib) * o.Memory)
}

// Enabled reports whether either dimension is oversubscribed, which is worth
// stating at startup: it changes what a node admits, and a node that quietly
// accepts three times the work is hard to explain after the fact.
func (o Overcommit) Enabled() bool {
	return o.CPU > 1.0 || o.Memory > 1.0
}

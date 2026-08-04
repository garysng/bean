package scheduler

import (
	"fmt"
	"sort"
	"strings"

	"github.com/garysng/bean/internal/control/store"
)

// A rejection that says only "no node fits" is close to useless when several
// resources can each be the binding one, because they masquerade as each other.
// Measured on one node, the same 30-concurrent burst was capped at 5, 8 and 16
// sandboxes depending on configuration — disk, then CPU, then create concurrency —
// and the reported error was identical every time. The 16 was mistaken for the
// create limit for exactly that reason; it was the core count.
//
// So a rejection names the resource that ran out and how far short it was. The
// point is not politeness: raising the wrong limit is the most likely response to
// an unattributed capacity error, and it does nothing.

// constraint identifies one reason a node was not feasible.
type constraint string

const (
	constraintCPU       constraint = "cpu"
	constraintMemory    constraint = "memory"
	constraintDisk      constraint = "disk"
	constraintGPU       constraint = "gpu"
	constraintCreates   constraint = "createConcurrency"
	constraintRuntime   constraint = "runtime"
	constraintLabels    constraint = "nodeSelector"
	constraintNodeState constraint = "nodeState"
	constraintCPUCompat constraint = "cpuCompatibility"
)

// blockers lists every reason a node was rejected, not just the first.
//
// Every reason matters because a node short on two resources is not fixed by
// raising one. Reporting only the first would send an operator round the loop
// twice.
func blockers(n *store.NodeRecord, req *Request) []constraint {
	var out []constraint
	if n.State != NodeReady {
		out = append(out, constraintNodeState)
	}
	if req.Runtime != "" && !hasRuntime(n.Runtimes, req.Runtime) {
		out = append(out, constraintRuntime)
	}
	if !labelsMatch(n.Labels, req.NodeSelector) {
		out = append(out, constraintLabels)
	}
	// Create concurrency is not a blocker any more: nothing refuses on it, in the
	// filter or at commit, so naming it here would attribute a rejection to a
	// constraint that is not binding -- and this function exists precisely because an
	// unattributed capacity error sends an operator to raise the wrong limit.
	//
	// It is still counted and still scored (Weights.CreatePressure), so a busy node
	// loses to a quiet one; it just never makes a node unusable.
	if req.CPUConstraint.CheckCPU(n.CPUVendor, n.CPUFamily) != nil {
		out = append(out, constraintCPUCompat)
	}
	if n.CPUCommitted+req.CPU > n.CPUAllocatable {
		out = append(out, constraintCPU)
	}
	if n.MemoryCommitMiB+req.MemoryMiB > n.MemoryAllocateMiB {
		out = append(out, constraintMemory)
	}
	if n.DiskCommitMiB+req.DiskMiB > n.DiskAllocateMiB {
		out = append(out, constraintDisk)
	}
	if n.GPUCommitted+req.GPU > n.GPUCount {
		out = append(out, constraintGPU)
	}
	return out
}

// explainRejection names why no node fit, counting how many nodes each resource
// blocked.
//
// Nodes in another region are excluded before counting: they were never
// candidates, and including them would report the whole fleet's shape rather than
// the shape of the region the request asked for.
func explainRejection(nodes []*store.NodeRecord, req *Request) string {
	counts := map[constraint]int{}
	// Worst-case headroom per resource, so the message can say how far short the
	// closest node was rather than only that it did not fit.
	shortfall := map[constraint]int64{}
	candidates := 0

	for _, n := range nodes {
		if n.Region != req.Region {
			continue
		}
		candidates++
		reasons := blockers(n, req)
		for _, r := range reasons {
			counts[r]++
		}
		record := func(r constraint, need int64) {
			if have, ok := shortfall[r]; !ok || need < have {
				shortfall[r] = need
			}
		}
		for _, r := range reasons {
			switch r {
			case constraintCPU:
				// Rounded up: a request for 0.5 vCPU short by 0.2 reads better as
				// "1" than as "0", and the exact fraction does not change the action.
				record(r, int64(n.CPUCommitted+req.CPU-n.CPUAllocatable+0.999))
			case constraintMemory:
				record(r, n.MemoryCommitMiB+req.MemoryMiB-n.MemoryAllocateMiB)
			case constraintDisk:
				record(r, n.DiskCommitMiB+req.DiskMiB-n.DiskAllocateMiB)
			}
		}
	}

	if candidates == 0 {
		return fmt.Sprintf("no node is registered in region %s", req.Region)
	}

	// Ranked by how many nodes each blocked: the resource blocking the most is the
	// one worth raising first.
	type ranked struct {
		c constraint
		n int
	}
	var order []ranked
	for c, n := range counts {
		order = append(order, ranked{c, n})
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].n != order[j].n {
			return order[i].n > order[j].n
		}
		return order[i].c < order[j].c
	})

	var parts []string
	for _, r := range order {
		part := fmt.Sprintf("%s blocked %d/%d", r.c, r.n, candidates)
		if short, ok := shortfall[r.c]; ok && short > 0 {
			switch r.c {
			case constraintCPU:
				part += fmt.Sprintf(" (short by %d vCPU on the closest node)", short)
			case constraintMemory, constraintDisk:
				part += fmt.Sprintf(" (short by %d MiB on the closest node)", short)
			}
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, "; ")
}

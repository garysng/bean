// Package scheduler picks a node for each sandbox. It is a control-plane
// module (not a separate service) so placement decisions and their resource
// accounting happen together — see docs/architecture.md D7.
//
// Accounting is durable, not in-memory. The scheduler scores candidates
// from a snapshot of node state, then asks the store to commit the
// reservation conditionally; the database rejects a commit that would
// oversell. This is what makes more than one gateway replica safe: two
// replicas can score the same node, but only one commit succeeds and the
// loser re-scores. It also means a replica restart loses nothing, because
// the reservations are in the database rather than in a process.
package scheduler

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/garysng/bean/internal/control/store"
)

// ErrNoCapacity reports that no node can host the request.
var ErrNoCapacity = errors.New("no capacity")

// ErrIncompatibleCPU reports that a restore was refused because no node has a
// CPU the snapshot's guest memory can run on.
//
// It is distinct from ErrNoCapacity because the remedies have nothing in common:
// capacity comes back on its own, while this needs either a node of the right
// kind or a snapshot taken differently. Collapsing them would send the reader to
// look at resource limits.
var ErrIncompatibleCPU = errors.New("incompatible cpu")

// Node liveness states. A node is only a placement candidate while READY.
const (
	NodeReady    = "READY"
	NodeSuspect  = "SUSPECT"
	NodeLost     = "LOST"
	NodeDraining = "DRAINING"
)

// Request is a placement request derived from a sandbox spec.
type Request struct {
	SandboxID string
	// Region is required: sandboxes never cross regions, because their
	// data (images, snapshots, volumes) is region-local.
	Region       string
	Image        string
	CPU          float64
	MemoryMiB    int64
	DiskMiB      int64
	GPU          int32
	Runtime      string
	NodeSelector map[string]string
	// SpreadKey groups related sandboxes (an eval run, say) so one node
	// failure cannot swallow the whole group.
	SpreadKey string
	// CPU restricts placement to nodes that can restore a memory snapshot. It
	// is set only for restores; a fresh sandbox has no guest memory to carry
	// and so is unconstrained.
	CPUConstraint CPUConstraint
}

// Weights tune scoring. They are configurable so the balance can be tuned
// from observed behaviour without a code change.
type Weights struct {
	ImageAffinity float64
	Packing       float64
	NVMeCache     float64
	Spread        float64
}

func DefaultWeights() Weights {
	return Weights{ImageAffinity: 10, Packing: 3, NVMeCache: 2, Spread: 4}
}

// Scheduler makes placement decisions against durable node state.
type Scheduler struct {
	store   *store.Store
	weights Weights

	suspectAfter time.Duration
	lostAfter    time.Duration
	now          func() time.Time

	// maxAttempts bounds re-scoring when replicas contend for the same
	// node. Each attempt re-reads state, so a bounded retry converges.
	maxAttempts int
}

func New(st *store.Store, w Weights) *Scheduler {
	return &Scheduler{
		store:        st,
		weights:      w,
		suspectAfter: 15 * time.Second,
		lostAfter:    45 * time.Second,
		now:          time.Now,
		maxAttempts:  5,
	}
}

// SetClock overrides the time source (tests).
func (s *Scheduler) SetClock(f func() time.Time) { s.now = f }

// SetLeaseTimeouts overrides liveness thresholds.
func (s *Scheduler) SetLeaseTimeouts(suspect, lost time.Duration) {
	s.suspectAfter, s.lostAfter = suspect, lost
}

// Schedule picks a node and durably reserves its resources. On success the
// caller owns a reservation that must be released with Release when the
// sandbox stops, and FinishCreate once the create call settles.
func (s *Scheduler) Schedule(req *Request) (string, error) {
	if req.Region == "" {
		return "", errors.New("region is required")
	}
	var lastErr error
	for attempt := 0; attempt < s.maxAttempts; attempt++ {
		nodes, err := s.store.LoadNodes()
		if err != nil {
			return "", fmt.Errorf("load nodes: %w", err)
		}
		spread, err := s.store.SpreadCounts(req.SpreadKey)
		if err != nil {
			return "", fmt.Errorf("spread counts: %w", err)
		}

		ranked := s.rank(nodes, spread, req)
		if len(ranked) == 0 {
			// A CPU mismatch would otherwise be reported as missing capacity,
			// which sends the reader looking at resource limits for a problem
			// that no amount of free capacity can fix.
			if why := s.cpuRejection(nodes, req); why != "" {
				return "", fmt.Errorf("%w: cannot restore %s: %s",
					ErrIncompatibleCPU, req.SandboxID, why)
			}
			// Naming the resource that ran out, because several can each be the
			// binding one and they are indistinguishable from the outside: the same
			// burst was capped at 5, 8 and 16 by disk, CPU and create concurrency in
			// turn, reporting the same error each time. Raising the wrong limit is
			// the natural response to an unattributed capacity error.
			return "", fmt.Errorf("%w: no node in region %s fits %s "+
				"(cpu=%.1f mem=%dMiB disk=%dMiB gpu=%d runtime=%s): %s",
				ErrNoCapacity, req.Region, req.SandboxID,
				req.CPU, req.MemoryMiB, req.DiskMiB, req.GPU, req.Runtime,
				explainRejection(nodes, req))
		}

		// Try candidates best-first. A node that just filled up is skipped
		// rather than failing the whole request.
		for _, n := range ranked {
			err := s.store.Reserve(n.ID, &store.Reservation{
				SandboxID: req.SandboxID,
				CPU:       req.CPU,
				MemoryMiB: req.MemoryMiB,
				DiskMiB:   req.DiskMiB,
				GPU:       req.GPU,
				SpreadKey: req.SpreadKey,
			})
			if err == nil {
				return n.ID, nil
			}
			if !errors.Is(err, store.ErrCapacityChanged) {
				return "", fmt.Errorf("reserve on %s: %w", n.ID, err)
			}
			lastErr = err
		}
		// Every candidate was taken; re-read and score again.
	}
	return "", fmt.Errorf("%w: capacity contended after %d attempts (%v)",
		ErrNoCapacity, s.maxAttempts, lastErr)
}

// ScheduleBatch places many requests, which is the hot path for evaluation
// bursts. Results are index-aligned with reqs.
func (s *Scheduler) ScheduleBatch(reqs []*Request) ([]string, []error) {
	nodes := make([]string, len(reqs))
	errs := make([]error, len(reqs))
	for i, r := range reqs {
		nodes[i], errs[i] = s.Schedule(r)
	}
	return nodes, errs
}

// Release returns a sandbox's reserved capacity. Idempotent.
func (s *Scheduler) Release(sandboxID string) error {
	return s.store.Release(sandboxID)
}

// FinishCreate clears the in-flight marker once a create settles.
func (s *Scheduler) FinishCreate(nodeID string) error {
	return s.store.FinishCreate(nodeID)
}

// candidate pairs a node with its score.
type candidate struct {
	*store.NodeRecord
	score float64
}

// rank filters infeasible nodes and returns the rest best-first.
func (s *Scheduler) rank(nodes []*store.NodeRecord, spread map[string]int,
	req *Request) []*store.NodeRecord {
	var cands []candidate
	for _, n := range nodes {
		if !s.feasible(n, req) {
			continue
		}
		cands = append(cands, candidate{NodeRecord: n, score: s.score(n, spread, req)})
	}
	// Sort by score, then by id so identical nodes produce a stable choice
	// and batch placement is reproducible.
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return cands[i].ID < cands[j].ID
	})
	out := make([]*store.NodeRecord, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.NodeRecord)
	}
	return out
}

// feasible applies the hard constraints. The database re-checks capacity at
// commit time; this pre-filter avoids obviously futile attempts.
func (s *Scheduler) feasible(n *store.NodeRecord, req *Request) bool {
	if n.State != NodeReady || n.Region != req.Region {
		return false
	}
	if req.Runtime != "" && !hasRuntime(n.Runtimes, req.Runtime) {
		return false
	}
	if !labelsMatch(n.Labels, req.NodeSelector) {
		return false
	}
	if n.MaxCreates > 0 && n.CreateInFlight >= n.MaxCreates {
		return false
	}
	// Restoring guest memory onto an incompatible CPU produces a sandbox that
	// resumes and then misbehaves, so this belongs in the hard filter rather
	// than in scoring.
	if req.CPUConstraint.CheckCPU(n.CPUVendor, n.CPUFamily) != nil {
		return false
	}
	return n.CPUCommitted+req.CPU <= n.CPUAllocatable &&
		n.MemoryCommitMiB+req.MemoryMiB <= n.MemoryAllocateMiB &&
		n.DiskCommitMiB+req.DiskMiB <= n.DiskAllocateMiB &&
		n.GPUCommitted+req.GPU <= n.GPUCount
}

// cpuRejection explains a CPU mismatch when that is why nothing was feasible.
//
// It returns "" unless the CPU constraint is the deciding factor: a node that
// was also out of capacity would be rejected either way, and blaming the CPU for
// it would be misleading. So this only speaks up when some node in the region
// would have fit apart from its CPU.
func (s *Scheduler) cpuRejection(nodes []*store.NodeRecord, req *Request) string {
	if !req.CPUConstraint.Constrained() {
		return ""
	}
	probe := *req
	probe.CPUConstraint = CPUConstraint{}
	for _, n := range nodes {
		if !s.feasible(n, &probe) {
			continue
		}
		if err := req.CPUConstraint.CheckCPU(n.CPUVendor, n.CPUFamily); err != nil {
			return err.Error()
		}
	}
	return ""
}

// score ranks a feasible node; higher is better.
func (s *Scheduler) score(n *store.NodeRecord, spread map[string]int, req *Request) float64 {
	var score float64

	// Image affinity dominates: a node that already has the image skips the
	// pull entirely, which is the largest term in cold-start latency.
	if bytes, ok := n.CachedImages[req.Image]; ok && bytes > 0 {
		score += s.weights.ImageAffinity
	}

	// Packing prefers the node that ends up most utilised, so large
	// requests still find room elsewhere.
	var cpuFrac, memFrac float64
	if n.CPUAllocatable > 0 {
		cpuFrac = (n.CPUCommitted + req.CPU) / n.CPUAllocatable
	}
	if n.MemoryAllocateMiB > 0 {
		memFrac = float64(n.MemoryCommitMiB+req.MemoryMiB) / float64(n.MemoryAllocateMiB)
	}
	score += s.weights.Packing * (cpuFrac + memFrac) / 2

	// A cold image benefits from a fast local cache disk.
	if n.NVMeCache {
		if _, cached := n.CachedImages[req.Image]; !cached {
			score += s.weights.NVMeCache
		}
	}

	// Spread penalises nodes already holding members of the same group.
	if req.SpreadKey != "" {
		if placed := spread[n.ID]; placed > 0 {
			score -= s.weights.Spread * float64(placed)
		}
	}
	return score
}

// SweepLiveness advances node states from heartbeat age and returns nodes
// that transitioned to LOST. The store reports whether each transition
// actually happened, so with several replicas sweeping concurrently a node
// is reported exactly once and its sandboxes are not marked lost twice.
func (s *Scheduler) SweepLiveness() ([]string, error) {
	now := s.now()

	lostCutoff := now.Add(-s.lostAfter)
	stale, err := s.store.StaleNodes(lostCutoff, NodeDraining, NodeLost)
	if err != nil {
		return nil, err
	}
	var lost []string
	for _, n := range stale {
		changed, err := s.store.SetNodeState(n.ID, NodeLost)
		if err != nil {
			return lost, err
		}
		if changed {
			lost = append(lost, n.ID)
		}
	}

	// Nodes past the suspect threshold but not yet lost stop receiving
	// placements while we wait to see whether they come back.
	suspectCutoff := now.Add(-s.suspectAfter)
	doubtful, err := s.store.StaleNodes(suspectCutoff, NodeDraining, NodeLost, NodeSuspect)
	if err != nil {
		return lost, err
	}
	for _, n := range doubtful {
		if _, err := s.store.SetNodeState(n.ID, NodeSuspect); err != nil {
			return lost, err
		}
	}
	sort.Strings(lost)
	return lost, nil
}

// ReclaimOrphanReservations releases reservations whose sandbox is gone or
// terminal. Without this, a gateway that died mid-create would leak that
// node's capacity permanently.
func (s *Scheduler) ReclaimOrphanReservations() (int, error) {
	orphans, err := s.store.OrphanReservations()
	if err != nil {
		return 0, err
	}
	for _, id := range orphans {
		if err := s.store.Release(id); err != nil {
			return 0, fmt.Errorf("release %s: %w", id, err)
		}
	}
	return len(orphans), nil
}

// Drain stops new placements on a node without disturbing what runs there.
func (s *Scheduler) Drain(nodeID string) error {
	node, err := s.store.GetNode(nodeID)
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("unknown node %s", nodeID)
	}
	_, err = s.store.SetNodeState(nodeID, NodeDraining)
	return err
}

// Nodes returns a snapshot of node state for the ops surface.
func (s *Scheduler) Nodes() ([]*store.NodeRecord, error) {
	return s.store.LoadNodes()
}

func hasRuntime(have []string, want string) bool {
	for _, r := range have {
		if strings.EqualFold(r, want) {
			return true
		}
	}
	return false
}

func labelsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// Utilisation reports a region's committed fraction, for capacity alerts.
func (s *Scheduler) Utilisation(region string) (cpu, memory float64, err error) {
	nodes, err := s.store.LoadNodes()
	if err != nil {
		return 0, 0, err
	}
	var cpuAlloc, cpuUsed, memAlloc, memUsed float64
	for _, n := range nodes {
		if region != "" && n.Region != region {
			continue
		}
		if n.State != NodeReady {
			continue
		}
		cpuAlloc += n.CPUAllocatable
		cpuUsed += n.CPUCommitted
		memAlloc += float64(n.MemoryAllocateMiB)
		memUsed += float64(n.MemoryCommitMiB)
	}
	if cpuAlloc > 0 {
		cpu = cpuUsed / cpuAlloc
	}
	if memAlloc > 0 {
		memory = memUsed / memAlloc
	}
	return cpu, memory, nil
}

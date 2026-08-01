// Package scheduler picks a node for each sandbox. It is a control-plane
// module (not a separate service) so placement decisions, quota accounting
// and command dispatch happen in one process — see docs/architecture.md D7.
package scheduler

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
)

// ErrNoCapacity reports that no node can host the request.
var ErrNoCapacity = errors.New("no capacity")

// NodeState tracks liveness derived from heartbeats.
type NodeState string

const (
	NodeReady    NodeState = "READY"
	NodeSuspect  NodeState = "SUSPECT"
	NodeLost     NodeState = "LOST"
	NodeDraining NodeState = "DRAINING"
)

// Node is the scheduler's view of one node.
type Node struct {
	ID       string
	Region   string
	Labels   map[string]string
	Runtimes []string // capabilities, e.g. ["fc"] or ["fc","runc"]

	// Allocatable capacity (already includes the overcommit factor).
	CPUAllocatable    float64
	MemoryMiBAllocate int64
	DiskMiBAllocate   int64
	GPUCount          int32

	// Committed = sum of specs placed here. Accounting is by commitment,
	// not live usage, so bursty workloads cannot oversell the node.
	CPUCommitted    float64
	MemoryMiBCommit int64
	DiskMiBCommit   int64
	GPUCommitted    int32

	// CachedImages holds image refs with locally cached blocks, and
	// CachedBytes their approximate cached size, for affinity scoring.
	CachedImages map[string]int64

	// NVMeCache reports whether the cache pool is on fast local disk.
	NVMeCache bool

	// CreateInFlight counts concurrent creates; bounded per node so a burst
	// becomes a pipeline instead of a stampede.
	CreateInFlight int
	MaxCreates     int

	State         NodeState
	LastHeartbeat time.Time
}

// Request is a placement request derived from a sandbox spec.
type Request struct {
	SandboxID    string
	Region       string // required: sandboxes never cross regions
	Image        string
	CPU          float64
	MemoryMiB    int64
	DiskMiB      int64
	GPU          int32
	Runtime      string            // required capability, e.g. "fc"
	NodeSelector map[string]string // must match node labels
	// SpreadKey groups related sandboxes (e.g. one eval run) so the
	// scheduler can spread them across nodes.
	SpreadKey string
}

// Weights tune the scoring function. All are configurable so the balance
// can be tuned from metrics without code changes.
type Weights struct {
	ImageAffinity float64
	Packing       float64
	NVMeCache     float64
	Spread        float64
}

func DefaultWeights() Weights {
	return Weights{ImageAffinity: 10, Packing: 3, NVMeCache: 2, Spread: 4}
}

// Scheduler holds node state and makes placement decisions.
type Scheduler struct {
	mu      sync.Mutex
	nodes   map[string]*Node
	weights Weights
	// spread counts placements per (SpreadKey, nodeID) for anti-affinity.
	spread map[string]map[string]int

	// leaseTimeout/suspectTimeout drive heartbeat-based state transitions.
	suspectTimeout time.Duration
	lostTimeout    time.Duration
	now            func() time.Time
}

func New(w Weights) *Scheduler {
	return &Scheduler{
		nodes:          map[string]*Node{},
		weights:        w,
		spread:         map[string]map[string]int{},
		suspectTimeout: 15 * time.Second,
		lostTimeout:    45 * time.Second,
		now:            time.Now,
	}
}

// SetClock overrides the time source (tests).
func (s *Scheduler) SetClock(f func() time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = f
}

// Register adds or replaces a node, preserving committed accounting for
// a node that re-registers (e.g. after a noded restart).
func (s *Scheduler) Register(n *Node) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n.MaxCreates <= 0 {
		n.MaxCreates = 16
	}
	if prev, ok := s.nodes[n.ID]; ok {
		n.CPUCommitted = prev.CPUCommitted
		n.MemoryMiBCommit = prev.MemoryMiBCommit
		n.DiskMiBCommit = prev.DiskMiBCommit
		n.GPUCommitted = prev.GPUCommitted
		n.CreateInFlight = prev.CreateInFlight
	}
	n.State = NodeReady
	n.LastHeartbeat = s.now()
	s.nodes[n.ID] = n
}

// Heartbeat refreshes liveness and optionally the cache manifest.
func (s *Scheduler) Heartbeat(nodeID string, cachedImages map[string]int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[nodeID]
	if !ok {
		return fmt.Errorf("unknown node %s", nodeID)
	}
	n.LastHeartbeat = s.now()
	if n.State == NodeSuspect || n.State == NodeLost {
		n.State = NodeReady
	}
	if cachedImages != nil {
		n.CachedImages = cachedImages
	}
	return nil
}

// Drain stops new placements on a node without disturbing running sandboxes.
func (s *Scheduler) Drain(nodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[nodeID]
	if !ok {
		return fmt.Errorf("unknown node %s", nodeID)
	}
	n.State = NodeDraining
	return nil
}

// SweepLiveness advances node states based on heartbeat age and returns
// nodes that just became LOST (their sandboxes must be marked lost).
func (s *Scheduler) SweepLiveness() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var lost []string
	now := s.now()
	for _, n := range s.nodes {
		if n.State == NodeDraining {
			continue
		}
		age := now.Sub(n.LastHeartbeat)
		switch {
		case age >= s.lostTimeout:
			if n.State != NodeLost {
				n.State = NodeLost
				lost = append(lost, n.ID)
			}
		case age >= s.suspectTimeout:
			if n.State == NodeReady {
				n.State = NodeSuspect
			}
		}
	}
	sort.Strings(lost)
	return lost
}

// Schedule picks a node and commits its resources. Callers must call
// Release (on failure) or Commit-then-Release semantics via ReleaseCreate
// once the create finishes.
func (s *Scheduler) Schedule(req *Request) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scheduleLocked(req)
}

// ScheduleBatch places many requests under a single lock acquisition,
// which is the hot path for eval bursts (docs/security-and-startup B4).
// Results are index-aligned with reqs; err is non-nil per failed item.
func (s *Scheduler) ScheduleBatch(reqs []*Request) ([]string, []error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	nodes := make([]string, len(reqs))
	errs := make([]error, len(reqs))
	for i, r := range reqs {
		nodes[i], errs[i] = s.scheduleLocked(r)
	}
	return nodes, errs
}

func (s *Scheduler) scheduleLocked(req *Request) (string, error) {
	if req.Region == "" {
		return "", errors.New("region is required")
	}
	var best *Node
	bestScore := math.Inf(-1)
	for _, n := range s.candidatesLocked(req) {
		score := s.scoreLocked(n, req)
		// Deterministic tie-break by node ID keeps batch placement stable.
		if score > bestScore || (score == bestScore && best != nil && n.ID < best.ID) {
			best, bestScore = n, score
		}
	}
	if best == nil {
		return "", fmt.Errorf("%w: no node in region %s fits %s (cpu=%.1f mem=%dMiB disk=%dMiB gpu=%d runtime=%s)",
			ErrNoCapacity, req.Region, req.SandboxID, req.CPU, req.MemoryMiB, req.DiskMiB, req.GPU, req.Runtime)
	}
	best.CPUCommitted += req.CPU
	best.MemoryMiBCommit += req.MemoryMiB
	best.DiskMiBCommit += req.DiskMiB
	best.GPUCommitted += req.GPU
	best.CreateInFlight++
	if req.SpreadKey != "" {
		if s.spread[req.SpreadKey] == nil {
			s.spread[req.SpreadKey] = map[string]int{}
		}
		s.spread[req.SpreadKey][best.ID]++
	}
	return best.ID, nil
}

// candidatesLocked applies the hard filters.
func (s *Scheduler) candidatesLocked(req *Request) []*Node {
	var out []*Node
	for _, n := range s.nodes {
		if n.State != NodeReady || n.Region != req.Region {
			continue
		}
		if req.Runtime != "" && !hasRuntime(n.Runtimes, req.Runtime) {
			continue
		}
		if !labelsMatch(n.Labels, req.NodeSelector) {
			continue
		}
		if n.MaxCreates > 0 && n.CreateInFlight >= n.MaxCreates {
			continue
		}
		if n.CPUCommitted+req.CPU > n.CPUAllocatable ||
			n.MemoryMiBCommit+req.MemoryMiB > n.MemoryMiBAllocate ||
			n.DiskMiBCommit+req.DiskMiB > n.DiskMiBAllocate ||
			n.GPUCommitted+req.GPU > n.GPUCount {
			continue
		}
		out = append(out, n)
	}
	return out
}

// scoreLocked ranks a feasible node; higher is better.
func (s *Scheduler) scoreLocked(n *Node, req *Request) float64 {
	var score float64

	// Image affinity: prefer nodes that already cache this image.
	if bytes, ok := n.CachedImages[req.Image]; ok && bytes > 0 {
		score += s.weights.ImageAffinity
	}

	// Packing: prefer the node that ends up most utilised, so large
	// requests still find room elsewhere.
	cpuFrac := 0.0
	if n.CPUAllocatable > 0 {
		cpuFrac = (n.CPUCommitted + req.CPU) / n.CPUAllocatable
	}
	memFrac := 0.0
	if n.MemoryMiBAllocate > 0 {
		memFrac = float64(n.MemoryMiBCommit+req.MemoryMiB) / float64(n.MemoryMiBAllocate)
	}
	score += s.weights.Packing * (cpuFrac + memFrac) / 2

	// Cold images benefit from a fast local cache disk.
	if n.NVMeCache {
		if _, cached := n.CachedImages[req.Image]; !cached {
			score += s.weights.NVMeCache
		}
	}

	// Spread: penalise nodes already holding members of the same group so a
	// single node failure cannot swallow an entire eval run.
	if req.SpreadKey != "" {
		if placed := s.spread[req.SpreadKey][n.ID]; placed > 0 {
			score -= s.weights.Spread * float64(placed)
		}
	}
	return score
}

// ReleaseCreate clears the in-flight marker once a create settles.
func (s *Scheduler) ReleaseCreate(nodeID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.nodes[nodeID]; ok && n.CreateInFlight > 0 {
		n.CreateInFlight--
	}
}

// Release returns committed resources (create failed, or sandbox stopped).
func (s *Scheduler) Release(nodeID string, req *Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n, ok := s.nodes[nodeID]
	if !ok {
		return
	}
	n.CPUCommitted = math.Max(0, n.CPUCommitted-req.CPU)
	n.MemoryMiBCommit = maxInt64(0, n.MemoryMiBCommit-req.MemoryMiB)
	n.DiskMiBCommit = maxInt64(0, n.DiskMiBCommit-req.DiskMiB)
	if n.GPUCommitted >= req.GPU {
		n.GPUCommitted -= req.GPU
	} else {
		n.GPUCommitted = 0
	}
	if req.SpreadKey != "" {
		if m, ok := s.spread[req.SpreadKey]; ok {
			if m[nodeID] > 0 {
				m[nodeID]--
			}
			if m[nodeID] == 0 {
				delete(m, nodeID)
			}
			if len(m) == 0 {
				delete(s.spread, req.SpreadKey)
			}
		}
	}
}

// Nodes returns a snapshot of node state for the ops API.
func (s *Scheduler) Nodes() []Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Node, 0, len(s.nodes))
	for _, n := range s.nodes {
		cp := *n
		cp.Labels = copyMap(n.Labels)
		cp.CachedImages = copyMapInt64(n.CachedImages)
		cp.Runtimes = append([]string(nil), n.Runtimes...)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
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

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func copyMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func copyMapInt64(m map[string]int64) map[string]int64 {
	if m == nil {
		return nil
	}
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

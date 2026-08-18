package api

import (
	"net/http"

	"github.com/garysng/bean/internal/control/store"
)

// Node endpoints are the operational view of the cluster: what capacity
// exists, how much is promised, and which nodes are healthy enough to
// receive work.

// nodeRecord returns one node's record, or nil if it is unknown.
//
// Callers use this for facts about a node that need copying elsewhere, such as
// the CPU a snapshot was taken on. A missing node yields nil rather than an
// error: the operations that need this are not worth failing over a node that
// has since been removed, and each decides for itself what to do without it.
func (s *Server) nodeRecord(nodeID string) *store.NodeRecord {
	if nodeID == "" {
		return nil
	}
	nodes, err := s.placer.Nodes()
	if err != nil {
		return nil
	}
	for _, n := range nodes {
		if n.ID == nodeID {
			return n
		}
	}
	return nil
}

func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.placer.Nodes()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, map[string]any{
			"id": n.ID, "region": n.Region, "state": n.State,
			"runtimes": n.Runtimes, "labels": n.Labels,
			"cpuAllocatable": n.CPUAllocatable, "cpuCommitted": n.CPUCommitted,
			"memoryAllocatableMiB": n.MemoryAllocateMiB,
			"memoryCommittedMiB":   n.MemoryCommitMiB,
			"diskAllocatableMiB":   n.DiskAllocateMiB,
			"diskCommittedMiB":     n.DiskCommitMiB,
			// What the node measured, beside what it promised. The two differ by
			// orders of magnitude because a sandbox's disk request is nominal while
			// its layer is sparse, and that gap is only diagnosable if both are
			// shown: a node refusing work at 5% committed is otherwise inexplicable.
			"diskUsedMiB": n.DiskUsedMiB,
			// Real measured load, beside the commitments above. These feed the
			// scheduler's soft load score; showing them makes a placement that
			// avoided a "lightly committed but hot" node explicable.
			"cpuUsedPercent": n.CPUUsedPercent,
			"memUsedPercent": n.MemUsedPercent,
			"gpuCount":       n.GPUCount, "gpuCommitted": n.GPUCommitted,
			"createInFlight": n.CreateInFlight, "maxCreates": n.MaxCreates,
			"cachedImages":  len(n.CachedImages),
			"lastHeartbeat": n.LastHeartbeat,
			// The CPU decides which memory snapshots can be restored here, so
			// it belongs in the operational view: without it, a refused restore
			// cannot be explained from the API alone.
			"cpuVendor": n.CPUVendor, "cpuFamily": n.CPUFamily,
			"cpuTemplate": n.CPUTemplate,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"nodes": out})
}

// handleDrainNode stops new placements on a node without disturbing what
// already runs there, which is how a node is taken out of service safely.
func (s *Server) handleDrainNode(w http.ResponseWriter, r *http.Request) {
	if err := s.placer.Drain(r.PathValue("id")); err != nil {
		writeErr(w, http.StatusNotFound, "NODE_NOT_FOUND", err.Error())
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

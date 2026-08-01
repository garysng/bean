package api

import (
	"net/http"
)

// Node endpoints are the operational view of the cluster: what capacity
// exists, how much is promised, and which nodes are healthy enough to
// receive work.

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
			"gpuCount":             n.GPUCount, "gpuCommitted": n.GPUCommitted,
			"createInFlight": n.CreateInFlight, "maxCreates": n.MaxCreates,
			"cachedImages":  len(n.CachedImages),
			"lastHeartbeat": n.LastHeartbeat,
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

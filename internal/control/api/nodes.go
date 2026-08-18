package api

import (
	"encoding/json"
	"net/http"

	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
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

// admissionPatch is the runtime-tunable admission surface. Every field is a
// pointer so the operator can push one knob without restating the rest: an
// absent field means "leave it", which is the same optional-overlay semantics
// the node applies. It maps one-to-one onto AdmissionConfig's optional fields.
type admissionPatch struct {
	MinFreeDiskMiB     *int64   `json:"minFreeDiskMiB"`
	MinFreeDiskPercent *float64 `json:"minFreeDiskPercent"`
	MaxMemPercent      *float64 `json:"maxMemPercent"`
}

// handleConfigureNodeAdmission retunes a node's disk/memory admission thresholds
// at runtime, so the point at which a node refuses new work can be changed
// without a restart -- the startup flags are only the initial default.
//
// The gateway holds no admission state: it forwards the patch straight to the
// node over the same SandboxService channel that carries placement, and the node
// validates and installs it. A bad threshold is the node's to reject, surfaced
// here as the node reported it.
func (s *Server) handleConfigureNodeAdmission(w http.ResponseWriter, r *http.Request) {
	nodeID := r.PathValue("id")
	// A node with no record cannot be reached and would otherwise surface as an
	// opaque dial error; refusing up front names the actual problem.
	if s.nodeRecord(nodeID) == nil {
		writeErr(w, http.StatusNotFound, "NODE_NOT_FOUND", "unknown node "+nodeID)
		return
	}

	var patch admissionPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	if patch.MinFreeDiskMiB == nil && patch.MinFreeDiskPercent == nil && patch.MaxMemPercent == nil {
		writeErr(w, http.StatusBadRequest, "BAD_REQUEST",
			"at least one of minFreeDiskMiB, minFreeDiskPercent, maxMemPercent is required")
		return
	}

	client, err := s.router.Client(nodeID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "NODE_UNREACHABLE", err.Error())
		return
	}
	if _, err := client.ConfigureAdmission(r.Context(), &nodev1.ConfigureAdmissionRequest{
		NodeId: nodeID,
		Config: &nodev1.AdmissionConfig{
			MinFreeDiskMib:     patch.MinFreeDiskMiB,
			MinFreeDiskPercent: patch.MinFreeDiskPercent,
			MaxMemPercent:      patch.MaxMemPercent,
		},
	}); err != nil {
		// A rejected threshold comes back as InvalidArgument (400); an unreachable
		// node as something grpcFault maps to 5xx. Either way the node's own words
		// reach the operator rather than a generic gateway error.
		grpcToHTTP(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/logging"
)

// Fork derives new sandboxes from a running one.
//
// The mechanism it exposes already existed: a restore gives each child its own
// copy-on-write rootfs layer while the unpacked memory image is mapped read-only
// and shared, so children are independent instances of one captured state. That
// is a fork, and it was reachable only as two calls -- snapshot, then create
// from the snapshot id. This is the single call, against the sandbox rather than
// against a checkpoint the caller had to name and then clean up.
//
// Nothing here is a new capability, so nothing here is a new failure mode: the
// same placement, CPU compatibility and reference counting apply, because it is
// the same code underneath.

// forkRequest asks for count independent copies of a sandbox's current state.
type forkRequest struct {
	// Count is how many children to produce, defaulting to 1.
	//
	// N in one call rather than N calls because the checkpoint is the expensive
	// part and it is taken once for the whole batch. N calls would capture the
	// source N times, and each capture would see a slightly later state, so the
	// children would not even be copies of the same instant.
	Count int `json:"count"`
	// Name labels the intermediate checkpoint, which is otherwise anonymous.
	// Useful only in logs and events, since the record does not outlive the call.
	Name string `json:"name"`
	// Labels are applied to the children, not to the checkpoint. Forking exists to
	// produce sandboxes; labelling them is what a caller needs to find them again.
	Labels map[string]string `json:"labels"`
	// Env and Cmd override what each child runs. A fork resumes a live guest, so
	// these reach the child's spec but do not restart anything already running
	// inside it -- the same as a restore.
	Env map[string]string `json:"env"`
	Cmd []string          `json:"cmd"`
}

// maxForkCount bounds one call.
//
// Each child holds a durable reservation and streams a chain, so an unbounded
// count would let one request commit the cluster and hold a connection for as
// long as it took. The limit is on one call, not on total children: a caller
// wanting more asks twice, and pays for one more checkpoint to do it.
const maxForkCount = 64

// handleFork checkpoints a sandbox and starts count children from it.
//
// The source is left exactly as it was found. Snapshot's keepRunning option is
// deliberately not plumbed through: a fork that stopped what it forked from
// would be a move, and every use for this -- fan out a prepared environment,
// branch a session to explore two continuations -- needs the original to survive.
// Inheriting the option's default would have got the right behaviour by accident
// and left it free to change under us.
func (s *Server) handleFork(w http.ResponseWriter, r *http.Request) {
	if s.snapshots == nil {
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "snapshot storage not configured")
		return
	}
	src := s.loadSandbox(w, r.PathValue("id"))
	if src == nil {
		return
	}

	var req forkRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid JSON: "+err.Error())
			return
		}
	}
	count := req.Count
	if count == 0 {
		count = 1
	}
	if count < 0 || count > maxForkCount {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			"count must be between 1 and 64")
		return
	}

	// Only a live guest has state worth forking. A stopped or failed source is
	// refused here, where the reason is still obvious, rather than at the node
	// after a checkpoint record has been written and abandoned.
	//
	// PAUSED is allowed: a paused guest's memory is intact, which is the whole
	// input to a fork, and pausing before forking is a reasonable way to make
	// sure the children branch from a quiescent instant.
	switch src.State {
	case store.SandboxRunning, store.SandboxPaused:
	default:
		writeErr(w, http.StatusConflict, "SANDBOX_NOT_RUNNING",
			"cannot fork sandbox in state "+string(src.State))
		return
	}

	nodeClient, err := s.nodeClientFor(src)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "NODE_UNREACHABLE", err.Error())
		return
	}

	// The checkpoint is internal: it exists to carry state to the children and is
	// deleted once they are running. A caller who forks a hundred times should not
	// be left with a hundred snapshot records to reap, and a fork is not a way of
	// asking for a checkpoint -- that endpoint already exists and is the honest way
	// to get one.
	//
	// Memory is always captured. A fork means "another one of these, as it is
	// now", and a filesystem-only copy would reboot, losing the running processes
	// that are usually the reason for forking. A caller who wants the cheap
	// portable kind asks for a filesystem-only snapshot explicitly.
	includeMemory := true
	snapReq := &snapshotRequest{Name: req.Name, IncludeMemory: &includeMemory}
	snap, err := s.captureSnapshot(r.Context(), src, nodeClient, snapReq)
	if err != nil {
		writeFault(w, err)
		return
	}
	s.emit(src.ID, "sandbox.fork.captured",
		map[string]string{"snapshotId": snap.ID, "count": strconv.Itoa(count)})

	// One reference covers the whole batch, so the checkpoint cannot be reclaimed
	// while any child is still reading it. Taken once rather than per child
	// because the batch is what owns it: releasing after the last child is the
	// point at which it is safe to delete, and that is here.
	if _, err := s.store.AcquireSnapshot(snap.ID); err != nil {
		s.discardForkSnapshot(snap.ID)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}

	children := make([]*store.Sandbox, 0, count)
	failures := make([]map[string]any, 0)
	for i := 0; i < count; i++ {
		rec, spec := s.childOf(src, &req)
		// Children are launched one at a time. Placement reserves durably and the
		// node merges a chain once and reuses it, so the second child onwards is
		// already cheap; doing them concurrently would mainly race the scheduler
		// into rejecting reservations it would otherwise have granted.
		if ferr := s.launchFromSnapshot(r.Context(), snap, spec, rec); ferr != nil {
			f := asFault(ferr)
			// A partial result is reported rather than rolled back. The children
			// that did start are real, usable sandboxes, and destroying them
			// because a later sibling could not be placed would throw away work
			// the caller can use -- a batch of 40 out of 50 is a batch.
			failures = append(failures, map[string]any{
				"index": i, "code": f.code, "message": f.msg,
			})
			continue
		}
		children = append(children, rec)
	}

	if rerr := s.store.ReleaseSnapshot(snap.ID); rerr != nil {
		slog.Error("cannot release fork checkpoint reference",
			logging.KeySnapshot, snap.ID, logging.KeyError, rerr)
	}
	// Reclaimed now that every child has finished reading it. The children do not
	// depend on it afterwards: each holds its own rootfs layer and its own copy of
	// the guest, which is what makes them independent rather than views of it.
	s.discardForkSnapshot(snap.ID)

	ids := make([]string, len(children))
	for i, c := range children {
		ids[i] = c.ID
	}
	body := map[string]any{
		"sourceId":  src.ID,
		"forkIds":   ids,
		"sandboxes": children,
	}
	if len(failures) > 0 {
		body["failures"] = failures
	}

	switch {
	case len(children) == 0:
		// Nothing was produced, so this is a failure and not a partial success.
		// The first child's own code is reused, because with every child failing
		// for the same reason -- no compatible CPU, no capacity -- that code is
		// the answer, and a generic one would bury it.
		f := failures[0]
		writeErr(w, forkFailureStatus(f["code"].(string)), f["code"].(string),
			"no fork could be started: "+f["message"].(string))
	case len(failures) > 0:
		// 207: some children exist and some do not, and a caller has to look at
		// the body either way. Reporting 201 would hide the shortfall from anything
		// that only checks the status.
		writeJSON(w, http.StatusMultiStatus, body)
	default:
		writeJSON(w, http.StatusCreated, body)
	}
}

// forkFailureStatus maps a child's failure code back to a status for the case
// where every child failed alike.
func forkFailureStatus(code string) int {
	switch code {
	case "INCOMPATIBLE_CPU":
		return http.StatusConflict
	case "NO_CAPACITY", "NODE_UNREACHABLE":
		return http.StatusServiceUnavailable
	case "SNAPSHOT_DATA_MISSING", "SNAPSHOT_BASE_MISSING":
		return http.StatusNotFound
	default:
		return http.StatusInternalServerError
	}
}

// childOf builds the record and spec for one fork child.
//
// Resources are inherited from the source rather than defaulted: a child runs
// the same guest, so it needs the same memory to hold it, and a smaller
// allocation would place successfully and then fail to load the checkpoint.
func (s *Server) childOf(src *store.Sandbox, req *forkRequest) (*store.Sandbox, *nodev1.SandboxSpec) {
	id := store.NewID(store.PrefixSandbox)

	labels := map[string]string{}
	for k, v := range src.Labels {
		labels[k] = v
	}
	for k, v := range req.Labels {
		labels[k] = v
	}
	// Recorded so a child can be traced back to what it was forked from. The
	// snapshot that carried the state is gone by the time anyone reads this, which
	// is exactly why the source sandbox is named instead.
	labels["bean.fork.source"] = src.ID

	// Placement is left to inherited labels. The scheduler spreads on the
	// "eval-run" label (see placementFor), so a caller who already groups a run
	// gets its forks spread across nodes, and one who does not gets them packed.
	//
	// A fork does not impose a spread key of its own, even though it knows the
	// children are related. Spreading would fight the thing that makes a fork
	// cheap: a node merges a chain once and every later child of the same
	// checkpoint skips it, so scattering children means re-merging per node. The
	// caller knows whether it is buying throughput or fault isolation; the fork
	// does not, and the existing label already says which.

	rec := &store.Sandbox{
		ID: id, State: store.SandboxPending,
		Image: src.Image, Region: s.region, Runtime: src.Runtime, Domain: s.domain,
		CPU: src.CPU, MemoryMiB: src.MemoryMiB, DiskMiB: src.DiskMiB,
		Labels: labels, CreatedAt: time.Now(), LastActivity: time.Now(),
		IdleTimeout: src.IdleTimeout, OnIdle: src.OnIdle,
	}
	spec := &nodev1.SandboxSpec{
		SandboxId: id, Image: src.Image,
		Cpu: src.CPU, MemoryMib: src.MemoryMiB, DiskMib: src.DiskMiB,
		Env: req.Env, Cmd: req.Cmd, Labels: labels,
	}
	if src.IdleTimeout != nil {
		spec.Lifecycle = &nodev1.Lifecycle{
			HasIdleTimeout: true, IdleTimeoutSeconds: *src.IdleTimeout, OnIdle: src.OnIdle,
		}
	}
	return rec, spec
}

// discardForkSnapshot removes the internal checkpoint and its blob.
//
// Failure is logged, not returned: the children are running and the caller's
// request succeeded, so the only consequence is a blob left on disk. Failing the
// fork over it would destroy usable sandboxes to tidy up.
func (s *Server) discardForkSnapshot(id string) {
	if err := s.store.DeleteSnapshot(id); err != nil {
		slog.Error("cannot delete fork checkpoint record",
			logging.KeySnapshot, id, logging.KeyError, err)
		return
	}
	if err := s.snapshots.Delete(id); err != nil {
		slog.Error("cannot delete fork checkpoint blob",
			logging.KeySnapshot, id, logging.KeyError, err)
	}
}

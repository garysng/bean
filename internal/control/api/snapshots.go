package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/garysng/bean/internal/control/scheduler"
	"github.com/garysng/bean/internal/control/snapshot"
	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/logging"
)

// Snapshot endpoints. A snapshot captures a sandbox so it can be recreated
// later, possibly on another node — the "set up an environment once, then
// fan out" pattern that batch evaluation depends on.

type snapshotRequest struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	// KeepRunning defaults to true: taking a snapshot should not disturb a
	// working sandbox unless the caller asks for it.
	KeepRunning *bool `json:"keepRunning"`
	// IncludeMemory defaults to true, which is what a snapshot has always meant
	// here — a restore resumes the guest. Setting it false captures only the
	// filesystem: the restore boots fresh, but it can land on any CPU, whereas
	// guest memory pins a snapshot to a compatible vendor and family.
	//
	// A pointer distinguishes "absent" from "false" so the default cannot be
	// mistaken for a caller's choice.
	IncludeMemory *bool `json:"includeMemory"`
	// Base names a checkpoint to capture against, so the new one holds only the
	// guest memory written since. It cuts what a snapshot costs to what actually
	// changed, which is the point of taking many of them.
	//
	// The result is not independent: restoring it replays the whole chain, and its
	// ancestors cannot be deleted while it exists.
	Base string `json:"base"`
}

// maxDiffChain bounds how many diffs may stack before a checkpoint is taken in
// full instead.
//
// Every link is another bundle a restore has to fetch and replay, so an unbounded
// chain makes restores steadily slower and pins every ancestor forever. Cutting a
// fresh full snapshot resets both, and callers never have to think about depth:
// asking for a diff always succeeds, it just occasionally costs more.
const maxDiffChain = 8

func (s *Server) handleCreateSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.snapshots == nil {
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "snapshot storage not configured")
		return
	}
	id := r.PathValue("id")
	rec := s.loadSandbox(w, id)
	if rec == nil {
		return
	}
	var req snapshotRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid JSON: "+err.Error())
			return
		}
	}
	keepRunning := true
	if req.KeepRunning != nil {
		keepRunning = *req.KeepRunning
	}

	nodeClient, err := s.nodeClientFor(rec)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "NODE_UNREACHABLE", err.Error())
		return
	}
	snap, err := s.captureSnapshot(r.Context(), rec, nodeClient, &req)
	if err != nil {
		writeFault(w, err)
		return
	}

	if !keepRunning {
		if _, err := nodeClient.DestroySandbox(r.Context(),
			&nodev1.DestroySandboxRequest{SandboxId: rec.ID}); err != nil {
			slog.Error("cannot destroy snapshot source sandbox", logging.KeySnapshot, snap.ID, logging.KeyError, err)
		} else {
			rec.State = store.SandboxStopped
			_ = s.store.PutSandbox(rec)
			s.releasePlacement(rec)
			s.emit(rec.ID, "sandbox.lifecycle.stopped", nil)
		}
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"snapshotId": snap.ID,
		"snapshot":   snap,
	})
}

// captureSnapshot checkpoints a running sandbox and stores the result, leaving
// the source untouched. Stopping the source, if the caller wants that, is the
// caller's own step: a fork must not do it, and keeping it out here is what
// makes the two callers unable to drift on the point.
//
// Errors are returned rather than written so a fan-out can attribute them.
func (s *Server) captureSnapshot(ctx context.Context, rec *store.Sandbox,
	nodeClient nodev1.SandboxServiceClient, req *snapshotRequest) (*store.Snapshot, error) {
	includeMemory := true
	if req.IncludeMemory != nil {
		includeMemory = *req.IncludeMemory
	}

	// A base is resolved before anything is captured, so an unusable one is
	// reported instead of leaving a failed snapshot record behind.
	baseID, chainDepth := "", 0
	if req.Base != "" {
		if !includeMemory {
			// Only guest memory can be captured incrementally. The filesystem
			// layer is already proportional to what changed, so a diff of a
			// memoryless checkpoint would save nothing and would still pin its
			// ancestors.
			return nil, faultf(http.StatusBadRequest, "INVALID_ARGUMENT",
				"base requires includeMemory: a filesystem-only checkpoint is already incremental")
		}
		base, err := s.store.GetSnapshot(req.Base)
		if err != nil {
			return nil, faultf(http.StatusInternalServerError, "INTERNAL", "%s", err.Error())
		}
		if base == nil {
			return nil, faultf(http.StatusNotFound, "SNAPSHOT_NOT_FOUND", "base snapshot not found")
		}
		if base.State != store.SnapshotReady {
			return nil, faultf(http.StatusConflict, "SNAPSHOT_NOT_READY",
				"base snapshot %s is %s", base.ID, base.State)
		}
		if !base.HasMemory() {
			return nil, faultf(http.StatusBadRequest, "INVALID_ARGUMENT",
				"base snapshot carries no guest memory, so there is nothing to capture a difference against")
		}
		// Past the limit the checkpoint is taken in full and becomes a new root.
		// Refusing instead would make callers manage depth, and silently
		// continuing would let restores get slower without bound.
		if base.ChainDepth+1 <= maxDiffChain {
			baseID, chainDepth = base.ID, base.ChainDepth+1
		}
	}

	snapID := store.NewID(store.PrefixSnapshot)
	snap := &store.Snapshot{
		ID: snapID, Name: req.Name, State: store.SnapshotCreating,
		SandboxID: rec.ID, Image: rec.Image, Runtime: rec.Runtime,
		NodeID: rec.NodeID, Labels: req.Labels, CreatedAt: time.Now(),
		IncludeMemory: &includeMemory,
		BaseID:        baseID, ChainDepth: chainDepth,
	}
	// The CPU is recorded only when guest memory travels with the snapshot.
	// A filesystem-only checkpoint restores on any CPU, so recording a
	// constraint it does not have would fragment placement for nothing.
	//
	// It is copied onto the snapshot rather than looked up from NodeID at restore
	// time: by then the node may be gone, or restarted under a different template.
	if includeMemory {
		if n := s.nodeRecord(rec.NodeID); n != nil {
			snap.CPUVendor, snap.CPUFamily, snap.CPUTemplate = n.CPUVendor, n.CPUFamily, n.CPUTemplate
		}
	}
	if err := s.store.PutSnapshot(snap); err != nil {
		return nil, faultf(http.StatusInternalServerError, "INTERNAL", "%s", err.Error())
	}

	blobW, err := s.snapshots.Writer(snapID)
	if err != nil {
		s.failSnapshot(snap, err)
		return nil, faultf(http.StatusInternalServerError, "INTERNAL", "%s", err.Error())
	}

	stream, err := nodeClient.SnapshotSandbox(ctx,
		&nodev1.SnapshotSandboxRequest{
			SandboxId: rec.ID, IncludeMemory: includeMemory,
			// Diff follows baseID rather than the request: a request that asked for
			// one past the depth limit is answered with a full checkpoint, and the
			// node has to write what the record claims.
			Diff: baseID != "",
		})
	if err != nil {
		snapshot.AbortWrite(s.snapshots, snapID, blobW)
		s.failSnapshot(snap, err)
		return nil, grpcFault(err)
	}

	var written int64
	for {
		chunk, rerr := stream.Recv()
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			snapshot.AbortWrite(s.snapshots, snapID, blobW)
			s.failSnapshot(snap, rerr)
			return nil, grpcFault(rerr)
		}
		n, werr := blobW.Write(chunk.Data)
		written += int64(n)
		if werr != nil {
			snapshot.AbortWrite(s.snapshots, snapID, blobW)
			s.failSnapshot(snap, werr)
			return nil, faultf(http.StatusInternalServerError, "INTERNAL", "%s", werr.Error())
		}
	}
	if err := blobW.Close(); err != nil {
		s.failSnapshot(snap, err)
		return nil, faultf(http.StatusInternalServerError, "INTERNAL", "%s", err.Error())
	}

	snap.State = store.SnapshotReady
	snap.SizeBytes = written
	if err := s.store.PutSnapshot(snap); err != nil {
		return nil, faultf(http.StatusInternalServerError, "INTERNAL", "%s", err.Error())
	}
	s.emit(rec.ID, "sandbox.snapshot.ready", map[string]string{"snapshotId": snapID})
	return snap, nil
}

// failSnapshot records a failed snapshot so a caller sees why, rather than
// finding a record stuck in CREATING.
func (s *Server) failSnapshot(snap *store.Snapshot, cause error) {
	snap.State = store.SnapshotFailed
	snap.Reason = cause.Error()
	if err := s.store.PutSnapshot(snap); err != nil {
		slog.Error("cannot record snapshot failure", logging.KeySnapshot, snap.ID, logging.KeyError, err)
	}
	s.emit(snap.SandboxID, "sandbox.snapshot.failed",
		map[string]string{"snapshotId": snap.ID, "reason": cause.Error()})
}

func (s *Server) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	labelKey, labelVal := parseLabelFilter(r.URL.Query().Get("label"))
	snaps, err := s.store.ListSnapshots(labelKey, labelVal,
		store.SnapshotState(r.URL.Query().Get("state")))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	if snaps == nil {
		snaps = []*store.Snapshot{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": snaps})
}

func (s *Server) handleGetSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := s.store.GetSnapshot(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	if snap == nil {
		writeErr(w, http.StatusNotFound, "SNAPSHOT_NOT_FOUND", "snapshot not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshot": snap})
}

func (s *Server) handleDeleteSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	switch err := s.store.DeleteSnapshot(id); {
	case err == nil:
		if s.snapshots != nil {
			if derr := s.snapshots.Delete(id); derr != nil {
				slog.Error("cannot delete snapshot blob", logging.KeySnapshot, id, logging.KeyError, derr)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, "SNAPSHOT_NOT_FOUND", "snapshot not found")
	case errors.Is(err, store.ErrInUse):
		writeErr(w, http.StatusConflict, "SNAPSHOT_IN_USE", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
	}
}

// createFromSnapshot restores a sandbox from a snapshot. It runs the same
// placement and bookkeeping as a normal create; only the source of the
// filesystem differs.
func (s *Server) createFromSnapshot(w http.ResponseWriter, r *http.Request,
	req *createRequest, spec *nodev1.SandboxSpec, rec *store.Sandbox) {
	if s.snapshots == nil {
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "snapshot storage not configured")
		return
	}
	// Hold a reference for the duration so the source cannot be deleted
	// mid-restore.
	snap, err := s.store.AcquireSnapshot(req.Snapshot)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "SNAPSHOT_NOT_FOUND", "snapshot not found")
			return
		}
		writeErr(w, http.StatusConflict, "SNAPSHOT_NOT_READY", err.Error())
		return
	}
	defer func() {
		if rerr := s.store.ReleaseSnapshot(snap.ID); rerr != nil {
			slog.Error("cannot release snapshot reference", logging.KeySnapshot, snap.ID, logging.KeyError, rerr)
		}
	}()

	if err := s.launchFromSnapshot(r.Context(), snap, spec, rec); err != nil {
		writeFault(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"sandbox": rec})
}

// launchFromSnapshot places a sandbox and streams a checkpoint chain into it,
// mutating rec to reflect where it landed and what state it reached.
//
// The caller owns the snapshot reference: this may be invoked many times against
// one acquired snapshot, which is what lets a fork of N children checkpoint once
// and stream the same chain N times.
//
// Errors are returned rather than written, so a fan-out can report which child
// failed and why instead of collapsing every outcome into one status line.
func (s *Server) launchFromSnapshot(ctx context.Context, snap *store.Snapshot,
	spec *nodev1.SandboxSpec, rec *store.Sandbox) error {
	// The restored sandbox inherits the snapshot's image so its rootfs base
	// matches what the checkpoint was taken from.
	rec.Image = snap.Image
	rec.SnapshotID = snap.ID
	spec.Image = snap.Image
	// The node reuses whatever it has already unpacked for this checkpoint, so
	// restoring the same snapshot repeatedly — a batch fanning out from one
	// prepared environment — unpacks it once.
	spec.SnapshotId = snap.ID

	// Guest memory carries the CPU it was captured on, so placement is restricted
	// to nodes that memory can actually run on. Without this the restore succeeds
	// and the sandbox then misbehaves for reasons nothing reports.
	place := s.placementFor(rec)
	// Only guest memory carries a CPU, so a filesystem-only snapshot places
	// anywhere. The branch is explicit rather than relying on the CPU fields
	// happening to be empty for such snapshots — that coupling would break
	// quietly if anything ever populated them.
	if snap.HasMemory() {
		place.CPUConstraint = scheduler.CPUConstraint{
			Vendor: snap.CPUVendor, Family: snap.CPUFamily, Template: snap.CPUTemplate,
		}
	}
	nodeID, err := s.placer.Schedule(place)
	if err != nil {
		if errors.Is(err, scheduler.ErrIncompatibleCPU) {
			// 409, not 503: waiting will not help, and a client retrying on
			// 503 would loop until its own deadline.
			//
			// A fork inherits its source's CPU constraint, so a cluster with no
			// compatible node fails every child this same way. Saying so per child
			// is the only place the reason is visible; a bare "no capacity" would
			// send the reader looking at resource limits.
			return faultf(http.StatusConflict, "INCOMPATIBLE_CPU", "%s", err.Error())
		}
		return faultf(http.StatusServiceUnavailable, "NO_CAPACITY", "%s", err.Error())
	}
	rec.NodeID = nodeID
	if err := s.store.PutSandbox(rec); err != nil {
		_ = s.placer.FinishCreate(nodeID)
		_ = s.placer.Release(rec.ID)
		return faultf(http.StatusInternalServerError, "INTERNAL", "%s", err.Error())
	}
	s.emit(rec.ID, "sandbox.lifecycle.created", map[string]string{"snapshotId": snap.ID})

	nodeClient, err := s.nodeClientFor(rec)
	if err != nil {
		s.failCreate(rec, err)
		return faultf(http.StatusServiceUnavailable, "NODE_UNREACHABLE", "%s", err.Error())
	}

	// The whole chain travels, base first: a diff holds only what changed since
	// its base, so the node cannot reconstruct the guest from the leaf alone.
	// Ancestors need no reference of their own — a base cannot be deleted while
	// anything descends from it, so the leaf's reference holds the chain.
	chain, err := s.store.SnapshotChain(snap.ID)
	if err != nil {
		s.failCreate(rec, err)
		if errors.Is(err, store.ErrNotFound) {
			return faultf(http.StatusNotFound, "SNAPSHOT_BASE_MISSING", "%s", err.Error())
		}
		return faultf(http.StatusInternalServerError, "INTERNAL", "%s", err.Error())
	}
	// Declared on the spec rather than discovered from the stream: the node has to
	// create one reader per layer before reading any of them, since each layer is
	// its own gzip stream.
	spec.SnapshotChain = make([]string, len(chain))
	for i, link := range chain {
		spec.SnapshotChain[i] = link.ID
	}

	// The proto RPC keeps its name: it is a published interface, and renaming it
	// would break every deployed node for a change that adds a verb.
	stream, err := nodeClient.RestoreSandbox(ctx)
	if err != nil {
		s.failCreate(rec, err)
		return grpcFault(err)
	}
	if err := stream.Send(&nodev1.RestoreSandboxFrame{
		Frame: &nodev1.RestoreSandboxFrame_Spec{Spec: spec},
	}); err != nil {
		s.failCreate(rec, err)
		return grpcFault(err)
	}

	buf := make([]byte, 1<<20)
	for i, link := range chain {
		if i > 0 {
			// Closes the previous layer so its reader on the node sees a clean end
			// of stream; without it the gzip reader would wait for more.
			if serr := stream.Send(&nodev1.RestoreSandboxFrame{
				Frame: &nodev1.RestoreSandboxFrame_LayerEnd{LayerEnd: true},
			}); serr != nil {
				s.failCreate(rec, serr)
				return grpcFault(serr)
			}
		}
		if err := s.sendSnapshotBlob(stream, link.ID, buf); err != nil {
			s.failCreate(rec, err)
			if errors.Is(err, snapshot.ErrBlobNotFound) {
				return faultf(http.StatusNotFound, "SNAPSHOT_DATA_MISSING", "%s", err.Error())
			}
			return grpcFault(err)
		}
	}

	resp, err := stream.CloseAndRecv()
	_ = s.placer.FinishCreate(nodeID)
	if err != nil {
		s.failCreate(rec, err)
		return grpcFault(err)
	}

	rec.State = store.SandboxState(resp.Status.State)
	_ = s.store.PutSandbox(rec)
	s.emit(rec.ID, "sandbox.lifecycle.running", nil)
	return nil
}

// sendSnapshotBlob streams one layer's bundle into an open restore stream.
func (s *Server) sendSnapshotBlob(stream nodev1.SandboxService_RestoreSandboxClient,
	id string, buf []byte) error {
	blobR, err := s.snapshots.Reader(id)
	if err != nil {
		return err
	}
	defer blobR.Close()

	for {
		n, rerr := blobR.Read(buf)
		if n > 0 {
			if serr := stream.Send(&nodev1.RestoreSandboxFrame{
				Frame: &nodev1.RestoreSandboxFrame_Data{Data: buf[:n]},
			}); serr != nil {
				return serr
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return fmt.Errorf("read snapshot %s: %w", id, rerr)
		}
	}
}

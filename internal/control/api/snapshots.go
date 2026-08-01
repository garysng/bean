package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/garysng/bean/internal/control/scheduler"
	"github.com/garysng/bean/internal/control/snapshot"
	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
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
}

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

	snapID := store.NewID(store.PrefixSnapshot)
	snap := &store.Snapshot{
		ID: snapID, Name: req.Name, State: store.SnapshotCreating,
		SandboxID: rec.ID, Image: rec.Image, Runtime: rec.Runtime,
		NodeID: rec.NodeID, Labels: req.Labels, CreatedAt: time.Now(),
	}
	if err := s.store.PutSnapshot(snap); err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}

	blobW, err := s.snapshots.Writer(snapID)
	if err != nil {
		s.failSnapshot(snap, err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}

	stream, err := nodeClient.SnapshotSandbox(r.Context(),
		&nodev1.SnapshotSandboxRequest{SandboxId: rec.ID})
	if err != nil {
		snapshot.AbortWrite(s.snapshots, snapID, blobW)
		s.failSnapshot(snap, err)
		grpcToHTTP(w, err)
		return
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
			grpcToHTTP(w, rerr)
			return
		}
		n, werr := blobW.Write(chunk.Data)
		written += int64(n)
		if werr != nil {
			snapshot.AbortWrite(s.snapshots, snapID, blobW)
			s.failSnapshot(snap, werr)
			writeErr(w, http.StatusInternalServerError, "INTERNAL", werr.Error())
			return
		}
	}
	if err := blobW.Close(); err != nil {
		s.failSnapshot(snap, err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}

	snap.State = store.SnapshotReady
	snap.SizeBytes = written
	if err := s.store.PutSnapshot(snap); err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	s.emit(rec.ID, "sandbox.snapshot.ready", map[string]string{"snapshotId": snapID})

	if !keepRunning {
		if _, err := nodeClient.DestroySandbox(r.Context(),
			&nodev1.DestroySandboxRequest{SandboxId: rec.ID}); err != nil {
			log.Printf("snapshot %s: destroy source sandbox: %v", snapID, err)
		} else {
			rec.State = store.SandboxStopped
			_ = s.store.PutSandbox(rec)
			s.releasePlacement(rec)
			s.emit(rec.ID, "sandbox.lifecycle.stopped", nil)
		}
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"snapshotId": snapID,
		"snapshot":   snap,
	})
}

// failSnapshot records a failed snapshot so a caller sees why, rather than
// finding a record stuck in CREATING.
func (s *Server) failSnapshot(snap *store.Snapshot, cause error) {
	snap.State = store.SnapshotFailed
	snap.Reason = cause.Error()
	if err := s.store.PutSnapshot(snap); err != nil {
		log.Printf("snapshot %s: record failure: %v", snap.ID, err)
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
				log.Printf("snapshot %s: delete blob: %v", id, derr)
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
			log.Printf("snapshot %s: release ref: %v", snap.ID, rerr)
		}
	}()

	// The restored sandbox inherits the snapshot's image so its rootfs base
	// matches what the checkpoint was taken from.
	rec.Image = snap.Image
	rec.SnapshotID = snap.ID
	spec.Image = snap.Image

	var placement *scheduler.Request
	if s.placer != nil {
		placement = &scheduler.Request{
			SandboxID: rec.ID, Region: s.region, Image: rec.Image,
			CPU: spec.Cpu, MemoryMiB: spec.MemoryMib, DiskMiB: spec.DiskMib,
			Runtime: s.runtimeTier, SpreadKey: req.Labels["eval-run"],
		}
		nodeID, perr := s.placer.Schedule(placement)
		if perr != nil {
			writeErr(w, http.StatusServiceUnavailable, "NO_CAPACITY", perr.Error())
			return
		}
		rec.NodeID = nodeID
	}
	if err := s.store.PutSandbox(rec); err != nil {
		if s.placer != nil {
			s.placer.ReleaseCreate(rec.NodeID)
			s.placer.Release(rec.NodeID, placement)
		}
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	s.emit(rec.ID, "sandbox.lifecycle.created", map[string]string{"snapshotId": snap.ID})

	nodeClient, err := s.nodeClientFor(rec)
	if err != nil {
		s.failCreate(rec, placement, err)
		writeErr(w, http.StatusServiceUnavailable, "NODE_UNREACHABLE", err.Error())
		return
	}

	blobR, err := s.snapshots.Reader(snap.ID)
	if err != nil {
		s.failCreate(rec, placement, err)
		if errors.Is(err, snapshot.ErrBlobNotFound) {
			writeErr(w, http.StatusNotFound, "SNAPSHOT_DATA_MISSING", err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	defer blobR.Close()

	stream, err := nodeClient.RestoreSandbox(r.Context())
	if err != nil {
		s.failCreate(rec, placement, err)
		grpcToHTTP(w, err)
		return
	}
	if err := stream.Send(&nodev1.RestoreSandboxFrame{
		Frame: &nodev1.RestoreSandboxFrame_Spec{Spec: spec},
	}); err != nil {
		s.failCreate(rec, placement, err)
		grpcToHTTP(w, err)
		return
	}

	buf := make([]byte, 1<<20)
	for {
		n, rerr := blobR.Read(buf)
		if n > 0 {
			if serr := stream.Send(&nodev1.RestoreSandboxFrame{
				Frame: &nodev1.RestoreSandboxFrame_Data{Data: buf[:n]},
			}); serr != nil {
				s.failCreate(rec, placement, serr)
				grpcToHTTP(w, serr)
				return
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			s.failCreate(rec, placement, rerr)
			writeErr(w, http.StatusInternalServerError, "INTERNAL", rerr.Error())
			return
		}
	}

	resp, err := stream.CloseAndRecv()
	if s.placer != nil {
		s.placer.ReleaseCreate(rec.NodeID)
	}
	if err != nil {
		s.failCreate(rec, placement, err)
		grpcToHTTP(w, err)
		return
	}

	rec.State = store.SandboxState(resp.Status.State)
	_ = s.store.PutSandbox(rec)
	s.emit(rec.ID, "sandbox.lifecycle.running", nil)
	writeJSON(w, http.StatusCreated, map[string]any{"sandbox": rec})
}

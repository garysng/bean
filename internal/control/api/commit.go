package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/garysng/bean/internal/control/image"
	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/logging"
)

// Commit turns a sandbox's filesystem into a base image others can start from.
//
// It is distinct from a snapshot, and the distinction is worth keeping clear: a
// snapshot captures memory and device state too and only restores on the tier
// that produced it, so it recovers one particular sandbox. A committed image is
// just a filesystem, usable as anyone's base — which is what "set the
// environment up interactively, then share it" needs.

type commitRequest struct {
	Tag string `json:"tag"`
}

func (s *Server) handleCommit(w http.ResponseWriter, r *http.Request) {
	if s.images == nil {
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "image service not configured")
		return
	}

	var req commitRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid JSON: "+err.Error())
		return
	}
	if err := image.ValidateRef(req.Tag); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}

	id := r.PathValue("id")
	rec, err := s.store.GetSandbox(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	if rec == nil {
		writeErr(w, http.StatusNotFound, "SANDBOX_NOT_FOUND", "sandbox not found")
		return
	}

	// A tag that already exists is refused here rather than by the node.
	// Otherwise the failure path would mark the existing image FAILED — damaging
	// an image that works, and that other sandboxes may already be running from.
	if existing, err := s.store.GetImage(req.Tag); err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	} else if existing != nil {
		writeErr(w, http.StatusConflict, "IMAGE_EXISTS",
			"image "+req.Tag+" already exists; images are immutable, use a new tag")
		return
	}

	// The image is recorded before the node is asked, so a client polling the
	// image endpoint sees it in progress rather than nothing at all.
	img := &store.Image{
		Ref: req.Tag, Source: store.ImageBuilt, State: store.ImageBuilding,
		CreatedAt: time.Now(),
	}
	if err := s.store.PutImage(img); err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}

	client, err := s.router.Client(rec.NodeID)
	if err != nil {
		s.failImage(req.Tag, err.Error())
		writeErr(w, http.StatusServiceUnavailable, "NODE_UNAVAILABLE", err.Error())
		return
	}

	resp, err := client.CommitSandbox(r.Context(), &nodev1.CommitSandboxRequest{
		SandboxId: id, Tag: req.Tag,
	})
	if err != nil {
		s.failImage(req.Tag, err.Error())
		grpcToHTTP(w, err)
		return
	}

	// A committed image needs no conversion — it is already the format the tier
	// boots — so it goes straight to READY rather than through CONVERTING.
	img.State = store.ImageReady
	if err := s.store.PutImage(img); err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	s.emit(id, "sandbox.commit.ready", map[string]string{"imageRef": resp.ImageRef})

	writeJSON(w, http.StatusCreated, map[string]string{"imageRef": resp.ImageRef})
}

// failImage marks a committed image failed so a client is not left waiting on
// something that will never become ready.
func (s *Server) failImage(ref, reason string) {
	if s.images == nil {
		return
	}
	if err := s.images.MarkFailed(ref, reason); err != nil {
		slog.Error("cannot mark commit failed", logging.KeyImage, ref, logging.KeyError, err)
	}
}

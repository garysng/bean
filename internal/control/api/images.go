package api

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/garysng/bean/internal/control/image"
	"github.com/garysng/bean/internal/control/store"
)

// Image endpoints. A caller only ever supplies a native OCI reference;
// digest resolution and any conversion are platform-internal (see
// internal/control/image for the terminology).

func (s *Server) handleListImages(w http.ResponseWriter, r *http.Request) {
	if s.images == nil {
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "image service not configured")
		return
	}
	imgs, err := s.images.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": imgs})
}

func (s *Server) handleImageStatus(w http.ResponseWriter, r *http.Request) {
	if s.images == nil {
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "image service not configured")
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "ref query param required")
		return
	}
	img, err := s.images.Get(ref)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	if img == nil {
		writeErr(w, http.StatusNotFound, "IMAGE_NOT_FOUND", "image "+ref+" is not registered")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ref":         img.Ref,
		"digest":      img.Digest,
		"state":       img.State,
		"reason":      img.Reason,
		"sizeBytes":   img.SizeBytes,
		"cachedNodes": img.CachedNodes,
		// format tells the caller which tier can run this image today.
		"format": imageFormat(img.State),
	})
}

// imageFormat reports the artifact form available for an image, which tells
// a caller which tier can run it today. Anything not converted is still
// runnable through the standard OCI pull path.
func imageFormat(state store.ImageState) string {
	if state == store.ImageReady {
		return "overlaybd"
	}
	return "oci"
}

type prewarmRequest struct {
	Refs        []string `json:"refs"`
	Region      string   `json:"region"`
	TargetNodes int      `json:"targetNodes"`
	Priority    string   `json:"priority"`
}

func (s *Server) handlePrewarm(w http.ResponseWriter, r *http.Request) {
	if s.images == nil {
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "image service not configured")
		return
	}
	var req prewarmRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid JSON: "+err.Error())
		return
	}
	job, err := s.images.Prewarm(image.PrewarmRequest{
		Refs:        req.Refs,
		Region:      req.Region,
		TargetNodes: req.TargetNodes,
		Priority:    req.Priority,
	})
	if err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, job)
}

func (s *Server) handlePrewarmStatus(w http.ResponseWriter, r *http.Request) {
	if s.images == nil {
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "image service not configured")
		return
	}
	job, err := s.images.JobStatus(r.PathValue("jobId"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	if job == nil {
		writeErr(w, http.StatusNotFound, "JOB_NOT_FOUND", "prewarm job not found")
		return
	}
	writeJSON(w, http.StatusOK, job)
}

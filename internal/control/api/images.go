package api

import (
	"context"
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

	// The job is accepted and the nodes are warmed in the background: a first
	// pull can take minutes, which is far longer than an HTTP request should be
	// held open. Callers follow progress through the job status endpoint.
	go s.runPrewarmJob(job.ID, req.Refs, req.Region)

	writeJSON(w, http.StatusAccepted, job)
}

// runPrewarmJob asks nodes to warm each image, recording which succeeded.
func (s *Server) runPrewarmJob(jobID string, refs []string, region string) {
	nodes, err := s.placer.Nodes()
	if err != nil {
		slog.Error("prewarm cannot list nodes", "job", jobID, logging.KeyError, err)
		return
	}

	for _, node := range nodes {
		// Only nodes that can take work are worth warming, and only in the
		// requested region: an image cached elsewhere does not help placement.
		if node.State != string(store.NodeReady) {
			continue
		}
		if region != "" && node.Region != region {
			continue
		}
		client, err := s.router.Client(node.ID)
		if err != nil {
			slog.Error("prewarm cannot reach node", "job", jobID, logging.KeyNode, node.ID, logging.KeyError, err)
			continue
		}
		for _, ref := range refs {
			// Each image gets its own generous deadline: converting a large
			// image legitimately takes minutes, and one slow image should not
			// abandon the rest.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			_, err := client.PrewarmImage(ctx, &nodev1.PrewarmImageRequest{Image: ref})
			cancel()
			if err != nil {
				slog.Error("prewarm failed", "job", jobID, logging.KeyImage, ref, logging.KeyNode, node.ID, logging.KeyError, err)
			}
			// Success is not recorded here: the node reports what it holds in
			// its heartbeat, and that is the authority. Writing it from this
			// side would let the two disagree after a node loses its disk.
		}
	}
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

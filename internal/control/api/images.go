package api

import (
	"context"
	"encoding/json"
	"errors"
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
	// Scoped to the caller when an identity is configured, so "the images I
	// built" is answerable. With no identity this returns everything, which is
	// the operator's view and the historical behaviour.
	//
	// ?source=built|imported narrows further. It is a filter on the listing
	// rather than a separate endpoint because provenance is a property of an
	// image, not a different kind of thing to list.
	imgs, err := s.images.ListFor(s.owner(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	switch source := r.URL.Query().Get("source"); source {
	case "":
	case string(store.ImageBuilt), string(store.ImageImported):
		imgs = filterBySource(imgs, store.ImageSource(source))
	default:
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			"source must be "+string(store.ImageBuilt)+" or "+string(store.ImageImported))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"images": imgs})
}

// filterBySource keeps images of one provenance.
//
// An image with no recorded source counts as imported: that is what a record
// from before the field was written back is, and the platform only ever set it
// explicitly for its own builds.
func filterBySource(imgs []*store.Image, want store.ImageSource) []*store.Image {
	out := make([]*store.Image, 0, len(imgs))
	for _, img := range imgs {
		source := img.Source
		if source == "" {
			source = store.ImageImported
		}
		if source == want {
			out = append(out, img)
		}
	}
	return out
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
		// source answers "is this ours or something pulled from outside", which
		// is the question a caller looking at an unfamiliar ref actually has.
		"source": imageSource(img.Source),
		// owner is empty for an unowned image, and stays in the response so a
		// caller can see that an image is shared rather than theirs.
		"owner":   img.Owner,
		"baseRef": img.BaseRef,
		"buildId": img.BuildID,
	})
}

// handleDeleteImage removes an image record. The ref comes in the query string, as
// it does for status, because a native reference carries slashes and colons that a
// path segment would have to escape.
//
// Scoped to the caller: an identity may delete its own images and the unowned ones it
// shares, but not another identity's -- which is reported as 404 rather than 403 for a
// ref the caller could otherwise not see, so delete cannot be used to probe.
func (s *Server) handleDeleteImage(w http.ResponseWriter, r *http.Request) {
	if s.images == nil {
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "image service not configured")
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "ref query param required")
		return
	}
	switch err := s.images.DeleteFor(ref, s.owner(r)); {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"ref": ref, "deleted": true})
	case errors.Is(err, image.ErrNotFound), errors.Is(err, image.ErrForbidden):
		// A forbidden delete is reported as not-found: an identity must not learn that
		// a ref it may not touch exists.
		writeErr(w, http.StatusNotFound, "IMAGE_NOT_FOUND", "image "+ref+" is not registered")
	case errors.Is(err, image.ErrInvalidRef):
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
	}
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

// imageSource reports provenance, defaulting an unset value to imported so a
// record written before the field was populated does not read as an absence of
// origin. Nothing but a platform build ever set it, so the default is right.
func imageSource(source store.ImageSource) store.ImageSource {
	if source == "" {
		return store.ImageImported
	}
	return source
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

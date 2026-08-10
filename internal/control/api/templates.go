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

// Template endpoints. A template is a startable overlaybd artifact -- produced
// by a Dockerfile build or by converting an OCI image -- addressed by its id
// (primary) or name. An OCI-converted template's name is the OCI reference it
// came from; a built template's name is its tag. See internal/control/image for
// the terminology.

func (s *Server) handleListTemplates(w http.ResponseWriter, r *http.Request) {
	if s.images == nil {
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "template service not configured")
		return
	}
	// Scoped to the caller when an identity is configured, so "the templates I
	// built" is answerable. With no identity this returns everything, which is
	// the operator's view and the historical behaviour.
	//
	// ?source=built|converted narrows further. It is a filter on the listing
	// rather than a separate endpoint because provenance is a property of a
	// template, not a different kind of thing to list.
	tpls, err := s.images.ListFor(s.owner(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	switch source := r.URL.Query().Get("source"); source {
	case "":
	case string(store.TemplateBuilt), string(store.TemplateConverted):
		tpls = filterBySource(tpls, store.TemplateSource(source))
	default:
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			"source must be "+string(store.TemplateBuilt)+" or "+string(store.TemplateConverted))
		return
	}
	out := make([]map[string]any, 0, len(tpls))
	for _, tpl := range tpls {
		out = append(out, templateJSON(tpl))
	}
	writeJSON(w, http.StatusOK, map[string]any{"templates": out})
}

// filterBySource keeps templates of one provenance.
//
// A template with no recorded source counts as converted: that is what a record
// from before the field was written back is, and the platform only ever set it
// explicitly for its own builds.
func filterBySource(tpls []*store.Template, want store.TemplateSource) []*store.Template {
	out := make([]*store.Template, 0, len(tpls))
	for _, tpl := range tpls {
		source := tpl.Source
		if source == "" {
			source = store.TemplateConverted
		}
		if source == want {
			out = append(out, tpl)
		}
	}
	return out
}

func (s *Server) handleTemplateStatus(w http.ResponseWriter, r *http.Request) {
	if s.images == nil {
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "template service not configured")
		return
	}
	// A template is addressed by id or by name; a converted OCI image's name is
	// its OCI reference, so ?name= carries the slashes and colons a path segment
	// would have to escape.
	tpl, err := s.resolveTemplate(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	if tpl == nil {
		writeErr(w, http.StatusNotFound, "TEMPLATE_NOT_FOUND", "template not found")
		return
	}
	writeJSON(w, http.StatusOK, templateJSON(tpl))
}

// resolveTemplate looks a template up by ?id= (primary) or ?name=. It returns
// (nil, nil) when neither names a known template, and an error only when the
// query itself is malformed (neither param supplied).
func (s *Server) resolveTemplate(r *http.Request) (*store.Template, error) {
	if id := r.URL.Query().Get("id"); id != "" {
		return s.store.GetTemplate(id)
	}
	if name := r.URL.Query().Get("name"); name != "" {
		return s.images.Get(name)
	}
	return nil, errors.New("id or name query param required")
}

// resolveTemplateRef resolves a create request's `template` field, which names a
// stored template by id (a "tpl_..." string) or by name. It tries id first and
// falls back to name, returning (nil, nil) when neither matches.
func (s *Server) resolveTemplateRef(ref string) (*store.Template, error) {
	if tpl, err := s.store.GetTemplate(ref); err != nil || tpl != nil {
		return tpl, err
	}
	return s.store.GetTemplateByName(ref)
}

// templateJSON renders a template for the API: id/name/labels + the embedded FS
// artifact's fields + config + an optional ociSource (present only for an
// OCI-converted template). It is the symmetric counterpart to a snapshot's JSON.
func templateJSON(tpl *store.Template) map[string]any {
	m := map[string]any{
		"id":          tpl.ID,
		"name":        tpl.Name,
		"labels":      tpl.Labels,
		"digest":      tpl.FS.Digest,
		"layerDigests": tpl.FS.LayerDigests,
		"sizeBytes":   tpl.FS.SizeBytes,
		"config":      tpl.FS.Config,
		"state":       tpl.State,
		"reason":      tpl.Reason,
		"cachedNodes": tpl.CachedNodes,
		// format tells the caller which tier can run this template today.
		"format": templateFormat(tpl.State),
		// source answers "is this ours or something converted from outside".
		"source": templateSource(tpl.Source),
		// owner is empty for an unowned template, and stays in the response so a
		// caller can see that a template is shared rather than theirs.
		"owner":   tpl.Owner,
		"baseRef": tpl.BaseRef,
		"buildId": tpl.BuildID,
	}
	// ociSource is the conversion-cache key (OCI ref + content sha256). It is
	// present only for an OCI-converted template; a build has no OCI origin.
	if tpl.OCISource != nil {
		m["ociSource"] = map[string]any{
			"ref":    tpl.OCISource.Ref,
			"digest": tpl.OCISource.Digest,
		}
	}
	return m
}

// handleDeleteTemplate removes a template record, addressed by ?id= or ?name=.
//
// Scoped to the caller: an identity may delete its own templates and the unowned
// ones it shares, but not another identity's -- which is reported as 404 rather
// than 403 for a template the caller could otherwise not see, so delete cannot be
// used to probe.
func (s *Server) handleDeleteTemplate(w http.ResponseWriter, r *http.Request) {
	if s.images == nil {
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "template service not configured")
		return
	}
	// Resolve to the record first so a delete by id and a delete by name share
	// one owner-scoped path; the service deletes by the resolved name.
	tpl, err := s.resolveTemplate(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	if tpl == nil {
		writeErr(w, http.StatusNotFound, "TEMPLATE_NOT_FOUND", "template not found")
		return
	}
	switch err := s.images.DeleteFor(tpl.Name, s.owner(r)); {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"id": tpl.ID, "deleted": true})
	case errors.Is(err, image.ErrNotFound), errors.Is(err, image.ErrForbidden):
		// A forbidden delete is reported as not-found: an identity must not learn
		// that a template it may not touch exists.
		writeErr(w, http.StatusNotFound, "TEMPLATE_NOT_FOUND", "template not found")
	case errors.Is(err, image.ErrInvalidRef):
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
	}
}

// templateFormat reports the artifact form available for a template, which tells
// a caller which tier can run it today. Anything not ready is still runnable
// through the standard OCI pull path.
func templateFormat(state store.TemplateState) string {
	if state == store.TemplateReady {
		return "overlaybd"
	}
	return "oci"
}

// templateSource reports provenance, defaulting an unset value to converted so a
// record written before the field was populated does not read as an absence of
// origin. Nothing but a platform build ever set it, so the default is right.
func templateSource(source store.TemplateSource) store.TemplateSource {
	if source == "" {
		return store.TemplateConverted
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
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "template service not configured")
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
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "template service not configured")
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

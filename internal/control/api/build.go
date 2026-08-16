package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"google.golang.org/grpc/status"

	"github.com/garysng/bean/internal/control/image"
	"github.com/garysng/bean/internal/control/s3"
	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/logging"
)

// Builds run on a node, where BuildKit and the image cache already live. The
// control plane picks a node, starts the build in the background and records the
// outcome, so a caller gets an id immediately and follows progress through the
// template endpoints — a build takes minutes, which is far longer than an HTTP
// request should be held open.
//
// Progress and cancellation are two endpoints on top of that shape:
//
//   - GET  /v1/templates/build/logs?ref=  streams the output
//   - POST /v1/templates/build/cancel?ref=  stops the build
//
// The build is keyed by the template's name (its tag) rather than by a separate
// build id. The name is already claimed for the duration (immutable tags, one
// build per tag), so a second identifier would be a second thing to plumb
// through and to explain without describing anything the name does not.

type buildRequest struct {
	Tag        string            `json:"tag"`
	Dockerfile string            `json:"dockerfile"`
	BuildArgs  map[string]string `json:"buildArgs,omitempty"`
	SizeMiB    int64             `json:"sizeMiB,omitempty"`
	// ContextTar is a base64-encoded tar of the build context, for COPY and
	// ADD. JSON has no byte type, and a multipart upload would complicate every
	// client for a field most builds leave empty.
	ContextTar string `json:"contextTar,omitempty"`
}

// maxContextBytes bounds an inline build context. A context larger than this
// wants a separate upload endpoint rather than a bigger request body.
const maxContextBytes = 64 << 20

// errNoReadyNode reports that no node can take a build.
var errNoReadyNode = errors.New("no ready node available to build on")

func (s *Server) handleBuild(w http.ResponseWriter, r *http.Request) {
	if s.images == nil {
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "image service not configured")
		return
	}

	var req buildRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, maxContextBytes*2)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid JSON: "+err.Error())
		return
	}
	if err := image.ValidateRef(req.Tag); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	if req.Dockerfile == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "dockerfile required")
		return
	}

	var contextTar []byte
	if req.ContextTar != "" {
		decoded, err := base64.StdEncoding.DecodeString(req.ContextTar)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT",
				"contextTar is not valid base64: "+err.Error())
			return
		}
		if len(decoded) > maxContextBytes {
			writeErr(w, http.StatusRequestEntityTooLarge, "CONTEXT_TOO_LARGE",
				"build context exceeds the inline limit")
			return
		}
		contextTar = decoded
	}

	// The tag is claimed before the build starts, both so a caller can poll and
	// so two builds cannot race to the same reference.
	if existing, err := s.store.GetTemplateByName(req.Tag); err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	} else if existing != nil {
		writeErr(w, http.StatusConflict, "TEMPLATE_EXISTS",
			"template "+req.Tag+" already exists; templates are immutable, use a new tag")
		return
	}

	// The build's caller owns the result. This is the case ownership exists
	// for: a caller asking "what did I build" is asking about exactly these. A
	// built template has no OCI origin, so OCISource stays nil.
	tpl := &store.Template{
		ID: store.NewID(store.PrefixTemplate), Name: req.Tag,
		Source: store.TemplateBuilt, State: store.TemplateBuilding,
		Owner: s.owner(r), CreatedAt: time.Now(),
	}
	if err := s.store.PutTemplate(tpl); err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}

	nodeID, err := s.pickBuilder()
	if err != nil {
		s.failTemplate(req.Tag, err.Error())
		writeErr(w, http.StatusServiceUnavailable, "NO_BUILDER", err.Error())
		return
	}

	// Record which node runs the build before starting it, so /cancel and /logs
	// on any replica resolve the owning node from the store rather than from this
	// process's memory. This is what makes the build reachable cluster-wide.
	if err := s.images.SetBuildNode(req.Tag, nodeID); err != nil {
		s.failTemplate(req.Tag, err.Error())
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}

	// A build legitimately takes minutes: it pulls a base image and runs the
	// Dockerfile's commands. The context is detached from the request on purpose
	// — see runBuild for why a client hanging up does not stop a build. Its
	// timeout bounds a wedged build; cancellation proper goes through the node
	// (handleBuildCancel), not this context.
	ctx, cancel := context.WithTimeout(context.Background(), maxBuildDuration)

	go s.runBuild(ctx, cancel, nodeID, req, contextTar)

	writeJSON(w, http.StatusAccepted, map[string]string{
		"template": req.Tag,
		"nodeId":   nodeID,
		"state":    string(store.TemplateBuilding),
	})
}

// maxBuildDuration is the ceiling on one build. It is generous because a build
// that pulls a large base image and compiles something is legitimately slow;
// what it prevents is a wedged buildctl holding a node's builder forever.
const maxBuildDuration = 60 * time.Minute

// pickBuilder chooses where to build. A node labelled as a builder is preferred,
// so a cluster can dedicate hosts to builds; otherwise any ready node will do,
// which keeps a single-node deployment working without configuration.
func (s *Server) pickBuilder() (string, error) {
	nodes, err := s.placer.Nodes()
	if err != nil {
		return "", err
	}
	var fallback string
	for _, n := range nodes {
		if n.State != string(store.NodeReady) {
			continue
		}
		if n.Labels["pool"] == "builder" {
			return n.ID, nil
		}
		if fallback == "" {
			fallback = n.ID
		}
	}
	if fallback == "" {
		return "", errNoReadyNode
	}
	return fallback, nil
}

// runBuild performs the build, forwarding its output to log and recording the
// outcome.
//
// The context is the build's own, not any request's, and that is the decision
// worth naming: a client that hangs up does not stop the build. A build is
// expensive and its result is shared — the tag it produces is what other callers
// wait on, and its layers warm the node's BuildKit cache for everything built on
// the same base — so tearing it down because one reader closed a socket would
// throw away work nobody asked to abandon, and would make `bean build` behave
// differently depending on whether someone was watching. Stopping a build is
// therefore something a caller has to ask for explicitly, which is what the
// cancel endpoint is for.
//
// The bounded lifetime above is a different concern: it stops a wedged build
// from holding a builder indefinitely, and is not a statement about readers.
func (s *Server) runBuild(ctx context.Context, cancel context.CancelFunc,
	nodeID string, req buildRequest, contextTar []byte) {

	// cancel here rather than only in the handler: releasing the context is what
	// stops the timer, and the build outliving the handler means the handler
	// cannot be the one to do it.
	defer cancel()

	// The build's log is uploaded to the shared store by the node, not relayed
	// through this call; failTemplate records the terminal state in the store,
	// which is what /logs consults for the outcome.
	fail := func(reason string) {
		s.failTemplate(req.Tag, reason)
	}

	client, err := s.router.Client(nodeID)
	if err != nil {
		fail(err.Error())
		return
	}

	// StartBuild returns as soon as the node has registered the build; it does not
	// wait for the build to finish. The build then runs under a node-owned context,
	// so ctx here bounds only how long this replica polls, not the build's life --
	// a different replica (or this one after a restart) re-attaches by polling the
	// same tag (see ReconcileBuilds).
	if _, err := client.StartBuild(ctx, &nodev1.BuildImageRequest{
		Tag:        req.Tag,
		Dockerfile: req.Dockerfile,
		ContextTar: contextTar,
		BuildArgs:  req.BuildArgs,
		SizeMib:    req.SizeMiB,
	}); err != nil {
		slog.Error("build failed", logging.KeyImage, req.Tag, logging.KeyNode, nodeID, logging.KeyError, err)
		fail(err.Error())
		return
	}

	s.pollBuild(ctx, nodeID, req.Tag)
}

// buildStatusPollInterval is how often a poller asks the node for a build's
// status. One second matches E2B's template-manager poll and is well under a
// build's minutes-long duration, so the outcome is recorded promptly without
// making the node's GetBuildStatus a hot path.
const buildStatusPollInterval = time.Second

// pollBuild polls the node that owns a build until it reaches a terminal phase,
// then writes the authoritative template state (READY or FAILED). It is the
// control-plane half of the poll model (docs/build-logs-s3.md §8): the node runs
// the build under its own context and only reports status, so this is what
// learns the outcome -- for a live build started by runBuild, and for an
// in-flight build re-attached after a restart by ReconcileBuilds.
//
// It is safe to run concurrently for the same tag across replicas: MarkReady and
// failTemplate route through the store's idempotent transition, so a duplicate
// poller at most repeats a terminal write.
//
// ctx bounds the poll (maxBuildDuration): when it expires the build is recorded
// FAILED, which stops a wedged build from leaving a template BUILDING forever.
// Transient GetBuildStatus errors (the node restarting, a momentarily
// unreachable connection) are tolerated -- they retry on the next tick rather
// than failing the build, since the build itself is unaffected.
func (s *Server) pollBuild(ctx context.Context, nodeID, tag string) {
	client, err := s.router.Client(nodeID)
	if err != nil {
		s.failTemplate(tag, err.Error())
		return
	}

	ticker := time.NewTicker(buildStatusPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// The poll deadline elapsed (or this replica is shutting down). The
			// build may still be running on the node; recording FAILED here is the
			// timeout ceiling, and a surviving build's later success is harmless
			// because the tag is retryable.
			slog.Info("build poll ended", logging.KeyImage, tag, logging.KeyNode, nodeID, logging.KeyError, ctx.Err())
			s.failTemplate(tag, "build cancelled")
			return
		case <-ticker.C:
			resp, err := client.GetBuildStatus(ctx, &nodev1.GetBuildStatusRequest{Tag: tag})
			if err != nil {
				// Transient: log at debug volume and retry on the next tick. A
				// genuinely gone node eventually trips the ctx deadline above.
				slog.Debug("build status poll error", logging.KeyImage, tag, logging.KeyNode, nodeID, logging.KeyError, err)
				continue
			}
			switch resp.GetPhase() {
			case nodev1.BuildPhase_BUILD_SUCCEEDED:
				result := resp.GetResult()
				// A built image needs no conversion — BuildKit's flat output is sealed
				// into an overlaybd layer and published, so it goes straight to READY
				// with the artifact's real coordinates.
				//
				// The node reports an empty overlaybd_ref when it has no object store:
				// the build then exists only in the building node's ImageDir, and READY
				// overstates its reach in a multi-node cluster. Ownership is recorded
				// regardless of where the bytes are, so a later prewarm can publish it
				// without revisiting who the image belongs to.
				if err := s.images.MarkReady(tag, result.GetOverlaybdRef(), result.GetSizeBytes(),
					result.GetLayerDigests(), "", protoImageConfig(result.GetConfig())); err != nil {
					slog.Error("cannot mark build ready", logging.KeyImage, tag, logging.KeyError, err)
					s.failTemplate(tag, err.Error())
				}
				return
			case nodev1.BuildPhase_BUILD_FAILED:
				// A cancelled build is not a failed one, but it is not a usable image
				// either: the tag has to stop claiming to be on its way. The node's
				// reason already distinguishes cancellation ("build cancelled").
				reason := resp.GetReason()
				if reason == "" {
					reason = "build failed"
				}
				slog.Info("build finished", logging.KeyImage, tag, logging.KeyNode, nodeID, "reason", reason)
				s.failTemplate(tag, reason)
				return
			default:
				// BUILD_RUNNING or BUILD_UNKNOWN: keep polling. UNKNOWN covers a poll
				// that raced registration or a node that has not yet learned of a
				// build the store says it owns (restart reconcile); it resolves to a
				// real phase or trips the deadline.
			}
		}
	}
}

// ReconcileBuilds re-attaches this replica to builds the store still records as
// in flight. A build runs under the node's own context and survives a
// control-plane restart, but the poller that records its outcome does not -- so
// on startup a replica lists BUILDING templates and resumes polling each, under
// a fresh maxBuildDuration bound. This is the restart half of the poll model:
// without it, a build that finishes while every replica was down would leave its
// template stuck BUILDING (docs/build-logs-s3.md §8).
//
// Templates with no NodeID (claimed but never assigned a builder before the
// crash) are failed: no node owns them, so no outcome will ever arrive.
func (s *Server) ReconcileBuilds(ctx context.Context) {
	if s.images == nil {
		return
	}
	templates, err := s.store.ListTemplates("")
	if err != nil {
		slog.Error("reconcile builds: list templates", logging.KeyError, err)
		return
	}
	for _, t := range templates {
		if t.State != store.TemplateBuilding {
			continue
		}
		if t.NodeID == "" {
			slog.Warn("reconcile builds: BUILDING template has no node, failing", logging.KeyImage, t.Name)
			s.failTemplate(t.Name, "build lost: no owning node after restart")
			continue
		}
		slog.Info("reconcile builds: resuming poll", logging.KeyImage, t.Name, logging.KeyNode, t.NodeID)
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), maxBuildDuration)
		go func(nodeID, tag string) {
			defer cancel()
			s.pollBuild(bctx, nodeID, tag)
		}(t.NodeID, t.Name)
	}
}

// handleBuildLogs streams a build's output as chunked plain text.
//
// Chunked text rather than SSE or a long-poll cursor. The payload is a log: it
// is already a byte stream, and SSE would mean framing every line as an event
// and unframing it in each client for no gain — /v1/events uses SSE because its
// payload is discrete typed JSON objects, which is the opposite case. A cursor
// endpoint would put reassembly in the client and turn one connection into a
// polling loop. Plain chunked text is what `curl` and `bean build --follow` can
// both consume without a parser.
//
// ?follow=false returns what has been produced so far and stops, which is what a
// script wants after a build has finished.
//
// The log is read from the shared store, not from this process, so any replica
// serves any build and a gateway restart loses nothing. Offset addressing makes
// a late reader and a reconnecting reader the same case: the client's byte offset
// is handed straight to the store reader.
func (s *Server) handleBuildLogs(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "ref query param required")
		return
	}
	if s.buildLogs == nil {
		writeErr(w, http.StatusNotImplemented, "NOT_IMPLEMENTED", "build log store not configured")
		return
	}
	reader, err := s3.NewBuildLogReader(s.buildLogs, ref)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	// A build with no log objects at all is either unknown or has not flushed its
	// first chunk. The store record decides which: a template in BUILDING has just
	// not produced output yet, anything else with no log is a 404.
	if ok, err := reader.Exists(r.Context()); err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	} else if !ok {
		if tpl, _ := s.store.GetTemplateByName(ref); tpl == nil || tpl.State != store.TemplateBuilding {
			writeErr(w, http.StatusNotFound, "BUILD_NOT_FOUND", "no build logs for "+ref)
			return
		}
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "streaming unsupported")
		return
	}

	follow := r.URL.Query().Get("follow") != "false"
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	// Buffering a log stream defeats it, and a proxy deciding to do so is the
	// usual cause of "no output until the build ends".
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	var offset int64
	for {
		next, err := reader.ReadFrom(r.Context(), offset, w)
		if err != nil {
			// The reader may have hung up, or the store may have hiccuped. Either
			// way the response is already committed, so stop rather than trying to
			// signal an error mid-body.
			return
		}
		if next > offset {
			offset = next
			flusher.Flush()
		}
		// The manifest is the log store's own progress marker; it tells a follower
		// when to stop without a round trip to the control record on every poll.
		m, merr := reader.Manifest(r.Context())
		done := merr == nil && m.Done
		if done || !follow {
			s.writeBuildOutcome(w, ref, m, merr)
			flusher.Flush()
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(buildLogPollInterval):
		}
	}
}

// buildLogPollInterval is how often a following reader re-checks the store for
// new output. Short enough to feel live, long enough not to hammer the store.
const buildLogPollInterval = 500 * time.Millisecond

// writeBuildOutcome writes the terminal line a reader sees at the end. The
// outcome goes in the body rather than the status: the response was committed
// with 200 before the build's fate was known, so a reader that only checked the
// status would call a failed build a success. The authoritative status is the
// store record; the manifest is used only when the record is unavailable.
func (s *Server) writeBuildOutcome(w io.Writer, ref string, m s3.BuildLogManifest, merr error) {
	failed, reason, known := false, "", false
	if tpl, err := s.store.GetTemplateByName(ref); err == nil && tpl != nil {
		switch tpl.State {
		case store.TemplateFailed:
			failed, reason, known = true, tpl.Reason, true
		case store.TemplateReady:
			known = true
		}
	}
	if !known && merr == nil && m.Done {
		failed, reason, known = m.Failed, m.Reason, true
	}
	if !known {
		return
	}
	if failed {
		_, _ = fmt.Fprintf(w, "\nbuild failed: %s\n", reason)
	} else {
		_, _ = io.WriteString(w, "\nbuild succeeded\n")
	}
}

// handleBuildCancel stops a running build.
//
// Cancelling is explicit because a build's result is shared: a reader
// disconnecting does not imply nobody wants the image (see runBuild). The build
// runs on a node and is cancelled there, so this handler resolves the build's
// node from the store record and calls the node's CancelBuild -- which works
// from any replica, not only the one that started the build. The node's build
// path then marks the tag FAILED, which frees the ref for another attempt.
func (s *Server) handleBuildCancel(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "ref query param required")
		return
	}
	tpl, err := s.store.GetTemplateByName(ref)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	if tpl == nil {
		writeErr(w, http.StatusNotFound, "BUILD_NOT_FOUND", "no build in progress for "+ref)
		return
	}
	if tpl.State != store.TemplateBuilding {
		// Already over (READY or FAILED): nothing to stop, and reporting success
		// would suggest this call changed something.
		writeErr(w, http.StatusConflict, "BUILD_FINISHED",
			"build for "+ref+" is not in progress")
		return
	}
	if tpl.NodeID == "" {
		writeErr(w, http.StatusConflict, "BUILD_NOT_PLACED",
			"build for "+ref+" has not been placed on a node yet")
		return
	}
	client, err := s.router.Client(tpl.NodeID)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "NODE_UNAVAILABLE", err.Error())
		return
	}
	// The node cancels the build's context, which kills buildctl; its build path
	// observes the cancellation and settles the record, so this handler does not
	// touch the state -- two writers to it is how it ends up disagreeing.
	if _, err := client.CancelBuild(r.Context(), &nodev1.CancelBuildRequest{Tag: ref}); err != nil {
		writeErr(w, http.StatusBadGateway, "CANCEL_FAILED", status.Convert(err).Message())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{
		"template": ref,
		"state":    "CANCELLING",
	})
}

// failTemplate marks a building template failed so a client is not left waiting
// on something that will never become ready.
func (s *Server) failTemplate(ref, reason string) {
	if s.images == nil {
		return
	}
	if err := s.images.MarkFailed(ref, reason); err != nil {
		slog.Error("cannot mark image failed", logging.KeyImage, ref, logging.KeyError, err)
	}
}

// protoImageConfig converts the node's reported image config into the control-plane
// record's form, or nil when the artifact declared none. The control plane keeps its
// own Config type rather than importing the node package, so a build's or conversion's
// recovered ENV/ENTRYPOINT/CMD/WORKDIR is copied field-for-field onto the template.
func protoImageConfig(cfg *nodev1.ImageConfig) *store.Config {
	if cfg == nil {
		return nil
	}
	return &store.Config{
		Env:        cfg.GetEnv(),
		Entrypoint: cfg.GetEntrypoint(),
		Cmd:        cfg.GetCmd(),
		WorkingDir: cfg.GetWorkingDir(),
		User:       cfg.GetUser(),
	}
}

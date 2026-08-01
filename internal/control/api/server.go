// Package api implements the bean-api REST gateway.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/garysng/bean/internal/control/image"
	"github.com/garysng/bean/internal/control/scheduler"
	"github.com/garysng/bean/internal/control/secret"
	"github.com/garysng/bean/internal/control/snapshot"
	"github.com/garysng/bean/internal/control/store"
	commonv1 "github.com/garysng/bean/internal/gen/bean/common/v1"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/obs"
)

const (
	maxInlineFileBytes    = 4 << 20 // 4 MiB
	maxExecTimeoutSeconds = 3600    // clamp: never hold a request for longer
)

// Placer selects a node for a sandbox and owns the resource accounting.
// Reservations are durable, so releasing one needs only the sandbox id —
// the amounts are recorded with the reservation.
type Placer interface {
	Schedule(req *scheduler.Request) (string, error)
	// FinishCreate clears the in-flight marker once a create settles.
	FinishCreate(nodeID string) error
	// Release returns a stopped sandbox's capacity. Idempotent.
	Release(sandboxID string) error
	// Nodes and Drain back the operational surface.
	Nodes() ([]*store.NodeRecord, error)
	Drain(nodeID string) error
}

// Server is the REST gateway. It holds no placement state of its own: node
// capacity and reservations live in the store, so replicas are
// interchangeable and a restart loses nothing.
type Server struct {
	store  *store.Store
	router Router
	placer Placer
	region string
	// runtimeTier is the node capability sandboxes require. Runtime tiers
	// are internal (docs/architecture.md D3): callers never choose one.
	runtimeTier string
	apiKey      string
	images      *image.Service
	snapshots   snapshot.Blobs
	secrets     *secret.Box
	bus         *eventBus
	metrics     *obs.Registry
	mux         *http.ServeMux
}

// Options configures the gateway.
type Options struct {
	Region string
	APIKey string
	// RuntimeTier is the node capability required for placement; defaults
	// to "fc" (the main tier) when empty.
	RuntimeTier string
	// Images enables the image endpoints and image registration on create.
	Images *image.Service
	// Secrets encrypts persisted credentials; nil disables the registry
	// credential endpoints rather than storing secrets in the clear.
	Secrets *secret.Box
	// Snapshots stores checkpoint blobs; nil disables snapshot endpoints.
	Snapshots snapshot.Blobs
}

// New builds a gateway. A placer is required: every sandbox is placed by
// the scheduler, whether the cluster has one node or many.
func New(st *store.Store, router Router, placer Placer, opts Options) *Server {
	tier := opts.RuntimeTier
	if tier == "" {
		tier = "fc"
	}
	region := opts.Region
	if region == "" {
		region = "local"
	}
	s := &Server{store: st, router: router, placer: placer, region: region,
		runtimeTier: tier, apiKey: opts.APIKey, images: opts.Images,
		secrets: opts.Secrets, snapshots: opts.Snapshots,
		bus: newEventBus(), metrics: obs.NewRegistry(), mux: http.NewServeMux()}
	s.routes()
	return s
}

// nodeClientFor resolves the SandboxService client owning a sandbox.
func (s *Server) nodeClientFor(rec *store.Sandbox) (nodev1.SandboxServiceClient, error) {
	return s.router.Client(rec.NodeID)
}

func (s *Server) Handler() http.Handler { return s.authMiddleware(s.mux) }

func (s *Server) routes() {
	s.mux.HandleFunc("POST /v1/sandboxes", s.handleCreate)
	s.mux.HandleFunc("GET /v1/sandboxes", s.handleList)
	s.mux.HandleFunc("GET /v1/sandboxes/{id}", s.handleGet)
	s.mux.HandleFunc("DELETE /v1/sandboxes/{id}", s.handleDelete)
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/exec", s.handleExec)
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/pause", s.handlePause)
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/resume", s.handleResume)
	s.mux.HandleFunc("PUT /v1/sandboxes/{id}/files", s.handleWriteFile)
	s.mux.HandleFunc("GET /v1/sandboxes/{id}/files", s.handleReadFile)
	s.mux.HandleFunc("DELETE /v1/sandboxes/{id}/files", s.handleDeleteFile)
	s.mux.HandleFunc("GET /v1/sandboxes/{id}/files/ls", s.handleListDir)
	s.mux.HandleFunc("GET /v1/sandboxes/{id}/logs", s.handleLogs)
	s.mux.HandleFunc("GET /v1/sandboxes/{id}/events", s.handleEvents)
	// Live subscription (Server-Sent Events): no extra dependency, works
	// through proxies, and the browser/SDK story is simple.
	s.mux.HandleFunc("GET /v1/events", s.handleEventStream)
	s.mux.HandleFunc("GET /v1/images", s.handleListImages)
	// ref goes in a query param: it contains slashes and colons, which
	// would otherwise collide with sibling routes like prewarm.
	s.mux.HandleFunc("GET /v1/images/status", s.handleImageStatus)
	s.mux.HandleFunc("POST /v1/images/prewarm", s.handlePrewarm)
	s.mux.HandleFunc("GET /v1/images/prewarm/{jobId}", s.handlePrewarmStatus)
	s.mux.HandleFunc("PUT /v1/registries", s.handlePutRegistry)
	s.mux.HandleFunc("GET /v1/registries", s.handleListRegistries)
	s.mux.HandleFunc("DELETE /v1/registries/{host}", s.handleDeleteRegistry)
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/snapshot", s.handleCreateSnapshot)
	s.mux.HandleFunc("GET /v1/snapshots", s.handleListSnapshots)
	s.mux.HandleFunc("GET /v1/snapshots/{id}", s.handleGetSnapshot)
	s.mux.HandleFunc("DELETE /v1/snapshots/{id}", s.handleDeleteSnapshot)
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s.mux.HandleFunc("GET /metrics", s.handleMetrics)
	s.mux.HandleFunc("GET /v1/nodes", s.handleListNodes)
	s.mux.HandleFunc("POST /v1/nodes/{id}/drain", s.handleDrainNode)
}

// ---- auth / errors ----

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Health and metrics are scraped locally and carry no sandbox data.
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(auth), []byte(s.apiKey)) != 1 {
			writeErr(w, http.StatusUnauthorized, "UNAUTHENTICATED", "invalid or missing API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type apiError struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeErr(w http.ResponseWriter, httpCode int, code, msg string) {
	var e apiError
	e.Error.Code = code
	e.Error.Message = msg
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpCode)
	_ = json.NewEncoder(w).Encode(e)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func grpcToHTTP(w http.ResponseWriter, err error) {
	st, _ := status.FromError(err)
	switch st.Code() {
	case codes.NotFound:
		writeErr(w, http.StatusNotFound, "SANDBOX_NOT_FOUND", st.Message())
	case codes.InvalidArgument:
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", st.Message())
	case codes.FailedPrecondition:
		writeErr(w, http.StatusConflict, "SANDBOX_NOT_RUNNING", st.Message())
	case codes.DeadlineExceeded:
		writeErr(w, http.StatusGatewayTimeout, "TIMEOUT", st.Message())
	default:
		writeErr(w, http.StatusInternalServerError, "INTERNAL", st.Message())
	}
}

// ---- sandbox lifecycle ----

type createRequest struct {
	// Image is the native OCI reference to run. Mutually exclusive with
	// Snapshot.
	Image string `json:"image"`
	// Snapshot restores a previously captured sandbox instead of starting
	// from an image.
	Snapshot  string `json:"snapshot"`
	Resources *struct {
		CPU       float64 `json:"cpu"`
		MemoryMiB int64   `json:"memoryMiB"`
		DiskMiB   int64   `json:"diskMiB"`
	} `json:"resources"`
	Env          map[string]string `json:"env"`
	Cmd          []string          `json:"cmd"`
	AutoStartCmd bool              `json:"autoStartCmd"`
	Labels       map[string]string `json:"labels"`
	Lifecycle    *struct {
		IdleTimeout string `json:"idleTimeout"` // e.g. "300s"; null = never
		OnIdle      string `json:"onIdle"`      // pause|kill
	} `json:"lifecycle"`
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	outcome := "error"
	defer func() {
		s.metrics.IncCounter("bean_sandbox_creates_total",
			"Sandbox create attempts by outcome.",
			map[string]string{"outcome": outcome}, 1)
		s.metrics.ObserveDuration("bean_sandbox_create_duration_seconds",
			"End-to-end sandbox create latency.",
			map[string]string{"outcome": outcome}, time.Since(start))
	}()

	var req createRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid JSON: "+err.Error())
		return
	}
	switch {
	case req.Image == "" && req.Snapshot == "":
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "image or snapshot is required")
		return
	case req.Image != "" && req.Snapshot != "":
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			"image and snapshot are mutually exclusive")
		return
	}

	// Register the image so its metadata (and later, digest and conversion
	// state) exists for anything the platform has been asked to run.
	if s.images != nil && req.Image != "" {
		if _, err := s.images.Resolve(req.Image); err != nil {
			outcome = "error"
			writeErr(w, http.StatusBadRequest, "IMAGE_REF_INVALID", err.Error())
			return
		}
	}

	id := store.NewID(store.PrefixSandbox)
	spec := &nodev1.SandboxSpec{
		SandboxId:    id,
		Image:        req.Image,
		Cpu:          1,
		MemoryMib:    512,
		DiskMib:      20480,
		Env:          req.Env,
		Cmd:          req.Cmd,
		AutoStartCmd: req.AutoStartCmd,
		Labels:       req.Labels,
	}
	if req.Resources != nil {
		if req.Resources.CPU > 0 {
			spec.Cpu = req.Resources.CPU
		}
		if req.Resources.MemoryMiB > 0 {
			spec.MemoryMib = req.Resources.MemoryMiB
		}
		if req.Resources.DiskMiB > 0 {
			spec.DiskMib = req.Resources.DiskMiB
		}
	}
	rec := &store.Sandbox{
		ID: id, Image: req.Image, State: store.SandboxPending,
		Region: s.region, Runtime: s.runtimeTier,
		CPU: spec.Cpu, MemoryMiB: spec.MemoryMib, DiskMiB: spec.DiskMib,
		Labels: req.Labels, CreatedAt: time.Now(), LastActivity: time.Now(),
	}
	if req.Lifecycle != nil && req.Lifecycle.IdleTimeout != "" {
		d, err := time.ParseDuration(req.Lifecycle.IdleTimeout)
		if err != nil || d < 0 {
			writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid lifecycle.idleTimeout")
			return
		}
		onIdle := req.Lifecycle.OnIdle
		if onIdle == "" {
			onIdle = "pause"
		}
		if onIdle != "pause" && onIdle != "kill" {
			writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "onIdle must be pause|kill")
			return
		}
		secs := int64(d.Seconds())
		spec.Lifecycle = &nodev1.Lifecycle{HasIdleTimeout: true, IdleTimeoutSeconds: secs, OnIdle: onIdle}
		rec.IdleTimeout = &secs
		rec.OnIdle = onIdle
	}

	if req.Snapshot != "" {
		outcome = "restore"
		s.createFromSnapshot(w, r, &req, spec, rec)
		return
	}

	// Placement reserves capacity durably before anything is persisted, so a
	// capacity failure never leaves a record pointing at a node that cannot
	// host it, and a crash here leaks nothing the sweep cannot reclaim.
	nodeID, err := s.placer.Schedule(s.placementFor(rec))
	if err != nil {
		outcome = "no_capacity"
		writeErr(w, http.StatusServiceUnavailable, "NO_CAPACITY", err.Error())
		return
	}
	rec.NodeID = nodeID

	if err := s.store.PutSandbox(rec); err != nil {
		_ = s.placer.FinishCreate(nodeID)
		_ = s.placer.Release(rec.ID)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	s.emit(id, "sandbox.lifecycle.created", nil)

	nodeClient, err := s.nodeClientFor(rec)
	if err != nil {
		s.failCreate(rec, err)
		writeErr(w, http.StatusServiceUnavailable, "NODE_UNREACHABLE", err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	resp, err := nodeClient.CreateSandbox(ctx, &nodev1.CreateSandboxRequest{Spec: spec})
	_ = s.placer.FinishCreate(nodeID)
	if err != nil {
		s.failCreate(rec, err)
		grpcToHTTP(w, err)
		return
	}
	rec.State = store.SandboxState(resp.Status.State)
	_ = s.store.PutSandbox(rec)
	s.emit(id, "sandbox.lifecycle.running", nil)

	outcome = "success"
	writeJSON(w, http.StatusCreated, map[string]any{"sandbox": rec})
}

// placementFor derives a placement request from a sandbox record, so the
// create and restore paths cannot drift apart.
func (s *Server) placementFor(rec *store.Sandbox) *scheduler.Request {
	return &scheduler.Request{
		SandboxID: rec.ID, Region: s.region, Image: rec.Image,
		CPU: rec.CPU, MemoryMiB: rec.MemoryMiB, DiskMiB: rec.DiskMiB,
		Runtime: s.runtimeTier, SpreadKey: rec.Labels["eval-run"],
	}
}

// failCreate records the failure and returns the reserved capacity.
func (s *Server) failCreate(rec *store.Sandbox, cause error) {
	rec.State = store.SandboxFailed
	rec.Reason = cause.Error()
	_ = s.store.PutSandbox(rec)
	s.emit(rec.ID, "sandbox.lifecycle.failed", map[string]string{"reason": cause.Error()})
	if err := s.placer.Release(rec.ID); err != nil {
		log.Printf("sandbox %s: release reservation: %v", rec.ID, err)
	}
}

// releasePlacement returns capacity for a sandbox that has stopped.
func (s *Server) releasePlacement(rec *store.Sandbox) {
	if err := s.placer.Release(rec.ID); err != nil {
		log.Printf("sandbox %s: release reservation: %v", rec.ID, err)
	}
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	labelKey, labelVal := "", ""
	if lbl := r.URL.Query().Get("label"); lbl != "" {
		parts := strings.SplitN(lbl, "=", 2)
		labelKey = parts[0]
		if len(parts) == 2 {
			labelVal = parts[1]
		}
	}
	recs, err := s.store.ListSandboxes(labelKey, labelVal, store.SandboxState(r.URL.Query().Get("state")))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	if recs == nil {
		recs = []*store.Sandbox{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": recs})
}

func (s *Server) loadSandbox(w http.ResponseWriter, id string) *store.Sandbox {
	rec, err := s.store.GetSandbox(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return nil
	}
	if rec == nil {
		writeErr(w, http.StatusNotFound, "SANDBOX_NOT_FOUND", "sandbox "+id+" not found")
		return nil
	}
	return rec
}

// Metrics exposes the registry so binaries can add their own series.
func (s *Server) Metrics() *obs.Registry { return s.metrics }

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.refreshStateGauges()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if err := s.metrics.WritePrometheus(w); err != nil {
		log.Printf("write metrics: %v", err)
	}
}

// refreshStateGauges recomputes sandbox counts per state at scrape time,
// which keeps the counters authoritative even after a restart.
func (s *Server) refreshStateGauges() {
	recs, err := s.store.ListSandboxes("", "", "")
	if err != nil {
		log.Printf("metrics: list sandboxes: %v", err)
		return
	}
	counts := map[string]float64{}
	for _, rec := range recs {
		counts[string(rec.State)]++
	}
	// Zero out every known state so a drained one reports 0 rather than
	// keeping its last value forever.
	for _, st := range store.AllSandboxStates() {
		if _, ok := counts[string(st)]; !ok {
			counts[string(st)] = 0
		}
	}
	for st, n := range counts {
		s.metrics.SetGauge("bean_sandboxes", "Sandboxes by state.",
			map[string]string{"state": st}, n)
	}
}

// resolveNode loads the sandbox and returns the client for its node,
// writing the error response and returning nil on failure.
func (s *Server) resolveNode(w http.ResponseWriter, id string) nodev1.SandboxServiceClient {
	rec := s.loadSandbox(w, id)
	if rec == nil {
		return nil
	}
	c, err := s.nodeClientFor(rec)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "NODE_UNREACHABLE", err.Error())
		return nil
	}
	return c
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec := s.loadSandbox(w, id)
	if rec == nil {
		return
	}
	// Refresh live state from the node for non-terminal sandboxes.
	if !store.IsTerminal(rec.State) {
		nodeClient, cerr := s.nodeClientFor(rec)
		if cerr != nil {
			writeJSON(w, http.StatusOK, map[string]any{"sandbox": rec})
			return
		}
		st, err := nodeClient.GetSandbox(r.Context(), &nodev1.GetSandboxRequest{SandboxId: id})
		switch {
		case err == nil:
			rec.State = store.SandboxState(st.Status.State)
			_ = s.store.PutSandbox(rec)
		case status.Code(err) == codes.NotFound:
			// Node no longer has it (e.g. idle sweep onIdle=kill).
			rec.State = store.SandboxStopped
			_ = s.store.PutSandbox(rec)
			s.emit(id, "sandbox.lifecycle.stopped", map[string]string{"reason": "reconciled: gone on node"})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandbox": rec})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec := s.loadSandbox(w, id)
	if rec == nil {
		return
	}
	force := r.URL.Query().Get("force") == "true"
	nodeClient, cerr := s.nodeClientFor(rec)
	if cerr != nil {
		writeErr(w, http.StatusServiceUnavailable, "NODE_UNREACHABLE", cerr.Error())
		return
	}
	if _, err := nodeClient.DestroySandbox(r.Context(), &nodev1.DestroySandboxRequest{SandboxId: id, Force: force}); err != nil {
		grpcToHTTP(w, err)
		return
	}
	rec.State = store.SandboxStopped
	_ = s.store.PutSandbox(rec)
	s.releasePlacement(rec)
	s.emit(id, "sandbox.lifecycle.stopped", nil)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	nodeClient := s.resolveNode(w, id)
	if nodeClient == nil {
		return
	}
	if _, err := nodeClient.PauseSandbox(r.Context(), &nodev1.PauseSandboxRequest{SandboxId: id}); err != nil {
		grpcToHTTP(w, err)
		return
	}
	s.updateState(id, store.SandboxPaused)
	s.emit(id, "sandbox.lifecycle.paused", nil)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	nodeClient := s.resolveNode(w, id)
	if nodeClient == nil {
		return
	}
	if _, err := nodeClient.ResumeSandbox(r.Context(), &nodev1.ResumeSandboxRequest{SandboxId: id}); err != nil {
		grpcToHTTP(w, err)
		return
	}
	s.updateState(id, store.SandboxRunning)
	s.emit(id, "sandbox.lifecycle.resumed", nil)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) updateState(id string, state store.SandboxState) {
	if rec, err := s.store.GetSandbox(id); err == nil && rec != nil {
		rec.State = state
		_ = s.store.PutSandbox(rec)
	}
}

// ---- exec ----

type execRequest struct {
	Cmd            []string          `json:"cmd"`
	Cwd            string            `json:"cwd"`
	Env            map[string]string `json:"env"`
	TimeoutSeconds int64             `json:"timeoutSeconds"`
	Stdin          []byte            `json:"stdin"`
	MaxOutputBytes int64             `json:"maxOutputBytes"`
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	nodeClient := s.resolveNode(w, id)
	if nodeClient == nil {
		return
	}
	var req execRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	if req.TimeoutSeconds < 0 {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "timeoutSeconds must be >= 0")
		return
	}
	if req.TimeoutSeconds > maxExecTimeoutSeconds {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			fmt.Sprintf("timeoutSeconds exceeds max of %d", maxExecTimeoutSeconds))
		return
	}
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout+10*time.Second)
	defer cancel()
	execStart := time.Now()
	resp, err := nodeClient.Exec(ctx, &commonv1.ExecRequest{
		SandboxId:      id,
		Cmd:            req.Cmd,
		Cwd:            req.Cwd,
		Env:            req.Env,
		TimeoutSeconds: req.TimeoutSeconds,
		Stdin:          req.Stdin,
		MaxOutputBytes: req.MaxOutputBytes,
	})
	s.metrics.ObserveDuration("bean_exec_duration_seconds",
		"Exec round-trip latency through the gateway.",
		map[string]string{"outcome": execOutcome(err)}, time.Since(execStart))
	if err != nil {
		grpcToHTTP(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exitCode":   resp.ExitCode,
		"stdout":     string(resp.Stdout),
		"stderr":     string(resp.Stderr),
		"truncated":  resp.Truncated,
		"durationMs": resp.DurationMs,
	})
}

// ---- files ----

func (s *Server) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	nodeClient := s.resolveNode(w, id)
	if nodeClient == nil {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "path query param required")
		return
	}
	mode := uint32(0o644)
	if ms := r.URL.Query().Get("mode"); ms != "" {
		if m, err := strconv.ParseUint(ms, 8, 32); err == nil {
			mode = uint32(m)
		}
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxInlineFileBytes+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	if len(body) > maxInlineFileBytes {
		writeErr(w, http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", "inline upload limited to 4MiB; use presigned flow")
		return
	}
	ws, err := nodeClient.WriteFile(r.Context())
	if err != nil {
		grpcToHTTP(w, err)
		return
	}
	if err := ws.Send(&commonv1.WriteFileFrame{Frame: &commonv1.WriteFileFrame_Meta{Meta: &commonv1.WriteFileMeta{
		SandboxId: id, Path: path, Mode: mode, Mkdirs: r.URL.Query().Get("mkdirs") == "true",
	}}}); err != nil {
		grpcToHTTP(w, err)
		return
	}
	for off := 0; off < len(body); off += 1 << 20 {
		end := min(off+1<<20, len(body))
		if err := ws.Send(&commonv1.WriteFileFrame{Frame: &commonv1.WriteFileFrame_Data{Data: body[off:end]}}); err != nil {
			grpcToHTTP(w, err)
			return
		}
	}
	resp, err := ws.CloseAndRecv()
	if err != nil {
		grpcToHTTP(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bytesWritten": resp.BytesWritten})
}

func (s *Server) handleReadFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	nodeClient := s.resolveNode(w, id)
	if nodeClient == nil {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "path query param required")
		return
	}
	stream, err := nodeClient.ReadFile(r.Context(), &commonv1.ReadFileRequest{SandboxId: id, Path: path})
	if err != nil {
		grpcToHTTP(w, err)
		return
	}
	first, err := stream.Recv()
	if err != nil && err != io.EOF {
		grpcToHTTP(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	if first != nil {
		if _, werr := w.Write(first.Data); werr != nil {
			return
		}
	}
	for {
		chunk, rerr := stream.Recv()
		if rerr == io.EOF {
			return
		}
		if rerr != nil {
			// Response already committed with 200: abort the connection so the
			// client sees a truncated transfer instead of a silent short read.
			log.Printf("readFile %s %s: mid-stream error: %v", id, path, rerr)
			panic(http.ErrAbortHandler)
		}
		if _, werr := w.Write(chunk.Data); werr != nil {
			return
		}
	}
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	nodeClient := s.resolveNode(w, id)
	if nodeClient == nil {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "path query param required")
		return
	}
	if _, err := nodeClient.DeleteFile(r.Context(), &commonv1.DeleteFileRequest{SandboxId: id, Path: path}); err != nil {
		grpcToHTTP(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListDir(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	nodeClient := s.resolveNode(w, id)
	if nodeClient == nil {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}
	resp, err := nodeClient.ListDir(r.Context(), &commonv1.ListDirRequest{SandboxId: id, Path: path})
	if err != nil {
		grpcToHTTP(w, err)
		return
	}
	entries := make([]map[string]any, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		entries = append(entries, map[string]any{
			"name": e.Name, "size": e.Size, "mode": fmt.Sprintf("%o", e.Mode),
			"mtime": time.Unix(e.MtimeUnix, 0).UTC().Format(time.RFC3339), "isDir": e.IsDir,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	nodeClient := s.resolveNode(w, id)
	if nodeClient == nil {
		return
	}
	tail := 0
	if ts := r.URL.Query().Get("tailLines"); ts != "" {
		tail, _ = strconv.Atoi(ts)
	}
	stream, err := nodeClient.GetLogs(r.Context(), &commonv1.GetLogsRequest{SandboxId: id, TailLines: int32(tail)})
	if err != nil {
		grpcToHTTP(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for {
		chunk, rerr := stream.Recv()
		if rerr == io.EOF {
			return
		}
		if rerr != nil {
			log.Printf("logs %s: mid-stream error: %v", id, rerr)
			panic(http.ErrAbortHandler)
		}
		if _, werr := w.Write(chunk.Data); werr != nil {
			return
		}
	}
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.loadSandbox(w, id) == nil {
		return
	}
	limit := 100
	if ls := r.URL.Query().Get("limit"); ls != "" {
		limit, _ = strconv.Atoi(ls)
	}
	events, err := s.store.ListEvents(id, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	if events == nil {
		events = []*store.Event{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (s *Server) emit(sandboxID, typ string, data map[string]string) {
	ev := &store.Event{
		Type: typ, Timestamp: time.Now(), SandboxID: sandboxID, Data: data, Version: "v1",
	}
	if err := s.store.AppendEvent(ev); err != nil {
		log.Printf("emit event %s %s: %v", sandboxID, typ, err)
	}
	// Label filtering for subscribers needs the sandbox's labels; a missing
	// record only costs label-filtered subscribers this one event.
	var labels map[string]string
	if rec, err := s.store.GetSandbox(sandboxID); err == nil && rec != nil {
		labels = rec.Labels
	}
	s.bus.publish(ev, labels)
	s.metrics.IncCounter("bean_events_total", "Lifecycle events emitted by type.",
		map[string]string{"type": typ}, 1)
	s.metrics.SetGauge("bean_event_subscribers", "Live event stream subscribers.",
		nil, float64(s.bus.subscriberCount()))
}

func execOutcome(err error) string {
	if err == nil {
		return "success"
	}
	return "error"
}

// handleEventStream streams lifecycle events as SSE until the client
// disconnects. Filters: ?sandbox=<id> and/or ?label=k=v.
func (s *Server) handleEventStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "streaming unsupported")
		return
	}
	labelKey, labelVal := parseLabelFilter(r.URL.Query().Get("label"))
	sub, unsubscribe := s.bus.subscribe(r.URL.Query().Get("sandbox"), labelKey, labelVal)
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	// Comment line so clients see a response immediately.
	_, _ = io.WriteString(w, ": connected\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub.ch:
			if !ok {
				return
			}
			payload, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, payload); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// Package api implements the bean-api REST gateway.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/garysng/bean/internal/control/image"
	"github.com/garysng/bean/internal/control/scheduler"
	"github.com/garysng/bean/internal/control/secret"
	"github.com/garysng/bean/internal/control/snapshot"
	"github.com/garysng/bean/internal/control/store"
	commonv1 "github.com/garysng/bean/internal/gen/bean/common/v1"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/logging"
	"github.com/garysng/bean/internal/obs"
)

const (
	// fileChunkBytes is how much of an upload is held at once while it is forwarded
	// to the node. It replaced a 4 MiB cap on the whole file: the gateway used to
	// buffer the entire body, which meant N concurrent uploads cost N file sizes in
	// the process that also runs the scheduler.
	//
	// 1 MiB because that is what the node's stream already used per frame, so this
	// changes how much is resident rather than how the wire looks. Larger would buy
	// fewer syscalls on an upload already bounded by the network; smaller would cost
	// frames for no gain.
	fileChunkBytes        = 1 << 20 // 1 MiB
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

// QueueingPlacer is implemented by placers that can wait for transient capacity
// rather than rejecting immediately.
//
// Optional so a placer that only knows how to answer now still satisfies Placer.
// The distinction it enables is between capacity that frees itself in seconds
// (create concurrency) and capacity held for a sandbox's lifetime (CPU, memory,
// disk) — waiting helps with the first and is pure added latency on the second.
type QueueingPlacer interface {
	ScheduleWait(ctx context.Context, req *scheduler.Request,
		opts scheduler.WaitOptions) (string, error)
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
	// builds holds in-flight and recently finished builds, which is what the
	// build log and cancel endpoints address. It is per-replica, unlike
	// everything else here: a build's log is only reachable from the gateway that
	// started it, and moving it to the store would mean writing a stream of bytes
	// through SQLite on the build's hot path.
	builds  *buildTracker
	metrics *obs.Registry
	mux     *http.ServeMux
	// identity attributes an image to a caller. Nil means every image is
	// unowned, which is what a deployment behind no identity-aware layer gets.
	identity IdentityFunc
	// createWait is how long a create may wait for create concurrency to drain
	// before being refused. Zero refuses immediately.
	createWait time.Duration
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
	// Identity derives the owner to attribute an image to. Nil leaves every
	// image unowned and every listing unfiltered, which is exactly the
	// behaviour of a deployment from before ownership existed. See
	// identity.go for what is assumed about the layer that supplies it.
	Identity IdentityFunc
	// CreateWait is how long a create waits for create concurrency to drain
	// before being refused. Zero refuses immediately, which is the historical
	// behaviour.
	//
	// It applies only to create concurrency, which frees itself in seconds. A
	// request that does not fit on CPU, memory or disk is refused without waiting:
	// those commitments are held for a sandbox's lifetime, so waiting would return
	// the same answer later and hold a client meanwhile.
	CreateWait time.Duration
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
		bus: newEventBus(), builds: newBuildTracker(),
		metrics: obs.NewRegistry(), mux: http.NewServeMux(),
		createWait: opts.CreateWait, identity: opts.Identity}
	s.routes()
	return s
}

// nodeClientFor resolves the SandboxService client owning a sandbox.
func (s *Server) nodeClientFor(rec *store.Sandbox) (nodev1.SandboxServiceClient, error) {
	return s.router.Client(rec.NodeID)
}

func (s *Server) Handler() http.Handler { return s.traceMiddleware(s.authMiddleware(s.mux)) }

// traceMiddleware opens the root span for a request and seeds the request id.
//
// It wraps auth rather than sitting inside it so that a rejected request is
// still one span: an authentication failure is a thing worth seeing in a trace,
// and putting the span inside auth would make those the only invisible requests.
//
// The request id is derived from the trace id rather than generated separately.
// Two ids for one request means every correlation needs a join, and the one
// place they would inevitably diverge is the path that matters — a request that
// crossed a process boundary.
func (s *Server) traceMiddleware(next http.Handler) http.Handler {
	tracer := obs.Tracer("bean-api")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		ctx := obs.HTTPExtract(r.Context(), propagation.HeaderCarrier(r.Header))
		ctx, span := tracer.Start(ctx, r.Method+" "+r.URL.Path,
			trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()

		if id := obs.TraceIDFrom(ctx); id != "" {
			ctx = logging.WithRequest(ctx, id)
			// Returning the id lets a caller reporting a slow request name
			// the trace to look up, instead of correlating by timestamp.
			w.Header().Set("X-Bean-Trace-Id", id)
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

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
	s.mux.HandleFunc("POST /v1/images/build", s.handleBuild)
	// ref goes in a query param for the same reason as image status: it contains
	// slashes, which a path segment cannot carry.
	s.mux.HandleFunc("GET /v1/images/build/logs", s.handleBuildLogs)
	s.mux.HandleFunc("POST /v1/images/build/cancel", s.handleBuildCancel)
	s.mux.HandleFunc("GET /v1/images/prewarm/{jobId}", s.handlePrewarmStatus)
	s.mux.HandleFunc("PUT /v1/registries", s.handlePutRegistry)
	s.mux.HandleFunc("GET /v1/registries", s.handleListRegistries)
	s.mux.HandleFunc("DELETE /v1/registries/{host}", s.handleDeleteRegistry)
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/snapshot", s.handleCreateSnapshot)
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/fork", s.handleFork)
	s.mux.HandleFunc("POST /v1/sandboxes/{id}/commit", s.handleCommit)
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

// place schedules a request, waiting for transient capacity when the placer
// supports it and a wait is configured.
//
// The context is the request's own, so a client that hangs up stops the wait
// rather than leaving the gateway holding a slot for nobody.
func (s *Server) place(ctx context.Context, req *scheduler.Request) (string, error) {
	if s.createWait <= 0 {
		return s.placer.Schedule(req)
	}
	q, ok := s.placer.(QueueingPlacer)
	if !ok {
		return s.placer.Schedule(req)
	}
	return q.ScheduleWait(ctx, req, scheduler.WaitOptions{Timeout: s.createWait})
}

// fault is an error that already knows how it should be reported.
//
// It exists because some operations produce several independently failing
// results in one request -- forking N children, say -- so the mapping from
// cause to status code has to be decided where the cause is known and carried
// back, rather than written to the ResponseWriter on the spot. A single-result
// handler unwraps it with writeFault and gets exactly what it would have
// written itself.
type fault struct {
	status int
	code   string
	msg    string
}

func (f *fault) Error() string { return f.code + ": " + f.msg }

func faultf(status int, code, format string, args ...any) *fault {
	return &fault{status: status, code: code, msg: fmt.Sprintf(format, args...)}
}

// asFault maps any error onto a reportable one, so a caller never has to decide
// what an unclassified error means.
func asFault(err error) *fault {
	var f *fault
	if errors.As(err, &f) {
		return f
	}
	return grpcFault(err)
}

func writeFault(w http.ResponseWriter, err error) {
	f := asFault(err)
	writeErr(w, f.status, f.code, f.msg)
}

// grpcFault translates a node's gRPC status into the gateway's vocabulary.
func grpcFault(err error) *fault {
	st, _ := status.FromError(err)
	switch st.Code() {
	case codes.NotFound:
		return &fault{http.StatusNotFound, "SANDBOX_NOT_FOUND", st.Message()}
	case codes.InvalidArgument:
		return &fault{http.StatusBadRequest, "INVALID_ARGUMENT", st.Message()}
	case codes.FailedPrecondition:
		return &fault{http.StatusConflict, "SANDBOX_NOT_RUNNING", st.Message()}
	case codes.DeadlineExceeded:
		return &fault{http.StatusGatewayTimeout, "TIMEOUT", st.Message()}
	case codes.ResourceExhausted:
		// A node declining work for want of capacity is the same answer the
		// scheduler gives when it can place nothing, so it gets the same code: 503
		// tells a client to retry, where 500 tells it to report a bug.
		return &fault{http.StatusServiceUnavailable, "NO_CAPACITY", st.Message()}
	default:
		return &fault{http.StatusInternalServerError, "INTERNAL", st.Message()}
	}
}

func grpcToHTTP(w http.ResponseWriter, err error) {
	f := grpcFault(err)
	writeErr(w, f.status, f.code, f.msg)
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
	// state) exists for anything the platform has been asked to run. This is
	// also where an operator's image policy is applied, before any capacity is
	// reserved: a refused image should cost the cluster nothing.
	if s.images != nil && req.Image != "" {
		if _, err := s.images.ResolveFor(req.Image, s.owner(r)); err != nil {
			// A policy refusal is a statement about what this deployment
			// permits, so it is 403 with its own code: a caller can tell it
			// apart from a malformed ref and knows retrying will not help.
			if errors.Is(err, image.ErrPolicyDenied) {
				outcome = "image_denied"
				writeErr(w, http.StatusForbidden, "IMAGE_NOT_PERMITTED", err.Error())
				return
			}
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
	nodeID, err := s.place(r.Context(), s.placementFor(rec))
	if err != nil {
		outcome = "no_capacity"
		code := http.StatusServiceUnavailable
		if errors.Is(err, scheduler.ErrQueueTimeout) {
			// The request was admissible and the node was merely busy for longer
			// than the wait allowed, so this is a timeout rather than a statement
			// about capacity. 504 tells a caller to retry with more patience; 503
			// would suggest the cluster is too small.
			outcome = "queue_timeout"
			code = http.StatusGatewayTimeout
			writeErr(w, code, "QUEUE_TIMEOUT", err.Error())
			return
		}
		writeErr(w, code, "NO_CAPACITY", err.Error())
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
		slog.Error("cannot release reservation", logging.KeySandbox, rec.ID, logging.KeyError, err)
	}
}

// releasePlacement returns capacity for a sandbox that has stopped.
func (s *Server) releasePlacement(rec *store.Sandbox) {
	if err := s.placer.Release(rec.ID); err != nil {
		slog.Error("cannot release reservation", logging.KeySandbox, rec.ID, logging.KeyError, err)
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
		slog.Error("cannot write metrics", logging.KeyError, err)
	}
}

// refreshStateGauges recomputes sandbox counts per state at scrape time,
// which keeps the counters authoritative even after a restart.
func (s *Server) refreshStateGauges() {
	recs, err := s.store.ListSandboxes("", "", "")
	if err != nil {
		slog.Error("metrics cannot list sandboxes", logging.KeyError, err)
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

	// Streamed chunk by chunk rather than read whole and then forwarded.
	//
	// This used to be io.ReadAll into a byte slice, which put the entire upload in
	// gateway memory before any of it reached the node. Two things were wrong with
	// that, and only the second was visible:
	//
	//   - N concurrent uploads cost N times the file size in the gateway, which is
	//     the process that also runs the scheduler. A burst of uploads and a burst of
	//     creates contend for the same heap, so file traffic could make placement slow
	//     -- and nothing in the create path would say why.
	//   - It forced a size cap, because a request that buffers has to bound what it
	//     buffers. The cap was 4 MiB and the error told the caller to "use presigned
	//     flow", which does not exist anywhere in this codebase: an instruction to do
	//     something impossible, which is worse than a bare limit.
	//
	// The read path already streamed (handleReadFile below, chunk by chunk with no
	// accumulation). The asymmetry was the defect rather than the design.
	//
	// The cap is gone with the buffer. What bounds an upload now is the sandbox's own
	// disk, enforced where the bytes land -- which is where it belongs, since the
	// gateway has no basis for a figure and 4 MiB was small enough to reject an
	// ordinary source tree.
	buf := make([]byte, fileChunkBytes)
	for {
		n, rerr := r.Body.Read(buf)
		if n > 0 {
			if serr := ws.Send(&commonv1.WriteFileFrame{
				Frame: &commonv1.WriteFileFrame_Data{Data: buf[:n]},
			}); serr != nil {
				grpcToHTTP(w, serr)
				return
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			// A client that hung up mid-upload leaves a partial file, which the node
			// owns: the stream is abandoned rather than closed, so the node's own
			// handler sees the failure and cleans up. Reporting here as a client error
			// rather than a server one, because that is what it is.
			writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", rerr.Error())
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
			slog.Error("readFile failed mid-stream", logging.KeySandbox, id, "path", path, logging.KeyError, rerr)
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
			slog.Error("logs failed mid-stream", logging.KeySandbox, id, logging.KeyError, rerr)
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
		slog.Error("cannot emit event", logging.KeySandbox, sandboxID, "event", typ, logging.KeyError, err)
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

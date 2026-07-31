// Package api implements the bean-api REST gateway.
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
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

	"github.com/garysng/bean/internal/control/store"
	commonv1 "github.com/garysng/bean/internal/gen/bean/common/v1"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
)

const maxInlineFileBytes = 4 << 20 // 4 MiB

// Server is the REST gateway. P0: single node, direct beand connection.
type Server struct {
	store  *store.Store
	node   nodev1.SandboxServiceClient
	nodeID string
	apiKey string
	mux    *http.ServeMux
}

func NewServer(st *store.Store, nodeClient nodev1.SandboxServiceClient, nodeID, apiKey string) *Server {
	s := &Server{store: st, node: nodeClient, nodeID: nodeID, apiKey: apiKey, mux: http.NewServeMux()}
	s.routes()
	return s
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
	s.mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

// ---- auth / errors ----

func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
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
	Image     string `json:"image"`
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
	var req createRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid JSON: "+err.Error())
		return
	}
	if req.Image == "" {
		writeErr(w, http.StatusBadRequest, "IMAGE_REF_INVALID", "image is required")
		return
	}

	id := "sbx_" + randHex(10)
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
	rec := &store.SandboxRecord{
		ID: id, Image: req.Image, State: "PENDING", NodeID: s.nodeID,
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

	if err := s.store.PutSandbox(rec); err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	s.emit(id, "sandbox.lifecycle.created", nil)

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	resp, err := s.node.CreateSandbox(ctx, &nodev1.CreateSandboxRequest{Spec: spec})
	if err != nil {
		rec.State = "FAILED"
		rec.Reason = err.Error()
		_ = s.store.PutSandbox(rec)
		s.emit(id, "sandbox.lifecycle.failed", map[string]string{"reason": err.Error()})
		grpcToHTTP(w, err)
		return
	}
	rec.State = resp.Status.State
	_ = s.store.PutSandbox(rec)
	s.emit(id, "sandbox.lifecycle.running", nil)

	writeJSON(w, http.StatusCreated, map[string]any{"sandbox": rec})
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
	recs, err := s.store.ListSandboxes(labelKey, labelVal, r.URL.Query().Get("state"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "INTERNAL", err.Error())
		return
	}
	if recs == nil {
		recs = []*store.SandboxRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": recs})
}

func (s *Server) loadSandbox(w http.ResponseWriter, id string) *store.SandboxRecord {
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

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	rec := s.loadSandbox(w, id)
	if rec == nil {
		return
	}
	// Refresh live state from the node for non-terminal sandboxes.
	if rec.State != "STOPPED" && rec.State != "FAILED" {
		if st, err := s.node.GetSandbox(r.Context(), &nodev1.GetSandboxRequest{SandboxId: id}); err == nil {
			rec.State = st.Status.State
			_ = s.store.PutSandbox(rec)
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
	if _, err := s.node.DestroySandbox(r.Context(), &nodev1.DestroySandboxRequest{SandboxId: id, Force: force}); err != nil {
		grpcToHTTP(w, err)
		return
	}
	rec.State = "STOPPED"
	_ = s.store.PutSandbox(rec)
	s.emit(id, "sandbox.lifecycle.stopped", nil)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.loadSandbox(w, id) == nil {
		return
	}
	if _, err := s.node.PauseSandbox(r.Context(), &nodev1.PauseSandboxRequest{SandboxId: id}); err != nil {
		grpcToHTTP(w, err)
		return
	}
	s.updateState(id, "PAUSED")
	s.emit(id, "sandbox.lifecycle.paused", nil)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.loadSandbox(w, id) == nil {
		return
	}
	if _, err := s.node.ResumeSandbox(r.Context(), &nodev1.ResumeSandboxRequest{SandboxId: id}); err != nil {
		grpcToHTTP(w, err)
		return
	}
	s.updateState(id, "RUNNING")
	s.emit(id, "sandbox.lifecycle.resumed", nil)
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) updateState(id, state string) {
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
	if s.loadSandbox(w, id) == nil {
		return
	}
	var req execRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}
	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout+10*time.Second)
	defer cancel()
	resp, err := s.node.Exec(ctx, &commonv1.ExecRequest{
		SandboxId:      id,
		Cmd:            req.Cmd,
		Cwd:            req.Cwd,
		Env:            req.Env,
		TimeoutSeconds: req.TimeoutSeconds,
		Stdin:          req.Stdin,
		MaxOutputBytes: req.MaxOutputBytes,
	})
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
	if s.loadSandbox(w, id) == nil {
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
	ws, err := s.node.WriteFile(r.Context())
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
	if s.loadSandbox(w, id) == nil {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "path query param required")
		return
	}
	stream, err := s.node.ReadFile(r.Context(), &commonv1.ReadFileRequest{SandboxId: id, Path: path})
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
		_, _ = w.Write(first.Data)
	}
	for {
		chunk, rerr := stream.Recv()
		if rerr != nil {
			return
		}
		if _, werr := w.Write(chunk.Data); werr != nil {
			return
		}
	}
}

func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.loadSandbox(w, id) == nil {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErr(w, http.StatusBadRequest, "INVALID_ARGUMENT", "path query param required")
		return
	}
	if _, err := s.node.DeleteFile(r.Context(), &commonv1.DeleteFileRequest{SandboxId: id, Path: path}); err != nil {
		grpcToHTTP(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListDir(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.loadSandbox(w, id) == nil {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}
	resp, err := s.node.ListDir(r.Context(), &commonv1.ListDirRequest{SandboxId: id, Path: path})
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
	if s.loadSandbox(w, id) == nil {
		return
	}
	tail := 0
	if ts := r.URL.Query().Get("tailLines"); ts != "" {
		tail, _ = strconv.Atoi(ts)
	}
	stream, err := s.node.GetLogs(r.Context(), &commonv1.GetLogsRequest{SandboxId: id, TailLines: int32(tail)})
	if err != nil {
		grpcToHTTP(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for {
		chunk, rerr := stream.Recv()
		if rerr != nil {
			return
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
	if err := s.store.AppendEvent(&store.Event{
		Type: typ, Timestamp: time.Now(), SandboxID: sandboxID, Data: data, Version: "v1",
	}); err != nil {
		log.Printf("emit event %s %s: %v", sandboxID, typ, err)
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

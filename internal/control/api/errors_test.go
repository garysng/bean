package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestGrpcToHTTPMapping pins the gRPC -> HTTP contract documented in
// docs/api-design.md §3.
func TestGrpcToHTTPMapping(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantHTTP int
		wantCode string
	}{
		{"not found", status.Error(codes.NotFound, "gone"), http.StatusNotFound, "SANDBOX_NOT_FOUND"},
		{"invalid arg", status.Error(codes.InvalidArgument, "bad"), http.StatusBadRequest, "INVALID_ARGUMENT"},
		{"not running", status.Error(codes.FailedPrecondition, "paused"), http.StatusConflict, "SANDBOX_NOT_RUNNING"},
		{"timeout", status.Error(codes.DeadlineExceeded, "slow"), http.StatusGatewayTimeout, "TIMEOUT"},
		{"internal", status.Error(codes.Internal, "boom"), http.StatusInternalServerError, "INTERNAL"},
		{"unknown maps to internal", status.Error(codes.Unavailable, "down"), http.StatusInternalServerError, "INTERNAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			grpcToHTTP(rec, tc.err)
			if rec.Code != tc.wantHTTP {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantHTTP)
			}
			var body apiError
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body not JSON: %v", err)
			}
			if body.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", body.Error.Code, tc.wantCode)
			}
			if body.Error.Message == "" {
				t.Error("message must not be empty")
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content-type = %q", ct)
			}
		})
	}
}

func TestWriteErrAndJSONShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeErr(rec, http.StatusTooManyRequests, "RATE_LIMITED", "slow down")
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d", rec.Code)
	}
	if got := rec.Body.String(); !json.Valid([]byte(got)) {
		t.Errorf("invalid JSON: %q", got)
	}

	rec = httptest.NewRecorder()
	writeJSON(rec, http.StatusCreated, map[string]string{"a": "b"})
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d", rec.Code)
	}
	var out map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out["a"] != "b" {
		t.Errorf("body = %q err=%v", rec.Body.String(), err)
	}
}

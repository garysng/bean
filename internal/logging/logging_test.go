package logging

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"debug": slog.LevelDebug,
		"DEBUG": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
		// An unrecognised level must not stop a process from starting, so it
		// falls back to info rather than erroring.
		"":         slog.LevelInfo,
		"vebrose":  slog.LevelInfo,
		"critical": slog.LevelInfo,
	} {
		if got := parseLevel(in); got != want {
			t.Errorf("parseLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestRequestIDTravelsOnTheContext(t *testing.T) {
	ctx := context.Background()
	if got := RequestFrom(ctx); got != "" {
		t.Errorf("empty context = %q", got)
	}

	ctx = WithRequest(ctx, "req_1")
	if got := RequestFrom(ctx); got != "req_1" {
		t.Errorf("RequestFrom = %q", got)
	}

	// An empty id must not overwrite one already there: a handler that did not
	// receive an id should not erase the caller's.
	if got := RequestFrom(WithRequest(ctx, "")); got != "req_1" {
		t.Errorf("empty id overwrote the existing one: %q", got)
	}
}

// TestFromAnnotatesWithTheRequestID checks the property the whole mechanism is
// for: a line logged deep in a call chain carries the id without the code there
// knowing about it.
func TestFromAnnotatesWithTheRequestID(t *testing.T) {
	var buf syncBuffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))

	From(WithRequest(context.Background(), "req_42")).
		Info("sandbox created", KeySandbox, "sbx_1")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %v (%s)", err, buf.String())
	}
	if rec[KeyRequest] != "req_42" {
		t.Errorf("request field = %v", rec[KeyRequest])
	}
	if rec[KeySandbox] != "sbx_1" {
		t.Errorf("sandbox field = %v", rec[KeySandbox])
	}
	// The message stays a message; the identifiers are separate fields, which is
	// the point of the change.
	if rec["msg"] != "sandbox created" {
		t.Errorf("msg = %v", rec["msg"])
	}
}

func TestFromWithoutARequestIDOmitsTheField(t *testing.T) {
	var buf syncBuffer
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))

	From(context.Background()).Info("node registered")

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec[KeyRequest]; ok {
		// An empty request field is worse than none: it looks like a correlation
		// that failed rather than one that was never applicable.
		t.Errorf("emitted an empty request field: %s", buf.String())
	}
}

// syncBuffer is a minimal io.Writer for capturing one log line.
type syncBuffer struct{ b []byte }

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.b = append(s.b, p...)
	return len(p), nil
}
func (s *syncBuffer) Bytes() []byte  { return s.b }
func (s *syncBuffer) String() string { return string(s.b) }

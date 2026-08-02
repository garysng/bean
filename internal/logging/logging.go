// Package logging configures the process logger.
//
// Log lines are read by two things: a person tailing a terminal, and whatever
// collects logs in production. A `log.Printf` line serves the first and defeats
// the second — filtering by severity, aggregating by sandbox, or following one
// request across the gateway and a node all require the fields to be separate
// from the prose.
package logging

import (
	"context"
	"log/slog"
	"os"
	"strings"
)

// Setup installs the process logger and returns it.
//
// format is "text" for a person or "json" for a collector; level is one of
// debug, info, warn, error. Both are wrong often enough by typo that an
// unrecognised value falls back rather than failing to start: a node that
// refuses to boot over a log setting is worse than one that logs verbosely.
func Setup(format, level string) *slog.Logger {
	var h slog.Handler
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	if strings.EqualFold(format, "json") {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Field names shared across components. A collector can only group by
// sandbox or follow a request if every component spells the key the same way.
const (
	KeySandbox  = "sandbox"
	KeyNode     = "node"
	KeySnapshot = "snapshot"
	KeyImage    = "image"
	KeyRequest  = "request"
	KeyError    = "error"
)

// requestKey carries a request id through a call chain. A create touches the
// gateway, the scheduler and a node, so correlating them needs an id that
// travels with the context rather than being passed by hand through signatures
// that have no other use for it.
type requestKey struct{}

// WithRequest returns a context carrying a request id.
func WithRequest(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, requestKey{}, id)
}

// RequestFrom returns the request id in a context, or "".
func RequestFrom(ctx context.Context) string {
	id, _ := ctx.Value(requestKey{}).(string)
	return id
}

// From returns a logger annotated with the context's request id, so a caller
// does not have to remember to add it at every call site.
func From(ctx context.Context) *slog.Logger {
	l := slog.Default()
	if id := RequestFrom(ctx); id != "" {
		return l.With(KeyRequest, id)
	}
	return l
}

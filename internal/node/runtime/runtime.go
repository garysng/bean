// Package runtime defines the sandbox runtime abstraction and implementations.
package runtime

import (
	"context"
	"time"
)

// State mirrors the sandbox state machine (subset owned by the node).
type State string

const (
	StatePulling  State = "PULLING"
	StateStarting State = "STARTING"
	StateRunning  State = "RUNNING"
	StatePausing  State = "PAUSING"
	StatePaused   State = "PAUSED"
	StateResuming State = "RESUMING"
	StateStopped  State = "STOPPED"
	StateFailed   State = "FAILED"
)

// Spec is the node-side sandbox spec (subset of proto SandboxSpec).
type Spec struct {
	SandboxID    string
	Image        string
	CPU          float64
	MemoryMiB    int64
	DiskMiB      int64
	Env          map[string]string
	Cmd          []string
	AutoStartCmd bool
}

// Handle represents a created sandbox instance.
type Handle struct {
	SandboxID  string
	AgentAddr  string // dialable agent address, e.g. unix:///path or vsock target
	StartedAt  time.Time
	PID        int // primary host process (fc process or local agent), 0 if n/a
	RuntimeTag string
}

// Runtime creates and manages sandbox instances.
type Runtime interface {
	Name() string
	Create(ctx context.Context, spec *Spec) (*Handle, error)
	Destroy(ctx context.Context, id string, force bool) error
	Pause(ctx context.Context, id string) error
	Resume(ctx context.Context, id string) error
}

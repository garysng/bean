// Package runtime defines the sandbox runtime abstraction and implementations.
package runtime

import (
	"context"
	"io"
	"time"
)

// State mirrors the sandbox state machine (subset owned by the node).
type State string

const (
	StatePulling      State = "PULLING"
	StateStarting     State = "STARTING"
	StateRunning      State = "RUNNING"
	StatePausing      State = "PAUSING"
	StatePaused       State = "PAUSED"
	StateResuming     State = "RESUMING"
	StateSnapshotting State = "SNAPSHOTTING"
	StateRestoring    State = "RESTORING"
	StateStopped      State = "STOPPED"
	StateFailed       State = "FAILED"
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

// Runtime creates and manages sandbox instances. Implementations are
// interchangeable from the Manager's point of view, which is what lets the
// same control plane drive process-level sandboxes in dev and microVMs in
// production.
type Runtime interface {
	Name() string
	Create(ctx context.Context, spec *Spec) (*Handle, error)
	Destroy(ctx context.Context, id string, force bool) error
	Pause(ctx context.Context, id string) error
	Resume(ctx context.Context, id string) error

	// Checkpoint writes a restorable representation of the sandbox to w.
	// The format is runtime-specific and not interchangeable between
	// tiers, which is why a snapshot records the runtime that produced it.
	Checkpoint(ctx context.Context, id string, w io.Writer) error

	// Restore creates a sandbox from a checkpoint previously written by the
	// same runtime. The spec supplies identity and resources; the
	// checkpoint supplies the filesystem (and, for microVMs, memory).
	Restore(ctx context.Context, spec *Spec, r io.Reader) (*Handle, error)
}

// ImageWarmer is implemented by runtimes that can make an image ready before a
// sandbox needs it. It is separate from Runtime because not every tier has a
// meaningful notion of a cached image — LocalRuntime runs a host binary — and a
// method that all implementations had to stub would say less than an optional
// one that callers check for.
type ImageWarmer interface {
	// PrewarmImage blocks until the image can be used without a further fetch.
	PrewarmImage(ctx context.Context, imageRef string) error
}

// ImageLister is implemented by runtimes that hold a local image cache. The node
// reports this so the scheduler can prefer a node that already has an image, and
// so a prewarm job can show progress.
type ImageLister interface {
	// CachedImages maps image reference to its size on this node.
	CachedImages() (map[string]int64, error)
}

// SandboxCommitter is implemented by runtimes that can turn a sandbox's
// filesystem into a base image.
//
// This is not a snapshot: the result has no memory state and is not bound to the
// runtime that produced it, so other sandboxes — on any tier — can start from
// it. The sandbox must be paused for the read to be consistent, which the caller
// arranges since only it knows whether the sandbox should keep running.
type SandboxCommitter interface {
	CommitSandbox(ctx context.Context, id, tag string) error
}

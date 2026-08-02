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
	SandboxID string
	// SnapshotID identifies the checkpoint a restore comes from, so a node can
	// reuse state it has already unpacked. Empty for a cold start.
	SnapshotID   string
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
	Checkpoint(ctx context.Context, id string, w io.Writer, opts CheckpointOptions) error

	// Restore creates a sandbox from checkpoints previously written by the same
	// runtime. The spec supplies identity and resources; the checkpoints supply
	// the filesystem (and, for microVMs, memory).
	//
	// Layers are ordered base-first and read in order. A self-contained
	// checkpoint is a single layer; an incremental one is its whole chain, since
	// a diff holds only what changed since its base and cannot be restored alone.
	Restore(ctx context.Context, spec *Spec, layers []SnapshotLayer) (*Handle, error)
}

// SnapshotLayer is one checkpoint in a restore chain.
//
// Order is the caller's contract and cannot be recovered from the data: a diff's
// pages legitimately overwrite its base's, so a chain replayed out of order
// yields a coherent-looking result assembled from stale pages, which nothing
// downstream can detect.
type SnapshotLayer struct {
	// ID identifies the checkpoint, so a failure names which layer of a chain was
	// bad rather than only that one was.
	ID string
	// Data is the layer's bundle. Layers are consumed in order, exactly once.
	Data io.Reader
}

// CheckpointOptions selects what a checkpoint captures.
type CheckpointOptions struct {
	// IncludeMemory captures guest memory and device state, so a restore
	// resumes the running guest — its process tree, open files and in-memory
	// state survive.
	//
	// The cost is portability. Guest memory records decisions the guest made
	// from the CPU it booted on, and its vendor and family cannot be masked
	// (see cpu_template.go), so such a checkpoint can only be restored on a
	// compatible CPU. Without memory the checkpoint is just a filesystem and
	// restores anywhere, but the guest boots fresh rather than resuming.
	//
	// The two are genuinely different operations rather than a size trade-off,
	// which is why this is a caller's choice and not a heuristic.
	IncludeMemory bool

	// Diff captures only the guest memory written since the sandbox started or
	// was restored, rather than all of it. The result is not restorable on its
	// own: it has to be layered onto the checkpoint it descends from.
	//
	// This only makes sense with IncludeMemory — the filesystem layer is already
	// proportional to what changed, because it is stored as an extent list over a
	// copy-on-write device.
	//
	// It requires the guest to have booted with dirty-page tracking on. A
	// runtime that cannot honour this returns an error rather than falling back
	// to a full capture: the caller asked for a cheap checkpoint and needs to
	// know it did not get one.
	Diff bool
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

// ImageBuilder is implemented by runtimes on nodes that can build images.
//
// Building is optional per node: it needs BuildKit, and a cluster may well want
// dedicated builder nodes rather than every sandbox host carrying the dependency.
type ImageBuilder interface {
	// BuildImage builds a base image and returns its reference.
	BuildImage(ctx context.Context, req BuildRequest) (string, error)
}

// BuildRequest describes a build at the runtime boundary. It mirrors the node's
// image.BuildRequest without importing it, so the runtime interface does not
// depend on the image package's internals.
type BuildRequest struct {
	Tag        string
	Dockerfile string
	ContextTar []byte
	BuildArgs  map[string]string
	SizeMiB    int64
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

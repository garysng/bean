// Package runtime defines the sandbox runtime abstraction and implementations.
package runtime

import (
	"context"
	"io"
	"net"
	"time"

	"github.com/garysng/bean/internal/node/image"
	"github.com/garysng/bean/internal/node/network"
)

// AgentGuestPort is the TCP port the agent listens on inside a networked guest.
//
// It exists so the agent is reached exactly the way any port a user exposes is
// reached -- connect to the guest on a port -- which is what lets one router serve
// both instead of a rule plus a special case.
//
// Fixed rather than allocated: each sandbox has its own namespace and its own guest,
// so there is nothing to collide with, and a constant keeps the guest's command line
// independent of host state.
//
// Reserved. A user exposing this port would be exposing the agent, so anything that
// maps ports on a user's behalf must refuse it.
//
// Declared in the portable file rather than beside the microVM code because it is a
// protocol constant that the forwarder and the proxy also need, on every platform.
const AgentGuestPort = 10001

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

	// Network is the addressing this sandbox was assigned, or nil on a node with
	// no networking configured.
	//
	// Nil has to keep working: sandboxes ran without any interface at all before
	// this existed, and a node that cannot set up namespaces should still start
	// them rather than refuse every create. So this is a pointer and the absence
	// of one means no interface is registered, not a zero-valued one.
	//
	// It rides on the spec rather than on runtime configuration because the slot
	// is per-sandbox: the host end of the veth pair is derived from an index, and
	// only the caller that reserved that index knows which one this sandbox got.
	// Everything else here is per-sandbox for the same reason.
	Network *network.Layout

	// AgentTokenHash is what the guest should expect from callers of its agent. It
	// is published through the metadata service, which the sandbox's own root can
	// read -- hence the hash rather than the token, which is enough to verify one
	// that is presented and useless for constructing one.
	//
	// Empty leaves the agent unauthenticated, which is correct only where reaching
	// it does not depend on a credential: the container runtime's agent listens on a
	// Unix socket outside the sandbox's mount namespace.
	AgentTokenHash string
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

	// Fork creates a sandbox from checkpoints previously written by the same
	// runtime. The spec supplies identity and resources; the checkpoints supply
	// the filesystem (and, for microVMs, memory).
	//
	// Named for what it does rather than for what it is usually called: one
	// checkpoint can be the source of any number of these, each getting its own
	// copy-on-write layer over shared read-only state, so the result is an
	// independent instance rather than the recovery of a particular one. Calling
	// it "restore" invited the assumption that a checkpoint is consumed, and it
	// is not.
	//
	// Layers are ordered base-first and read in order. A self-contained
	// checkpoint is a single layer; an incremental one is its whole chain, since
	// a diff holds only what changed since its base and cannot be used alone.
	Fork(ctx context.Context, spec *Spec, layers []SnapshotLayer) (*Handle, error)
}

// BootDiagnoser reports what a sandbox's console said, for runtimes that have one.
//
// It exists because of a specific failure that cost an afternoon: the agent was
// passed a flag it did not recognise, exited immediately, and the guest kernel
// panicked with "Attempted to kill init!". What noded reported was "agent not
// healthy after 20s" -- the timeout is the *symptom* of any guest that never
// finishes booting, and it names none of them. The cause was one line of
// console.log, on disk, in a directory the failure path then deleted.
//
// So this is not a convenience. A boot failure that leaves no diagnosis in the
// error is indistinguishable from a slow boot, a broken vsock, an unbootable image
// and a wrong kernel, and every one of those has been mistaken for another.
//
// Optional because only the microVM tier has a console at all; the container
// runtime's agent failures surface through the container's own logs.
type BootDiagnoser interface {
	// BootLogTail returns the last few lines of the guest console, or "" if there
	// is nothing to report. Errors are not returned: this is called on a path that
	// is already failing, and a diagnostic that can itself fail the create would
	// replace one confusing error with another.
	BootLogTail(id string, lines int) string
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

// SnapshotWarmer is implemented by runtimes that can hold one booted-and-
// checkpointed guest per image, so a create restores instead of booting.
//
// Separate from ImageWarmer because preparing an image file and capturing a booted
// guest are different costs with different lifetimes: the first removes a pull, the
// second removes the ~5 CPU-seconds of a boot, and only the second is bound to the
// CPU it was taken on. A tier can meaningfully do the first and not the second.
type SnapshotWarmer interface {
	// WarmKeyFor reports the key a warm snapshot for this image would have on this
	// node, and whether the image can be warmed at all. An image with no digest
	// cannot: see warmKey's documentation for why a tag is not a safe substitute.
	WarmKeyFor(imageRef string) (key string, ok bool, err error)
	// WarmLookup reports whether this node already holds a warm snapshot for the
	// image, returning the layer a restore would use.
	WarmLookup(imageRef string) (layer SnapshotLayer, release func(), ok bool)
	// WarmStore writes a checkpoint of a running sandbox as the warm snapshot for
	// an image. The caller has already established that the sandbox is a faithful
	// freshly-booted instance of it.
	WarmStore(ctx context.Context, imageRef, sandboxID string) error
	// WarmEnabled reports whether the node is configured to use warm snapshots at
	// all. A node that is not must behave exactly as it did before they existed.
	WarmEnabled() bool
}

// ImageLister is implemented by runtimes that hold a local image cache. The node
// reports this so the scheduler can prefer a node that already has an image, and
// so a prewarm job can show progress.
type ImageLister interface {
	// CachedImages maps image reference to what this node knows about it.
	CachedImages() (map[string]image.CachedImage, error)
}

// CacheReporter is implemented by runtimes that hold node-local caches whose size
// the control plane should know about.
//
// It exists because these caches are otherwise invisible: they consume no
// commitment, so a node can fill its disk with them while placement still
// believes it has room. Reporting them does not make them a resource the
// scheduler allocates — it makes the gap between committed and actual disk
// something an operator can see before it becomes an incident.
type CacheReporter interface {
	// SnapshotCacheBytes is the space held by unpacked snapshots, in allocated
	// blocks rather than apparent size: a merged memory image is sparse where no
	// ancestor wrote, and its apparent size overstates it by orders of magnitude.
	SnapshotCacheBytes() (int64, error)
}

// SandboxAddresser is implemented by runtimes whose sandboxes are not reached at the
// guest address the network layout assigns.
//
// The layout gives every sandbox a tap with GuestIP on it and a veth pair joining the
// namespace to the host. A microVM's guest kernel brings the tap up and configures
// GuestIP, so that is where its processes listen. A container has no guest kernel: the
// tap stays DOWN, GuestIP exists nowhere, and its processes are reachable at the
// namespace end of the veth instead.
//
// Optional because the tap address is right for the tier that has been here longest,
// and a caller that finds this unimplemented keeps using it -- which is what every
// forward did before a container tier existed.
//
// This is the second place the distinction mattered. The first was the agent's own
// address, fixed when a create failed with "network is unreachable"; port forwarding
// is a separate path and kept dialling GuestIP, which surfaced as "no route to host"
// on a port that was plainly listening. Behind one interface now, so a third caller
// does not have to rediscover it.
type SandboxAddresser interface {
	// SandboxIP reports the address a sandbox's own processes listen on, or nil to
	// use the layout's guest address.
	SandboxIP(l *network.Layout) net.IP
}

// ImageConfigReader is implemented by runtimes that start sandboxes from OCI images
// and can report what an image declared -- its ENV, ENTRYPOINT, CMD and WORKDIR --
// so a caller can honour it when starting the user process.
//
// Optional for the same reason ImageLister is: LocalRuntime runs a host binary and
// has no image whose configuration it could report. A caller that finds this
// unimplemented starts the process from its own request alone, which is what every
// tier did before image configs were recorded.
type ImageConfigReader interface {
	// ImageConfig reports what the image declared, or nil if this node holds no
	// record of it.
	//
	// Nil is a normal answer rather than an error: an image converted before configs
	// were stored has none, and neither does a build's output.
	ImageConfig(imageRef string) (*image.Config, error)
}

// ImageBuilder is implemented by runtimes on nodes that can build images.
//
// Building is optional per node: it needs BuildKit, and a cluster may well want
// dedicated builder nodes rather than every sandbox host carrying the dependency.
type ImageBuilder interface {
	// BuildImage builds a base image and returns its reference and, when the node
	// published it to a shared store, where the artifact lives.
	BuildImage(ctx context.Context, req BuildRequest) (BuildResult, error)
}

// BuildResult is what a build produced, at the runtime boundary. It mirrors the
// node's image.BuildResult without importing it, so the runtime interface stays free
// of the image package's internals.
type BuildResult struct {
	// ImageRef is the built image's reference, always set on success.
	ImageRef string
	// OverlaybdRef names the published shared artifact, empty when the build stayed
	// node-local (no store configured, or the upload was declined).
	OverlaybdRef string
	// SizeBytes is the published layer's sealed length, zero when nothing was
	// published.
	SizeBytes int64
	// LayerDigests is the published layer chain, base first.
	LayerDigests []string
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
	// Logs receives the builder's output as it is produced. Nil discards it.
	//
	// A writer rather than a returned buffer because the point is to see a build
	// while it runs: a build takes minutes, and output that arrives only at the
	// end cannot tell anyone which layer is stuck. Writes happen on the
	// builder's goroutine, so an implementation that blocks here slows the
	// build down.
	Logs io.Writer
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

package store

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// SandboxState is the control-plane view of the sandbox state machine
// (docs/architecture.md §4.3). The node owns a subset of these and reports
// them through heartbeats.
type SandboxState string

const (
	SandboxPending   SandboxState = "PENDING"
	SandboxScheduled SandboxState = "SCHEDULED"
	SandboxPulling   SandboxState = "PULLING"
	SandboxStarting  SandboxState = "STARTING"
	SandboxRunning   SandboxState = "RUNNING"
	SandboxPaused    SandboxState = "PAUSED"
	// SandboxSnapshotting is transient: the sandbox returns to its prior
	// state once the snapshot completes.
	SandboxSnapshotting SandboxState = "SNAPSHOTTING"
	SandboxRestoring    SandboxState = "RESTORING"
	SandboxStopping     SandboxState = "STOPPING"
	SandboxStopped      SandboxState = "STOPPED"
	SandboxFailed       SandboxState = "FAILED"
	// SandboxLost means the owning node's lease expired; callers rebuild.
	SandboxLost SandboxState = "LOST"
)

// AllSandboxStates lists every state, used to zero out metric gauges so a
// drained state reports 0 instead of keeping a stale value.
func AllSandboxStates() []SandboxState {
	return []SandboxState{
		SandboxPending, SandboxScheduled, SandboxPulling, SandboxStarting,
		SandboxRunning, SandboxPaused, SandboxSnapshotting, SandboxRestoring,
		SandboxStopping, SandboxStopped, SandboxFailed, SandboxLost,
	}
}

// IsTerminal reports whether a sandbox will never run again, so the node is
// not expected to hold it and its capacity can be released.
func IsTerminal(s SandboxState) bool {
	switch s {
	case SandboxStopped, SandboxFailed, SandboxLost:
		return true
	default:
		return false
	}
}

// SnapshotState tracks a snapshot object's own lifecycle, independent of
// the sandbox it came from.
type SnapshotState string

const (
	SnapshotCreating SnapshotState = "CREATING"
	SnapshotReady    SnapshotState = "READY"
	SnapshotDeleting SnapshotState = "DELETING"
	SnapshotFailed   SnapshotState = "FAILED"
)

// ImageState tracks platform-side image preparation. Users only ever
// supply a native OCI reference; conversion is invisible to them.
type ImageState string

const (
	// ImagePending: the ref is registered but nothing has been prepared.
	ImagePending ImageState = "PENDING"
	// ImageBuilding: a platform-side build is producing this image.
	ImageBuilding ImageState = "BUILDING"
	// ImageConverting: an overlaybd conversion is in progress.
	ImageConverting ImageState = "CONVERTING"
	// ImageReady: an overlaybd artifact exists and the fc tier can use it.
	ImageReady  ImageState = "READY"
	ImageFailed ImageState = "FAILED"
)

// NodeState is a node's liveness as the control plane sees it. The scheduler
// places only on READY nodes; the rest describe stages of losing one.
type NodeState string

const (
	// NodeReady: heartbeating and accepting placements.
	NodeReady NodeState = "READY"
	// NodeSuspect: a heartbeat is overdue but the lease has not expired.
	NodeSuspect NodeState = "SUSPECT"
	// NodeLost: the lease expired; its sandboxes are considered gone.
	NodeLost NodeState = "LOST"
	// NodeDraining: still serving its sandboxes, taking no new ones.
	NodeDraining NodeState = "DRAINING"
)

// ImageSource records how an image came to exist, which determines its
// conversion cost (see docs/image-build.md §2).
type ImageSource string

const (
	// ImageImported is a native OCI reference the caller supplied. Its
	// layers are tar.gz and must be converted before the fc tier can use it.
	ImageImported ImageSource = "imported"
	// ImageBuilt was produced by the platform. A commit-built image needs no
	// conversion because an overlaybd writable layer is already LSMT; a
	// BuildKit-built one still does, because BuildKit emits standard OCI.
	ImageBuilt ImageSource = "built"
)

// BuildState tracks a build's progress.
type BuildState string

const (
	BuildPending    BuildState = "PENDING"
	BuildRunning    BuildState = "RUNNING"
	BuildConverting BuildState = "CONVERTING"
	BuildReady      BuildState = "READY"
	BuildFailed     BuildState = "FAILED"
	BuildCancelled  BuildState = "CANCELLED"
)

// IsBuildTerminal reports whether a build will make no further progress.
func IsBuildTerminal(s BuildState) bool {
	switch s {
	case BuildReady, BuildFailed, BuildCancelled:
		return true
	default:
		return false
	}
}

// BuildKind is how the build was described. All kinds compile to the same
// plan, so the executor does not branch on this; it exists for reporting.
type BuildKind string

const (
	// BuildKindDockerfile uses BuildKit for full Dockerfile semantics.
	BuildKindDockerfile BuildKind = "dockerfile"
	// BuildKindSteps is the declarative form; the SDK compiles chained
	// calls into ordered steps.
	BuildKindSteps BuildKind = "steps"
	// BuildKindCommit captures a running sandbox's filesystem.
	BuildKindCommit BuildKind = "commit"
)

// BuildStepKind enumerates the operations a plan step can perform.
type BuildStepKind string

const (
	StepRun     BuildStepKind = "run"
	StepCopy    BuildStepKind = "copy"
	StepEnv     BuildStepKind = "env"
	StepWorkdir BuildStepKind = "workdir"
	StepUser    BuildStepKind = "user"
)

// BuildStep is one operation in a plan.
type BuildStep struct {
	Kind BuildStepKind `json:"kind"`
	// CacheKey hashes the preceding step chain together with this step's
	// content, so an unchanged prefix reuses cached layers. This is what
	// makes per-step caching work regardless of which front end produced
	// the plan.
	CacheKey string `json:"cacheKey,omitempty"`

	// Run is the shell command for StepRun.
	Run string `json:"run,omitempty"`
	// Source and Dest apply to StepCopy; Source is relative to the build
	// context.
	Source string `json:"source,omitempty"`
	Dest   string `json:"dest,omitempty"`
	// Env applies to StepEnv.
	Env map[string]string `json:"env,omitempty"`
	// Value carries the argument for StepWorkdir and StepUser.
	Value string `json:"value,omitempty"`
}

// BuildPlan is the single intermediate representation every build form
// compiles to (docs/image-build.md §5). Adding a front end does not touch
// the executor, and changing executors does not touch the API.
type BuildPlan struct {
	// From is the base image ref; either source kind is acceptable.
	From string `json:"from"`
	// Tag is the ref the finished image will be known by.
	Tag   string      `json:"tag"`
	Kind  BuildKind   `json:"kind"`
	Steps []BuildStep `json:"steps,omitempty"`

	// Dockerfile holds the file's contents for BuildKindDockerfile; the
	// context is uploaded separately and referenced by digest.
	Dockerfile    string `json:"dockerfile,omitempty"`
	ContextDigest string `json:"contextDigest,omitempty"`

	Env     map[string]string `json:"env,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	// SandboxID is the source sandbox for BuildKindCommit.
	SandboxID string `json:"sandboxId,omitempty"`
}

// ImageBuild is a build request and its progress.
type ImageBuild struct {
	ID    string     `json:"buildId"`
	State BuildState `json:"state"`
	// Reason explains a FAILED or CANCELLED build.
	Reason string `json:"reason,omitempty"`

	Plan *BuildPlan `json:"plan"`

	// NodeID is where the build ran; builds execute on nodes so they share
	// the local block cache with sandboxes.
	NodeID string `json:"nodeId,omitempty"`
	// ImageDigest is the finished artifact, set once READY.
	ImageDigest string `json:"imageDigest,omitempty"`
	SizeBytes   int64  `json:"sizeBytes,omitempty"`
	// CachedSteps counts steps satisfied from cache, which is the number
	// users care about when a rebuild is unexpectedly slow.
	CachedSteps int `json:"cachedSteps"`

	Labels    map[string]string `json:"labels,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
	// StartedAt and FinishedAt bound the build for duration reporting.
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

// ID prefixes. Every user-visible identifier is prefixed so it is obvious
// what an ID refers to in logs, errors and support requests.
const (
	PrefixSandbox    = "sbx"
	PrefixSnapshot   = "snap"
	PrefixImage      = "img"
	PrefixVolume     = "vol"
	PrefixPrewarmJob = "pw"
	PrefixBuild      = "bld"
)

// NewID returns a prefixed random identifier, e.g. "sbx_9f8745cbb1".
func NewID(prefix string) string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is unrecoverable and must not yield colliding IDs.
		panic("store: crypto/rand failed: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(b)
}

// Sandbox is the control plane's record of one sandbox. It is the
// persisted form of a user's create request plus placement and status.
type Sandbox struct {
	ID     string       `json:"id"`
	State  SandboxState `json:"state"`
	Reason string       `json:"reason,omitempty"`

	// Image is the native OCI reference the caller asked for. Digest and
	// any converted artifact live on the Image record (see Image).
	Image string `json:"image"`
	// SnapshotID is set when the sandbox was restored from a snapshot
	// instead of created from an image.
	SnapshotID string `json:"snapshotId,omitempty"`

	NodeID    string  `json:"nodeId"`
	Region    string  `json:"region,omitempty"`
	Runtime   string  `json:"runtime,omitempty"`
	CPU       float64 `json:"cpu"`
	MemoryMiB int64   `json:"memoryMiB"`
	DiskMiB   int64   `json:"diskMiB"`

	Labels map[string]string `json:"labels,omitempty"`

	// Lifecycle: nil IdleTimeout means the sandbox runs until stopped.
	IdleTimeout *int64 `json:"idleTimeoutSeconds,omitempty"`
	OnIdle      string `json:"onIdle,omitempty"`

	CreatedAt    time.Time `json:"createdAt"`
	LastActivity time.Time `json:"lastActivityAt"`
}

// Snapshot is a persisted sandbox state that can be restored later,
// possibly on another node.
type Snapshot struct {
	ID        string        `json:"id"`
	Name      string        `json:"name,omitempty"`
	State     SnapshotState `json:"state"`
	Reason    string        `json:"reason,omitempty"`
	SandboxID string        `json:"sandboxId"`

	// Image identifies the base the snapshot was taken from; a restore
	// needs it to reassemble the rootfs.
	Image string `json:"image"`
	// Runtime records which tier produced the checkpoint, because formats
	// are not interchangeable between tiers.
	Runtime   string `json:"runtime"`
	NodeID    string `json:"nodeId"`
	SizeBytes int64  `json:"sizeBytes"`

	// CPUVendor, CPUFamily and CPUTemplate record the CPU the guest's memory was
	// captured on. They are copied here rather than looked up from NodeID at
	// restore time: the node may be gone, or may have been restarted with a
	// different template, and either way the constraint belongs to the snapshot.
	//
	// Empty means the snapshot predates this and its portability is unknown, so
	// restore treats it as unconstrained — refusing old snapshots outright would
	// break them for no gain, since nothing worse can happen than before.
	CPUVendor   string `json:"cpuVendor,omitempty"`
	CPUFamily   int32  `json:"cpuFamily,omitempty"`
	CPUTemplate string `json:"cpuTemplate,omitempty"`

	// BaseID names the checkpoint this one was taken against, empty for a
	// self-contained one. A snapshot with a base holds only the guest memory
	// written since it, so restoring means replaying the whole chain from the
	// root — which is why the link is stored rather than derived: the ancestors
	// have to be locatable long after the sandbox that produced them is gone.
	BaseID string `json:"baseId,omitempty"`
	// ChainDepth counts ancestors, so 0 is a full checkpoint and 1 is a diff
	// against a full one.
	//
	// It is stored rather than walked because it gates whether the next
	// checkpoint may be a diff at all, and that decision is made on the write
	// path where walking a chain of database reads to answer it would put the
	// cost on every snapshot.
	ChainDepth int `json:"chainDepth,omitempty"`

	// IncludeMemory reports whether the checkpoint carries guest memory.
	//
	// It is what makes the CPU fields above meaningful: a filesystem-only
	// snapshot restores on any CPU, so recording a constraint it does not have
	// would fragment placement for nothing. It is also the honest answer to
	// "will my process tree survive this", which is not derivable from the size.
	//
	// A pointer because snapshots taken before this field existed have no value
	// for it, and a plain bool would decode those as false — claiming they carry
	// no memory when they do, which would drop the CPU constraint that keeps
	// them from being restored onto an incompatible host. Absent therefore means
	// "assume memory", the behaviour those snapshots were created under.
	IncludeMemory *bool `json:"includeMemory,omitempty"`

	Labels map[string]string `json:"labels,omitempty"`
	// RefCount counts in-progress restores; a snapshot with refs cannot be
	// deleted out from under them.
	RefCount  int       `json:"-"`
	CreatedAt time.Time `json:"createdAt"`
}

// HasMemory reports whether the checkpoint carries guest memory.
//
// An unset IncludeMemory means the snapshot predates the field, and every
// snapshot from that time captured memory — so absent reads as true. Deciding
// this at each use site invites the opposite reading, which would silently drop
// the CPU constraint on exactly the snapshots that need it.
func (s *Snapshot) HasMemory() bool {
	return s.IncludeMemory == nil || *s.IncludeMemory
}

// Image is platform-side metadata for a native OCI reference. Callers
// supply Ref only; everything else is derived by the platform.
type Image struct {
	// Ref is the native OCI reference exactly as the caller wrote it,
	// e.g. "python:3.12" or "registry/swebench/django-12345:latest".
	Ref string `json:"ref"`
	// Digest pins the resolved content. Scheduling, caching and
	// reproducibility all key off the digest, never the tag.
	Digest string `json:"digest,omitempty"`
	// OverlaybdRef points at the converted block-device artifact. It is
	// internal — API handlers select fields explicitly and never include
	// it — but it must persist, so it keeps a JSON name.
	OverlaybdRef string `json:"overlaybdRef,omitempty"`

	State  ImageState `json:"state"`
	Reason string     `json:"reason,omitempty"`

	// Source distinguishes an imported OCI reference from a platform build.
	Source ImageSource `json:"source"`
	// BaseRef is the image this one was built on top of; layer reuse and
	// garbage collection both need it.
	BaseRef string `json:"baseRef,omitempty"`
	// BuildID traces a built image back to the build that produced it.
	BuildID string `json:"buildId,omitempty"`
	// LayerDigests is the layer manifest, which drives layer-level dedup
	// and cache accounting.
	LayerDigests []string `json:"layerDigests,omitempty"`

	SizeBytes int64 `json:"sizeBytes,omitempty"`
	// CachedNodes counts nodes reporting local blocks for this image,
	// which drives image-affinity scoring and prewarm decisions.
	CachedNodes int `json:"cachedNodes"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// RegistryCredential authenticates pulls from a private registry.
//
// The secret is never returned by the API and never reaches a sandbox: the
// control plane uses it to obtain short-lived registry tokens, and only
// those are handed to nodes. Storing it here (encrypted at rest) is what
// lets a caller reference a private image with nothing but its ref.
type RegistryCredential struct {
	// Host is the registry the credential applies to, e.g.
	// "registry.example.com" or "index.docker.io".
	Host     string `json:"host"`
	Username string `json:"username"`
	// Secret is the password or token. Encrypted at rest; the `json:"-"`
	// tag keeps it out of every API response and log line.
	Secret string `json:"-"`
	// SecretCiphertext holds the encrypted secret as persisted.
	SecretCiphertext []byte `json:"-"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// PrewarmJob tracks a request to pull an image set onto nodes ahead of a
// batch, so the batch does not pay the cold-pull cost.
type PrewarmJob struct {
	ID          string    `json:"jobId"`
	Refs        []string  `json:"refs"`
	Region      string    `json:"region,omitempty"`
	TargetNodes int       `json:"targetNodes"`
	Priority    string    `json:"priority,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	// Ready maps image ref to the number of nodes that have it cached.
	Ready map[string]int `json:"ready,omitempty"`
	Done  bool           `json:"done"`
}

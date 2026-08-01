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
	// ImageConverting: an overlaybd build is in progress.
	ImageConverting ImageState = "CONVERTING"
	// ImageReady: an overlaybd artifact exists and the fc tier can use it.
	ImageReady  ImageState = "READY"
	ImageFailed ImageState = "FAILED"
)

// ID prefixes. Every user-visible identifier is prefixed so it is obvious
// what an ID refers to in logs, errors and support requests.
const (
	PrefixSandbox    = "sbx"
	PrefixSnapshot   = "snap"
	PrefixImage      = "img"
	PrefixVolume     = "vol"
	PrefixPrewarmJob = "pw"
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

	Labels map[string]string `json:"labels,omitempty"`
	// RefCount counts in-progress restores; a snapshot with refs cannot be
	// deleted out from under them.
	RefCount  int       `json:"-"`
	CreatedAt time.Time `json:"createdAt"`
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

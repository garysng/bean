package store

import "time"

// The store's consumers each use a small slice of it, and the slices barely overlap:
// the scheduler touches placement and nothing else, nodesvc touches node registration,
// the API touches sandbox and snapshot records, the image service touches images. So
// the interfaces are cut that way rather than as one surface of 39 methods.
//
// Two reasons, and the second is the one that matters.
//
// A single large interface makes every fake implement 39 methods to exercise four, so
// tests either use the real store or carry a pile of panics. That is the ordinary
// argument and it is true.
//
// The other is that a narrow interface says who may change what. `Scheduler` cannot
// delete a snapshot; `Images` cannot move a reservation. With one interface those
// statements are true only by convention, and convention is what erodes when someone
// needs a value in a hurry.
//
// What every method here has in common: its atomicity belongs to the database, not to
// the caller's process. That is a precondition for these interfaces being honest
// rather than a nice property -- an interface implies a second implementation, and a
// second implementation implies a second process. See AcquireSnapshot for the shape
// that took, and for what the alternative looked like.

// Sandboxes is the sandbox record and its event log.
type Sandboxes interface {
	GetSandbox(id string) (*Sandbox, error)
	PutSandbox(sb *Sandbox) error
	ListSandboxes(labelKey, labelVal string, state SandboxState) ([]*Sandbox, error)
	DeleteSandbox(id string) error

	// AppendEvent and ListEvents are here rather than in their own interface because
	// an event only exists as a fact about a sandbox: nothing reads the log without a
	// sandbox id, and nothing writes to it that is not already changing a sandbox.
	AppendEvent(ev *Event) error
	ListEvents(sandboxID string, limit int) ([]*Event, error)
}

// Snapshots is the checkpoint catalogue, including the reference counting that keeps a
// snapshot alive while a restore reads it.
//
// AcquireSnapshot and ReleaseSnapshot are a pair a caller must balance, which is the
// kind of obligation an interface should make visible: they are named next to each
// other here so that adding one call without the other is a visible omission rather
// than a buried one.
type Snapshots interface {
	GetSnapshot(id string) (*Snapshot, error)
	PutSnapshot(sn *Snapshot) error
	ListSnapshots(labelKey, labelVal string, state SnapshotState) ([]*Snapshot, error)
	DeleteSnapshot(id string) error

	// AcquireSnapshot refuses unless the snapshot is ready, and the refusal is a
	// condition of the write rather than a check the caller makes first. A caller
	// therefore cannot create the window in which a snapshot is deleted between the
	// check and the use.
	AcquireSnapshot(id string) (*Snapshot, error)
	ReleaseSnapshot(id string) error

	// SnapshotChain returns a snapshot and its ancestors, base first. An incremental
	// snapshot cannot be restored without them.
	SnapshotChain(id string) ([]*Snapshot, error)
}

// Templates is the launch-template catalogue and the prewarm jobs that populate it.
// A template is addressed by id (primary) or by name; a converted OCI image's name
// is its OCI reference.
type Templates interface {
	GetTemplate(id string) (*Template, error)
	GetTemplateByName(name string) (*Template, error)
	PutTemplate(t *Template) error
	ListTemplates(owner string) ([]*Template, error)
	DeleteTemplate(id string) error

	GetPrewarmJob(id string) (*PrewarmJob, error)
	PutPrewarmJob(j *PrewarmJob) error
}

// Placement is the resource ledger: which node holds what, and how much of each node
// is already promised.
//
// This is the interface where the atomicity requirement is not academic. Reserve
// decides whether a node fits inside the statement that commits the resources, so two
// schedulers cannot both be told yes for capacity only one of them can have. A version
// that read the node, compared, and then wrote would oversell -- and overselling memory
// kills a guest while overselling disk destroys a copy-on-write layer, neither of which
// recovers on its own.
type Placement interface {
	// Reserve commits a node's resources to a sandbox, or returns ErrCapacityChanged
	// if the node no longer fits. The check and the commitment are one statement.
	Reserve(nodeID string, res *Reservation) error
	// Release returns a reservation's resources. Idempotent, because the caller that
	// releases may be a cleanup path that cannot know whether an earlier one ran.
	Release(sandboxID string) error
	// FinishCreate clears the in-flight marker Reserve set, leaving the commitment.
	FinishCreate(sandboxID string) error

	// OrphanReservations finds reservations whose sandbox is gone, which is how
	// resources committed by a process that died are found again.
	OrphanReservations() ([]string, error)
	// SpreadCounts reports how many sandboxes each node holds per spread key, which is
	// what lets placement spread a batch without any single decision knowing about the
	// others.
	SpreadCounts(key string) (map[string]int, error)
}

// Nodes is node registration and liveness.
type Nodes interface {
	GetNode(id string) (*NodeRecord, error)
	UpsertNode(n *NodeRecord) error
	LoadNodes() ([]*NodeRecord, error)
	// SetNodeState is a compare-and-set: the returned bool reports whether the state
	// actually changed, because the UPDATE carries `AND state != ?` and a no-op is
	// indistinguishable from a success otherwise. That distinction is what stops a
	// health sweep from logging a node as newly lost on every pass.
	//
	// The bool is deliberately in the interface rather than dropped for tidiness. It
	// is the same shape as Reserve's RowsAffected check -- the database decides, and
	// the caller is told what it decided.
	SetNodeState(nodeID, state string) (bool, error)
	// RenewLease records a heartbeat and revives a node the sweep had marked
	// SUSPECT or LOST. Liveness only -- nothing about what the node holds.
	//
	// Separate from UpsertNode because a heartbeat must not be able to change a node's
	// advertised capacity: a partial record arriving on the liveness path would
	// otherwise silently shrink the cluster. The revival is part of the same statement
	// for the same reason the other conditions are -- a heartbeat that arrived and a
	// state that changed must not be two decisions.
	RenewLease(nodeID string) error
	// SetNodeDiskUsed records the node's measured disk usage, reported through
	// UpdateNodeStatus rather than the heartbeat so it stays off the lease path.
	SetNodeDiskUsed(nodeID string, diskUsedMiB int64) error
	// StaleNodes lists nodes whose last heartbeat is older than the cutoff.
	StaleNodes(olderThan time.Time, excludeStates ...string) ([]*NodeRecord, error)
	// PutNodeImages records which images a node has cached, which is what image
	// affinity scores against.
	PutNodeImages(nodeID string, images map[string]CachedImage) error
}

// RegistryCredentials holds the per-registry secrets image pulls authenticate with.
//
// Its own interface rather than part of Images because the sensitivity differs: these
// are encrypted at rest and a caller that lists images has no business reading them.
type RegistryCredentials interface {
	GetRegistryCredential(registry string) (*RegistryCredential, error)
	PutRegistryCredential(c *RegistryCredential) error
	ListRegistryCredentials() ([]*RegistryCredential, error)
	DeleteRegistryCredential(registry string) error
}

// Builds is the image-build record.
type Builds interface {
	GetBuild(id string) (*ImageBuild, error)
	PutBuild(b *ImageBuild) error
	ListBuilds(state BuildState) ([]*ImageBuild, error)
}

// Compile-time assertions that the concrete store satisfies every interface.
//
// These are the whole enforcement mechanism: without them a method could be renamed
// and the interfaces would still compile, failing only where they are consumed. With
// them the break is here, next to the declaration that made the promise.
var (
	_ Sandboxes           = (*Store)(nil)
	_ Snapshots           = (*Store)(nil)
	_ Templates           = (*Store)(nil)
	_ Placement           = (*Store)(nil)
	_ Nodes               = (*Store)(nil)
	_ RegistryCredentials = (*Store)(nil)
	_ Builds              = (*Store)(nil)
)

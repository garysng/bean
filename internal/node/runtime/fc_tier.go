package runtime

import "github.com/garysng/bean/internal/node/image"

// FCTierConfig describes a node's microVM tier. It is declared outside the
// platform-specific files so noded parses the same flags everywhere and reports
// an unsupported platform as a clear error rather than a build failure.
type FCTierConfig struct {
	FirecrackerBin string
	KernelPath     string
	AgentDiskPath  string
	// BaseDir holds per-sandbox VM state and rootfs files.
	BaseDir string
	// ImageDir holds prepared base images.
	ImageDir string
	// DefaultDiskMiB sizes a sandbox rootfs when the spec does not.
	DefaultDiskMiB int64
	// RegistryAuth supplies credentials for private registries. Nil pulls
	// anonymously, which covers public images.
	RegistryAuth image.CredentialSource
	// BuildkitAddr enables image builds on this node, e.g.
	// "unix:///run/bean/buildkitd.sock". Empty leaves builds disabled.
	BuildkitAddr string
	// BuildctlBin is the BuildKit client binary.
	BuildctlBin string
	// DebugConsole attaches guests to the serial console. It costs roughly
	// 500ms per boot, so it is a debugging aid rather than a default.
	DebugConsole bool
	// CPUTemplate masks CPU features from guests so memory snapshots survive a
	// move between CPU generations. Empty means none.
	CPUTemplate CPUTemplate
	// TrackDirtyPages has KVM log guest writes so a checkpoint can capture only
	// the memory that changed. It must be on before a guest starts and is not
	// carried in a snapshot, so this is node configuration: a guest booted without
	// it can never produce an incremental checkpoint.
	TrackDirtyPages bool
	// SnapshotCache bounds the unpacked snapshot cache. The zero value leaves it
	// unbounded, which grows by roughly one guest's memory per distinct snapshot
	// restored on this node and is invisible to the scheduler.
	SnapshotCache EvictionPolicy
}

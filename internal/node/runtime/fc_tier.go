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
}

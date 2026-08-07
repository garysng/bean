//go:build !linux

package runtime

import (
	"errors"

	"github.com/garysng/bean/internal/node/image"
)

// A container tier needs namespaces, cgroups and a block device mounted on the host,
// none of which exist off Linux. The stub keeps the package building on a developer's
// machine, where the local tier is what runs.
func NewOCITier(cfg OCITierConfig) (Runtime, error) {
	return nil, errors.New("oci tier: containers require linux")
}

// OCITierConfig mirrors the linux definition so callers compile everywhere.
type OCITierConfig struct {
	Bin       string
	ExtraArgs []string
	AgentBin  string
	AgentPort int

	BaseDir        string
	ImageDir       string
	DefaultDiskMiB int64
	GuestDNS       string

	RegistryAuth image.CredentialSource
	BuildkitAddr string
	BuildctlBin  string

	Overlaybd         bool
	OverlaybdLazyPull bool
	OverlaybdBinDir   string
	OverlaybdLayerDir string
	OverlaybdBlobs    image.BlobStore
	OverlaybdIndex    image.ImageIndex
}

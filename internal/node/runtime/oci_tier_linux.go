//go:build linux

package runtime

import (
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/garysng/bean/internal/node/image"
)

// NewOCITier assembles the container runtime and the rootfs provider it needs.
//
// Prerequisites are checked here rather than at the first create, for the same reason
// the fc tier does it: a node that cannot run containers should fail to start, not
// accept placements and then reject every sandbox.
//
// The rootfs provider is chosen by the same selectProvider the fc tier uses, so a
// container sandbox and a microVM sandbox on one node share the image cache, the
// converted layers and the object store. That sharing is the point of driving the OCI
// runtime directly rather than bringing in containerd, which would arrive with its own
// content store and snapshotter and give the node two image systems that cannot see
// each other's layers.
func NewOCITier(cfg OCITierConfig) (Runtime, error) {
	for _, dir := range []string{cfg.BaseDir, cfg.ImageDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("oci tier: create %s: %w", dir, err)
		}
	}

	provider, err := selectProvider(FCTierConfig{
		BaseDir:           cfg.BaseDir,
		ImageDir:          cfg.ImageDir,
		DefaultDiskMiB:    cfg.DefaultDiskMiB,
		RegistryAuth:      cfg.RegistryAuth,
		BuildkitAddr:      cfg.BuildkitAddr,
		BuildctlBin:       cfg.BuildctlBin,
		Overlaybd:         cfg.Overlaybd,
		OverlaybdLazyPull: cfg.OverlaybdLazyPull,
		OverlaybdBinDir:   cfg.OverlaybdBinDir,
		OverlaybdBlobs:    cfg.OverlaybdBlobs,
		OverlaybdIndex:    cfg.OverlaybdIndex,
		OverlaybdLayerDir: cfg.OverlaybdLayerDir,
	})
	if err != nil {
		return nil, err
	}

	rt := NewOCIRuntime(cfg.Bin, cfg.AgentBin, cfg.BaseDir, provider)
	rt.GuestDNS = cfg.GuestDNS
	if cfg.AgentPort > 0 {
		rt.AgentPort = cfg.AgentPort
	}
	rt.ExtraArgs = cfg.ExtraArgs

	// gVisor needs --network=host, and this is a real trade rather than a detail.
	//
	// runsc defaults to --network=sandbox, which implements TCP/IP in its own userspace
	// stack (netstack): it takes over the veth, so a listener inside the sandbox is not
	// visible to the host's stack on that interface. Measured -- with the default, the
	// agent logged "beand listening addr=tcp:0.0.0.0:8111" while `ss -ltn` in the
	// namespace showed zero listeners and the node got "network is unreachable"; with
	// --network=host the same listener appeared in the namespace and was reachable
	// three ways (from inside, from 127.0.0.1, and from the host).
	//
	// What it costs: netstack is one of gVisor's isolation boundaries, and host
	// networking gives it up -- the sandbox uses the host kernel's network stack, so a
	// vulnerability there is reachable from inside. What remains is still the whole
	// Sentry: filesystem and process syscalls are intercepted either way, and the
	// sandbox is confined to its own network namespace, so it sees one veth pair rather
	// than the host's interfaces.
	//
	// The alternative would be for the node to reach the agent through netstack, which
	// means either a port forwarded out of the sandbox or a socket gVisor proxies. Both
	// are work; neither is obviously better than accepting one namespace-scoped stack.
	if rt.Name() == "runsc" && !hasNetworkFlag(rt.ExtraArgs) {
		rt.ExtraArgs = append([]string{"--network=host"}, rt.ExtraArgs...)
	}

	if err := rt.Available(); err != nil {
		return nil, fmt.Errorf("oci tier: %w", err)
	}

	// Stated at startup because the two runtimes differ in ways a reader of the logs
	// would otherwise have to infer: runsc intercepts syscalls in userspace and does
	// not support GPU passthrough, while runc shares the host kernel.
	slog.Info("sandboxes via an OCI runtime", "binary", rt.Name(),
		"agentPort", rt.AgentPort, "overlaybd", cfg.Overlaybd)
	if rt.Name() == "runc" {
		slog.Warn("runc shares the host kernel: a kernel vulnerability is a container " +
			"escape, so this tier is for trusted or GPU work rather than untrusted code")
	}
	// The agent is reached through the sandbox's network namespace, so a node with no
	// pool cannot talk to any sandbox it starts. Create refuses in that case; saying
	// so here means the operator finds out at startup instead.
	slog.Info("the container tier requires --guest-subnet; sandboxes are reached " +
		"through their network namespace rather than over vsock")

	return rt, nil
}

// OCITierConfig is what the container tier needs. The image-related fields mirror
// FCTierConfig's because both tiers use the same providers.
type OCITierConfig struct {
	// Bin is the OCI runtime binary: runsc (gVisor) or runc.
	Bin string
	// ExtraArgs are global flags for that binary, before the subcommand.
	ExtraArgs []string
	// AgentBin is the agent binary, copied into each sandbox's rootfs.
	AgentBin string
	// AgentPort is where the agent listens inside the namespace. Zero uses the
	// default.
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

// hasNetworkFlag reports whether the operator already chose a network mode.
//
// Checked so the default is a default rather than an override: a deployment that has
// worked out how to reach the agent through netstack, or that wants --network=none for
// something with no need to be reached, keeps its choice.
func hasNetworkFlag(args []string) bool {
	for _, a := range args {
		if a == "--network" || strings.HasPrefix(a, "--network=") {
			return true
		}
	}
	return false
}

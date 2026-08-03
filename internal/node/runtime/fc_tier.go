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
	// GuestDNS is the resolver the in-guest agent writes into /etc/resolv.conf.
	// Empty leaves the user image's own file alone, which is what a node with no
	// sandbox networking wants: a nameserver the guest has no egress to reach is
	// not an improvement on whatever the image shipped.
	//
	// It is node configuration rather than a per-sandbox field because the guest's
	// addresses are identical in every sandbox (docs/network.md section 2), so
	// there is nothing per-sandbox for it to vary with.
	GuestDNS string
}

// GuestDNSBootArgs renders the agent flags that carry a resolver into a guest,
// for appending to the kernel command line after the "--" that separates the
// kernel's arguments from the agent's.
//
// It returns the empty string when no resolver is configured, because the guest
// must then boot with a command line identical to the one it used before this
// existed: an empty --guest-dns would be a new argument for the agent to
// interpret, and the whole point of "unset" is that nothing changes.
//
// This is a function rather than an inlined concatenation so the quoting rule
// lives next to the reason for it: the kernel command line is split on
// whitespace with no quoting whatsoever, so a value containing a space would
// silently become a separate argument. ValidateResolver rejects anything that is
// not a bare IP literal, which is what makes that safe here.
func GuestDNSBootArgs(guestDNS string) string {
	if guestDNS == "" {
		return ""
	}
	return " --guest-dns " + guestDNS
}

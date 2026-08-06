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

	// Overlaybd assembles rootfs devices from overlaybd layers instead of
	// flattening each image into its own ext4.
	//
	// The gain is storage: layers are shared by digest, so a set of images built on
	// one base stores that base once rather than once per image -- measured at 3.1x
	// less disk for a SWE-bench-shaped set. It also removes the per-image conversion
	// of shared layers, which is CPU the flattening path pays every time.
	//
	// Off by default because it is newer than the device-mapper path and depends on
	// more of the host: the TCMU kernel modules, a running overlaybd-tcmu, and the
	// overlaybd binaries. A node configured for it refuses to start if any of those
	// are missing rather than silently falling back, since falling back would give
	// the cluster a node whose storage behaviour differs from what was asked for.
	Overlaybd bool
	// OverlaybdLazyPull leaves layers in the registry for overlaybd to range-read on
	// demand rather than converting them locally.
	//
	// Requires Overlaybd. Off by default: a locally converted layer is a file this
	// node owns, while a lazily pulled one makes every block read depend on the
	// registry still being reachable and still serving that digest.
	OverlaybdLazyPull bool
	// OverlaybdBinDir holds the overlaybd binaries. Empty resolves them on PATH.
	OverlaybdBinDir string
	// OverlaybdBlobs publishes sealed layers where the overlaybd daemon can
	// range-read them, which is what makes lazy pull work for images that arrive as
	// ordinary OCI: convert once, publish under the digest, and every later create
	// reading the same store skips the conversion.
	//
	// Nil with OverlaybdLazyPull set restricts lazy reads to images whose registry
	// blobs are already sealed overlaybd layers, which almost nothing is.
	OverlaybdBlobs image.BlobStore
	// OverlaybdIndex records which layers make up an image and what a tag points at,
	// so a node that has never seen an image can resolve it from the store instead of
	// the registry.
	//
	// Nil leaves the store a layer cache: the blobs are there, but nothing says which
	// of them form an image, so every create resolves against the registry first.
	OverlaybdIndex image.ImageIndex
	// OverlaybdLayerDir holds sealed layers, shared across images. Empty puts them
	// beside the base images.
	OverlaybdLayerDir string

	// WarmSnapshots has prewarm boot one guest per image and checkpoint it, so
	// later creates of that image restore instead of booting.
	//
	// Off by default for a storage reason rather than a correctness one: each warm
	// snapshot is a full memory image, so the cost is roughly one guest's memory per
	// image per CPU generation, and nothing reclaims them yet. A miss always boots,
	// so turning this on cannot make a create fail that would otherwise have worked.
	WarmSnapshots bool
	// WarmEviction bounds the warm store. Zero leaves it unbounded. Separate from
	// SnapshotCache on purpose: see FCRuntime.WarmEviction.
	WarmEviction EvictionPolicy

	// Cgroups puts each sandbox's VMM in a cgroup with limits from its own spec.
	// Off by default: the memory ceiling is derived from a headroom that has not
	// been measured against real workloads, and a ceiling set too low does not
	// degrade gracefully -- the kernel kills the VMM, and from outside that is a
	// sandbox that died for no stated reason.
	//
	// Turning it on is what makes the committed quantity something the kernel
	// enforces rather than a line in the scheduler's ledger, which is the
	// prerequisite overcommit.go names for raising memory overcommit above 1.0.
	Cgroups bool

	// VMMPidNamespace hides the host's process table from the VMM, and
	// VMMKillOnExit has the kernel SIGKILL it if noded dies.
	//
	// Two flags rather than one because they buy different things. The first is
	// containment -- Firecracker signals nothing on the host and spawns no children,
	// so it has no reason to see host processes. The second is a leak fix:
	// reconciliation already reclaims a VMM that outlived noded, but only at the next
	// startup, and until then it holds memory the scheduler has promised elsewhere.
	//
	// Both are applied as clone flags during the fork that starts Firecracker, so no
	// wrapper process is introduced and the recorded pid stays the VMM's own -- which
	// is what keeps destroy reliable. See vmmisolation_linux.go for why that matters
	// and how it differs from e2b's `unshare ... bash -c ...` arrangement.
	VMMPidNamespace bool
	VMMKillOnExit   bool

	// VMMMountNamespace gives the VMM a private mount namespace.
	//
	// Separate from the two above because its failure mode is worse and quieter:
	// bean's rootfs is a device-mapper node under /dev rather than a file, so a guest
	// that cannot resolve it finds no root device, and that appears only in the guest
	// console. Left off until measured on real hardware.
	VMMMountNamespace bool

	// VMMUid and VMMGid run the VMM as an unprivileged user instead of root. Zero
	// leaves it as noded's own identity, which is the pre-existing behaviour.
	//
	// One uid for every sandbox on the node rather than one each: see vmmcreds.go
	// for what that does and does not bound. It also requires the node's shared
	// assets (kernel, agent disk) to be readable by others, which is checked at
	// startup rather than discovered one failed create at a time.
	VMMUid int
	VMMGid int
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

// guestIPBootArg is in fc_linux.go, beside the NIC registration it pairs with and
// where guestIfaceID is declared.

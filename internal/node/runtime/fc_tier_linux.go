//go:build linux

package runtime

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/garysng/bean/internal/logging"
	"github.com/garysng/bean/internal/node/image"
)

// NewFCTier assembles the microVM runtime and the rootfs provider it needs.
//
// The prerequisites are checked here rather than at first create: a node that
// cannot run microVMs should fail to start, not accept placements and then
// reject every sandbox.
func NewFCTier(cfg FCTierConfig) (Runtime, error) {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return nil, fmt.Errorf("fc tier: /dev/kvm unavailable: %w", err)
	}
	if err := checkSnapshotSupport(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(cfg.FirecrackerBin); err != nil {
		return nil, fmt.Errorf("fc tier: firecracker binary %s: %w", cfg.FirecrackerBin, err)
	}
	if _, err := os.Stat(cfg.KernelPath); err != nil {
		return nil, fmt.Errorf("fc tier: kernel %s: %w", cfg.KernelPath, err)
	}
	if cfg.AgentDiskPath != "" {
		if _, err := os.Stat(cfg.AgentDiskPath); err != nil {
			return nil, fmt.Errorf("fc tier: agent disk %s: %w", cfg.AgentDiskPath, err)
		}
	}
	for _, dir := range []string{cfg.BaseDir, cfg.ImageDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("fc tier: create %s: %w", dir, err)
		}
	}

	provider, err := selectProvider(cfg)
	if err != nil {
		return nil, err
	}
	rt := NewFCRuntime(cfg.FirecrackerBin, cfg.KernelPath, cfg.AgentDiskPath,
		cfg.BaseDir, provider)
	rt.DebugConsole = cfg.DebugConsole
	if cfg.DebugConsole {
		slog.Warn("guest serial console on", "costPerBoot", "~500ms")
	}
	rt.CPUTemplate = cfg.CPUTemplate
	rt.TrackDirtyPages = cfg.TrackDirtyPages
	// Stated at startup because the alternative to noticing it here is noticing it
	// as a package install that cannot resolve a name, several minutes into
	// somebody else's build. The absence is the more surprising case of the two:
	// a guest with working egress and no resolver looks like a broken network.
	if cfg.GuestDNS == "" {
		slog.Warn("no --guest-dns set; guests keep whatever /etc/resolv.conf their " +
			"image shipped, which resolves nothing unless it happens to name a " +
			"reachable server")
	} else {
		slog.Info("guest resolver configured", "nameserver", cfg.GuestDNS,
			"agentArgs", GuestDNSBootArgs(cfg.GuestDNS))
	}
	// Assigned, not merely logged. This line was missing: the value was validated
	// at startup and rendered into the log message above, which made the node
	// report a resolver it never passed to a guest -- the log was the only evidence
	// and it said the feature was on.
	rt.GuestDNS = cfg.GuestDNS
	rt.WarmSnapshots = cfg.WarmSnapshots
	rt.WarmEviction = cfg.WarmEviction
	if cfg.WarmSnapshots {
		// Swept at startup rather than on a timer: a temporary bundle can only be
		// orphaned by a process that died, so this is the one moment the set of
		// orphans is both knowable and stable.
		if err := rt.warm.Clean(); err != nil {
			slog.Warn("cannot clean partial warm snapshots", logging.KeyError, err)
		}
		bytes, err := rt.WarmBytes()
		if err != nil {
			slog.Warn("cannot size the warm snapshots", logging.KeyError, err)
		}
		// Stated with its size because nothing reclaims these yet, so the number an
		// operator needs to watch is the one that only grows.
		if cfg.WarmEviction.Enabled() {
			slog.Info("warm snapshots on; a create restores instead of booting when "+
				"this node holds one for the image and CPU",
				"heldBytes", bytes, "highBytes", cfg.WarmEviction.HighBytes,
				"lowBytes", cfg.WarmEviction.LowBytes,
				"dir", filepath.Join(cfg.BaseDir, ".warm"))
		} else {
			// Warned rather than merely stated: unbounded is the setting that fills a
			// disk, and the growth is invisible to the scheduler because a warm bundle
			// consumes no commitment.
			slog.Warn("warm snapshots on but unbounded; they grow by roughly one "+
				"guest's memory per image per CPU generation and nothing will reclaim "+
				"them. Set --warm-snapshot-high-mib",
				"heldBytes", bytes, "dir", filepath.Join(cfg.BaseDir, ".warm"))
		}
	}
	rt.SnapshotCache = cfg.SnapshotCache
	if !cfg.SnapshotCache.Enabled() {
		// Stated because the growth is otherwise invisible: the cache consumes no
		// commitment, so a node can fill its disk while placement still believes it
		// has room.
		slog.Warn("snapshot cache is unbounded; it grows by roughly one guest's " +
			"memory per distinct snapshot restored on this node")
	} else {
		slog.Info("snapshot cache bounded",
			"highBytes", cfg.SnapshotCache.HighBytes,
			"lowBytes", cfg.SnapshotCache.LowBytes)
	}
	if cfg.TrackDirtyPages {
		// Stated at startup because every guest pays for it while only some
		// sandboxes are ever checkpointed twice, and because a guest booted
		// without it cannot be given the ability later.
		slog.Info("dirty-page tracking on; guests can produce incremental snapshots")
	}
	if cfg.CPUTemplate == CPUTemplatePortable {
		slog.Info("cpu template masks features for snapshot portability",
			"masked", MaskedCPUFeatures(cfg.CPUTemplate),
			// Snapshots still bind to these, so the limit is stated at startup
			// rather than left for a failed restore to reveal.
			"cannotMask", UnmaskableCPUFeatures(cfg.CPUTemplate))
	}
	// Host confinement of the VMM. Both halves are off unless asked for, and each
	// is stated at startup whether or not it is in force: a limit believed to be
	// enforced and silently absent is the failure mode the A3 documentation error
	// in docs/security-and-startup.md had, and it is worse here because somebody
	// would raise memory overcommit on the strength of it.
	if cfg.Cgroups {
		// Fatal on a v1 host, rather than a fall back to running unlimited. An
		// operator who asked for limits and got none silently would raise
		// --overcommit-memory believing the kernel enforces a ceiling; a node that
		// refuses to start says so where it cannot be missed. Not asking for limits
		// at all remains fine -- that is the else branch below.
		h, err := detectCgroupHost()
		if err != nil {
			return nil, fmt.Errorf("fc tier: %w", err)
		}
		rt.Cgroups = h
		slog.Info("VMM resource limits: " + rt.Cgroups.Summary())
		// Swept here, at startup, before this process has created anything: every
		// bean group standing now belongs to a previous noded. rmdir refuses a group
		// that still holds a process, so a sandbox that survived the restart keeps
		// its limits and is counted rather than disturbed. See SweepOrphans for why
		// this is not in internal/node/reclaim.
		if removed, inUse := rt.Cgroups.SweepOrphans(); removed > 0 || inUse > 0 {
			slog.Info("swept cgroups left by a previous noded",
				"removed", removed, "stillInUse", inUse)
		}
		if !rt.Cgroups.Enabled() {
			// Not fatal. Refusing to start would take a node that was running fine
			// out of service to enforce a limit it never had; the honest cost is
			// that an operator must be told the limits are not in force.
			slog.Warn("--fc-cgroups was requested but no usable cgroup controller " +
				"was found; the VMM runs with no host limit, so do not raise " +
				"--overcommit-memory on this node")
		}
	} else {
		slog.Info("VMM runs outside any cgroup; the committed quantity is the " +
			"scheduler's ledger and nothing in the kernel enforces it")
	}

	creds, err := parseVMMCreds(cfg.VMMUid, cfg.VMMGid, kvmGroupID())
	if err != nil {
		return nil, fmt.Errorf("fc tier: %w", err)
	}
	rt.VMMCreds = creds
	slog.Info("VMM privileges: " + creds.Summary())
	if creds.Enabled() {
		// Checked at startup, fatally, because each of these fails every create on
		// the node and none of them is diagnosable from the symptom: no /dev/kvm is
		// EACCES from the VMM before it logs anything, an unreadable kernel is a
		// guest that does not boot, and an unreadable agent disk is a guest that
		// boots with no init.
		if err := kvmAccessible(creds); err != nil {
			return nil, fmt.Errorf("fc tier: %w", err)
		}
		if bad := checkSharedAssets(cfg.KernelPath, cfg.AgentDiskPath); len(bad) > 0 {
			return nil, fmt.Errorf("fc tier: uid %d cannot read this node's shared "+
				"assets, so no guest would boot: %s; these are read-only and shared "+
				"by every sandbox, so make them world-readable rather than chowning "+
				"them to one sandbox's identity", cfg.VMMUid, strings.Join(bad, "; "))
		}
	}

	// Committed images land beside pulled ones, because a committed image is a
	// base image like any other — that is the point of committing rather than
	// snapshotting.
	rt.Committer = &image.Committer{
		ImageDir: cfg.ImageDir,
		WorkDir:  filepath.Join(cfg.ImageDir, ".work"),
	}

	// Builds are enabled only when BuildKit is reachable, so a node that cannot
	// build says so at startup rather than accepting a build and failing it.
	if cfg.BuildkitAddr != "" {
		builder := &image.Builder{
			Buildctl:       cfg.BuildctlBin,
			Addr:           cfg.BuildkitAddr,
			ImageDir:       cfg.ImageDir,
			WorkDir:        filepath.Join(cfg.ImageDir, ".work"),
			DefaultSizeMiB: cfg.DefaultDiskMiB,
		}
		if err := builder.Available(); err != nil {
			return nil, fmt.Errorf("fc tier: builds requested but %w", err)
		}
		rt.Builder = builder
		slog.Info("image builds enabled", "buildkit", cfg.BuildkitAddr)
	}
	return rt, nil
}

// selectProvider picks how a rootfs is assembled.
//
// device-mapper is preferred because it shares one read-only base across every
// sandbox and gives each a copy-on-write store: creating a sandbox copies
// nothing, and a write costs kilobytes instead of the image's size. Copying the
// base image works everywhere but makes a fan-out of clones cost the image size
// each time, so it is the fallback rather than the default.
func selectProvider(cfg FCTierConfig) (image.Provider, error) {
	// overlaybd, when asked for, replaces the whole chain rather than sitting under
	// the pulling wrapper: it resolves an image to its layers itself, because
	// deciding which layers to fetch is the same decision as deciding whether to
	// fetch them at all. Wrapping it in PullingProvider would have the ext4
	// converter run first and produce an artifact overlaybd has no use for.
	if cfg.Overlaybd {
		layerDir := cfg.OverlaybdLayerDir
		if layerDir == "" {
			layerDir = filepath.Join(cfg.ImageDir, "layers")
		}
		p := image.NewOverlaybdProvider(cfg.BaseDir, layerDir, cfg.ImageDir,
			image.NewRegistry(cfg.RegistryAuth),
			image.NewOverlaybdBuilder(cfg.OverlaybdBinDir, layerDir,
				filepath.Join(cfg.ImageDir, ".work")),
			cfg.DefaultDiskMiB)
		p.LazyPull = cfg.OverlaybdLazyPull
		// Reported as a startup failure rather than a fallback. A node asked for
		// overlaybd and given device-mapper instead would differ from the cluster's
		// expectation in storage cost and in whether layers are shared, and nothing
		// downstream can see that.
		if err := p.Available(); err != nil {
			return nil, fmt.Errorf("fc tier: overlaybd requested but %w", err)
		}
		slog.Info("rootfs via overlaybd", "lazyPull", cfg.OverlaybdLazyPull,
			"layerDir", layerDir)
		return p, nil
	}

	var assembler image.Provider
	dm := image.NewDevMapperProvider(cfg.BaseDir, cfg.ImageDir, cfg.DefaultDiskMiB)
	if err := dm.Available(); err != nil {
		slog.Warn("device-mapper unavailable, copying base images instead", logging.KeyError, err)
		assembler = &image.FileProvider{
			BaseDir:        cfg.BaseDir,
			ImageDir:       cfg.ImageDir,
			DefaultSizeMiB: cfg.DefaultDiskMiB,
		}
	} else {
		slog.Info("rootfs via device-mapper copy-on-write")
		assembler = dm
	}

	// Pulling wraps assembly rather than replacing it: obtaining a base image
	// and giving a sandbox a writable view of it are separate problems, and the
	// copy-on-write assembly is worth having whichever way the base arrived.
	return image.NewPullingProvider(assembler, &image.Converter{
		Registry:       image.NewRegistry(cfg.RegistryAuth),
		ImageDir:       cfg.ImageDir,
		WorkDir:        filepath.Join(cfg.ImageDir, ".work"),
		DefaultSizeMiB: cfg.DefaultDiskMiB,
	}), nil
}

// checkSnapshotSupport verifies the host can save a vCPU's state.
//
// Firecracker reads IA32_FEATURE_CONTROL (MSR 0x3a) when snapshotting. That
// register is Intel-specific, so on AMD hosts KVM rejects the read unless
// ignore_msrs is set, and snapshot creation fails. Checking here turns a
// confusing per-snapshot failure into a startup error naming the fix, before
// the node has advertised itself as snapshot-capable.
func checkSnapshotSupport() error {
	const param = "/sys/module/kvm/parameters/ignore_msrs"
	if !isAMDHost() {
		return nil
	}
	val, err := os.ReadFile(param)
	if err != nil {
		// A host without the parameter exposed is not necessarily broken, and
		// refusing to start over a missing sysfs entry would be worse than
		// letting the snapshot path report the problem.
		return nil
	}
	if strings.TrimSpace(string(val)) == "Y" || strings.TrimSpace(string(val)) == "1" {
		return nil
	}
	return fmt.Errorf("fc tier: snapshots need kvm.ignore_msrs on this AMD host "+
		"(Firecracker reads an Intel-only MSR); run: echo 1 > %s", param)
}

func isAMDHost() bool {
	info, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return false
	}
	return strings.Contains(string(info), "AuthenticAMD")
}

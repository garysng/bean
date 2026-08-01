//go:build linux

package runtime

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

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

	rt := NewFCRuntime(cfg.FirecrackerBin, cfg.KernelPath, cfg.AgentDiskPath,
		cfg.BaseDir, selectProvider(cfg))
	// Committed images land beside pulled ones, because a committed image is a
	// base image like any other — that is the point of committing rather than
	// snapshotting.
	rt.Committer = &image.Committer{
		ImageDir: cfg.ImageDir,
		WorkDir:  filepath.Join(cfg.ImageDir, ".work"),
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
func selectProvider(cfg FCTierConfig) image.Provider {
	var assembler image.Provider
	dm := image.NewDevMapperProvider(cfg.BaseDir, cfg.ImageDir, cfg.DefaultDiskMiB)
	if err := dm.Available(); err != nil {
		log.Printf("fc tier: %v; falling back to copying base images", err)
		assembler = &image.FileProvider{
			BaseDir:        cfg.BaseDir,
			ImageDir:       cfg.ImageDir,
			DefaultSizeMiB: cfg.DefaultDiskMiB,
		}
	} else {
		log.Printf("fc tier: rootfs via device-mapper copy-on-write")
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
	})
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

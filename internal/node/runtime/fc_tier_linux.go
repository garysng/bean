//go:build linux

package runtime

import (
	"fmt"
	"os"
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

	provider := &image.FileProvider{
		BaseDir:        cfg.BaseDir,
		ImageDir:       cfg.ImageDir,
		DefaultSizeMiB: cfg.DefaultDiskMiB,
	}
	return NewFCRuntime(cfg.FirecrackerBin, cfg.KernelPath, cfg.AgentDiskPath,
		cfg.BaseDir, provider), nil
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

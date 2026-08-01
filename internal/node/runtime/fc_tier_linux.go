//go:build linux

package runtime

import (
	"fmt"
	"os"

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

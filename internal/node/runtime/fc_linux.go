//go:build linux

package runtime

import (
	"context"
	"fmt"
)

// FCRuntime is the Firecracker microVM runtime (P0 main tier on Linux/KVM).
// Skeleton: jailer+FC process management, overlaybd ublk block device,
// agent disk injection, vsock transport. Reference implementation:
// /Users/mac/project/agentenv (uvm-ublk, envd).
type FCRuntime struct {
	FirecrackerBin string
	JailerBin      string
	KernelPath     string
	AgentDiskPath  string
	BaseDir        string
}

func NewFCRuntime(fcBin, jailerBin, kernel, agentDisk, baseDir string) *FCRuntime {
	return &FCRuntime{
		FirecrackerBin: fcBin,
		JailerBin:      jailerBin,
		KernelPath:     kernel,
		AgentDiskPath:  agentDisk,
		BaseDir:        baseDir,
	}
}

func (r *FCRuntime) Name() string { return "fc" }

func (r *FCRuntime) Create(ctx context.Context, spec *Spec) (*Handle, error) {
	return nil, fmt.Errorf("fc runtime: not yet implemented (requires linux+KVM host; see docs/roadmap.md P0)")
}

func (r *FCRuntime) Destroy(ctx context.Context, id string, force bool) error {
	return fmt.Errorf("fc runtime: not yet implemented")
}

func (r *FCRuntime) Pause(ctx context.Context, id string) error {
	return fmt.Errorf("fc runtime: not yet implemented")
}

func (r *FCRuntime) Resume(ctx context.Context, id string) error {
	return fmt.Errorf("fc runtime: not yet implemented")
}

func (r *FCRuntime) List(ctx context.Context) ([]string, error) {
	return nil, nil
}

//go:build linux

package beand

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// PivotToRootfs makes the user's image the guest's root filesystem.
//
// The agent boots as PID 1 from its own disk, so the kernel has an init to exec
// without the user image containing one. This function then moves the user
// image into place, which is what lets an arbitrary OCI image run unmodified:
// it needs no beand, no init system and no knowledge of the platform.
//
// The agent binary stays reachable afterwards because its disk is mounted
// inside the new root before the switch.
func PivotToRootfs(device string) error {
	const (
		newRoot    = "/rootfs"
		agentMount = "/bean"
	)

	if err := mountPseudoFS(); err != nil {
		return err
	}

	// ext4 covers what the image builder produces today. A read-only mount
	// would break any sandbox that writes, so this is deliberately writable.
	// newRoot exists on the agent disk by construction.
	if err := unix.Mount(device, newRoot, "ext4", 0, ""); err != nil {
		return fmt.Errorf("beand: mount rootfs %s: %w", device, err)
	}

	// The agent's own disk has to survive the pivot: after switching roots the
	// old root goes away, and with it the binary that is running. Bind-mounting
	// it into the new root keeps the path valid.
	agentTarget := filepath.Join(newRoot, agentMount)
	if err := os.MkdirAll(agentTarget, 0o755); err != nil {
		return fmt.Errorf("beand: create %s: %w", agentTarget, err)
	}
	if err := unix.Mount(agentMount, agentTarget, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("beand: bind agent disk: %w", err)
	}

	// The pseudo-filesystems are moved rather than remounted: processes the
	// sandbox starts need /proc and /dev, and re-mounting them after the pivot
	// would race with the first exec.
	for _, fs := range []string{"/proc", "/sys", "/dev"} {
		target := filepath.Join(newRoot, fs)
		if err := os.MkdirAll(target, 0o755); err != nil {
			return fmt.Errorf("beand: create %s: %w", target, err)
		}
		if err := unix.Mount(fs, target, "", unix.MS_MOVE, ""); err != nil {
			return fmt.Errorf("beand: move %s: %w", fs, err)
		}
	}

	// chroot after mounting the new root over / is simpler than pivot_root and
	// sufficient here: the guest has a single mount namespace and no other
	// process to isolate from.
	if err := unix.Chdir(newRoot); err != nil {
		return fmt.Errorf("beand: chdir %s: %w", newRoot, err)
	}
	if err := unix.Mount(newRoot, "/", "", unix.MS_MOVE, ""); err != nil {
		return fmt.Errorf("beand: move new root: %w", err)
	}
	if err := unix.Chroot("."); err != nil {
		return fmt.Errorf("beand: chroot: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return fmt.Errorf("beand: chdir /: %w", err)
	}

	return mountTmpfs()
}

// mountPseudoFS brings up the filesystems the kernel and the agent need before
// anything else can run.
//
// The mountpoints are not created here: this runs against the agent disk, which
// is mounted read-only, so they are part of its build-time layout (see
// hack/build-assets.sh). A missing one is a broken agent disk and should say so
// rather than fail as a confusing mkdir error.
func mountPseudoFS() error {
	mounts := []struct {
		source, target, fstype string
		flags                  uintptr
	}{
		{"proc", "/proc", "proc", 0},
		{"sysfs", "/sys", "sysfs", 0},
		{"devtmpfs", "/dev", "devtmpfs", 0},
	}
	for _, m := range mounts {
		if err := unix.Mount(m.source, m.target, m.fstype, m.flags, ""); err != nil {
			// Already-mounted is not a failure: the kernel mounts devtmpfs
			// itself when configured to.
			if err == unix.EBUSY {
				continue
			}
			return fmt.Errorf("beand: mount %s (is it present on the agent disk?): %w",
				m.target, err)
		}
	}
	return nil
}

// mountTmpfs provides the writable scratch space ordinary programs assume
// exists. These are in the sandbox's root, so they are sized rather than
// unbounded: a runaway write should hit a limit instead of exhausting guest
// memory.
func mountTmpfs() error {
	for _, m := range []struct{ target, opts string }{
		{"/tmp", "size=64m,mode=1777"},
		{"/run", "size=16m,mode=0755"},
		{"/dev/shm", "size=64m,mode=1777"},
	} {
		if err := os.MkdirAll(m.target, 0o755); err != nil {
			return fmt.Errorf("beand: create %s: %w", m.target, err)
		}
		if err := unix.Mount("tmpfs", m.target, "tmpfs", 0, m.opts); err != nil {
			return fmt.Errorf("beand: mount %s: %w", m.target, err)
		}
	}
	return nil
}

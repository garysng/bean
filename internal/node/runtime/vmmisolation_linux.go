//go:build linux

package runtime

import (
	"os/exec"
	"syscall"
)

// This file narrows what the VMM process can see of the host, without putting a
// wrapper process between noded and Firecracker.
//
// The distinction matters more than it looks. e2b reaches the same isolation by
// exec'ing `unshare -pfm --kill-child -- bash -c "... ip netns exec ... firecracker"`
// (packages/orchestrator/internal/sandbox/fc/process.go), which is three processes
// deep: what cmd.Process.Pid names is `unshare`, not the VMM. Whether killing that
// reaches Firecracker then depends on whether each layer execs in place or forks, and
// the failure mode is a destroy that reports success while the microVM keeps running
// and keeps holding memory the scheduler has already handed out. netns_linux.go
// records the same reasoning for why the network namespace is entered with setns
// rather than with `ip netns exec`.
//
// Clone flags avoid that: they are applied by the kernel during the fork that starts
// Firecracker, so no additional process exists and cmd.Process.Pid is still the VMM's.
// killVMM's negative pid still names its process group.
//
// What is deliberately NOT here: CLONE_NEWNET. A new network namespace is an empty
// one, and this sandbox's namespace already exists with its tap in it -- joining an
// existing namespace is setns, not clone. See startInNetns.
//
// The two compose rather than compete, and the order is what makes that true. The
// network namespace is joined by the calling thread with setns *before* the fork, and
// these flags are applied by the kernel *during* that fork -- so the child inherits
// the joined netns and gets fresh pid and mount namespaces in one operation. A test
// asserts both halves on a live process, because "the flag was set" and "the process
// ended up isolated" are different claims and only the second one matters.

// isolateVMM adds namespace isolation and parent-death cleanup to cmd.
//
// Added to whatever SysProcAttr the caller built rather than replacing it: Setpgid has
// to survive because killVMM signals the negative pid, and Credential has to survive
// because it is what drops the VMM's uid.
func isolateVMM(cmd *exec.Cmd, opts VMMIsolation) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	if opts.PIDNamespace {
		// The VMM cannot see or signal any process on the host. It needs none: it
		// talks to noded over its API socket and to the guest over KVM, and it spawns
		// no children of its own.
		//
		// Nothing inside needs to reap, either. Firecracker is the only process in the
		// namespace, so there are no orphans for it to inherit as PID 1.
		cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWPID
	}

	if opts.MountNamespace {
		// A private mount namespace, plus Unshareflags so its propagation type is
		// private: without that, CLONE_NEWNS still starts as a copy of the host's
		// namespace with shared propagation, and mounts made inside would travel back
		// out. e2b spells the same thing `mount --make-rprivate /` inside its
		// unshare'd shell; Unshareflags is Go asking the kernel for it directly.
		//
		// This is the one flag whose failure is not visible from the host: bean's
		// rootfs is a device-mapper node under /dev rather than a file, so if /dev
		// does not resolve the same way inside, the guest finds no root device -- and
		// that appears only in the guest console.
		cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWNS
		cmd.SysProcAttr.Unshareflags |= syscall.CLONE_NEWNS
	}

	if opts.KillOnNodedDeath {
		// SIGKILL rather than SIGTERM, and this is the one place that choice is not
		// about politeness. In a PID namespace the VMM is PID 1, and PID 1 ignores
		// signals it has no handler for -- so a catchable signal would be dropped
		// precisely when the sandbox most needs to die. SIGKILL cannot be caught or
		// ignored, PID 1 or not.
		//
		// This covers noded being killed, which reconciliation cannot: reconciliation
		// runs at the *next* startup and finds leftovers, whereas a VMM that outlives
		// noded holds committed memory for however long that gap is. It does not
		// replace reconciliation, which still handles a VMM whose noded died before
		// recording it.
		cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	}
}

//go:build linux

package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// kvmDevice is the device Firecracker must open to create a VM. A dropped uid
// that cannot open it fails every create with EACCES on this path.
const kvmDevice = "/dev/kvm"

// kvmGroupID is the group that owns /dev/kvm, or 0 if it cannot be read.
//
// Read from the device rather than by looking up a group called "kvm": the group
// name differs between distributions and the ownership is what actually decides
// access. Returning 0 rather than an error keeps a node without KVM startable --
// the fc tier already refuses to start without /dev/kvm (fc_tier_linux.go), so
// this is not the place to fail.
func kvmGroupID() int {
	st, err := os.Stat(kvmDevice)
	if err != nil {
		return 0
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0
	}
	return int(sys.Gid)
}

// kvmAccessible reports whether creds can open /dev/kvm, and why not when they
// cannot.
//
// Checked by mode and ownership rather than by attempting an open, because noded
// runs as root and an open would succeed whatever the mode says. That false pass
// is the whole reason this is written out: the real failure is every create
// returning EACCES on a device the node's own startup check found present.
func kvmAccessible(creds *vmmCreds) error {
	if !creds.Enabled() {
		return nil
	}
	st, err := os.Stat(kvmDevice)
	if err != nil {
		return err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot read ownership of %s", kvmDevice)
	}
	perm := st.Mode().Perm()
	if sys.Uid == creds.UID && perm&0o600 == 0o600 {
		return nil
	}
	for _, g := range creds.Groups {
		if sys.Gid == g && perm&0o060 == 0o060 {
			return nil
		}
	}
	if perm&0o006 == 0o006 {
		return nil
	}
	return fmt.Errorf("%s is %d:%d mode %#o: uid %d with groups %v cannot open it "+
		"read-write, so every create will fail; add the uid to group %d",
		kvmDevice, sys.Uid, sys.Gid, perm, creds.UID, creds.Groups, sys.Gid)
}

// applyCreds sets the identity the VMM will exec with.
//
// The drop happens in the child between fork and exec, so noded's own identity is
// untouched -- which is required, since noded goes on to run dmsetup, ip and
// losetup as root for the next sandbox.
//
// Left alongside Setpgid rather than replacing the SysProcAttr the caller built:
// killVMM signals the negative pid and depends on the VMM leading its own process
// group, and a Credential written over that would produce a destroy that reports
// success while the microVM keeps running.
func applyCreds(cmd *exec.Cmd, creds *vmmCreds) {
	if !creds.Enabled() {
		return
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Credential = &syscall.Credential{
		Uid: creds.UID,
		Gid: creds.GID,
		// Set explicitly, and always containing the primary gid: with Credential
		// present Go calls setgroups with exactly this list, so the kvm group is
		// only held if it is named here.
		Groups: creds.Groups,
	}
	// PR_SET_NO_NEW_PRIVS is not set here, and it is not an oversight.
	// docs/jailer.md section 7 states that "Go's SysProcAttr has NoNewPrivs on
	// Linux"; that is wrong -- checked against go1.26.1's syscall.SysProcAttr,
	// which has Credential, AmbientCaps, Cloneflags and no such field. Setting it
	// needs a prctl between fork and exec, which Go does not expose, so it needs a
	// wrapper binary -- and a wrapper is what netns_linux.go rules out, because the
	// pid noded records would be the wrapper's and killVMM would signal the wrong
	// process group. It arrives with jailer in phase 2, which does the prctl
	// itself.
}

// applyRlimits bounds the started VMM's descriptors and processes.
//
// Applied to the child by pid after it starts, rather than before the fork.
// setrlimit is per-process on Linux and noded's threads share one set, so
// lowering the limit around a fork would lower it for noded itself and for every
// other goroutine forking at that moment -- an intermittent failure in unrelated
// code. prlimit(2) names the target process instead, which is the only way to
// bound one child from Go without a wrapper binary, and a wrapper is ruled out
// here for the same reason netns_linux.go rules one out: the recorded pid would be
// the wrapper's and killVMM would signal the wrong process group.
//
// The cost is a window between exec and this call in which the VMM has the
// inherited limits. It is harmless in this specific order: Firecracker has been
// given nothing to do yet -- no machine configuration, no drives, no
// InstanceStart -- so it has opened its API socket and nothing else, and a VMM
// cannot reach either limit before the guest exists.
//
// A failure is returned rather than logged. The process is running by now, so the
// caller has to decide, and its cleanup already knows how to kill it.
func applyRlimits(pid int, creds *vmmCreds) error {
	if !creds.Enabled() {
		return nil
	}
	for _, l := range []struct {
		name     string
		resource int
		value    uint64
	}{
		{"nofile", unix.RLIMIT_NOFILE, creds.NoFile},
		{"nproc", unix.RLIMIT_NPROC, creds.NProc},
	} {
		if l.value == 0 {
			continue
		}
		lim := unix.Rlimit{Cur: l.value, Max: l.value}
		if err := unix.Prlimit(pid, l.resource, &lim, nil); err != nil {
			return fmt.Errorf("set %s rlimit on pid %d: %w", l.name, pid, err)
		}
	}
	return nil
}

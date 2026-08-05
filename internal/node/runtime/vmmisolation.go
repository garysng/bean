package runtime

import "strings"

// VMMIsolation selects the host isolation applied to a sandbox's VMM process.
//
// Each field is separate rather than one boolean, because they were verified
// separately on real hardware and have different failure modes: a PID namespace that
// misbehaves shows up as a sandbox that cannot be destroyed, while a mount namespace
// that misbehaves shows up as a guest that cannot find its root device -- and the
// second is only visible in the guest console.
//
// The zero value applies nothing, which is what every deployment before this ran with.
type VMMIsolation struct {
	// PIDNamespace hides the host's processes from the VMM.
	//
	// Cheap and low-risk: Firecracker signals nothing on the host and spawns no
	// children, so it has no reason to see the host's process table.
	PIDNamespace bool

	// KillOnNodedDeath makes the kernel SIGKILL the VMM if noded dies.
	//
	// Reconciliation already reclaims leftovers, but only at the next startup: a VMM
	// that outlives noded holds committed memory for the length of that gap, and on a
	// node that is overcommitting, that memory is promised to someone else.
	KillOnNodedDeath bool

	// MountNamespace gives the VMM a private mount namespace, so mounts it makes are
	// invisible to the host and the host's later mounts are invisible to it.
	//
	// Verified on real hardware rather than reasoned about, because the concern was
	// specific and turned out to be unfounded: bean's rootfs is a device-mapper node
	// under /dev rather than a file, and the worry was that it would stop being
	// openable inside a private mount namespace. It does not -- a dm node reads fine
	// under `unshare -m --propagation private`, and a sandbox booted with this flag
	// has a working eth0 and its own mnt, pid and net namespaces at once.
	//
	// Recording the wrong prediction because the failure it feared is real for
	// something else: a guest that cannot resolve its root device reports nothing
	// except a boot that never finishes, so anything in this area has to be measured
	// on a guest rather than inferred from the flags. Two apparent failures during
	// that measurement were both cold image pulls on a freshly restarted stack, which
	// look identical to a broken flag from the create's timing alone.
	MountNamespace bool
}

// Summary renders what is on, for the startup log.
//
// Names what is *absent* as well, because the zero value is the pre-existing
// behaviour and a log line that only lists what is enabled reads identically on a node
// with no isolation and on one where the flags were forgotten.
func (i VMMIsolation) Summary() string {
	var on []string
	if i.PIDNamespace {
		on = append(on, "pid namespace")
	}
	if i.MountNamespace {
		on = append(on, "mount namespace")
	}
	if i.KillOnNodedDeath {
		on = append(on, "killed if noded dies")
	}
	if len(on) == 0 {
		return "VMM shares the host's namespaces and survives noded (no isolation flags set)"
	}
	return strings.Join(on, ", ")
}

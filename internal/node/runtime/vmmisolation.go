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
	// Riskier than the other two and therefore separate: bean's rootfs is a
	// device-mapper node under /dev, not a file, so whether it stays openable inside a
	// private mount namespace is a question about /dev propagation rather than about
	// paths. A wrong answer is a guest with no root device, and that failure is
	// visible only in the guest console.
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

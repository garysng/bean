package scheduler

import "fmt"

// CPUConstraint is what a memory snapshot requires of a node that restores it.
//
// Restoring guest memory onto an incompatible CPU does not fail at load time.
// Firecracker accepts the state and the guest resumes; it then misbehaves later,
// when it executes something it decided was available during its original boot,
// or when its kernel takes an errata path meant for another vendor. So this has
// to be a placement filter — by the time anything could detect the problem at
// runtime, the sandbox is already wrong.
type CPUConstraint struct {
	// Vendor is the CPUID vendor string, e.g. "AuthenticAMD" or
	// "GenuineIntel". Empty means unconstrained.
	Vendor string
	// Family is the CPU family. Zero means unconstrained.
	Family int32
	// Template is the masking policy the snapshot was taken under. Without one
	// ("none" or empty) the guest may have latched onto instruction-set
	// features specific to its original host, so only the same family will do.
	Template string
}

// Constrained reports whether the constraint restricts placement at all.
func (c CPUConstraint) Constrained() bool { return c.Vendor != "" }

// CheckCPU reports whether a node can restore a snapshot taken under c.
//
// The returned error is written for whoever sees the API response, because the
// alternative to a clear refusal here is a sandbox that starts and then behaves
// incorrectly for reasons nothing will explain.
func (c CPUConstraint) CheckCPU(nodeVendor string, nodeFamily int32) error {
	if !c.Constrained() {
		return nil
	}
	// A node that reports no CPU identity cannot be shown to be compatible.
	// Allowing it would make the check depend on a node having been upgraded,
	// which is exactly when a silent mismatch would slip through.
	if nodeVendor == "" {
		return fmt.Errorf("node reports no CPU vendor, cannot confirm it matches "+
			"the snapshot's %s", c.Vendor)
	}
	if nodeVendor != c.Vendor {
		return fmt.Errorf("snapshot was taken on %s, node is %s: guest memory "+
			"cannot move between CPU vendors", c.Vendor, nodeVendor)
	}

	// Within a vendor, a template is what makes generations interchangeable. It
	// masks the instruction-set features a guest would otherwise have latched
	// onto at boot; what it cannot mask is the family, which a guest kernel
	// reads for errata handling. So family still has to match — the template
	// buys portability across models, not across families.
	if c.Family != 0 && nodeFamily != 0 && c.Family != nodeFamily {
		return fmt.Errorf("snapshot was taken on %s family %d, node is family %d: "+
			"a CPU template cannot hide the family from the guest kernel",
			c.Vendor, c.Family, nodeFamily)
	}
	return nil
}

// portableTemplate is the template name that masks instruction-set features.
// It matches runtime.CPUTemplatePortable; the scheduler compares the string
// rather than importing the node package, which would pull a node-side
// dependency into the control plane for one constant.
const portableTemplate = "portable"

// RequiresSameModel reports whether a snapshot is pinned to its original CPU
// model because it was taken without a template.
//
// Nothing acts on this yet — the model is deliberately not recorded (see
// store.NodeRecord) — so it exists to name the gap rather than to hide it: a
// snapshot taken under "none" is only truly safe back on the same model, and a
// same-family restore of one is a calculated risk, not a guarantee.
func (c CPUConstraint) RequiresSameModel() bool {
	return c.Constrained() && c.Template != portableTemplate
}

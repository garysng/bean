package scheduler

import (
	"errors"
	"strings"
	"testing"

	"github.com/garysng/bean/internal/control/store"
)

func TestCheckCPUUnconstrainedAllowsAnything(t *testing.T) {
	var c CPUConstraint
	if c.Constrained() {
		t.Error("zero constraint reports itself as constraining")
	}
	if err := c.CheckCPU("", 0); err != nil {
		t.Errorf("unconstrained rejected an unknown node: %v", err)
	}
	if err := c.CheckCPU("GenuineIntel", 6); err != nil {
		t.Errorf("unconstrained rejected a known node: %v", err)
	}
}

// TestCheckCPURefusesVendorChange is the case that matters most: a guest kernel
// reads the vendor for errata handling and MSR access, and no template can hide
// it, so this mismatch has to be refused at placement.
func TestCheckCPURefusesVendorChange(t *testing.T) {
	c := CPUConstraint{Vendor: "AuthenticAMD", Family: 23, Template: "portable"}
	err := c.CheckCPU("GenuineIntel", 6)
	if err == nil {
		t.Fatal("cross-vendor restore allowed")
	}
	// The message has to name both sides; "incompatible" alone leaves the reader
	// unable to tell which node would work.
	if !strings.Contains(err.Error(), "AuthenticAMD") || !strings.Contains(err.Error(), "GenuineIntel") {
		t.Errorf("error does not name both CPUs: %v", err)
	}
}

// TestCheckCPURefusesFamilyChangeEvenWithTemplate pins the limit of what a
// template buys. Masking hides instruction-set features, so a snapshot survives
// a move between models; it cannot hide the family, which the guest kernel reads
// directly. Allowing a family change because a template was used would be
// exactly the wrong conclusion to draw from having one.
func TestCheckCPURefusesFamilyChangeEvenWithTemplate(t *testing.T) {
	c := CPUConstraint{Vendor: "AuthenticAMD", Family: 23, Template: "portable"}
	if err := c.CheckCPU("AuthenticAMD", 25); err == nil {
		t.Error("restore across CPU families allowed under a template")
	}
	if err := c.CheckCPU("AuthenticAMD", 23); err != nil {
		t.Errorf("same vendor and family rejected: %v", err)
	}
}

// TestCheckCPURefusesNodeWithUnknownCPU covers the mixed-version cluster. A node
// that predates CPU reporting cannot be shown to be compatible, and treating
// silence as agreement would let a mismatch through precisely while nodes are
// being upgraded.
func TestCheckCPURefusesNodeWithUnknownCPU(t *testing.T) {
	c := CPUConstraint{Vendor: "AuthenticAMD", Family: 23}
	if err := c.CheckCPU("", 0); err == nil {
		t.Error("a node reporting no CPU identity was accepted for a restore")
	}
}

// TestCheckCPUToleratesUnknownFamily accepts a node that reports a vendor but no
// family. The vendor is the constraint that cannot be worked around; refusing on
// a missing family as well would exclude nodes for a fact nobody has claimed.
func TestCheckCPUToleratesUnknownFamily(t *testing.T) {
	c := CPUConstraint{Vendor: "AuthenticAMD", Family: 23}
	if err := c.CheckCPU("AuthenticAMD", 0); err != nil {
		t.Errorf("node with known vendor and unknown family rejected: %v", err)
	}
	// And a snapshot with no recorded family constrains only the vendor.
	c = CPUConstraint{Vendor: "AuthenticAMD"}
	if err := c.CheckCPU("AuthenticAMD", 25); err != nil {
		t.Errorf("snapshot without a family should not constrain it: %v", err)
	}
}

// TestRequiresSameModelTracksTemplate documents which snapshots are only really
// safe back on their original CPU model. Restoring one of these onto a different
// model in the same family is a calculated risk rather than a guarantee, and
// naming it keeps that from being forgotten.
func TestRequiresSameModelTracksTemplate(t *testing.T) {
	if !(CPUConstraint{Vendor: "AuthenticAMD", Template: "none"}).RequiresSameModel() {
		t.Error("a snapshot taken without masking should be flagged as model-bound")
	}
	if !(CPUConstraint{Vendor: "AuthenticAMD"}).RequiresSameModel() {
		t.Error("an empty template is not masking either")
	}
	if (CPUConstraint{Vendor: "AuthenticAMD", Template: "portable"}).RequiresSameModel() {
		t.Error("a masked snapshot should not be model-bound")
	}
	if (CPUConstraint{}).RequiresSameModel() {
		t.Error("an unconstrained snapshot cannot be model-bound")
	}
}

// TestScheduleFiltersIncompatibleCPU checks that the constraint is actually
// applied during placement. CheckCPU passing its own unit tests proves nothing
// about whether feasible() consults it.
func TestScheduleFiltersIncompatibleCPU(t *testing.T) {
	st := newTestStore(t)
	amd := node("amd-1", 8, 8192, func(n *store.NodeRecord) {
		n.CPUVendor, n.CPUFamily, n.CPUTemplate = "AuthenticAMD", 23, "portable"
	})
	intel := node("intel-1", 8, 8192, func(n *store.NodeRecord) {
		n.CPUVendor, n.CPUFamily, n.CPUTemplate = "GenuineIntel", 6, "portable"
	})
	for _, n := range []*store.NodeRecord{amd, intel} {
		if err := st.UpsertNode(n); err != nil {
			t.Fatal(err)
		}
	}
	s := New(st, DefaultWeights())

	// A restore of an AMD snapshot must land on the AMD node even though the
	// Intel one has identical capacity.
	got, err := s.Schedule(req("sbx-1", 1, 512, func(r *Request) {
		r.CPUConstraint = CPUConstraint{Vendor: "AuthenticAMD", Family: 23, Template: "portable"}
	}))
	if err != nil {
		t.Fatalf("scheduling a compatible restore failed: %v", err)
	}
	if got != "amd-1" {
		t.Errorf("placed on %s, want amd-1: the CPU constraint was not applied", got)
	}
}

// TestScheduleReportsIncompatibleCPUDistinctly guards the diagnosis. A CPU
// mismatch surfacing as ErrNoCapacity sends the reader to look at resource
// limits for something no amount of free capacity can fix.
func TestScheduleReportsIncompatibleCPUDistinctly(t *testing.T) {
	st := newTestStore(t)
	intel := node("intel-only", 8, 8192, func(n *store.NodeRecord) {
		n.CPUVendor, n.CPUFamily = "GenuineIntel", 6
	})
	if err := st.UpsertNode(intel); err != nil {
		t.Fatal(err)
	}
	s := New(st, DefaultWeights())

	_, err := s.Schedule(req("sbx-2", 1, 512, func(r *Request) {
		r.CPUConstraint = CPUConstraint{Vendor: "AuthenticAMD", Family: 23}
	}))
	if err == nil {
		t.Fatal("restore onto an incompatible cluster succeeded")
	}
	if !errors.Is(err, ErrIncompatibleCPU) {
		t.Errorf("error is %v, want ErrIncompatibleCPU so the API can answer 409", err)
	}
	if errors.Is(err, ErrNoCapacity) {
		t.Error("a CPU mismatch is reported as missing capacity")
	}
}

// TestScheduleBlamesCapacityWhenThatIsTheRealProblem is the other half: when a
// node is both too small and the wrong CPU, the CPU must not take the blame,
// because fixing it would not make the placement succeed.
func TestScheduleBlamesCapacityWhenThatIsTheRealProblem(t *testing.T) {
	st := newTestStore(t)
	tiny := node("tiny", 1, 256, func(n *store.NodeRecord) {
		n.CPUVendor, n.CPUFamily = "GenuineIntel", 6
	})
	if err := st.UpsertNode(tiny); err != nil {
		t.Fatal(err)
	}
	s := New(st, DefaultWeights())

	_, err := s.Schedule(req("sbx-3", 64, 65536, func(r *Request) {
		r.CPUConstraint = CPUConstraint{Vendor: "AuthenticAMD", Family: 23}
	}))
	if err == nil {
		t.Fatal("expected placement to fail")
	}
	if errors.Is(err, ErrIncompatibleCPU) {
		t.Errorf("blamed the CPU for a node that was also far too small: %v", err)
	}
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("error is %v, want ErrNoCapacity", err)
	}
}

// TestScheduleUnconstrainedIgnoresCPU makes sure a fresh create is not filtered.
// Only a restore carries guest memory; requiring a CPU match for new sandboxes
// would fragment the cluster for no reason.
func TestScheduleUnconstrainedIgnoresCPU(t *testing.T) {
	st := newTestStore(t)
	n := node("any", 8, 8192, func(n *store.NodeRecord) {
		n.CPUVendor, n.CPUFamily = "GenuineIntel", 6
	})
	if err := st.UpsertNode(n); err != nil {
		t.Fatal(err)
	}
	s := New(st, DefaultWeights())
	if _, err := s.Schedule(req("sbx-4", 1, 512)); err != nil {
		t.Errorf("a fresh create was filtered by CPU: %v", err)
	}
}

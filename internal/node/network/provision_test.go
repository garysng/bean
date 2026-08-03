package network

import (
	"errors"
	"strings"
	"testing"
)

// provHost records what was built and torn down, and can be made to fail.
//
// It also serves as the Lister, which is the point: the pool's guarantee is that
// an index in use on the host is never handed out again, and that only holds if
// the thing creating namespaces is the thing reporting them. A fake that let the
// two drift would make these tests pass for a reason the real code does not have.
type provHost struct {
	live      map[string]bool // namespaces currently on the "host"
	setupCmds []string
	tornDown  []string
	routes    []string

	setupErr    error
	teardownErr error
	listErr     error
}

func newProvHost() *provHost {
	return &provHost{live: map[string]bool{}}
}

func (f *provHost) ListNamespaces() ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]string, 0, len(f.live))
	for ns := range f.live {
		out = append(out, ns)
	}
	return out, nil
}

func (f *provHost) ListRoutes() ([]string, error) { return f.routes, nil }

func (f *provHost) Setup(l *Layout) error {
	if f.setupErr != nil {
		// Mirrors the real Setup, which cleans up its own partial work before
		// returning. A fake that left the namespace behind would hide whether the
		// provisioner double-frees.
		return f.setupErr
	}
	f.setupCmds = append(f.setupCmds, l.Netns)
	f.live[l.Netns] = true
	return nil
}

func (f *provHost) Teardown(l *Layout) error {
	f.tornDown = append(f.tornDown, l.Netns)
	if f.teardownErr != nil {
		return f.teardownErr
	}
	delete(f.live, l.Netns)
	return nil
}

const testSubnet = "172.31.0.0/30"

func newTestProvisioner(t *testing.T) (*Provisioner, *provHost) {
	t.Helper()
	host := newProvHost()
	p, err := NewProvisioner(testSubnet, host)
	if err != nil {
		t.Fatal(err)
	}
	return p, host
}

func TestProvisionBuildsTheNamespaceItReserved(t *testing.T) {
	p, host := newTestProvisioner(t)
	layout, err := p.Provision("sbx-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(host.setupCmds) != 1 || host.setupCmds[0] != layout.Netns {
		t.Fatalf("setup built %v, want exactly the reserved namespace %s",
			host.setupCmds, layout.Netns)
	}
	if layout.GuestIP.String() != "172.31.0.2" {
		t.Errorf("guest IP = %s, want the constant 172.31.0.2 that lets a snapshot "+
			"fan out", layout.GuestIP)
	}
}

// TestProvisionReturnsTheSlotWhenSetupFails is the leak this ordering exists
// for. A create whose setup fails must cost nothing: if the index stays held, a
// node loses one addressable slot per failed create and the only symptom is
// refusing a create at a count nobody can explain.
func TestProvisionReturnsTheSlotWhenSetupFails(t *testing.T) {
	p, host := newTestProvisioner(t)
	host.setupErr = errors.New("synthetic setup failure")

	if _, err := p.Provision("sbx-doomed"); err == nil {
		t.Fatal("expected the setup failure to fail the provision")
	}

	// The next sandbox must get index 0, which only happens if the failed one gave
	// its slot back. Asserting on the index rather than on internal state is
	// deliberate: the index is what collides.
	host.setupErr = nil
	layout, err := p.Provision("sbx-next")
	if err != nil {
		t.Fatal(err)
	}
	if layout.Index != 0 {
		t.Errorf("index = %d after a failed provision, want 0; the failed create "+
			"kept its slot, so this node leaks one address per failure", layout.Index)
	}
}

// TestProvisionGivesConcurrentSandboxesDistinctHostAddresses is the collision the
// whole module is arranged to prevent, checked end to end rather than on the
// allocator alone.
func TestProvisionGivesConcurrentSandboxesDistinctHostAddresses(t *testing.T) {
	p, _ := newTestProvisioner(t)
	seen := map[string]string{}
	for _, id := range []string{"a", "b", "c", "d"} {
		l, err := p.Provision(id)
		if err != nil {
			t.Fatal(err)
		}
		if prev, dup := seen[l.HostLinkCIDR()]; dup {
			t.Fatalf("sandboxes %s and %s both got host link %s; two sandboxes "+
				"sharing veth addresses is intermittent connectivity on both",
				prev, id, l.HostLinkCIDR())
		}
		seen[l.HostLinkCIDR()] = id
	}
}

// TestDeprovisionRemovesTheNamespaceAndFreesTheSlot is the destroy-side leak.
// Without the teardown the namespace stays on the host, the allocator sees it on
// the next Reserve and adopts the index, and the node's capacity falls by one per
// destroy.
func TestDeprovisionRemovesTheNamespaceAndFreesTheSlot(t *testing.T) {
	p, host := newTestProvisioner(t)
	first, err := p.Provision("sbx-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Deprovision("sbx-a"); err != nil {
		t.Fatal(err)
	}
	if len(host.tornDown) != 1 || host.tornDown[0] != first.Netns {
		t.Fatalf("tore down %v, want %s", host.tornDown, first.Netns)
	}
	if host.live[first.Netns] {
		t.Fatalf("namespace %s still on the host after deprovision; the allocator "+
			"will adopt its index and the node loses a slot", first.Netns)
	}

	// The slot must come back. Reserving again has to return the same index, which
	// is only free because the namespace actually went away.
	again, err := p.Provision("sbx-b")
	if err != nil {
		t.Fatal(err)
	}
	if again.Index != first.Index {
		t.Errorf("index = %d after deprovision, want %d reused; a destroyed sandbox "+
			"is still holding its address", again.Index, first.Index)
	}
}

// TestDeprovisionIgnoresASandboxItNeverAssigned is the restart case from
// docs/network.md section 3. After a restart the pool knows nothing about
// namespaces the previous noded created, and those may still be serving running
// guests. A destroy for an unknown id must not touch the host.
func TestDeprovisionIgnoresASandboxItNeverAssigned(t *testing.T) {
	p, host := newProvisionerWithPreexisting(t)

	if err := p.Deprovision("sbx-from-a-previous-process"); err != nil {
		t.Fatal(err)
	}
	if len(host.tornDown) != 0 {
		t.Fatalf("tore down %v for a sandbox this process never assigned; that "+
			"namespace may be serving a guest that predates this process, and "+
			"deciding it is an orphan needs the control plane's expected set",
			host.tornDown)
	}
	if !host.live["bean-0"] {
		t.Error("a pre-existing namespace was removed; adoption, not cleanup")
	}
}

// TestProvisionSkipsAnIndexTheHostAlreadyHolds covers adoption on the allocation
// side: a namespace from a previous process makes its index unavailable rather
// than reusable.
func TestProvisionSkipsAnIndexTheHostAlreadyHolds(t *testing.T) {
	p, _ := newProvisionerWithPreexisting(t)
	l, err := p.Provision("sbx-new")
	if err != nil {
		t.Fatal(err)
	}
	if l.Index == 0 {
		t.Fatal("index 0 was handed out while bean-0 is on the host; the new " +
			"sandbox and the pre-existing one now share veth addresses")
	}
}

func newProvisionerWithPreexisting(t *testing.T) (*Provisioner, *provHost) {
	t.Helper()
	host := newProvHost()
	// A namespace left by an earlier incarnation of this process.
	host.live["bean-0"] = true
	p, err := NewProvisioner(testSubnet, host)
	if err != nil {
		t.Fatal(err)
	}
	return p, host
}

// TestDeprovisionFreesTheSlotEvenWhenTeardownFails records why the release is
// unconditional. The host still has the namespace, so the next Reserve adopts
// the index rather than reusing it -- holding it in memory as well would cost the
// node a slot for no added safety.
func TestDeprovisionFreesTheSlotEvenWhenTeardownFails(t *testing.T) {
	p, host := newTestProvisioner(t)
	if _, err := p.Provision("sbx-a"); err != nil {
		t.Fatal(err)
	}
	host.teardownErr = errors.New("synthetic teardown failure")

	err := p.Deprovision("sbx-a")
	if err == nil {
		t.Fatal("a failed teardown must be reported; a namespace left standing is " +
			"a slot this node cannot use again")
	}
	if !strings.Contains(err.Error(), "sbx-a") {
		t.Errorf("error %q does not name the sandbox", err)
	}

	// The still-present namespace must keep its index out of circulation.
	next, err := p.Provision("sbx-b")
	if err != nil {
		t.Fatal(err)
	}
	if next.Index == 0 {
		t.Error("index 0 reissued while its namespace is still on the host after a " +
			"failed teardown; two sandboxes now share addresses")
	}
}

// TestProvisionRefusesWhenTheHostCannotBeListed: not knowing what the host holds
// has to fail the create. Guessing produces the address collision, which is
// intermittent and hard to attribute -- much worse than a create that fails.
func TestProvisionRefusesWhenTheHostCannotBeListed(t *testing.T) {
	p, host := newTestProvisioner(t)
	host.listErr = errors.New("synthetic list failure")
	if _, err := p.Provision("sbx-a"); err == nil {
		t.Fatal("provisioned without knowing which indices the host holds")
	}
	if len(host.setupCmds) != 0 {
		t.Errorf("built %v despite not knowing what the host holds", host.setupCmds)
	}
}

func TestNewProvisionerRejectsASubnetTheLayoutCannotUse(t *testing.T) {
	// A /24 rather than a /30. Caught at startup so the operator who typed it sees
	// it, instead of every create failing for what looks like a per-sandbox reason.
	if _, err := NewProvisioner("172.31.0.0/24", newProvHost()); err == nil {
		t.Error("accepted a subnet wider than a point-to-point link")
	}
	if _, err := NewProvisioner(testSubnet, nil); err == nil {
		t.Error("accepted a nil host, which would provision nothing while reporting success")
	}
}

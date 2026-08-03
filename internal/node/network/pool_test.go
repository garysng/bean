package network

import (
	"errors"
	"testing"
)

// fakeHost stands in for the host's namespace list so allocation can be tested
// without creating namespaces.
type fakeHost struct {
	names []string
	err   error
	calls int
}

func (f *fakeHost) ListNamespaces() ([]string, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.names, nil
}

func TestLayoutGivesEverySandboxTheSameGuestAddress(t *testing.T) {
	// This is the property the whole design rests on: a restored snapshot comes
	// back with the address it had, so every sandbox must already be using that
	// address. If these ever differ, a restore lands on a guest whose configuration
	// does not match its namespace.
	first, err := LayoutFor(0, "172.31.0.0/30")
	if err != nil {
		t.Fatal(err)
	}
	last, err := LayoutFor(MaxIndex, "172.31.0.0/30")
	if err != nil {
		t.Fatal(err)
	}
	if !first.GuestIP.Equal(last.GuestIP) {
		t.Errorf("guest addresses differ across slots (%s vs %s); a restored "+
			"snapshot would not match its namespace", first.GuestIP, last.GuestIP)
	}
	if !first.GuestGateway.Equal(last.GuestGateway) {
		t.Errorf("gateways differ across slots (%s vs %s)",
			first.GuestGateway, last.GuestGateway)
	}
	if first.TapName != last.TapName {
		t.Errorf("tap names differ (%s vs %s); Firecracker records the device name "+
			"in the snapshot and looks for it again on restore",
			first.TapName, last.TapName)
	}
}

func TestLayoutGivesEverySandboxADistinctHostLink(t *testing.T) {
	// The host end cannot be shared: both ends of every veth pair live in the
	// host's own namespace.
	seen := map[string]int{}
	for idx := 0; idx < 200; idx++ {
		l, err := LayoutFor(idx, "172.31.0.0/30")
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{
			l.HostLinkIP.String(), l.NetnsLinkIP.String(),
			l.HostVeth, l.NetnsVeth, l.Netns,
		} {
			if prev, dup := seen[key]; dup {
				t.Fatalf("slot %d reuses %q from slot %d", idx, key, prev)
			}
			seen[key] = idx
		}
	}
}

func TestLayoutLinkSubnetsDoNotOverlap(t *testing.T) {
	// A /30 stride of four is what keeps each link independent. An off-by-one here
	// would put two sandboxes on one subnet, which routes but misdelivers.
	for idx := 0; idx < 200; idx++ {
		a, err := LayoutFor(idx, "172.31.0.0/30")
		if err != nil {
			t.Fatal(err)
		}
		b, err := LayoutFor(idx+1, "172.31.0.0/30")
		if err != nil {
			t.Fatal(err)
		}
		if a.LinkSubnet.Contains(b.HostLinkIP) || b.LinkSubnet.Contains(a.HostLinkIP) {
			t.Fatalf("slots %d and %d share a subnet: %s vs %s",
				idx, idx+1, a.LinkCIDR(), b.LinkCIDR())
		}
	}
}

func TestLayoutRejectsASubnetWiderThanAPointToPointLink(t *testing.T) {
	for _, cidr := range []string{"172.31.0.0/24", "172.31.0.0/29", "172.31.0.0/31"} {
		if _, err := LayoutFor(0, cidr); err == nil {
			t.Errorf("%s should be rejected: a sandbox link holds exactly a gateway "+
				"and a guest", cidr)
		}
	}
}

func TestLayoutRejectsAnOutOfRangeIndex(t *testing.T) {
	for _, idx := range []int{-1, MaxIndex + 1} {
		if _, err := LayoutFor(idx, "172.31.0.0/30"); err == nil {
			t.Errorf("index %d should be rejected", idx)
		}
	}
}

func TestLayoutRendersAddressesWithTheirMask(t *testing.T) {
	l, err := LayoutFor(0, "172.31.0.0/30")
	if err != nil {
		t.Fatal(err)
	}
	if got := l.GuestCIDR(); got != "172.31.0.2/30" {
		t.Errorf("guest address is %q, want 172.31.0.2/30", got)
	}
	if got := l.GatewayCIDR(); got != "172.31.0.1/30" {
		t.Errorf("gateway is %q, want 172.31.0.1/30", got)
	}
	if got := l.HostLinkCIDR(); got != "10.0.0.1/30" {
		t.Errorf("host link is %q, want 10.0.0.1/30", got)
	}
	if got := l.LinkCIDR(); got != "10.0.0.0/30" {
		t.Errorf("link subnet is %q, want 10.0.0.0/30", got)
	}
}

func TestReserveHandsOutTheLowestFreeSlot(t *testing.T) {
	a := NewAllocator("172.31.0.0/30", &fakeHost{})
	for want := 0; want < 3; want++ {
		l, err := a.Reserve("sbx_" + string(rune('a'+want)))
		if err != nil {
			t.Fatal(err)
		}
		if l.Index != want {
			t.Errorf("got slot %d, want %d", l.Index, want)
		}
	}
}

// The property the loop-device leak taught us: the host is the authority, because
// a count in process memory does not survive a restart while the namespaces do.
func TestReserveSkipsSlotsTheHostAlreadyHolds(t *testing.T) {
	host := &fakeHost{names: []string{"bean-0", "bean-1", "bean-3"}}
	a := NewAllocator("172.31.0.0/30", host)

	l, err := a.Reserve("sbx_new")
	if err != nil {
		t.Fatal(err)
	}
	if l.Index != 2 {
		t.Fatalf("got slot %d; slots 0, 1 and 3 exist on the host and reusing one "+
			"would give two sandboxes the same veth addresses", l.Index)
	}
}

func TestReserveIgnoresNamespacesThatAreNotOurs(t *testing.T) {
	// A shared host runs other workloads. Counting their namespaces would shrink
	// the pool for no reason, and a teardown matching them would destroy their
	// networking.
	host := &fakeHost{names: []string{"cni-1234", "docker-abc", "0", "bean-", "beanx-0"}}
	a := NewAllocator("172.31.0.0/30", host)
	l, err := a.Reserve("sbx_new")
	if err != nil {
		t.Fatal(err)
	}
	if l.Index != 0 {
		t.Errorf("foreign namespaces were counted as ours: got slot %d, want 0", l.Index)
	}
}

func TestReserveRefusesWhenTheHostCannotBeRead(t *testing.T) {
	// Guessing here produces two sandboxes sharing addresses, which fails
	// intermittently and is hard to attribute. A failed create is much better.
	a := NewAllocator("172.31.0.0/30", &fakeHost{err: errors.New("ip: command not found")})
	if _, err := a.Reserve("sbx_new"); err == nil {
		t.Fatal("expected a refusal when the host's namespaces cannot be listed")
	}
}

func TestReserveIsIdempotentForOneSandbox(t *testing.T) {
	// Create is retried on more than one path; a second Reserve must not leak a
	// second namespace for the same sandbox.
	a := NewAllocator("172.31.0.0/30", &fakeHost{})
	first, err := a.Reserve("sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Reserve("sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	if first.Index != second.Index {
		t.Errorf("one sandbox was given two slots (%d and %d)", first.Index, second.Index)
	}
}

func TestReleaseFreesTheSlotForReuse(t *testing.T) {
	a := NewAllocator("172.31.0.0/30", &fakeHost{})
	first, err := a.Reserve("sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	a.Release("sbx_a")
	second, err := a.Reserve("sbx_b")
	if err != nil {
		t.Fatal(err)
	}
	if second.Index != first.Index {
		t.Errorf("released slot %d was not reused; got %d", first.Index, second.Index)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	a := NewAllocator("172.31.0.0/30", &fakeHost{})
	if _, err := a.Reserve("sbx_a"); err != nil {
		t.Fatal(err)
	}
	a.Release("sbx_a")
	// Teardown runs from an error return and from a deferred cleanup, so a repeated
	// release must not free a slot that now belongs to someone else.
	if _, err := a.Reserve("sbx_b"); err != nil {
		t.Fatal(err)
	}
	a.Release("sbx_a")
	if _, ok := a.LayoutOf("sbx_b"); !ok {
		t.Error("a repeated release for one sandbox dropped another's slot")
	}
}

func TestLayoutOfReportsWhatWasAssigned(t *testing.T) {
	a := NewAllocator("172.31.0.0/30", &fakeHost{})
	want, err := a.Reserve("sbx_a")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := a.LayoutOf("sbx_a")
	if !ok {
		t.Fatal("assigned sandbox has no layout")
	}
	if got.Index != want.Index {
		t.Errorf("layout reports slot %d, assigned %d", got.Index, want.Index)
	}
	if _, ok := a.LayoutOf("sbx_unknown"); ok {
		t.Error("an unassigned sandbox reported a layout")
	}
}

func TestIndexOfNetnsAcceptsOnlyOurOwnNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		idx  int
		ok   bool
	}{
		{"bean-0", 0, true},
		{"bean-4095", 4095, true},
		{"bean-4096", 0, false}, // above MaxIndex
		{"bean--1", 0, false},   // negative
		{"bean-", 0, false},     // no index
		{"bean-abc", 0, false},  // not a number
		{"beanx-0", 0, false},   // different prefix
		{"cni-0", 0, false},     // another workload
		{"0", 0, false},         // no prefix
		{"bean-0-extra", 0, false},
	} {
		idx, ok := indexOfNetns(tc.name)
		if ok != tc.ok {
			t.Errorf("%q: accepted=%v, want %v", tc.name, ok, tc.ok)
			continue
		}
		if ok && idx != tc.idx {
			t.Errorf("%q: index %d, want %d", tc.name, idx, tc.idx)
		}
	}
}

// Package network assigns each sandbox its own network namespace and the
// addresses that reach it.
//
// The shape of this is decided by one Firecracker property: a restored snapshot
// resumes with the same network configuration it had, most importantly the same
// IP. A fan-out — one prepared environment cloned N times, which is the core
// evaluation workload — therefore produces N guests holding identical addresses.
//
// Rather than renumber each restored guest, every sandbox gets its own network
// namespace and the guest address is a constant. Renumbering would add three
// steps to the restore path (send the instruction, wait for the guest to apply
// it, wait out the ARP cache) and a failure in any of them looks like
// "networking works, but only sometimes" — the least debuggable kind.
//
// What cannot be constant is the host end of the veth pair, since every pair
// lives in the host's own namespace. Those are derived from an index, and this
// file is the allocation of that index.
//
// See docs/network.md for the full design and the two properties verified on
// hardware before it was written.
package network

import (
	"fmt"
	"net"
)

// Layout describes the addresses one sandbox uses.
//
// GuestIP and GuestGateway are the same for every sandbox on a node. That is the
// point, not an oversight: it is what lets a snapshot restore without the guest
// learning anything new.
type Layout struct {
	// Index is the sandbox's slot, and the only thing that varies.
	Index int
	// Netns is the namespace name, prefixed so an operator sweeping a shared host
	// can tell bean's namespaces from anything else's.
	Netns string
	// TapName is the device the VMM attaches to. It is identical in every
	// namespace, which is why a snapshot finds the device it recorded.
	TapName string
	// GuestIP is what the guest configures on eth0.
	GuestIP net.IP
	// GuestGateway is the tap's address inside the namespace, the guest's default
	// route.
	GuestGateway net.IP
	// GuestSubnet covers exactly the gateway and the guest.
	GuestSubnet *net.IPNet

	// HostVeth and NetnsVeth are the two ends of the pair joining the namespace to
	// the host. Both names carry the index because both are visible from the host.
	HostVeth  string
	NetnsVeth string
	// HostLinkIP and NetnsLinkIP are unique per sandbox.
	HostLinkIP  net.IP
	NetnsLinkIP net.IP
	// LinkSubnet is the /30 holding just those two.
	LinkSubnet *net.IPNet
}

const (
	// tapName is deliberately not derived from the sandbox id. Firecracker
	// records the host device name in the snapshot and looks for it again on
	// restore; a per-sandbox name would mean every restore needed a
	// network_overrides entry, and the guest would have to be told about it.
	tapName = "beantap0"

	// netnsPrefix marks bean's namespaces on a host shared with other workloads.
	// Reconciliation and teardown both match on it, so nothing outside the prefix
	// can be touched by accident.
	netnsPrefix = "bean-"

	// vethHostPrefix and vethNetnsPrefix are separated so a stray interface is
	// identifiable as one end or the other. Interface names are capped at 15
	// characters by the kernel, which bounds how long these can be.
	vethHostPrefix  = "bnv"
	vethNetnsPrefix = "bnp"
)

// MaxIndex bounds the pool.
//
// The limit is not the address space — 10/8 in /30 steps holds far more links
// than a host could ever run sandboxes. It is interface naming: "bnv" plus a
// decimal index has to fit in the kernel's 15-character limit, and a cap well
// inside that leaves the failure impossible rather than merely unlikely.
//
// A sandbox count is bounded by CPU and memory long before this, so a limit here
// should never be the thing an operator hits. If it is, that is a bug in
// accounting rather than a reason to raise this.
const MaxIndex = 4095

// LayoutFor derives every address for a slot.
//
// guestCIDR is the subnet the guest sees, identical for all sandboxes. It must be
// a /30: exactly two usable addresses, one gateway and one guest. Anything larger
// would suggest the point-to-point invariant can be broken.
func LayoutFor(index int, guestCIDR string) (*Layout, error) {
	if index < 0 || index > MaxIndex {
		return nil, fmt.Errorf("network: index %d out of range [0,%d]", index, MaxIndex)
	}
	guestIP, guestNet, err := net.ParseCIDR(guestCIDR)
	if err != nil {
		return nil, fmt.Errorf("network: parse guest subnet %q: %w", guestCIDR, err)
	}
	ones, bits := guestNet.Mask.Size()
	if bits != 32 {
		return nil, fmt.Errorf("network: guest subnet %q must be IPv4", guestCIDR)
	}
	if ones != 30 {
		return nil, fmt.Errorf("network: guest subnet %q must be a /30 — a sandbox "+
			"link holds exactly a gateway and a guest, and a wider mask would imply "+
			"otherwise", guestCIDR)
	}
	_ = guestIP

	base := guestNet.IP.To4()
	if base == nil {
		return nil, fmt.Errorf("network: guest subnet %q must be IPv4", guestCIDR)
	}

	// Host end: 10.<index/64>.<(index%64)*4>.1/30, netns end .2. The /30 stride of
	// four is what keeps each sandbox's link independent.
	a := byte(index / 64)
	b := byte((index % 64) * 4)

	return &Layout{
		Index:        index,
		Netns:        fmt.Sprintf("%s%d", netnsPrefix, index),
		TapName:      tapName,
		GuestGateway: net.IPv4(base[0], base[1], base[2], base[3]+1),
		GuestIP:      net.IPv4(base[0], base[1], base[2], base[3]+2),
		GuestSubnet:  guestNet,
		HostVeth:     fmt.Sprintf("%s%d", vethHostPrefix, index),
		NetnsVeth:    fmt.Sprintf("%s%d", vethNetnsPrefix, index),
		HostLinkIP:   net.IPv4(10, a, b, 1),
		NetnsLinkIP:  net.IPv4(10, a, b, 2),
		LinkSubnet: &net.IPNet{
			IP:   net.IPv4(10, a, b, 0),
			Mask: net.CIDRMask(30, 32),
		},
	}, nil
}

// GuestCIDR renders the address the guest configures, with its mask.
func (l *Layout) GuestCIDR() string {
	ones, _ := l.GuestSubnet.Mask.Size()
	return fmt.Sprintf("%s/%d", l.GuestIP, ones)
}

// GatewayCIDR renders the tap's address inside the namespace.
func (l *Layout) GatewayCIDR() string {
	ones, _ := l.GuestSubnet.Mask.Size()
	return fmt.Sprintf("%s/%d", l.GuestGateway, ones)
}

// HostLinkCIDR and NetnsLinkCIDR render the veth ends.
func (l *Layout) HostLinkCIDR() string  { return l.HostLinkIP.String() + "/30" }
func (l *Layout) NetnsLinkCIDR() string { return l.NetnsLinkIP.String() + "/30" }

// LinkCIDR renders the link subnet, which is what a NAT rule matches on.
func (l *Layout) LinkCIDR() string { return l.LinkSubnet.String() }

// GuestSubnetCIDR renders the guest subnet, the other NAT rule's match.
func (l *Layout) GuestSubnetCIDR() string { return l.GuestSubnet.String() }

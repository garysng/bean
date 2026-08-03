package network

import (
	"fmt"
	"net"
	"strings"
)

// Startup check for docs/network.md section 2: refuse to run if the configured
// guest subnet is already routed on this host.
//
// The guest subnet is a choice, not a safe constant. 172.31.0.0/30 sits at the
// tail of Docker's default range precisely because the six /16s Docker has
// already taken on the target hosts (172.17 through 172.22, measured with ip
// route) make the rest of 172.16/12 unusable. "Smallest collision surface" is
// not "no collision", so overlap is checked rather than assumed away.
//
// The reason this is fatal rather than a warning is the shape of the failure. An
// overlapping route means sandbox traffic is matched by another subsystem's
// MASQUERADE rules -- Docker's, typically -- and what an operator sees is not a
// broken node but sandboxes whose network works sometimes. That is the single
// hardest fault to attribute in this system, and it is cheap to refuse at
// startup on the machine somebody typed the flag on.

// RouteLister reports the destination prefixes the host routes.
//
// An interface for the same reason the rest of this package has one: the
// decision worth testing is which overlaps count, and that is decidable without
// a host that happens to have a conflicting route configured.
type RouteLister interface {
	// ListRoutes returns route destinations in CIDR form. A default route may be
	// reported as "default" or "0.0.0.0/0"; both are ignored, see CheckSubnetFree.
	ListRoutes() ([]string, error)
}

// CheckSubnetFree fails if guestCIDR overlaps a prefix the host already routes.
//
// A listing failure is fatal too. The check exists because the consequence of
// colliding is invisible, so "could not tell" has to be treated as "might
// collide" -- a node that refuses to start is diagnosable in a way that
// intermittent sandbox connectivity is not.
func CheckSubnetFree(guestCIDR string, routes RouteLister) error {
	_, guestNet, err := net.ParseCIDR(guestCIDR)
	if err != nil {
		return fmt.Errorf("network: parse guest subnet %q: %w", guestCIDR, err)
	}
	if routes == nil {
		return fmt.Errorf("network: cannot check whether %s is already routed", guestCIDR)
	}
	list, err := routes.ListRoutes()
	if err != nil {
		return fmt.Errorf("network: cannot list host routes, so it is unknown whether "+
			"%s collides with an existing one: %w", guestCIDR, err)
	}
	for _, dst := range list {
		dst = strings.TrimSpace(dst)
		if dst == "" {
			continue
		}
		// The default route is skipped rather than treated as overlapping
		// everything. It does overlap in the arithmetic sense, and on any host with
		// egress it exists -- so counting it would make this check refuse every
		// node. What it is looking for is a specific competing claim.
		if dst == "default" || dst == "0.0.0.0/0" || dst == "::/0" {
			continue
		}
		_, dstNet, err := net.ParseCIDR(dst)
		if err != nil {
			// An unparsable entry is skipped rather than fatal. "ip route" prints
			// more shapes than destinations (blackhole, multipath continuations), and
			// refusing to start over one this does not recognise would make the node
			// hostage to route table formatting.
			continue
		}
		if netsOverlap(guestNet, dstNet) {
			return fmt.Errorf("network: guest subnet %s overlaps the existing host "+
				"route %s. Sandbox traffic in that range would be matched by whatever "+
				"owns it -- Docker's MASQUERADE rules on these hosts -- and the symptom "+
				"is a sandbox network that works only sometimes, so this node refuses "+
				"to start. Pick a free range with --guest-subnet", guestCIDR, dst)
		}
	}
	return nil
}

// netsOverlap reports whether two prefixes share any address.
//
// Containment is checked in both directions. A /30 guest subnet inside a routed
// /16 is the case that actually happens (Docker's networks are /16), and testing
// only whether the guest subnet contains the route's base address would miss it
// entirely -- the failure would then be exactly the silent one this file exists
// to prevent.
func netsOverlap(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

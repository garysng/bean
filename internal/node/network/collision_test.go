package network

import (
	"errors"
	"strings"
	"testing"
)

type fakeRoutes struct {
	dsts []string
	err  error
}

func (f fakeRoutes) ListRoutes() ([]string, error) { return f.dsts, f.err }

// TestSubnetInsideADockerNetworkIsRefused is the case docs/network.md section 2
// is written about. Docker's networks are /16s and the guest subnet is a /30
// inside one, so the overlap is containment in the direction a naive check misses.
// Letting it through means Docker's MASQUERADE eats sandbox traffic and the
// operator sees a network that works sometimes.
func TestSubnetInsideADockerNetworkIsRefused(t *testing.T) {
	routes := fakeRoutes{dsts: []string{
		"default", "172.17.0.0/16", "172.18.0.0/16", "172.31.0.0/16", "192.168.1.0/24",
	}}
	err := CheckSubnetFree("172.31.0.0/30", routes)
	if err == nil {
		t.Fatal("accepted a guest subnet sitting inside a routed /16; sandbox " +
			"traffic there is matched by whoever owns the range")
	}
	if !strings.Contains(err.Error(), "172.31.0.0/16") {
		t.Errorf("error %q does not name the colliding route, which is the one thing "+
			"an operator needs to act on", err)
	}
}

// TestSubnetContainingARouteIsRefused is the other direction: a route more
// specific than the configured subnet. Checking containment only one way would
// pass this.
func TestSubnetContainingARouteIsRefused(t *testing.T) {
	routes := fakeRoutes{dsts: []string{"172.31.0.1/32"}}
	if err := CheckSubnetFree("172.31.0.0/30", routes); err == nil {
		t.Error("accepted a guest subnet that contains an existing host route")
	}
}

// TestAFreeSubnetIsAccepted guards against the check being so eager that no node
// can start. The six /16s Docker holds are all present here and none of them
// overlap the chosen range.
func TestAFreeSubnetIsAccepted(t *testing.T) {
	routes := fakeRoutes{dsts: []string{
		"default via 10.0.0.1", "172.17.0.0/16", "172.18.0.0/16", "172.19.0.0/16",
		"172.20.0.0/16", "172.21.0.0/16", "172.22.0.0/16", "10.0.0.0/24",
	}}
	if err := CheckSubnetFree("172.31.0.0/30", routes); err != nil {
		t.Errorf("refused a free subnet: %v", err)
	}
}

// TestTheDefaultRouteIsNotACollision: a default route overlaps everything
// arithmetically and exists on every host with egress, so counting it would make
// this check refuse every node it ran on.
func TestTheDefaultRouteIsNotACollision(t *testing.T) {
	for _, dst := range []string{"default", "0.0.0.0/0"} {
		if err := CheckSubnetFree("172.31.0.0/30", fakeRoutes{dsts: []string{dst}}); err != nil {
			t.Errorf("route %q treated as a collision: %v", dst, err)
		}
	}
}

// TestAnUnreadableRouteTableIsFatal: the failure this check prevents is silent,
// so "cannot tell" has to be treated as "might collide". A node that refuses to
// start is diagnosable; intermittent sandbox connectivity is not.
func TestAnUnreadableRouteTableIsFatal(t *testing.T) {
	err := CheckSubnetFree("172.31.0.0/30", fakeRoutes{err: errors.New("synthetic")})
	if err == nil {
		t.Fatal("started without being able to tell whether the subnet collides")
	}
	if err := CheckSubnetFree("172.31.0.0/30", nil); err == nil {
		t.Error("a nil route lister passed the check, so nothing was checked")
	}
}

// TestAnUnparsableRouteEntryDoesNotStopTheNode: ip route prints shapes this does
// not model, and being unable to start because of route table formatting would be
// a worse failure than the one being prevented.
func TestAnUnparsableRouteEntryDoesNotStopTheNode(t *testing.T) {
	routes := fakeRoutes{dsts: []string{"broadcast", "proto kernel scope link", "10.1.0.0/24"}}
	if err := CheckSubnetFree("172.31.0.0/30", routes); err != nil {
		t.Errorf("an unrecognised route entry stopped the node: %v", err)
	}
}

func TestCheckRejectsAnUnparsableSubnet(t *testing.T) {
	if err := CheckSubnetFree("not-a-cidr", fakeRoutes{}); err == nil {
		t.Error("accepted a guest subnet that is not a CIDR")
	}
}

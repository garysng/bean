//go:build linux

package network

import (
	"strings"
	"testing"
)

// routeOutput is real "ip -4 -o route list" output from a host running Docker:
// the six /16s that make 172.16/12 unusable, plus the shapes the parser has to
// step over. Using recorded output rather than a hand-made list is the point --
// the field positions are what this parses, and inventing them would test the
// invention.
const routeOutput = `default via 10.0.0.1 dev eth0 proto dhcp src 10.0.0.15 metric 100
10.0.0.0/24 dev eth0 proto kernel scope link src 10.0.0.15 metric 100
172.17.0.0/16 dev docker0 proto kernel scope link src 172.17.0.1 linkdown
172.18.0.0/16 dev br-1a2b3c proto kernel scope link src 172.18.0.1
172.19.0.0/16 dev br-4d5e6f proto kernel scope link src 172.19.0.1 linkdown
172.20.0.0/16 dev br-778899 proto kernel scope link src 172.20.0.1
172.21.0.0/16 dev br-aabbcc proto kernel scope link src 172.21.0.1
172.22.0.0/16 dev br-ddeeff proto kernel scope link src 172.22.0.1
blackhole 10.42.0.0/24 proto bird
`

func TestListRoutesReadsTheDestinationOfEveryLine(t *testing.T) {
	s := &LinuxSetup{Uplink: "eth0", Cmd: &recorder{out: []byte(routeOutput)}}
	got, err := s.ListRoutes()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"default", "10.0.0.0/24", "172.17.0.0/16", "172.18.0.0/16", "172.19.0.0/16",
		"172.20.0.0/16", "172.21.0.0/16", "172.22.0.0/16", "10.42.0.0/24",
	}
	if len(got) != len(want) {
		t.Fatalf("routes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("route %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestListRoutesSkipsTheRouteTypeKeyword: "blackhole 10.42.0.0/24" would
// otherwise contribute "blackhole", which fails to parse and is silently
// dropped -- taking a real route claim with it. The prefix has to survive.
func TestListRoutesSkipsTheRouteTypeKeyword(t *testing.T) {
	s := &LinuxSetup{Cmd: &recorder{out: []byte("blackhole 172.31.0.0/16 proto bird\n")}}
	got, err := s.ListRoutes()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "172.31.0.0/16" {
		t.Fatalf("routes = %v, want [172.31.0.0/16]", got)
	}
	// The whole reason to parse it: this claim must be able to stop a node.
	if err := CheckSubnetFree("172.31.0.0/30", s); err == nil {
		t.Error("a blackhole route over the guest subnet did not stop the node")
	}
}

// TestListRoutesSkipsMultipathContinuations: "nexthop" lines repeat a route whose
// destination was already recorded, and they carry no destination of their own.
func TestListRoutesSkipsMultipathContinuations(t *testing.T) {
	out := "10.1.0.0/24 proto bird metric 32 \n\tnexthop via 10.0.0.2 dev eth0 weight 1 \n"
	s := &LinuxSetup{Cmd: &recorder{out: []byte(out)}}
	got, err := s.ListRoutes()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "10.1.0.0/24" {
		t.Fatalf("routes = %v, want [10.1.0.0/24]", got)
	}
}

// TestListRoutesAsksForIPv4Only: the check compares v4 prefixes, and v6
// destinations in the list would all fail to parse as v4 and be dropped. Asking
// the kernel for v4 keeps the filtering in one place.
func TestListRoutesAsksForIPv4Only(t *testing.T) {
	rec := &recorder{out: []byte("")}
	s := &LinuxSetup{Cmd: rec}
	if _, err := s.ListRoutes(); err != nil {
		t.Fatal(err)
	}
	if len(rec.cmds) != 1 || !strings.Contains(rec.cmds[0], "-4") {
		t.Errorf("commands = %v, want a single IPv4 route listing", rec.cmds)
	}
}

// TestCheckSubnetFreeAcceptsTheDocumentedSubnetOnADockerHost is the end-to-end
// claim from docs/network.md section 2: 172.31.0.0/30 is free on a host holding
// Docker's six /16s. If this ever fails, the documented default is wrong.
func TestCheckSubnetFreeAcceptsTheDocumentedSubnetOnADockerHost(t *testing.T) {
	s := &LinuxSetup{Cmd: &recorder{out: []byte(routeOutput)}}
	if err := CheckSubnetFree("172.31.0.0/30", s); err != nil {
		t.Errorf("the documented guest subnet was refused on a normal Docker host: %v", err)
	}
	// And the ranges Docker does hold must be refused, or the check is inert.
	if err := CheckSubnetFree("172.17.0.0/30", s); err == nil {
		t.Error("a subnet inside Docker's 172.17.0.0/16 was accepted; its traffic " +
			"would be matched by Docker's MASQUERADE")
	}
}

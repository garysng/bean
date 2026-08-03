//go:build linux

package network

import (
	"fmt"
	"strings"
)

// ListRoutes reads the host's IPv4 route destinations, implementing RouteLister
// so the startup check in collision.go can run against the real host.
//
// This is on LinuxSetup rather than a type of its own so it goes through the same
// Commander seam as everything else here: a test can supply route output without
// a host that has the conflicting route configured.
//
// Only the destination field is taken. "ip route" lines carry the device, the
// source hint, the metric and more, and the only thing this check compares is the
// prefix -- parsing more would be extra surface for no gain.
func (s *LinuxSetup) ListRoutes() ([]string, error) {
	out, err := s.cmd().Output("ip", "-4", "-o", "route", "list")
	if err != nil {
		return nil, fmt.Errorf("network: ip route list: %w", err)
	}
	var dsts []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) == 0 {
			continue
		}
		// Route types precede the destination: "blackhole 10.1.0.0/24", and
		// multipath lines begin with "nexthop". The destination is the first field
		// on a plain line and the second when a type is named, so a known set of
		// leading keywords is skipped rather than assuming position.
		dst := fields[0]
		switch dst {
		case "blackhole", "unreachable", "prohibit", "throw", "local", "broadcast",
			"multicast", "anycast", "unicast", "nat":
			if len(fields) < 2 {
				continue
			}
			dst = fields[1]
		case "nexthop":
			// A continuation of the previous multipath route. Its destination was
			// already recorded from the line that introduced it.
			continue
		}
		dsts = append(dsts, dst)
	}
	return dsts, nil
}

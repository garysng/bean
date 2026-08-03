//go:build linux

package network

// NewHost returns the host implementation that creates namespaces for real.
//
// Constructed behind a build tag for the same reason reclaim.NewLinuxHost is:
// namespaces, veth pairs and iptables are Linux facilities, and noded still has
// to build and be testable on a developer's machine. The non-Linux half returns
// nil, and a nil host means networking is off -- which is the only correct state
// on a platform that cannot create a namespace.
func NewHost(uplink string) Host {
	return &LinuxSetup{Uplink: uplink}
}

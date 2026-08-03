//go:build !linux

package network

// NewHost reports that sandbox networking cannot be provided off Linux.
//
// Network namespaces, veth pairs and iptables have no equivalent here, so there
// is nothing to fall back to. Returning nil rather than an error keeps noded
// buildable and testable on a developer's machine: the caller treats a nil host
// as "networking not configured", which is the pre-existing no-interface
// behaviour and the only honest state on this platform.
func NewHost(uplink string) Host {
	return nil
}

//go:build !linux

package node

import (
	"context"
	"fmt"
	"net"
)

// dialInNetns is unavailable off Linux. Network namespaces are a Linux facility, and
// so is the microVM tier that needs them, so this returns an error rather than
// silently dialling in the host namespace -- which on a developer machine would
// connect to whatever happens to answer on that address.
func dialInNetns(_ context.Context, nsPath, addr string) (net.Conn, error) {
	return nil, fmt.Errorf("agent dial: network namespaces are Linux-only "+
		"(wanted %s in %s)", addr, nsPath)
}

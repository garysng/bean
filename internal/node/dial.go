package node

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/garysng/bean/internal/node/vsock"
)

// dialAgentAddr connects to an agent address, choosing the transport from its
// scheme. gRPC's default dialer handles host:port but not the two forms the
// node uses — a Unix socket for process-level sandboxes, vsock for microVMs —
// so the runtime tier decides the transport without the Manager knowing which
// tier it is talking to.
func dialAgentAddr(ctx context.Context, target string) (net.Conn, error) {
	// gRPC strips a registered scheme but passes through the rest, so the
	// address arrives with or without the prefix depending on the target form.
	switch {
	// netns:<path>|<host>:<port> -- a TCP address that only exists inside one
	// sandbox's network namespace. The namespace is part of the address because the
	// host:port half is identical for every sandbox on the node: dialling it from the
	// host namespace would reach nothing, or another sandbox.
	case strings.HasPrefix(target, "netns:"):
		rest := strings.TrimPrefix(target, "netns:")
		nsPath, addr, ok := strings.Cut(rest, "|")
		if !ok || nsPath == "" || addr == "" {
			return nil, fmt.Errorf("agent dial: malformed netns target %q", target)
		}
		return dialInNetns(ctx, nsPath, addr)

	case strings.HasPrefix(target, vsock.Scheme+":"):
		addr, err := vsock.ParseAddr(target)
		if err != nil {
			return nil, err
		}
		return vsock.Dial(ctx, addr)

	case strings.HasPrefix(target, "unix://"):
		var d net.Dialer
		return d.DialContext(ctx, "unix", strings.TrimPrefix(target, "unix://"))

	case strings.HasPrefix(target, "/"):
		// A bare absolute path is a Unix socket: gRPC hands the dialer the
		// authority-stripped target for unix:// endpoints.
		var d net.Dialer
		return d.DialContext(ctx, "unix", target)

	case target == "":
		return nil, fmt.Errorf("agent dial: empty address")

	default:
		var d net.Dialer
		return d.DialContext(ctx, "tcp", target)
	}
}

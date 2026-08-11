//go:build probe

// vsock-health-probe dials a live firecracker vsock UDS exactly as noded does --
// CONNECT handshake, then a gRPC client over that conn -- and calls the agent's
// Health method. It reports the precise gRPC error, which the create path hides
// behind a generic "agent not healthy" timeout.
//
//	go run -tags probe ./hack/vsock-health-probe.go <uds-path> <port>
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/garysng/bean/internal/gen/bean/agent/v1"
	"github.com/garysng/bean/internal/node/vsock"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: vsock-health-probe <uds-path> <port>")
		os.Exit(2)
	}
	addr := vsock.Addr{SocketPath: os.Args[1]}
	fmt.Sscanf(os.Args[2], "%d", &addr.Port)

	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return vsock.Dial(ctx, addr)
	}
	conn, err := grpc.NewClient("passthrough:///vsock",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer))
	if err != nil {
		fmt.Println("DIAL_ERR:", err)
		os.Exit(1)
	}
	defer conn.Close()

	c := agentv1.NewAgentServiceClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.Health(ctx, &agentv1.HealthRequest{})
	if err != nil {
		fmt.Println("HEALTH_ERR:", err)
		os.Exit(1)
	}
	fmt.Printf("HEALTH_OK: %+v\n", resp)
}

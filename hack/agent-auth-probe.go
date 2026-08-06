//go:build probe

// agent-auth-probe asks whether a sandbox's agent can be used by whoever can reach
// it, which is the question serving the agent over TCP raises.
//
// On vsock the answer was structural: no process inside the guest could dial the
// agent, because the address family is host-to-guest. On TCP any process in the
// sandbox can connect -- verified -- so the only thing left is the token check, and
// a check that is present but ineffective looks exactly like a check that works.
//
// Run it from a context that can reach the agent's address. It makes three calls:
// without a credential, with a wrong one, and with the hash (which the guest can read
// from the metadata service and must not be usable as a token).
//
//	go run -tags probe ./hack/agent-auth-probe.go <addr> [hash]
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	agentv1 "github.com/garysng/bean/internal/gen/bean/agent/v1"
	"github.com/garysng/bean/internal/sbxtoken"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: agent-auth-probe <host:port> [published-hash] [authority]")
		os.Exit(2)
	}
	addr := os.Args[1]

	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	// When the target is a Host-routed forwarder rather than the agent itself, the
	// routing key is the authority -- gRPC's :authority header, which is what the
	// forwarder reads as Host. Without it the request reaches the forwarder's
	// unroutable-host path and the gRPC client reports a bad server preface, because
	// what it is reading is an HTTP/1.1 error page.
	if len(os.Args) > 3 {
		opts = append(opts, grpc.WithAuthority(os.Args[3]))
	}
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		fmt.Println("dial:", err)
		os.Exit(1)
	}
	defer conn.Close()
	client := agentv1.NewAgentServiceClient(conn)

	attempt := func(label, token string) {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if token != "" {
			ctx = metadata.AppendToOutgoingContext(ctx, sbxtoken.MDKey, token)
		}
		_, err := client.Health(ctx, &agentv1.HealthRequest{})
		if err == nil {
			fmt.Printf("%-24s -> ADMITTED (this is a finding)\n", label)
			return
		}
		fmt.Printf("%-24s -> refused: %v\n", label, err)
	}

	attempt("no credential", "")
	attempt("wrong credential", "deadbeefdeadbeef")
	if len(os.Args) > 2 {
		// The published hash is readable by the sandbox. If presenting it were
		// accepted, the credential would be one the sandbox already holds and the
		// whole arrangement would be decorative.
		attempt("the published hash", os.Args[2])
	}
	fmt.Println("note: a correct token is not tried here; noded holds it and this " +
		"probe deliberately runs without it")
	_ = sbxtoken.Hash
}

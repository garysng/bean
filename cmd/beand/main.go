// beand is the in-sandbox init/PID1 agent. On the fc tier it is
// injected via the agent disk and listens on vsock; in dev/container
// mode it listens on a unix socket.
package main

import (
	"flag"
	"log"
	"net"
	"os"

	"google.golang.org/grpc"

	"github.com/garysng/bean/internal/beand"
	agentv1 "github.com/garysng/bean/internal/gen/bean/agent/v1"
)

var version = "dev"

func main() {
	listenAddr := flag.String("listen", "/run/bean/agent.sock", "unix socket path (or vsock:PORT on fc tier)")
	rootDir := flag.String("root", "", "confine file ops under this dir (dev mode); empty = host root")
	flag.Parse()

	_ = os.Remove(*listenAddr)
	lis, err := net.Listen("unix", *listenAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", *listenAddr, err)
	}

	srv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(srv, beand.NewServer(version, *rootDir))
	log.Printf("beand %s listening on %s (root=%q)", version, *listenAddr, *rootDir)
	if err := srv.Serve(lis); err != nil {
		log.Fatal(err)
	}
}

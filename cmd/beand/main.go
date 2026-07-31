// beand is the node daemon: it manages sandbox lifecycle on one node and
// exposes SandboxService over gRPC for the control plane / gateway.
package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"google.golang.org/grpc"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node"
	"github.com/garysng/bean/internal/node/runtime"
)

var version = "dev"

func main() {
	listen := flag.String("listen", "127.0.0.1:7443", "gRPC listen address")
	rtName := flag.String("runtime", "local", "runtime: local|fc")
	agentBin := flag.String("agent-bin", "bean-agent", "path to bean-agent binary (local runtime)")
	baseDir := flag.String("base-dir", "/var/lib/bean/sandboxes", "sandbox base directory")
	flag.Parse()

	var rt runtime.Runtime
	switch *rtName {
	case "local":
		rt = runtime.NewLocalRuntime(*agentBin, *baseDir)
	default:
		log.Fatalf("runtime %q not supported on this build (fc requires linux+KVM)", *rtName)
	}

	mgr := node.NewManager(rt)
	defer mgr.Close()

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	nodev1.RegisterSandboxServiceServer(srv, node.NewGRPCServer(mgr))

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("shutting down")
		srv.GracefulStop()
	}()

	log.Printf("beand %s (runtime=%s) listening on %s", version, rt.Name(), *listen)
	if err := srv.Serve(lis); err != nil {
		log.Fatal(err)
	}
}

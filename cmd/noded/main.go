// noded is the node daemon: it manages sandbox lifecycle on one node and
// exposes SandboxService over gRPC for the control plane / gateway.
package main

import (
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
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
	agentBin := flag.String("agent-bin", "beand", "path to beand binary (local runtime)")
	baseDir := flag.String("base-dir", "/var/lib/bean/sandboxes", "sandbox base directory")
	nodeToken := flag.String("node-token", os.Getenv("BEAN_NODE_TOKEN"),
		"shared token required from callers (empty = no auth, loopback dev only)")
	flag.Parse()

	if *nodeToken == "" && !isLoopback(*listen) {
		log.Fatalf("refusing to listen on %s without --node-token (or BEAN_NODE_TOKEN)", *listen)
	}

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
	unaryAuth, streamAuth := node.TokenAuth(*nodeToken)
	srv := grpc.NewServer(
		grpc.UnaryInterceptor(unaryAuth),
		grpc.StreamInterceptor(streamAuth),
	)
	nodev1.RegisterSandboxServiceServer(srv, node.NewGRPCServer(mgr))

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("shutting down")
		srv.GracefulStop()
	}()

	log.Printf("noded %s (runtime=%s) listening on %s", version, rt.Name(), *listen)
	if err := srv.Serve(lis); err != nil {
		log.Fatal(err)
	}
}

// isLoopback reports whether addr binds only to a loopback interface.
func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

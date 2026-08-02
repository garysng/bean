// beand is the in-sandbox init/PID1 agent. On the fc tier it is
// injected via the agent disk and listens on vsock; in dev/container
// mode it listens on a unix socket.
package main

import (
	"flag"
	"log"
	"log/slog"

	"google.golang.org/grpc"

	"github.com/garysng/bean/internal/beand"
	agentv1 "github.com/garysng/bean/internal/gen/bean/agent/v1"
	"github.com/garysng/bean/internal/logging"
)

var version = "dev"

func main() {
	listenAddr := flag.String("listen", "/run/bean/agent.sock", "unix socket path (or vsock:PORT on fc tier)")
	rootDir := flag.String("root", "", "confine file ops under this dir (dev mode); empty = host root")
	pivot := flag.String("pivot", "",
		"block device holding the user image; mounted as / before serving (fc tier)")
	logFormat := flag.String("log-format", "text", "log format: text|json")
	logLevel := flag.String("log-level", "info", "log level: debug|info|warn|error")
	flag.Parse()

	logging.Setup(*logFormat, *logLevel)

	// As PID 1 in a microVM the agent owns early boot: the user image is not
	// the root filesystem until this runs, so it happens before the listener is
	// bound and before any user process can observe a half-built root.
	if *pivot != "" {
		if err := beand.PivotToRootfs(*pivot); err != nil {
			log.Fatalf("pivot to %s: %v", *pivot, err)
		}
	}

	// PID 1 starts with an empty environment, which leaves no PATH to resolve a
	// bare command name against.
	beand.EnsurePath()

	lis, err := beand.Listen(*listenAddr)
	if err != nil {
		log.Fatalf("listen %s: %v", *listenAddr, err)
	}

	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(beand.UnaryTraceLogging()),
		grpc.ChainStreamInterceptor(beand.StreamTraceLogging()),
	)
	agentv1.RegisterAgentServiceServer(srv, beand.NewServer(version, *rootDir))
	slog.Info("beand listening", "version", version, "addr", *listenAddr, "root", *rootDir)
	if err := srv.Serve(lis); err != nil {
		log.Fatal(err)
	}
}

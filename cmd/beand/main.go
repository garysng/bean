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
	guestDNS := flag.String("guest-dns", "",
		"resolver written into the guest's /etc/resolv.conf; empty leaves the "+
			"image's own file untouched, which is correct on a node with no sandbox "+
			"networking. Must not be loopback: the host's own resolv.conf often holds "+
			"127.0.0.53, which inside a guest names the guest")
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

	// Between the pivot and the listener, and for two independent reasons.
	//
	// After: before the pivot, /etc is the agent disk's, which is read-only and
	// is not the filesystem the user's processes will see -- the write would
	// appear to succeed against a root that is about to be replaced. The pivot
	// also mounts the tmpfs on /run, which is what makes replacing a
	// systemd-resolved symlink into /run resolvable at all.
	//
	// Before: every path that runs user code -- Exec, StreamExec,
	// StartUserProcess -- arrives over this listener, so binding it is the moment
	// the guest becomes writable from outside. A resolver written afterwards
	// would be a race that only loses under load, which is the shape of bug that
	// gets blamed on the network.
	//
	// A failed write is fatal rather than a warning: a sandbox that resolves no
	// names fails every package install, and the platform should say so at boot
	// instead of letting the user's build report it.
	if *guestDNS != "" {
		if err := beand.WriteResolvConf(*rootDir, *guestDNS); err != nil {
			log.Fatalf("guest dns: %v", err)
		}
		slog.Info("guest resolver configured", "nameserver", *guestDNS)
	}

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

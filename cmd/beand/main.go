// beand is the in-sandbox init/PID1 agent. On the fc tier it is
// injected via the agent disk and listens on vsock; in dev/container
// mode it listens on a unix socket.
package main

import (
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/garysng/bean/internal/beand"
	"github.com/garysng/bean/internal/gen/bean/agent/v1/agentv1connect"
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

	// An unrecognised flag must not be fatal, because this process is PID 1 in a
	// microVM and its arguments come from a noded that may be newer than the agent
	// image on disk. Go's default is to print usage and exit(2); as init that is an
	// immediate "Attempted to kill init!" panic, and the sandbox surfaces as an agent
	// that never answered rather than as a version mismatch.
	//
	// Continuing is the safe direction here. The flags this could skip configure the
	// guest's resolver and its listen address -- degradations, not privileges -- and
	// an agent that boots without one is diagnosable, while a guest that panicked is
	// only diagnosable if someone reads its console. A flag that ever grants
	// something must not be added to this set.
	flag.CommandLine.Init(os.Args[0], flag.ContinueOnError)
	if err := flag.CommandLine.Parse(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr,
			"beand: ignoring unusable arguments (%v); this agent image predates a "+
				"flag noded passed it, so the sandbox may lack what that flag configures\n",
			err)
	}

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

	// The agent is served over Connect, which speaks the Connect protocol, gRPC
	// and gRPC-Web from one set of handlers. noded's control path keeps dialling
	// as a gRPC client and reaches it unchanged; the data-plane client and the SDK
	// reach the same methods over HTTP/JSON. It is served over h2c (cleartext
	// HTTP/2) because there is no TLS on any of these transports -- vsock and the
	// unix socket are host-local, and the tcp listener sits behind the node.
	interceptors := []connect.Interceptor{beand.ConnectTraceLogging()}

	// Authentication is required by the transport, not by a flag.
	//
	// A TCP listener is reachable from inside the sandbox: any process there can
	// dial it, and this agent runs as root and will setuid to whatever the image
	// asks for. So a token check is not hardening on that transport, it is the only
	// thing separating noded from the sandbox's own root.
	//
	// Deriving it from the address rather than accepting a --require-auth flag means
	// the unauthenticated combination cannot be produced by a caller at all. A flag
	// would eventually be omitted by a script, and the result would be a sandbox
	// that works -- and hands its own occupant the agent API.
	//
	// vsock and Unix sockets need none: the first is a host-to-guest address family
	// no guest process can dial, and the second is a path outside the guest's mount
	// namespace.
	authRequired := strings.HasPrefix(*listenAddr, "tcp:")
	if authRequired {
		interceptors = append(interceptors, beand.NewAuthenticator().Interceptor())
	}

	agent := beand.NewServer(version, *rootDir)
	path, handler := agentv1connect.NewAgentServiceHandler(
		beand.NewConnectServer(agent),
		connect.WithInterceptors(interceptors...),
	)
	mux := http.NewServeMux()
	mux.Handle(path, handler)

	httpSrv := &http.Server{Handler: h2c.NewHandler(mux, &http2.Server{})}
	slog.Info("beand listening", "version", version, "addr", *listenAddr, "root", *rootDir,
		"authenticated", authRequired)
	if err := httpSrv.Serve(lis); err != nil {
		log.Fatal(err)
	}
}

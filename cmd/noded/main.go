// noded is the node daemon: it manages sandbox lifecycle on one node and
// exposes SandboxService over gRPC for the control plane / gateway.
package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"google.golang.org/grpc"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/logging"
	"github.com/garysng/bean/internal/node"
	"github.com/garysng/bean/internal/node/runtime"
	"github.com/garysng/bean/internal/obs"
)

var version = "dev"

func main() {
	listen := flag.String("listen", "127.0.0.1:7443", "gRPC listen address")
	rtName := flag.String("runtime", "local", "runtime: local|fc")
	agentBin := flag.String("agent-bin", "beand", "path to beand binary (local runtime)")
	baseDir := flag.String("base-dir", "/var/lib/bean/sandboxes", "sandbox base directory")
	nodeToken := flag.String("node-token", os.Getenv("BEAN_NODE_TOKEN"),
		"shared token required from callers (empty = no auth, loopback dev only)")
	controlPlane := flag.String("control-plane", "",
		"NodeService address; set to register with a control plane (multi-node mode)")
	nodeID := flag.String("node-id", "", "node id (default: derived from listen address)")
	region := flag.String("region", "local", "region this node belongs to")
	bootstrapToken := flag.String("bootstrap-token", os.Getenv("BEAN_BOOTSTRAP_TOKEN"),
		"token presented when registering")
	advertise := flag.String("advertise", "", "address the control plane should dial (default: --listen)")
	cpuAlloc := flag.Float64("cpu", 4, "allocatable vCPU advertised to the scheduler")
	memAlloc := flag.Int64("memory-mib", 8192, "allocatable memory (MiB)")
	diskAlloc := flag.Int64("disk-mib", 102400, "allocatable sandbox disk (MiB)")
	labelsFlag := flag.String("labels", "", "comma-separated node labels, e.g. pool=nvme,zone=a")
	metricsAddr := flag.String("metrics", "", "HTTP address for /metrics (empty = disabled)")
	fcBin := flag.String("firecracker-bin", "firecracker", "Firecracker binary (fc runtime)")
	fcKernel := flag.String("kernel", "/var/lib/bean/assets/vmlinux",
		"guest kernel image (fc runtime)")
	fcAgentDisk := flag.String("agent-disk", "",
		"read-only image holding beand, attached to every microVM (fc runtime)")
	imageDir := flag.String("image-dir", "/var/lib/bean/images",
		"prepared base images (fc runtime)")
	defaultDiskMiB := flag.Int64("default-disk-mib", 2048,
		"sandbox rootfs size when the spec does not bound it (fc runtime)")
	buildkitAddr := flag.String("buildkit-addr", "",
		"buildkitd address enabling image builds on this node, e.g. unix:///run/bean/buildkitd.sock")
	buildctlBin := flag.String("buildctl-bin", "buildctl", "BuildKit client binary")
	debugConsole := flag.Bool("debug-console", false,
		"attach guests to the serial console; costs ~500ms per boot (fc runtime)")
	cpuTemplate := flag.String("cpu-template", "none",
		"mask guest CPU features so memory snapshots survive a move between CPU "+
			"generations: none|portable (fc runtime)")
	overcommitCPU := flag.Float64("overcommit-cpu", 1.0,
		"multiply allocatable CPU by this factor; evaluation workloads are bursty, "+
			"so 1.0 leaves capacity idle. Oversubscribing CPU degrades gracefully "+
			"(the kernel time-slices)")
	overcommitMemory := flag.Float64("overcommit-memory", 1.0,
		"multiply allocatable memory by this factor. Unlike CPU this does not "+
			"degrade gracefully — being wrong means a killed process — and there is "+
			"no cgroup around the VMM processes to enforce fairness, so raise it "+
			"only with measurements")
	trackDirtyPages := flag.Bool("track-dirty-pages", false,
		"log guest writes so checkpoints can capture only what changed; must be on "+
			"from boot, so a guest started without it can never produce an "+
			"incremental snapshot (fc runtime)")
	snapCacheHighMiB := flag.Int64("snapshot-cache-high-mib", 0,
		"reclaim unpacked snapshots once the cache reaches this size; 0 leaves it "+
			"unbounded, which grows by roughly one guest's memory per distinct "+
			"snapshot restored and is invisible to the scheduler (fc runtime)")
	snapCacheLowMiB := flag.Int64("snapshot-cache-low-mib", 0,
		"size a reclaim brings the snapshot cache down to; must be below the high "+
			"mark, so eviction runs as an occasional batch rather than on every "+
			"restore past the trigger. 0 derives 80% of the high mark (fc runtime)")
	logFormat := flag.String("log-format", "text", "log format: text|json")
	logLevel := flag.String("log-level", "info", "log level: debug|info|warn|error")
	otlpEndpoint := flag.String("otlp-endpoint", os.Getenv("BEAN_OTLP_ENDPOINT"),
		"OTLP/gRPC collector for traces, e.g. localhost:4317 (empty = tracing off)")
	flag.Parse()

	logging.Setup(*logFormat, *logLevel)

	shutdownTracing, err := obs.SetupTracing(context.Background(), obs.TracingConfig{
		Endpoint: *otlpEndpoint, Service: "noded", Version: version, Insecure: true,
	})
	if err != nil {
		log.Fatalf("tracing: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(ctx); err != nil {
			slog.Warn("flushing traces", logging.KeyError, err)
		}
	}()

	if *nodeToken == "" && !isLoopback(*listen) {
		log.Fatalf("refusing to listen on %s without --node-token (or BEAN_NODE_TOKEN)", *listen)
	}

	// A misspelled template must stop the node rather than fall back to none:
	// the fallback silently produces snapshots bound to this host's CPU, and
	// nothing surfaces that until a restore elsewhere misbehaves.
	overcommit := node.Overcommit{CPU: *overcommitCPU, Memory: *overcommitMemory}
	if err := overcommit.Validate(); err != nil {
		log.Fatalf("--overcommit-*: %v", err)
	}
	if overcommit.Enabled() {
		// Stated at startup because it changes what the node admits, and a node
		// quietly accepting several times the work is hard to explain afterwards.
		slog.Warn("resource overcommit on",
			"cpu", *overcommitCPU, "memory", *overcommitMemory,
			"reportedCPU", overcommit.ApplyCPU(*cpuAlloc),
			"reportedMemoryMiB", overcommit.ApplyMemory(*memAlloc))
	}

	// A low mark defaulted from the high one rather than required alongside it:
	// the ratio is the part an operator has no basis to choose, while the size the
	// cache may reach is the part they do.
	snapCache := runtime.EvictionPolicy{HighBytes: *snapCacheHighMiB << 20, LowBytes: *snapCacheLowMiB << 20}
	if snapCache.HighBytes > 0 && snapCache.LowBytes == 0 {
		snapCache.LowBytes = snapCache.HighBytes / 5 * 4
	}
	if err := snapCache.Validate(); err != nil {
		log.Fatalf("--snapshot-cache-*: %v", err)
	}

	tmpl, err := runtime.ParseCPUTemplate(*cpuTemplate)
	if err != nil {
		log.Fatalf("--cpu-template: %v", err)
	}

	// The CPU identity is reported so the control plane can refuse to restore a
	// memory snapshot onto a CPU its guest cannot run on. A node that cannot
	// read it still starts: the effect is that it will not be chosen for
	// restores, which is the safe direction to fail in.
	cpuVendor, cpuFamily, err := runtime.HostCPUIdentity()
	if err != nil {
		slog.Warn("cannot read host CPU identity; this node will not be "+
			"eligible for snapshot restores", logging.KeyError, err)
	}

	var rt runtime.Runtime
	switch *rtName {
	case "local":
		rt = runtime.NewLocalRuntime(*agentBin, *baseDir)
	case "fc":
		fcRT, err := runtime.NewFCTier(runtime.FCTierConfig{
			FirecrackerBin:  *fcBin,
			KernelPath:      *fcKernel,
			AgentDiskPath:   *fcAgentDisk,
			BaseDir:         *baseDir,
			ImageDir:        *imageDir,
			DefaultDiskMiB:  *defaultDiskMiB,
			BuildkitAddr:    *buildkitAddr,
			BuildctlBin:     *buildctlBin,
			DebugConsole:    *debugConsole,
			CPUTemplate:     tmpl,
			TrackDirtyPages: *trackDirtyPages,
			SnapshotCache:   snapCache,
		})
		if err != nil {
			log.Fatalf("fc runtime: %v", err)
		}
		rt = fcRT
	default:
		log.Fatalf("runtime %q not supported (want local or fc)", *rtName)
	}

	mgr := node.NewManager(rt)
	defer mgr.Close()

	lis, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	unaryAuth, streamAuth := node.TokenAuth(*nodeToken)
	// Trace extraction runs before auth so a rejected call still appears in the
	// trace: "the node refused the token" is a diagnosis, and it is only
	// visible if the span exists.
	srv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(obs.UnaryServerTrace("noded"), unaryAuth),
		grpc.ChainStreamInterceptor(obs.StreamServerTrace("noded"), streamAuth),
	)
	nodev1.RegisterSandboxServiceServer(srv, node.NewGRPCServer(mgr))

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		slog.Info("shutting down")
		srv.GracefulStop()
	}()

	// Metrics endpoint: scraped locally, so no auth and no sandbox contents.
	if *metricsAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
			mgr.RefreshGauges()
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			if err := mgr.Metrics().WritePrometheus(w); err != nil {
				slog.Error("cannot write metrics", logging.KeyError, err)
			}
		})
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		metricsSrv := &http.Server{
			Addr:              *metricsAddr,
			Handler:           mux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				slog.Error("metrics server stopped", logging.KeyError, err)
			}
		}()
		slog.Info("metrics listening", "addr", *metricsAddr)
	}

	// Multi-node mode: dial out to the control plane, register, then keep
	// the heartbeat alive. Nodes need no inbound path for this.
	if *controlPlane != "" {
		id := *nodeID
		if id == "" {
			id = "node-" + strings.ReplaceAll(*listen, ":", "-")
		}
		adv := *advertise
		if adv == "" {
			adv = *listen
		}
		reg := node.NewRegistrar(mgr, *controlPlane, id, *region, *bootstrapToken,
			parseLabels(*labelsFlag), []string{rt.Name()},
			// Scaled here rather than in the scheduler: the right factor depends on
			// what this node is for, and the scheduler treats what it receives as
			// the final allocatable figure.
			&nodev1.NodeResources{
				CpuAllocatable:       overcommit.ApplyCPU(*cpuAlloc),
				MemoryAllocatableMib: overcommit.ApplyMemory(*memAlloc),
				DiskSandboxesMib:     *diskAlloc,
				CpuVendor:            cpuVendor,
				CpuFamily:            cpuFamily,
				CpuTemplate:          string(tmpl),
			})
		reg.Advertise = adv
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			if err := reg.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Error("registrar stopped", logging.KeyError, err)
			}
		}()
		slog.Info("registering with control plane",
			"controlPlane", *controlPlane, logging.KeyNode, id, "advertise", adv)
	}

	slog.Info("noded listening", "version", version, "runtime", rt.Name(), "addr", *listen)
	if err := srv.Serve(lis); err != nil {
		log.Fatal(err)
	}
}

// parseLabels turns "k=v,k2=v2" into a map.
func parseLabels(s string) map[string]string {
	if s == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		if k, v, ok := strings.Cut(pair, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
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

// noded is the node daemon: it manages sandbox lifecycle on one node and
// exposes SandboxService over gRPC for the control plane / gateway.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

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

	// Metrics endpoint: scraped locally, so no auth and no sandbox contents.
	if *metricsAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
			mgr.RefreshGauges()
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			if err := mgr.Metrics().WritePrometheus(w); err != nil {
				log.Printf("write metrics: %v", err)
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
				log.Printf("metrics server: %v", err)
			}
		}()
		log.Printf("metrics on http://%s/metrics", *metricsAddr)
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
			&nodev1.NodeResources{
				CpuAllocatable:       *cpuAlloc,
				MemoryAllocatableMib: *memAlloc,
				DiskSandboxesMib:     *diskAlloc,
			})
		reg.Advertise = adv
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			if err := reg.Run(ctx); err != nil && ctx.Err() == nil {
				log.Printf("registrar stopped: %v", err)
			}
		}()
		log.Printf("registering with control plane %s as %s (advertise=%s)", *controlPlane, id, adv)
	}

	log.Printf("noded %s (runtime=%s) listening on %s", version, rt.Name(), *listen)
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

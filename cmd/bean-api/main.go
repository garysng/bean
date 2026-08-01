// bean-api is the REST gateway. In single-node mode it dials one noded
// directly; in multi-node mode it serves NodeService so nodes register
// themselves, and places sandboxes with the scheduler.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/garysng/bean/internal/control/api"
	"github.com/garysng/bean/internal/control/nodesvc"
	"github.com/garysng/bean/internal/control/scheduler"
	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node"
)

var version = "dev"

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	nodedAddr := flag.String("noded", "127.0.0.1:7443", "noded gRPC address (single-node mode)")
	nodeGRPC := flag.String("node-grpc", "", "listen address for NodeService; set to enable multi-node mode")
	region := flag.String("region", "local", "region this control plane serves")
	dbPath := flag.String("db", "bean.db", "SQLite database path")
	apiKey := flag.String("api-key", os.Getenv("BEAN_API_KEY"), "API key (or BEAN_API_KEY env)")
	nodeToken := flag.String("node-token", os.Getenv("BEAN_NODE_TOKEN"), "token for noded calls")
	bootstrapToken := flag.String("bootstrap-token", os.Getenv("BEAN_BOOTSTRAP_TOKEN"),
		"token nodes must present to register (multi-node mode)")
	runtimeTier := flag.String("runtime-tier", "fc",
		"node capability required for placement (fc|local|runc|runsc)")
	flag.Parse()

	if *apiKey == "" {
		log.Fatal("api key required: set --api-key or BEAN_API_KEY")
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var srv *api.Server
	if *nodeGRPC != "" {
		srv = setupMultiNode(ctx, st, *nodeGRPC, *region, *apiKey, *nodeToken, *bootstrapToken, *runtimeTier)
	} else {
		srv = setupSingleNode(st, *nodedAddr, *apiKey, *nodeToken)
		log.Printf("single-node mode (noded=%s)", *nodedAddr)
	}

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Body reads are bounded per-handler via LimitReader; keep a generous
		// ReadTimeout to stop slow-body attacks without breaking uploads.
		ReadTimeout: 5 * time.Minute,
		IdleTimeout: 120 * time.Second,
		// No WriteTimeout: exec/logs/file reads legitimately stream for long.
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Printf("bean-api %s listening on %s", version, *listen)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// setupSingleNode keeps the P0 path: one gateway, one noded.
func setupSingleNode(st *store.Store, nodedAddr, apiKey, nodeToken string) *api.Server {
	unaryTok, streamTok := node.TokenClientInterceptors(nodeToken)
	conn, err := grpc.NewClient(nodedAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(unaryTok),
		grpc.WithStreamInterceptor(streamTok))
	if err != nil {
		log.Fatalf("dial noded: %v", err)
	}
	return api.NewServer(st, nodev1.NewSandboxServiceClient(conn), "node-0", apiKey)
}

// setupMultiNode serves NodeService and places sandboxes via the scheduler.
func setupMultiNode(ctx context.Context, st *store.Store,
	nodeGRPCAddr, region, apiKey, nodeToken, bootstrapToken, runtimeTier string) *api.Server {
	sched := scheduler.New(scheduler.DefaultWeights())

	svc := nodesvc.New(sched, nodesvc.Options{
		BootstrapToken: bootstrapToken,
		Lister:         &storeLister{store: st},
		OnLost:         func(nodeID string) { markNodeSandboxesLost(st, nodeID) },
	})

	lis, err := net.Listen("tcp", nodeGRPCAddr)
	if err != nil {
		log.Fatalf("listen NodeService: %v", err)
	}
	grpcSrv := grpc.NewServer()
	nodev1.RegisterNodeServiceServer(grpcSrv, svc)
	go func() {
		if err := grpcSrv.Serve(lis); err != nil {
			log.Printf("NodeService server stopped: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		grpcSrv.GracefulStop()
	}()
	go svc.RunLivenessSweep(ctx, 5*time.Second)

	router := api.NewNodeRouter(svc, nodeToken)
	go func() {
		<-ctx.Done()
		router.Close()
	}()

	log.Printf("multi-node mode: NodeService on %s (region=%s runtime-tier=%s)",
		nodeGRPCAddr, region, runtimeTier)
	return api.NewServerWithOptions(st, router, sched, api.Options{
		Region: region, APIKey: apiKey, RuntimeTier: runtimeTier,
	})
}

// markNodeSandboxesLost flags a lost node's sandboxes so callers can
// rebuild them elsewhere.
func markNodeSandboxesLost(st *store.Store, nodeID string) {
	recs, err := st.ListSandboxes("", "", "")
	if err != nil {
		log.Printf("mark lost for %s: %v", nodeID, err)
		return
	}
	for _, rec := range recs {
		if rec.NodeID != nodeID || isTerminal(rec.State) {
			continue
		}
		rec.State = "LOST"
		if err := st.PutSandbox(rec); err != nil {
			log.Printf("mark %s lost: %v", rec.ID, err)
			continue
		}
		_ = st.AppendEvent(&store.Event{
			Type: "sandbox.lifecycle.lost", Timestamp: time.Now(),
			SandboxID: rec.ID, Version: "v1",
			Data: map[string]string{"nodeId": nodeID},
		})
	}
}

func isTerminal(state string) bool {
	return state == "STOPPED" || state == "FAILED" || state == "LOST"
}

// storeLister answers SyncState from the records the control plane believes
// belong to a node.
type storeLister struct{ store *store.Store }

func (l *storeLister) ExpectedForNode(nodeID string) []*nodev1.SandboxSpec {
	recs, err := l.store.ListSandboxes("", "", "")
	if err != nil {
		log.Printf("ExpectedForNode %s: %v", nodeID, err)
		return nil
	}
	var out []*nodev1.SandboxSpec
	for _, rec := range recs {
		if rec.NodeID != nodeID || isTerminal(rec.State) {
			continue
		}
		out = append(out, &nodev1.SandboxSpec{
			SandboxId: rec.ID,
			Image:     rec.Image,
			Cpu:       rec.CPU,
			MemoryMib: rec.MemoryMiB,
			DiskMib:   rec.DiskMiB,
		})
	}
	return out
}

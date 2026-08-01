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
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/garysng/bean/internal/control/api"
	"github.com/garysng/bean/internal/control/image"
	"github.com/garysng/bean/internal/control/nodesvc"
	"github.com/garysng/bean/internal/control/scheduler"
	"github.com/garysng/bean/internal/control/secret"
	"github.com/garysng/bean/internal/control/snapshot"
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
	secretKey := flag.String("secret-key", os.Getenv("BEAN_SECRET_KEY"),
		"master key encrypting persisted credentials (empty disables registry credentials)")
	snapshotDir := flag.String("snapshot-dir", "",
		"directory holding snapshot blobs (default: <db dir>/snapshots; S3 later)")
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

	// Encryption for persisted credentials. Without a key the registry
	// endpoints refuse rather than storing secrets in the clear.
	var secrets *secret.Box
	if *secretKey != "" {
		var err error
		if secrets, err = secret.NewBox(*secretKey); err != nil {
			log.Fatalf("secret key: %v", err)
		}
	} else {
		log.Print("no --secret-key: registry credentials disabled (public images only)")
	}

	// Snapshot blobs live next to the database by default. The Blobs
	// interface is what lets this become S3 without touching the handlers.
	blobDir := *snapshotDir
	if blobDir == "" {
		blobDir = filepath.Join(filepath.Dir(*dbPath), "snapshots")
	}
	blobs, err := snapshot.NewDirBlobs(blobDir)
	if err != nil {
		log.Fatalf("snapshot storage: %v", err)
	}
	log.Printf("snapshot blobs in %s", blobDir)

	var srv *api.Server
	if *nodeGRPC != "" {
		srv = setupMultiNode(ctx, st, multiNodeConfig{
			nodeGRPCAddr: *nodeGRPC, region: *region, apiKey: *apiKey,
			nodeToken: *nodeToken, bootstrapToken: *bootstrapToken,
			runtimeTier: *runtimeTier, secrets: secrets, blobs: blobs,
		})
	} else {
		srv = setupSingleNode(st, *nodedAddr, *apiKey, *nodeToken, secrets, blobs)
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
func setupSingleNode(st *store.Store, nodedAddr, apiKey, nodeToken string,
	secrets *secret.Box, blobs snapshot.Blobs) *api.Server {
	unaryTok, streamTok := node.TokenClientInterceptors(nodeToken)
	conn, err := grpc.NewClient(nodedAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(unaryTok),
		grpc.WithStreamInterceptor(streamTok))
	if err != nil {
		log.Fatalf("dial noded: %v", err)
	}
	return api.NewServerWithOptions(st, api.NewStaticRouter(nodev1.NewSandboxServiceClient(conn)),
		nil, api.Options{
			DefaultNodeID: "node-0", Region: "local", APIKey: apiKey,
			RuntimeTier: "local", Images: image.New(st, nil), Secrets: secrets,
			Snapshots: blobs,
		})
}

// multiNodeConfig groups the multi-node wiring parameters.
type multiNodeConfig struct {
	nodeGRPCAddr   string
	region         string
	apiKey         string
	nodeToken      string
	bootstrapToken string
	runtimeTier    string
	secrets        *secret.Box
	blobs          snapshot.Blobs
}

// setupMultiNode serves NodeService and places sandboxes via the scheduler.
func setupMultiNode(ctx context.Context, st *store.Store, cfg multiNodeConfig) *api.Server {
	sched := scheduler.New(scheduler.DefaultWeights())

	svc := nodesvc.New(sched, nodesvc.Options{
		BootstrapToken: cfg.bootstrapToken,
		Lister:         &storeLister{store: st},
		OnLost:         func(nodeID string) { markNodeSandboxesLost(st, nodeID) },
	})

	lis, err := net.Listen("tcp", cfg.nodeGRPCAddr)
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

	router := api.NewNodeRouter(svc, cfg.nodeToken)
	go func() {
		<-ctx.Done()
		router.Close()
	}()

	log.Printf("multi-node mode: NodeService on %s (region=%s runtime-tier=%s)",
		cfg.nodeGRPCAddr, cfg.region, cfg.runtimeTier)
	return api.NewServerWithOptions(st, router, sched, api.Options{
		Region: cfg.region, APIKey: cfg.apiKey, RuntimeTier: cfg.runtimeTier,
		Images: image.New(st, nil), Secrets: cfg.secrets, Snapshots: cfg.blobs,
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
		if rec.NodeID != nodeID || store.IsTerminal(rec.State) {
			continue
		}
		rec.State = store.SandboxLost
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
		if rec.NodeID != nodeID || store.IsTerminal(rec.State) {
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

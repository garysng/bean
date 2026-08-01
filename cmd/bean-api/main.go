// bean-api is the REST gateway. It serves the user-facing API and
// NodeService, so nodes register themselves and every sandbox is placed by
// the scheduler — there is no separate single-node mode, because a cluster
// with one node is just a cluster with one node.
//
// The process holds no placement state: node capacity and reservations live
// in the store, so replicas are interchangeable and a restart loses nothing.
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

	"github.com/garysng/bean/internal/control/api"
	"github.com/garysng/bean/internal/control/image"
	"github.com/garysng/bean/internal/control/nodesvc"
	"github.com/garysng/bean/internal/control/s3"
	"github.com/garysng/bean/internal/control/scheduler"
	"github.com/garysng/bean/internal/control/secret"
	"github.com/garysng/bean/internal/control/snapshot"
	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
)

var version = "dev"

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	nodeGRPC := flag.String("node-grpc", "127.0.0.1:7440", "NodeService listen address")
	region := flag.String("region", "local", "region this control plane serves")
	dbPath := flag.String("db", "bean.db", "SQLite database path")
	apiKey := flag.String("api-key", os.Getenv("BEAN_API_KEY"), "API key (or BEAN_API_KEY env)")
	nodeToken := flag.String("node-token", os.Getenv("BEAN_NODE_TOKEN"),
		"token presented when calling a node's data plane")
	bootstrapToken := flag.String("bootstrap-token", os.Getenv("BEAN_BOOTSTRAP_TOKEN"),
		"token nodes must present to register")
	runtimeTier := flag.String("runtime-tier", "fc",
		"node capability required for placement (fc|local|runc|runsc)")
	secretKey := flag.String("secret-key", os.Getenv("BEAN_SECRET_KEY"),
		"master key encrypting persisted credentials (empty disables registry credentials)")
	snapshotDir := flag.String("snapshot-dir", "",
		"directory holding snapshot blobs when no object store is configured (default: <db dir>/snapshots)")
	s3Endpoint := flag.String("s3-endpoint", os.Getenv("BEAN_S3_ENDPOINT"),
		"S3-compatible endpoint for snapshot blobs (or BEAN_S3_ENDPOINT); empty uses --snapshot-dir")
	s3Bucket := flag.String("s3-snapshot-bucket", "bean-snapshots",
		"bucket holding snapshot blobs")
	s3Region := flag.String("s3-region", "us-east-1", "S3 region")
	s3PathStyle := flag.Bool("s3-path-style", true,
		"address buckets as /bucket/key (required by MinIO and most self-hosted gateways)")
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
		if secrets, err = secret.NewBox(*secretKey); err != nil {
			log.Fatalf("secret key: %v", err)
		}
	} else {
		log.Print("no --secret-key: registry credentials disabled (public images only)")
	}

	// Snapshot blobs go to object storage in production; multiple gateway
	// replicas can only serve the same snapshot if it does not live on one
	// replica's disk. A local directory remains the default for development
	// and CI, where standing up MinIO would be friction for no benefit.
	var blobs snapshot.Blobs
	if *s3Endpoint != "" {
		// Credentials come from the environment only. A flag would put the
		// secret key in the process command line, visible to every local user.
		s3c, err := s3.New(s3.Config{
			Endpoint:  *s3Endpoint,
			Region:    *s3Region,
			AccessKey: os.Getenv("BEAN_S3_ACCESS_KEY"),
			SecretKey: os.Getenv("BEAN_S3_SECRET_KEY"),
			PathStyle: *s3PathStyle,
		})
		if err != nil {
			log.Fatalf("snapshot storage: %v", err)
		}
		if blobs, err = snapshot.NewS3Blobs(ctx, s3c, *s3Bucket); err != nil {
			log.Fatalf("snapshot storage: %v", err)
		}
		log.Printf("snapshot blobs: s3 %s bucket %s", *s3Endpoint, *s3Bucket)
	} else {
		blobDir := *snapshotDir
		if blobDir == "" {
			blobDir = filepath.Join(filepath.Dir(*dbPath), "snapshots")
		}
		dir, err := snapshot.NewDirBlobs(blobDir)
		if err != nil {
			log.Fatalf("snapshot storage: %v", err)
		}
		blobs = dir
		log.Printf("snapshot blobs: directory %s", blobDir)
	}

	sched := scheduler.New(st, scheduler.DefaultWeights())
	images := image.New(st, nodeCacheSource{store: st})

	nodeSvc := nodesvc.New(st, sched, nodesvc.Options{
		BootstrapToken: *bootstrapToken,
		Lister:         &storeLister{store: st},
		OnLost:         func(nodeID string) { markNodeSandboxesLost(st, sched, nodeID) },
	})

	grpcSrv := grpc.NewServer()
	nodev1.RegisterNodeServiceServer(grpcSrv, nodeSvc)
	nodeLis, err := net.Listen("tcp", *nodeGRPC)
	if err != nil {
		log.Fatalf("listen NodeService: %v", err)
	}
	go func() {
		if err := grpcSrv.Serve(nodeLis); err != nil {
			log.Printf("NodeService stopped: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		grpcSrv.GracefulStop()
	}()
	go nodeSvc.RunLivenessSweep(ctx, 5*time.Second)

	router := api.NewNodeRouter(nodeSvc, *nodeToken)
	defer router.Close()

	srv := api.New(st, router, sched, api.Options{
		Region: *region, APIKey: *apiKey, RuntimeTier: *runtimeTier,
		Images: images, Secrets: secrets, Snapshots: blobs,
	})

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Body reads are bounded per-handler via LimitReader; a generous
		// ReadTimeout stops slow-body attacks without breaking uploads.
		ReadTimeout: 5 * time.Minute,
		IdleTimeout: 120 * time.Second,
		// No WriteTimeout: exec, logs and file reads legitimately stream.
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
	}()

	log.Printf("bean-api %s: HTTP %s, NodeService %s (region=%s runtime-tier=%s)",
		version, *listen, *nodeGRPC, *region, *runtimeTier)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// markNodeSandboxesLost flags a lost node's sandboxes and returns their
// capacity, so a node failure strands neither the records nor the
// reservations.
func markNodeSandboxesLost(st *store.Store, sched *scheduler.Scheduler, nodeID string) {
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
		if err := sched.Release(rec.ID); err != nil {
			log.Printf("release %s after node loss: %v", rec.ID, err)
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

// nodeCacheSource reports how many nodes cache an image, which drives
// prewarm progress and image-affinity scoring.
type nodeCacheSource struct{ store *store.Store }

func (n nodeCacheSource) CachedNodeCount(ref string) int {
	nodes, err := n.store.LoadNodes()
	if err != nil {
		return 0
	}
	count := 0
	for _, node := range nodes {
		if bytes, ok := node.CachedImages[ref]; ok && bytes > 0 {
			count++
		}
	}
	return count
}

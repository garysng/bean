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
	"fmt"
	"log"
	"log/slog"
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
	"github.com/garysng/bean/internal/logging"
	"github.com/garysng/bean/internal/obs"
)

var version = "dev"

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	nodeGRPC := flag.String("node-grpc", "127.0.0.1:7440", "NodeService listen address")
	region := flag.String("region", "local", "region this control plane serves")
	dbPath := flag.String("db", "bean.db", "SQLite database path")
	postgresDSN := flag.String("postgres", os.Getenv("BEAN_POSTGRES_DSN"),
		"Postgres connection string (or BEAN_POSTGRES_DSN); overrides --db when set. "+
			"This is what allows more than one bean-api replica: SQLite is a single file "+
			"and two replicas cannot share it, so on SQLite the count is exactly one")
	apiKey := flag.String("api-key", os.Getenv("BEAN_API_KEY"), "API key (or BEAN_API_KEY env)")
	nodeToken := flag.String("node-token", os.Getenv("BEAN_NODE_TOKEN"),
		"token presented when calling a node's data plane")
	bootstrapToken := flag.String("bootstrap-token", os.Getenv("BEAN_BOOTSTRAP_TOKEN"),
		"token nodes must present to register")
	sandboxDomain := flag.String("sandbox-domain", os.Getenv("BEAN_SANDBOX_DOMAIN"),
		"bean-proxy public base stamped on each sandbox as its data-plane domain "+
			"(clients address a port as {port}-{id}.{domain}); empty uses the relay path")
	runtimeTier := flag.String("runtime-tier", "fc",
		"node capability required for placement (fc|local|runc|runsc)")
	createWait := flag.Duration("create-wait", 0,
		"how long a create waits for a node's create concurrency to drain before "+
			"being refused; 0 refuses immediately. An evaluation batch arrives as a "+
			"burst by construction and a rejected caller retries as another burst, so "+
			"waiting turns a retry storm into a predictable queue. It does not raise "+
			"throughput. What bounds throughput is the rootfs setup, not host CPU: measured "+
			"at 0.31-0.44 CPU-seconds per create on a 128-core host, where observed "+
			"throughput was 0.16-0.28 of what that cost predicts. An earlier version of "+
			"this text said cores/5, from a 16-core host when every create booted. "+
			"Only create concurrency is waited on: CPU, memory and disk are "+
			"held for a sandbox's lifetime and will not free themselves")
	secretKey := flag.String("secret-key", os.Getenv("BEAN_SECRET_KEY"),
		"master key encrypting persisted credentials (empty disables registry credentials)")
	allowedImageSources := flag.String("allowed-image-sources", "",
		"comma-separated image provenance allowlist: built,imported. Empty allows "+
			"any, which is the default because refusing what previously ran would "+
			"break a deployment for no gain. Set to \"built\" to accept only images "+
			"this platform produced")
	allowedRegistries := flag.String("allowed-registries", "",
		"comma-separated registry host allowlist for imported images, e.g. "+
			"index.docker.io,registry.example.com. Empty allows any. Images this "+
			"platform built are not checked against it: their host is a push "+
			"destination the operator chose, not one a caller asked to pull from")
	ownerHeader := flag.String("owner-header", "",
		"request header naming the caller an image is attributed to, e.g. "+
			"X-Bean-Owner. Empty leaves every image unowned and every listing "+
			"unfiltered. Only set this behind a trusted layer that authenticates "+
			"callers and sets the header itself: bean does not verify it, so a "+
			"client able to reach this API directly could name anyone")
	snapshotDir := flag.String("snapshot-dir", "",
		"directory holding snapshot blobs when no object store is configured (default: <db dir>/snapshots)")
	s3Endpoint := flag.String("s3-endpoint", os.Getenv("BEAN_S3_ENDPOINT"),
		"S3-compatible endpoint for snapshot blobs (or BEAN_S3_ENDPOINT); empty uses --snapshot-dir")
	s3Bucket := flag.String("s3-snapshot-bucket", "bean-snapshots",
		"bucket holding snapshot blobs")
	s3LogsBucket := flag.String("s3-logs-bucket", envOr("BEAN_S3_LOGS_BUCKET", "bean-build-logs"),
		"bucket build logs are read from (or BEAN_S3_LOGS_BUCKET), the same dedicated "+
			"logs bucket the nodes upload to. Separate from --s3-snapshot-bucket "+
			"because logs expire under a lifecycle rule while snapshots do not. With "+
			"no --s3-endpoint the gateway reads logs from a local directory (dev "+
			"single-host), matching the node's local fallback")
	s3Region := flag.String("s3-region", "us-east-1", "S3 region")
	s3PathStyle := flag.Bool("s3-path-style", true,
		"address buckets as /bucket/key (required by MinIO and most self-hosted gateways)")
	logFormat := flag.String("log-format", "text", "log format: text|json")
	logLevel := flag.String("log-level", "info", "log level: debug|info|warn|error")
	otlpEndpoint := flag.String("otlp-endpoint", os.Getenv("BEAN_OTLP_ENDPOINT"),
		"OTLP/gRPC collector for traces, e.g. localhost:4317 (empty = tracing off)")
	flag.Parse()

	logging.Setup(*logFormat, *logLevel)

	shutdownTracing, err := obs.SetupTracing(context.Background(), obs.TracingConfig{
		Endpoint: *otlpEndpoint, Service: "bean-api", Version: version, Insecure: true,
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

	if *apiKey == "" {
		log.Fatal("api key required: set --api-key or BEAN_API_KEY")
	}

	// Postgres when a DSN is given, SQLite otherwise. The engine is chosen by which
	// flag is set rather than by a --db-driver value, so an inconsistent pair -- a
	// driver naming one engine and a path pointing at the other -- cannot be expressed.
	var st *store.Store
	if *postgresDSN != "" {
		st, err = store.OpenPostgres(*postgresDSN)
	} else {
		st, err = store.Open(*dbPath)
	}
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()
	// Logged because it decides whether a second replica is safe to start, and that is
	// not visible from anything else in the startup output.
	if *postgresDSN != "" {
		slog.Info("state store: postgres (multiple replicas can share it)")
	} else {
		slog.Info("state store: sqlite", "path", *dbPath,
			"note", "one file, so exactly one bean-api replica")
	}

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
		slog.Warn("registry credentials disabled: no --secret-key, so only public images work")
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
		slog.Info("snapshot blobs in object storage",
			"endpoint", *s3Endpoint, "bucket", *s3Bucket)
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
		slog.Info("snapshot blobs on local disk", "dir", blobDir)
	}

	// The build-log store is read here the same way the node writes it: a
	// dedicated logs bucket over the shared object-store contract, or a local
	// directory in the dev single-host case. It is separate from the snapshot
	// blobs so the two can have different lifecycles.
	buildLogs, err := buildLogsStore(*s3Endpoint, *s3LogsBucket, *s3Region, *s3PathStyle,
		filepath.Join(filepath.Dir(*dbPath), "build-logs"))
	if err != nil {
		log.Fatalf("build log storage: %v", err)
	}

	sched := scheduler.New(st, scheduler.DefaultWeights())

	imagePolicy, err := image.ParsePolicy(*allowedImageSources, *allowedRegistries)
	if err != nil {
		log.Fatalf("image policy: %v", err)
	}
	if imagePolicy.Enabled() {
		slog.Info("image policy active",
			"allowedSources", *allowedImageSources, "allowedRegistries", *allowedRegistries)
	}
	images := image.NewWithPolicy(st, nodeCacheSource{store: st}, imagePolicy)

	var identity api.IdentityFunc
	if *ownerHeader != "" {
		identity = api.OwnerFromHeader(*ownerHeader)
		slog.Info("image ownership attributed from header", "header", *ownerHeader)
	}

	nodeSvc := nodesvc.New(st, sched, nodesvc.Options{
		BootstrapToken: *bootstrapToken,
		Lister:         &storeLister{store: st},
		OnLost:         func(nodeID string) { markNodeSandboxesLost(st, sched, nodeID) },
	})

	grpcSrv := grpc.NewServer(
		grpc.ChainUnaryInterceptor(obs.UnaryServerTrace("bean-api")),
		grpc.ChainStreamInterceptor(obs.StreamServerTrace("bean-api")),
	)
	nodev1.RegisterNodeServiceServer(grpcSrv, nodeSvc)
	nodeLis, err := net.Listen("tcp", *nodeGRPC)
	if err != nil {
		log.Fatalf("listen NodeService: %v", err)
	}
	go func() {
		if err := grpcSrv.Serve(nodeLis); err != nil {
			slog.Error("NodeService stopped", logging.KeyError, err)
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
		Domain: *sandboxDomain,
		Images: images, Secrets: secrets, Snapshots: blobs,
		BuildLogs:  buildLogs,
		CreateWait: *createWait, Identity: identity,
	})

	// Re-attach to builds that were in flight when this replica last stopped. A
	// build runs under its node's own context and survives a control-plane
	// restart, but the poller that records its outcome does not, so on startup we
	// resume polling each BUILDING template (docs/build-logs-s3.md §8). Runs in a
	// goroutine so a slow node does not delay serving.
	go srv.ReconcileBuilds(ctx)

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

	slog.Info("bean-api listening", "version", version, "http", *listen,
		"nodeGrpc", *nodeGRPC, "region", *region, "runtimeTier", *runtimeTier)
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// markNodeSandboxesLost flags a lost node's sandboxes and returns their
// capacity, so a node failure strands neither the records nor the
// reservations.
func markNodeSandboxesLost(st store.Sandboxes, sched *scheduler.Scheduler, nodeID string) {
	recs, err := st.ListSandboxes("", "", "")
	if err != nil {
		slog.Error("cannot list sandboxes to mark lost",
			logging.KeyNode, nodeID, logging.KeyError, err)
		return
	}
	for _, rec := range recs {
		if rec.NodeID != nodeID || store.IsTerminal(rec.State) {
			continue
		}
		rec.State = store.SandboxLost
		if err := st.PutSandbox(rec); err != nil {
			slog.Error("cannot mark sandbox lost",
				logging.KeySandbox, rec.ID, logging.KeyNode, nodeID,
				logging.KeyError, err)
			continue
		}
		if err := sched.Release(rec.ID); err != nil {
			slog.Error("cannot release capacity after node loss",
				logging.KeySandbox, rec.ID, logging.KeyNode, nodeID,
				logging.KeyError, err)
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
type storeLister struct{ store store.Sandboxes }

func (l *storeLister) ExpectedForNode(nodeID string) []*nodev1.SandboxSpec {
	recs, err := l.store.ListSandboxes("", "", "")
	if err != nil {
		slog.Error("cannot list a node's expected sandboxes",
			logging.KeyNode, nodeID, logging.KeyError, err)
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
type nodeCacheSource struct{ store store.Nodes }

func (n nodeCacheSource) CachedNodeCount(ref string) int {
	nodes, err := n.store.LoadNodes()
	if err != nil {
		return 0
	}
	count := 0
	for _, node := range nodes {
		if img, ok := node.CachedImages[ref]; ok && img.SizeBytes > 0 {
			count++
		}
	}
	return count
}

// envOr returns the environment variable's value, or fallback when unset.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// buildLogsStore opens the object store build logs are read from. With an
// endpoint it is a BucketStore over the dedicated logs bucket; without one it is
// a local DirStore, the dev single-host case where the node wrote to the same
// directory. Credentials come from the environment only, never a flag.
func buildLogsStore(endpoint, bucket, region string, pathStyle bool, localDir string) (s3.ObjectStore, error) {
	if endpoint == "" {
		return s3.NewDirStore(localDir)
	}
	if bucket == "" {
		return nil, fmt.Errorf("build logs: bucket required with an endpoint")
	}
	client, err := s3.New(s3.Config{
		Endpoint:  endpoint,
		Region:    region,
		AccessKey: os.Getenv("BEAN_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("BEAN_S3_SECRET_KEY"),
		PathStyle: pathStyle,
	})
	if err != nil {
		return nil, err
	}
	return s3.NewBucketStore(context.Background(), client, bucket)
}

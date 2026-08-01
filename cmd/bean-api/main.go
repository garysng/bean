// bean-api is the REST gateway. P0: single-node deployment connecting
// directly to one noded.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/garysng/bean/internal/control/api"
	"github.com/garysng/bean/internal/control/store"
	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node"
)

var version = "dev"

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	nodedAddr := flag.String("noded", "127.0.0.1:7443", "noded gRPC address")
	dbPath := flag.String("db", "bean.db", "SQLite database path")
	apiKey := flag.String("api-key", os.Getenv("BEAN_API_KEY"), "API key (or BEAN_API_KEY env)")
	nodeToken := flag.String("node-token", os.Getenv("BEAN_NODE_TOKEN"), "token for noded calls")
	flag.Parse()

	if *apiKey == "" {
		log.Fatal("api key required: set --api-key or BEAN_API_KEY")
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	unaryTok, streamTok := node.TokenClientInterceptors(*nodeToken)
	conn, err := grpc.NewClient(*nodedAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(unaryTok),
		grpc.WithStreamInterceptor(streamTok))
	if err != nil {
		log.Fatalf("dial noded: %v", err)
	}
	defer conn.Close()

	srv := api.NewServer(st, nodev1.NewSandboxServiceClient(conn), "node-0", *apiKey)
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
	log.Printf("bean-api %s listening on %s (noded=%s)", version, *listen, *nodedAddr)
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

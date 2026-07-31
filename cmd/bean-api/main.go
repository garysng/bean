// bean-api is the REST gateway. P0: single-node deployment connecting
// directly to one beand.
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
)

var version = "dev"

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	beandAddr := flag.String("beand", "127.0.0.1:7443", "beand gRPC address")
	dbPath := flag.String("db", "bean.db", "SQLite database path")
	apiKey := flag.String("api-key", os.Getenv("BEAN_API_KEY"), "API key (or BEAN_API_KEY env)")
	flag.Parse()

	if *apiKey == "" {
		log.Fatal("api key required: set --api-key or BEAN_API_KEY")
	}

	st, err := store.Open(*dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	conn, err := grpc.NewClient(*beandAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial beand: %v", err)
	}
	defer conn.Close()

	srv := api.NewServer(st, nodev1.NewSandboxServiceClient(conn), "node-0", *apiKey)
	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("bean-api %s listening on %s (beand=%s)", version, *listen, *beandAddr)
	if err := httpSrv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

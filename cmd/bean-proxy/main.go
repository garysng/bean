// bean-proxy routes traffic into sandboxes.
//
// It answers one question -- which node holds this sandbox -- and forwards. The
// protocol work happens on the node, because a sandbox's agent and a user's server are
// only reachable from inside that sandbox's network namespace, which only a process on
// that host can enter.
//
// It exists as its own binary so that bulk traffic does not pass through the process
// that makes placement decisions. An upload and a scheduling decision competing for
// one heap is a coupling nobody chose, and the failure it produces -- a slow create
// during a large transfer -- looks like a scheduler problem.
package main

import (
	"flag"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/garysng/bean/internal/control/proxy"
	"github.com/garysng/bean/internal/logging"
)

var version = "dev"

func main() {
	listen := flag.String("listen", "127.0.0.1:7480", "HTTP listen address")
	controlPlane := flag.String("control-plane", "",
		"bean-api base URL, e.g. http://bean-api.internal:8080. Placement is read "+
			"through the API rather than out of its database: a SQLite file does not "+
			"cross machines, and this proxy belongs near the nodes it forwards to")
	apiKey := flag.String("api-key", os.Getenv("BEAN_API_KEY"),
		"key this proxy authenticates to bean-api with (or BEAN_API_KEY). The proxy "+
			"is a cluster component and holds its own credential rather than "+
			"forwarding a caller's")
	cacheFor := flag.Duration("placement-cache", 5*time.Second,
		"how long a sandbox's node is remembered. Placement changes when a sandbox is "+
			"created or destroyed, not per request, so without this every proxied "+
			"request costs two control-plane round trips -- which is most of what "+
			"moving the data plane off the control plane was for")
	nodeToken := flag.String("node-token", os.Getenv("BEAN_NODE_TOKEN"),
		"token presented to a node's forwarding port (or BEAN_NODE_TOKEN)")
	logFormat := flag.String("log-format", "text", "log format: text|json")
	logLevel := flag.String("log-level", "info", "log level: debug|info|warn|error")
	flag.Parse()

	logging.Setup(*logFormat, *logLevel)

	if *controlPlane == "" {
		log.Fatal("--control-plane is required: the proxy asks bean-api which node " +
			"holds a sandbox")
	}

	// No user authentication here, by design: an external layer does that, and bean is
	// the infrastructure underneath it. So whatever can reach this port can reach any
	// sandbox it can name. Saying so at startup rather than in a document, because the
	// mistake is silent -- bound to a public address it works perfectly.
	if isPubliclyRoutable(*listen) {
		log.Fatalf("refusing to listen on %s: this proxy performs no user "+
			"authentication, so anything that reaches it can reach any sandbox in the "+
			"cluster. Bind it to loopback or a private address and put your auth layer "+
			"in front", *listen)
	}

	handler := proxy.New(
		proxy.NewAPISandboxes(*controlPlane, *apiKey, *cacheFor), *nodeToken)

	srv := &http.Server{
		Addr: *listen,
		// h2c, because gRPC to a sandbox's agent arrives here as cleartext HTTP/2 and
		// a plain server answers its preface with an HTTP/1.1 400 -- which a gRPC
		// client reports as a bad server preface, naming neither the port nor the
		// protocol.
		Handler: h2c.NewHandler(handler, &http2.Server{}),
		// Only the headers are bounded. What passes through is a user's own traffic: a
		// long poll, a websocket, a slow upload, an exec that runs for minutes. A
		// timeout short enough to be useful for an API call would cut those off, and
		// the failure would look like their code misbehaving.
		ReadHeaderTimeout: 15 * time.Second,
	}

	slog.Info("bean-proxy listening", "version", version, "addr", *listen,
		"controlPlane", *controlPlane, "nodeTokenSet", *nodeToken != "")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// isPubliclyRoutable reports whether addr binds somewhere reachable from outside the
// operator's own network. Conservative in the direction that matters: a wildcard bind,
// or anything that has to be resolved, counts as public.
func isPubliclyRoutable(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return true
	}
	if host == "" {
		return true
	}
	if host == "localhost" {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return true
	}
	if ip.IsUnspecified() {
		return true
	}
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())
}

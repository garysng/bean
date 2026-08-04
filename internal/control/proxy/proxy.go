// Package proxy resolves which node holds a sandbox and forwards to it.
//
// This is the whole of bean-proxy. It is a reverse proxy and deliberately nothing
// more: it does not speak gRPC, does not know what an exec is, and does not interpret
// paths. The one thing it knows is which node a sandbox is on, which is the one thing
// a node cannot know about a sandbox it does not hold.
//
// The protocol conversion lives on the node, in internal/node's forwarder, because the
// agent is reachable only from inside its own network namespace. Something on that host
// has to make the connection, and putting it there is what leaves this a plain proxy.
//
// It performs no user authentication. An external layer does that -- a Traefik
// middleware in the deployment this is built for -- and bean is the infrastructure
// underneath it (see GitHub #27, "not in scope"). What that means concretely: anything
// that can reach this proxy can reach any sandbox it can name, so it belongs behind
// that layer and not on a public address.
package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/http2"

	"github.com/garysng/bean/internal/logging"
	"github.com/garysng/bean/internal/node/runtime"
)

// Sandboxes answers which node holds a sandbox.
//
// An interface over the store rather than the store itself, so this package does not
// depend on the control plane's schema and a test can drive routing without a
// database. Read-only by construction: a proxy that could write would be a second
// writer to the sandbox ledger.
type Sandboxes interface {
	// NodeAddrFor returns the address of the forwarding port on the node holding
	// sandboxID.
	NodeAddrFor(sandboxID string) (addr string, err error)
}

// ErrNoSandbox is returned for a sandbox the control plane does not know.
var ErrNoSandbox = errors.New("sandbox not found")

// ErrNoForwarding is returned when the node holding a sandbox does not serve the
// forwarding port. Distinct from ErrNoSandbox because the remedy is a node flag
// rather than a corrected id.
var ErrNoForwarding = errors.New("node does not serve sandbox port forwarding")

// Server is the proxy.
type Server struct {
	sandboxes Sandboxes

	// nodeToken authenticates this proxy to a node's forwarding port. It is the
	// cluster's shared secret, held here and never sent to a client: a caller
	// presents whatever the external auth layer requires, and this is what is
	// presented onward.
	nodeToken string

	mu      sync.Mutex
	proxies map[string]*httputil.ReverseProxy
}

// New builds the proxy.
func New(sandboxes Sandboxes, nodeToken string) *Server {
	return &Server{
		sandboxes: sandboxes,
		nodeToken: nodeToken,
		proxies:   map[string]*httputil.ReverseProxy{},
	}
}

// SandboxIDFromHost extracts the sandbox from a Host header.
//
// Only the sandbox: the port stays in the Host and travels to the node untouched,
// because the node is what turns it into an address inside a guest. Parsing the port
// here as well would mean two parsers for one format, and the one that drifts is
// whichever is not exercised by the failing case.
func SandboxIDFromHost(host string) (string, error) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	label := host
	if dot := strings.Index(label, "."); dot >= 0 {
		label = label[:dot]
	}
	portStr, rest, ok := strings.Cut(label, "-")
	if !ok || rest == "" {
		return "", fmt.Errorf("host %q is not {port}-{sandbox}", host)
	}
	// The port is validated even though it is not returned, and this is not
	// belt-and-braces: the node applies the same rule, so accepting a host it will
	// reject turns a 400 into a request forwarded across the network to be refused
	// there -- reported to the caller as a 502, which points at the sandbox rather
	// than at their URL. Found by a test asserting the status for an empty port.
	p, err := strconv.Atoi(portStr)
	if err != nil || p < 1 || p > 65535 {
		return "", fmt.Errorf("host %q: %q is not a port", host, portStr)
	}
	return rest, nil
}

// proxyFor returns the reverse proxy for one node, building it once.
//
// Cached per node rather than per request, so a node's connections are pooled across
// the sandboxes on it. Keyed by address rather than by node id: the address is what
// the connection is to, and a node that moved is a different destination even under
// the same name.
func (s *Server) proxyFor(addr string) *httputil.ReverseProxy {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.proxies[addr]; ok {
		return p
	}

	target := &url.URL{Scheme: "http", Host: addr}
	p := httputil.NewSingleHostReverseProxy(target)

	// Two transports, chosen per request by the port named in the Host.
	//
	// The agent's port carries gRPC, which is HTTP/2 without TLS: over HTTP/1.1 it
	// fails with a bad server preface -- an HTTP/2 frame read as a status line. A
	// user's port is the reverse, almost always HTTP/1.1, and breaks if addressed with
	// an HTTP/2 preface.
	//
	// Cleartext has no ALPN, so nothing is negotiated and the choice has to be made
	// before connecting. This is the same split the node's forwarder makes, for the
	// same reason, and both hops need it independently: fixing one leaves the other
	// failing with a different error.
	h2c := &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, a string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, a)
		},
	}
	h1 := http.DefaultTransport
	p.Transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if portFromHost(r.Header.Get(headerOriginalHost)) == runtime.AgentGuestPort {
			return h2c.RoundTrip(r)
		}
		return h1.RoundTrip(r)
	})

	inner := p.Director
	p.Director = func(r *http.Request) {
		inner(r)
		// The Host is left exactly as the client sent it. It carries the port the
		// caller is addressing, and the node reads it to decide where inside the guest
		// to connect -- so rewriting it to the node's address, which is what a proxy
		// normally does, would erase the routing information.
		r.Host = r.Header.Get(headerOriginalHost)
		if r.Host == "" {
			r.Host = r.URL.Host
		}
	}
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		slog.Warn("proxy to node failed", logging.KeyError, err,
			"node", addr, "host", r.Host)
		http.Error(w, "node did not answer", http.StatusBadGateway)
	}
	s.proxies[addr] = p
	return p
}

// headerOriginalHost carries the client's Host across the director, which rewrites
// r.Host as part of retargeting the request.
const headerOriginalHost = "X-Bean-Original-Host"

// roundTripperFunc adapts a function to http.RoundTripper, so the protocol can be
// chosen per request rather than fixed per node.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// portFromHost reads the port half of {port}-{sandbox}, or 0 if there is none.
//
// Only used to pick a protocol, so an unparseable host yields 0 and the HTTP/1.1
// transport: that is the right default for a user's port, and a malformed host has
// already been rejected by the time this runs.
func portFromHost(host string) int {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	label := host
	if dot := strings.Index(label, "."); dot >= 0 {
		label = label[:dot]
	}
	portStr, _, ok := strings.Cut(label, "-")
	if !ok {
		return 0
	}
	p, err := strconv.Atoi(portStr)
	if err != nil {
		return 0
	}
	return p
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sandboxID, err := SandboxIDFromHost(r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	addr, err := s.sandboxes.NodeAddrFor(sandboxID)
	if err != nil {
		switch {
		case errors.Is(err, ErrNoSandbox):
			http.Error(w, err.Error(), http.StatusNotFound)
		case errors.Is(err, ErrNoForwarding):
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	r.Header.Set(headerOriginalHost, r.Host)
	if s.nodeToken != "" {
		r.Header.Set(headerNodeToken, s.nodeToken)
	}
	s.proxyFor(addr).ServeHTTP(w, r)
}

// headerNodeToken is how the proxy authenticates to a node's forwarding port.
const headerNodeToken = "X-Bean-Node-Token"

// Handler returns the proxy as an http.Handler.
func (s *Server) Handler() http.Handler { return s }

package node

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http2"

	"github.com/garysng/bean/internal/logging"
	"github.com/garysng/bean/internal/node/runtime"
	"github.com/garysng/bean/internal/sbxtoken"
)

// This file reaches a port inside a sandbox from outside the node.
//
// It is one mechanism for two things that look different: the agent's own port, which
// carries exec and file operations, and any port a user's process listens on. Both are
// "connect to this guest on this port", so the router does not distinguish them -- the
// caller names a port and gets that port. A special case for the agent would be a
// second code path with the same job and its own bugs.
//
// The routing key is the Host header, because that is what survives a browser. A user
// opening a preview URL sends only a Host and a path, and the path belongs to their
// application -- so anything the platform needs to know has to be in the Host.

// PortTarget is the sandbox-local address a request was routed to.
type PortTarget struct {
	NetnsPath string
	Addr      string
	// Port is kept apart from Addr because the protocol is chosen from it: the
	// agent's port speaks h2c and a user's port speaks HTTP/1.1, and re-parsing it
	// out of Addr at that decision would be a second place to get it wrong.
	Port int
	// AgentToken is the sandbox's per-sandbox credential, populated only when the
	// request is addressed to the agent's port. noded holds the plaintext (the
	// guest has only its hash), and the forwarder injects it so a client dialling
	// through the proxy never has to -- the auth lives in the node's trust domain,
	// not in the caller's. Empty for a user port, and for a tier that mints no
	// token (vsock/local), where the agent does not check one.
	AgentToken string
}

// roundTripperFunc adapts a function to http.RoundTripper, so the transport can be
// selected per request rather than fixed when the proxy is built.
type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// ParseSandboxHost extracts the sandbox and port from a Host header.
//
// The form is {port}-{sandboxID}, with the port first. That order is deliberate: a
// sandbox id is variable-length and a port is not, so taking the port from the front
// leaves the remainder unambiguous even though ids may contain the separator.
//
// A port and a sandbox and nothing else. No defaulting to the agent's port when the
// port is absent: a request that did not name a port is a request whose author did
// not know what they were addressing, and quietly sending it to the agent would route
// a user's mistyped preview URL at the control interface.
func ParseSandboxHost(host string) (sandboxID string, port int, err error) {
	// The port the request arrived on is not the port being addressed, and a Host
	// header carries it when the listener is not on 80.
	if h, _, splitErr := net.SplitHostPort(host); splitErr == nil {
		host = h
	}
	label := host
	if dot := strings.Index(label, "."); dot >= 0 {
		label = label[:dot]
	}
	portStr, rest, ok := strings.Cut(label, "-")
	if !ok || portStr == "" || rest == "" {
		return "", 0, fmt.Errorf("host %q is not {port}-{sandbox}", host)
	}
	p, convErr := strconv.Atoi(portStr)
	if convErr != nil {
		return "", 0, fmt.Errorf("host %q: port %q is not a number", host, portStr)
	}
	if p < 1 || p > 65535 {
		return "", 0, fmt.Errorf("host %q: port %d out of range", host, p)
	}
	return rest, p, nil
}

// TargetFor resolves a sandbox and port to a namespace and address.
//
// Returns an error for a sandbox this node does not hold, which is the common case
// once a sandbox moves or is destroyed while a preview tab is still open.
func (m *Manager) TargetFor(sandboxID string, port int) (*PortTarget, error) {
	m.mu.Lock()
	sb, ok := m.sandboxes[sandboxID]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrSandboxNotFound, sandboxID)
	}
	net_, state, token := sb.net, sb.State, sb.agentToken
	m.mu.Unlock()

	if net_ == nil {
		// A node without networking has no address to forward to. Distinguished from
		// "not found" because the fix is different: this is a node configuration
		// problem, not a wrong sandbox id.
		return nil, fmt.Errorf("sandbox %s has no network interface; this node was "+
			"started without --guest-subnet", sandboxID)
	}
	if state != runtime.StateRunning {
		// A paused guest holds its addresses but answers nothing, so forwarding would
		// hang until the client gave up. Saying so is the difference between a
		// diagnosable state and an unexplained timeout.
		return nil, fmt.Errorf("sandbox %s is %s, not running", sandboxID, state)
	}

	// The guest address unless the runtime says otherwise. A container's processes
	// listen on the veth rather than the tap, because there is no guest kernel to
	// bring the tap up -- dialling GuestIP there gives "no route to host" on a port
	// that is plainly listening.
	ip := net_.GuestIP
	if addresser, ok := m.rt.(runtime.SandboxAddresser); ok {
		if own := addresser.SandboxIP(net_); own != nil {
			ip = own
		}
	}

	t := &PortTarget{
		NetnsPath: netnsHandlePath(net_.Netns),
		Addr:      fmt.Sprintf("%s:%d", ip, port),
		Port:      port,
	}
	// Only the agent's port carries the credential. A user's process on any other
	// port must never receive noded's per-sandbox token: it runs code the sandbox
	// controls, and handing it the token would leak the very secret that keeps that
	// code from dialling the agent as noded.
	if port == runtime.AgentGuestPort {
		t.AgentToken = token
	}
	return t, nil
}

// PortForwarder serves the Host-routed port.
type PortForwarder struct {
	mgr *Manager

	// token is required of callers. Empty means no check, which is only correct when
	// network position substitutes for authentication -- the same arrangement the
	// gRPC listener has, and the reason cmd/noded refuses a non-loopback bind
	// without one.
	token string

	// proxy is shared across sandboxes. Its transport dials per request through the
	// namespace named in the request's own context, so one instance can serve every
	// sandbox on the node without a connection from one leaking to another.
	proxy *httputil.ReverseProxy

	// Two transports because the destinations speak different protocols and cleartext
	// offers no way to discover which: h1 for a user's server, h2c for the agent.
	h1  http.RoundTripper
	h2c http.RoundTripper
}

// targetKey carries the resolved destination from the director to the dialer. A
// context value rather than a field, because one forwarder serves every sandbox
// concurrently and a field would be a race whose symptom is a request answered by
// the wrong sandbox.
type targetKey struct{}

// dialTarget connects to the destination named in the request's context.
func dialTarget(ctx context.Context) (net.Conn, error) {
	t, ok := ctx.Value(targetKey{}).(*PortTarget)
	if !ok {
		return nil, fmt.Errorf("port forward: no target in context")
	}
	return dialInNetns(ctx, t.NetnsPath, t.Addr)
}

// h2cTransport speaks cleartext HTTP/2, which is what gRPC needs.
//
// The agent's port is one of the ports reachable through this forwarder, and gRPC is
// HTTP/2 with no TLS. An HTTP/1.1 transport reading that connection fails with
// `malformed HTTP response "\x00\x00\x06\x04..."` -- an HTTP/2 SETTINGS frame parsed
// as a status line. Measured rather than anticipated: forwarding to the agent returned
// exactly that before this existed.
//
// AllowHTTP with a DialTLSContext that returns a plain connection is the documented
// way to get h2c out of this package: without it the transport insists on TLS, and
// there is none here -- the hop is node-local, into a namespace on the same host.
func h2cTransport() http.RoundTripper {
	return &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, _, _ string, _ *tls.Config) (net.Conn, error) {
			return dialTarget(ctx)
		},
	}
}

// transportFor picks the protocol for a destination.
//
// Chosen by port rather than negotiated, because there is nothing to negotiate over
// cleartext: h2c has no ALPN, so a client either speaks HTTP/2 first or does not. A
// user's server on port 8000 is overwhelmingly HTTP/1.1 and would break if addressed
// with an HTTP/2 preface; the agent is the opposite. The agent's port is a constant,
// so the split is decidable.
func (f *PortForwarder) transportFor(port int) http.RoundTripper {
	if port == runtime.AgentGuestPort {
		return f.h2c
	}
	return f.h1
}

// HeaderNodeToken is the credential a caller of this port must present.
//
// The same name the gRPC surface uses for the same secret (MetadataTokenKey): gRPC
// metadata travels in HTTP/2 headers, so one credential under two names is one name
// too many, and a mismatch rejects every request while looking like a bad token.
const HeaderNodeToken = MetadataTokenKey

// NewPortForwarder builds the forwarder.
//
// token is the shared node token this port requires. Empty disables the check, which
// is correct only on loopback -- cmd/noded refuses a non-loopback bind without one, the
// same rule the gRPC listener follows.
func NewPortForwarder(mgr *Manager, token string) *PortForwarder {
	f := &PortForwarder{mgr: mgr, token: token}
	f.h2c = h2cTransport()
	f.h1 = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialTarget(ctx)
		},
		// No connection reuse. A pooled connection is keyed by the placeholder host
		// the director sets, which is identical for every sandbox, so reuse would hand
		// one sandbox's connection to a request for another -- the worst failure
		// available here, and one that only appears under concurrency.
		DisableKeepAlives:   true,
		MaxIdleConns:        0,
		IdleConnTimeout:     time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	f.proxy = &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			// The scheme and host are placeholders: the dialer below ignores them and
			// connects to the address in the context. They have to be set for the
			// request to be well-formed.
			r.URL.Scheme = "http"
			r.URL.Host = "sandbox"

			// Inject the per-sandbox agent token, which the agent reads as gRPC
			// metadata -- and gRPC metadata is carried in HTTP/2 headers, so setting
			// the header here is setting the metadata the agent checks. The token is
			// present only for the agent's port (TargetFor populates it nowhere else),
			// so a user's port is never given it.
			//
			// The header is overwritten, not appended: whatever a caller sent under
			// this name is discarded, so a client cannot smuggle its own value past
			// the node. When there is no token (a user port, or a tier that mints
			// none) the header is deleted outright, leaving the agent to see "no
			// credential" rather than an empty one.
			if t, ok := r.Context().Value(targetKey{}).(*PortTarget); ok && t.AgentToken != "" {
				r.Header.Set(sbxtoken.MDKey, t.AgentToken)
			} else {
				r.Header.Del(sbxtoken.MDKey)
			}
		},
		// Chosen per request by transportFor, so the agent's port gets h2c and a
		// user's port gets HTTP/1.1.
		Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			t, _ := r.Context().Value(targetKey{}).(*PortTarget)
			if t == nil {
				return nil, fmt.Errorf("port forward: no target in context")
			}
			return f.transportFor(t.Port).RoundTrip(r)
		}),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			// 502 rather than 500: the failure is between this node and the guest, and
			// a user looking at a preview that will not load needs to know the
			// difference between "the platform is broken" and "your process is not
			// listening on that port".
			slog.Warn("port forward failed", logging.KeyError, err,
				"host", r.Host, "path", r.URL.Path)
			http.Error(w, "sandbox did not answer on that port", http.StatusBadGateway)
		},
	}
	return f
}

func (f *PortForwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Checked before the Host is parsed, so an unauthenticated caller learns nothing
	// about which sandbox ids exist: a 401 for a bad token and a 404 for an unknown
	// sandbox would otherwise let anyone enumerate the node's sandboxes.
	if !f.authorized(r) {
		http.Error(w, "invalid node token", http.StatusUnauthorized)
		return
	}

	sandboxID, port, err := ParseSandboxHost(r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target, err := f.mgr.TargetFor(sandboxID, port)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	f.proxy.ServeHTTP(w, r.WithContext(
		context.WithValue(r.Context(), targetKey{}, target)))
}

// authorized reports whether a caller presented the node token.
//
// This port grants access to every sandbox on the node, including the agent's own
// interface which runs commands as root, so it is not a place for a permissive check.
// It is nonetheless allowed to be off: with no token set, network position is the
// authentication, exactly as it is for the gRPC listener and for the Docker daemon on
// a Unix socket. cmd/noded is what keeps that honest by refusing a non-loopback bind
// without a token.
func (f *PortForwarder) authorized(r *http.Request) bool {
	if f.token == "" {
		return true
	}
	// Constant-time because the comparison is against a secret and the caller is
	// remote; a byte-at-a-time compare leaks a prefix oracle.
	return subtle.ConstantTimeCompare(
		[]byte(r.Header.Get(HeaderNodeToken)), []byte(f.token)) == 1
}

// netnsHandlePath renders the bind-mounted namespace handle's path.
//
// Duplicated from the runtime's netnsPathFor rather than exported from there,
// because that function takes a runtime.Spec and this caller has only a name. The
// directory is the one `ip netns` uses, which is what makes a sandbox's namespace
// visible to an operator running `ip netns list`.
func netnsHandlePath(name string) string {
	if name == "" {
		return ""
	}
	return "/var/run/netns/" + name
}

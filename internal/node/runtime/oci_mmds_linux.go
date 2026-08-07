//go:build linux

package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"log/slog"

	"github.com/garysng/bean/internal/logging"
)

// The agent requires a token on a TCP listener, and reads the expected hash from a
// metadata service at 169.254.169.254. On the fc tier Firecracker provides that; a
// container has no such thing, so the node provides it.
//
// # Why not simply skip authentication
//
// Because the agent refuses to. cmd/beand/main.go derives the requirement from the
// transport rather than from a flag: any TCP listener demands a token, on the reasoning
// that a TCP address is reachable *from inside the sandbox*, so the token is not
// hardening but the only thing separating noded from the sandbox's own root. A flag
// would eventually be omitted by a script and hand a sandbox its own agent API.
//
// Spec.AgentTokenHash's comment says the container tier can go unauthenticated because
// its socket sits outside the mount namespace -- true of a Unix socket, and not of the
// TCP transport gVisor forced. Reading that comment as permission was a mistake worth
// naming, since the difference is the entire security argument.
//
// # Why one server per sandbox
//
// The document holds one sandbox's credential. A service shared between sandboxes would
// let any of them read another's token hash, and a hash is what the agent compares
// against -- so that is a hole, not an inefficiency. Each server listens only inside one
// sandbox's network namespace, which is exactly as far as it should reach.
type mmdsServer struct {
	// tokenHash is what the agent will compare a presented token against.
	tokenHash string

	srv *http.Server

	mu       sync.Mutex
	sessions map[string]time.Time
}

// mmdsAddr is the address the agent reads metadata from.
//
// Firecracker's convention, kept because the agent has it compiled in: changing it would
// mean changing the agent, and then an image built against one node would not work on
// another.
const mmdsAddr = "169.254.169.254"

// mmdsListenAddr is what the metadata service binds.
const mmdsListenAddr = mmdsAddr + ":80"

// mmdsSessionWindow bounds how long a session token from PUT /latest/api/token stays
// valid. The agent asks for a TTL and re-reads per handshake, so this only has to
// outlive one exchange.
const mmdsSessionWindow = 5 * time.Minute

// startMMDS serves the metadata document inside nsPath until the returned stop is
// called.
//
// The listener is opened inside the namespace, which is the whole point: 169.254.169.254
// is a link-local address that exists in every namespace, so a server bound in the host
// namespace would answer every sandbox, and one bound in the wrong namespace would
// answer none.
func startMMDS(nsPath, iface, tokenHash string) (stop func() error, err error) {
	if tokenHash == "" {
		// Refused rather than served empty: the agent treats an empty hash as "nothing
		// was published" and denies every call, so an empty document produces a sandbox
		// that starts and cannot be reached -- with no indication why.
		return nil, errors.New("runtime: metadata service needs an agent token hash")
	}

	m := &mmdsServer{tokenHash: tokenHash, sessions: map[string]time.Time{}}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /latest/api/token", m.handleSession)
	mux.HandleFunc("GET /", m.handleDocument)
	m.srv = &http.Server{
		Handler: mux,
		// Bounded because this serves a process inside the sandbox: a client that
		// opens a connection and never speaks would otherwise hold a goroutine.
		ReadHeaderTimeout: 5 * time.Second,
	}

	// The address has to exist on an interface before anything can bind it.
	//
	// 169.254.169.254 is link-local: it is routable in every namespace but owned by no
	// interface, so a bind fails with "cannot assign requested address". Firecracker
	// never hits this because it does not bind -- it answers that address on the tap
	// itself, below the forwarding path, as internal/node/network/rules.go describes.
	// A real listener needs the address assigned.
	//
	// Added by this tier rather than by the network pool: the pool serves both tiers and
	// the fc path must not grow an address it does not use. Removed again when the
	// service stops, so a namespace that outlives one sandbox does not accumulate them.
	if err := addMMDSAddr(nsPath, iface); err != nil {
		return nil, err
	}

	lis, err := listenInNetns(nsPath, mmdsListenAddr)
	if err != nil {
		_ = delMMDSAddr(nsPath, iface)
		return nil, err
	}

	served := make(chan error, 1)
	go func() { served <- m.srv.Serve(lis) }()

	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err := m.srv.Shutdown(ctx)
		<-served
		// The address goes with it. Reported but not fatal: the namespace is normally
		// torn down straight after, so a stray address costs nothing -- unlike leaving
		// the caller thinking teardown failed.
		if derr := delMMDSAddr(nsPath, iface); derr != nil {
			slog.Warn("could not remove the metadata address from the sandbox namespace",
				"netns", nsPath, logging.KeyError, derr)
		}
		return err
	}, nil
}

// addMMDSAddr assigns the metadata address to an interface inside the namespace.
//
// /32 so it adds an address without implying a subnet: the interface already carries the
// veth's own /30, and a second prefix would change routing inside the sandbox.
func addMMDSAddr(nsPath, iface string) error {
	out, err := exec.Command("ip", "netns", "exec", filepath.Base(nsPath),
		"ip", "addr", "add", mmdsAddr+"/32", "dev", iface).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "File exists") {
		return fmt.Errorf("runtime: assign %s in %s: %w: %s", mmdsAddr, nsPath, err,
			strings.TrimSpace(string(out)))
	}
	return nil
}

func delMMDSAddr(nsPath, iface string) error {
	out, err := exec.Command("ip", "netns", "exec", filepath.Base(nsPath),
		"ip", "addr", "del", mmdsAddr+"/32", "dev", iface).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "Cannot assign") {
		return fmt.Errorf("runtime: remove %s from %s: %w: %s", mmdsAddr, nsPath, err,
			strings.TrimSpace(string(out)))
	}
	return nil
}

// handleSession implements the V2 handshake's first half.
//
// V2 rather than V1 because the agent uses V2, and for the reason its own comment
// gives: V1 answers a bare GET, so any process in the sandbox that can be induced to
// fetch a URL reads the metadata as a side effect. A PUT is not something a redirect
// can produce.
func (m *mmdsServer) handleSession(w http.ResponseWriter, r *http.Request) {
	token, err := sessionToken()
	if err != nil {
		// Without a token the agent cannot read the document and will deny every call,
		// so failing loudly beats handing out a predictable one.
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	m.mu.Lock()
	// Expired entries are dropped on write rather than by a timer: the map only grows
	// while a sandbox is handshaking, and a sweep here costs nothing at this rate.
	now := time.Now()
	for t, exp := range m.sessions {
		if exp.Before(now) {
			delete(m.sessions, t)
		}
	}
	m.sessions[token] = now.Add(mmdsSessionWindow)
	m.mu.Unlock()

	w.Header().Set("Content-Type", "text/plain")
	_, _ = w.Write([]byte(token))
}

// handleDocument serves the metadata, and only to a caller holding a session token.
func (m *mmdsServer) handleDocument(w http.ResponseWriter, r *http.Request) {
	token := r.Header.Get("X-metadata-token")
	m.mu.Lock()
	exp, ok := m.sessions[token]
	m.mu.Unlock()
	if !ok || exp.Before(time.Now()) {
		// 401 rather than 403: the caller may retry after another PUT, which is what
		// the agent does.
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	// The field name is the agent's, not ours -- mmdsDoc in internal/beand decodes
	// exactly this key, and a rename here would be a sandbox that never authenticates.
	if err := json.NewEncoder(w).Encode(map[string]string{
		"agentTokenHash": m.tokenHash,
	}); err != nil {
		return
	}
}

// listenInNetns opens a listener inside a network namespace.
//
// The mirror of dialInNetns in internal/node, and it carries the same constraints as
// startInNetns in this package: setns is per-thread, the Go runtime moves goroutines
// between threads at every blocking point, so the thread is locked and nothing between
// the two setns calls may block on anything that could reschedule it.
//
// Only the bind happens inside. The returned listener is an ordinary file descriptor
// with no namespace affinity, so Accept and everything after it need nothing special --
// which is what keeps this to one narrow window.
func listenInNetns(nsPath, addr string) (net.Listener, error) {
	target, err := os.Open(nsPath)
	if err != nil {
		return nil, fmt.Errorf("runtime: open %s: %w", nsPath, err)
	}
	defer target.Close()

	type result struct {
		lis net.Listener
		err error
	}
	ch := make(chan result, 1)

	go func() {
		goruntime.LockOSThread()

		// Not deferred, and not always called -- the same reasoning startInNetns
		// documents. If the restore below fails the thread is still in the sandbox's
		// namespace, and unlocking it would return it to the runtime's pool where the
		// next goroutine to land on it would silently get sandbox networking. Leaving
		// the goroutine with the thread still locked makes the runtime destroy the
		// thread instead, which is the only safe disposal.
		unlock := false
		defer func() {
			if unlock {
				goruntime.UnlockOSThread()
			}
		}()

		// thread-self rather than self: the namespace being saved belongs to this
		// thread, while /proc/self/ns/net is the process's main thread's.
		host, err := os.Open("/proc/thread-self/ns/net")
		if err != nil {
			ch <- result{nil, fmt.Errorf("runtime: open current network namespace: %w", err)}
			unlock = true
			return
		}
		defer host.Close()

		if err := unix.Setns(int(target.Fd()), unix.CLONE_NEWNET); err != nil {
			ch <- result{nil, fmt.Errorf("runtime: enter %s: %w", nsPath, err)}
			unlock = true
			return
		}

		lis, listenErr := net.Listen("tcp", addr)

		if err := unix.Setns(int(host.Fd()), unix.CLONE_NEWNET); err != nil {
			// unlock stays false: see above. A listener that was opened is reported
			// even so, because it exists and the caller has to be told about it or it
			// leaks.
			if listenErr != nil {
				ch <- result{nil, listenErr}
				return
			}
			ch <- result{lis, nil}
			return
		}
		unlock = true

		if listenErr != nil {
			ch <- result{nil, fmt.Errorf("runtime: metadata service listen in %s: %w",
				nsPath, listenErr)}
			return
		}
		ch <- result{lis, nil}
	}()

	r := <-ch
	return r.lis, r.err
}

// sessionToken is an unguessable handle for the V2 handshake.
//
// crypto/rand rather than a uuid dependency: uuid is only an indirect requirement of
// this module, and promoting it for one opaque string is not worth it. Unguessability is
// what matters here, not the format -- the agent treats it as an opaque token.
func sessionToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

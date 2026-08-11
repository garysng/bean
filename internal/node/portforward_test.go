package node

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
	"github.com/garysng/bean/internal/node/runtime"
	"github.com/garysng/bean/internal/sbxtoken"
)

func TestParseSandboxHost(t *testing.T) {
	for _, tc := range []struct {
		host string
		id   string
		port int
	}{
		{"10001-sbx_abc.sandbox.ai", "sbx_abc", 10001},
		{"8000-sbx_abc.sandbox.ai", "sbx_abc", 8000},
		// The listener is rarely on 80, so a real browser request carries the port
		// it connected to. Treating that as part of the name would fail every
		// request in any deployment that is not behind a load balancer on 80.
		{"8000-sbx_abc.sandbox.ai:9999", "sbx_abc", 8000},
		// No domain at all, which is what a curl with an explicit Host sends and what
		// a self-hosted deployment without DNS uses.
		{"8000-sbx_abc", "sbx_abc", 8000},
		// A sandbox id containing the separator. This is why the port is first: taking
		// it from the front leaves the rest intact however many separators it holds.
		{"8000-sbx-with-dashes.sandbox.ai", "sbx-with-dashes", 8000},
	} {
		id, port, err := ParseSandboxHost(tc.host)
		if err != nil {
			t.Errorf("%s: %v", tc.host, err)
			continue
		}
		if id != tc.id || port != tc.port {
			t.Errorf("%s -> (%q, %d), want (%q, %d)", tc.host, id, port, tc.id, tc.port)
		}
	}
}

func TestParseSandboxHostRejectsWhatItCannotRoute(t *testing.T) {
	// A request that did not name a port must fail rather than default. Defaulting to
	// the agent's port would send a user's mistyped preview URL at the interface that
	// runs commands as root.
	for _, host := range []string{
		"sbx_abc.sandbox.ai", // no port
		"sandbox.ai",
		"-sbx_abc.sandbox.ai", // empty port
		"8000-.sandbox.ai",    // empty sandbox
		"notaport-sbx_abc.sandbox.ai",
		"0-sbx_abc.sandbox.ai",     // out of range
		"99999-sbx_abc.sandbox.ai", // out of range
		"",
	} {
		if id, port, err := ParseSandboxHost(host); err == nil {
			t.Errorf("host %q was accepted as (%q, %d); an unroutable host must be "+
				"rejected rather than guessed at", host, id, port)
		}
	}
}

func TestForwardingRejectsAnUnroutableHost(t *testing.T) {
	m, _ := newNetworkedManager(t)
	f := NewPortForwarder(m, "")

	r := httptest.NewRequest(http.MethodGet, "http://example/", nil)
	r.Host = "no-port-here.sandbox.ai"
	w := httptest.NewRecorder()
	f.ServeHTTP(w, r)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400 for a host that names no port", w.Code)
	}
}

func TestForwardingRejectsASandboxThisNodeDoesNotHold(t *testing.T) {
	// The common case once a preview tab outlives its sandbox. A 404 rather than a
	// hang or a 502, so the client can tell "gone" from "not answering".
	m, _ := newNetworkedManager(t)
	f := NewPortForwarder(m, "")

	r := httptest.NewRequest(http.MethodGet, "http://example/", nil)
	r.Host = "8000-sbx_not_here.sandbox.ai"
	w := httptest.NewRecorder()
	f.ServeHTTP(w, r)

	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404 for an unknown sandbox", w.Code)
	}
}

func TestTargetForResolvesTheNamespaceAndAddress(t *testing.T) {
	// Both halves are required, and the address half is the same for every sandbox on
	// the node by design -- so a target missing its namespace would silently address
	// whichever sandbox the host namespace happens to route to.
	m, _ := newNetworkedManager(t)
	if _, err := m.Create(context.Background(), &nodev1.SandboxSpec{
		SandboxId: "sbx_fwd", Image: "scratch",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	target, err := m.TargetFor("sbx_fwd", 8000)
	if err != nil {
		t.Fatalf("TargetFor: %v", err)
	}
	if target.NetnsPath == "" {
		t.Error("no namespace in the target; the address alone does not identify a " +
			"sandbox, so this would forward into whatever the host namespace routes to")
	}
	if !strings.HasSuffix(target.Addr, ":8000") {
		t.Errorf("target address %q does not carry the requested port", target.Addr)
	}
}

func TestTargetForSaysWhyWhenTheNodeHasNoNetworking(t *testing.T) {
	// Distinguished from "not found" because the fix is different: the sandbox exists
	// and the id is right, but this node was started without --guest-subnet and has
	// no address to forward to.
	m := newTestManager(t)
	if _, err := m.Create(context.Background(), &nodev1.SandboxSpec{
		SandboxId: "sbx_nonet_fwd", Image: "scratch",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	_, err := m.TargetFor("sbx_nonet_fwd", 8000)
	if err == nil {
		t.Fatal("a sandbox with no network resolved to a forwarding target")
	}
	if !strings.Contains(err.Error(), "guest-subnet") {
		t.Errorf("error %q does not name the missing configuration, so an operator "+
			"cannot tell it from a wrong sandbox id", err)
	}
}

// This port reaches every sandbox on the node, including the agent's interface which
// runs commands as root. The proxy in front presented a token from the beginning; for
// a while nothing here read it, so the header was decoration and the port was
// protected by network position alone.

func TestForwardingRequiresTheNodeToken(t *testing.T) {
	m, _ := newNetworkedManager(t)
	f := NewPortForwarder(m, "sekret")

	for _, tc := range []struct {
		name, token string
	}{
		{"no token", ""},
		{"wrong token", "guessed"},
		// A prefix of the real token, which a byte-at-a-time comparison would accept
		// more slowly than a wrong first byte -- the oracle the constant-time compare
		// exists to close.
		{"prefix of the token", "sek"},
	} {
		r := httptest.NewRequest(http.MethodGet, "http://example/", nil)
		r.Host = "8000-sbx_abc.sandbox.ai"
		if tc.token != "" {
			r.Header.Set(HeaderNodeToken, tc.token)
		}
		w := httptest.NewRecorder()
		f.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", tc.name, w.Code)
		}
	}
}

func TestTheTokenCheckDoesNotRevealWhichSandboxesExist(t *testing.T) {
	// An unauthenticated caller must not be able to tell a real sandbox from an
	// invented one. Checking the token after resolving the sandbox would answer 404
	// for one and 401 for the other, which is an enumeration oracle for the whole
	// node.
	m, _ := newNetworkedManager(t)
	if _, err := m.Create(context.Background(), &nodev1.SandboxSpec{
		SandboxId: "sbx_real", Image: "scratch",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	f := NewPortForwarder(m, "sekret")

	codes := map[string]int{}
	for _, id := range []string{"sbx_real", "sbx_invented"} {
		r := httptest.NewRequest(http.MethodGet, "http://example/", nil)
		r.Host = "8000-" + id + ".sandbox.ai"
		w := httptest.NewRecorder()
		f.ServeHTTP(w, r)
		codes[id] = w.Code
	}
	if codes["sbx_real"] != codes["sbx_invented"] {
		t.Fatalf("a real sandbox answered %d and an invented one %d; the difference "+
			"lets an unauthenticated caller enumerate this node's sandboxes",
			codes["sbx_real"], codes["sbx_invented"])
	}
	if codes["sbx_real"] != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401 for both", codes["sbx_real"])
	}
}

func TestTheRightTokenIsAdmitted(t *testing.T) {
	// The negative control for the tests above: with the correct token the request
	// gets past authentication and fails on something else (an unknown sandbox), which
	// is what shows the 401s were about the token rather than about everything.
	m, _ := newNetworkedManager(t)
	f := NewPortForwarder(m, "sekret")

	r := httptest.NewRequest(http.MethodGet, "http://example/", nil)
	r.Host = "8000-sbx_absent.sandbox.ai"
	r.Header.Set(HeaderNodeToken, "sekret")
	w := httptest.NewRecorder()
	f.ServeHTTP(w, r)

	if w.Code == http.StatusUnauthorized {
		t.Fatal("the correct token was rejected")
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404: authentication passed and the sandbox does not "+
			"exist on this node", w.Code)
	}
}

func TestAnEmptyTokenDisablesTheCheck(t *testing.T) {
	// Loopback development, and the same arrangement the gRPC listener has. cmd/noded
	// is what keeps it honest by refusing an off-loopback bind without a token.
	m, _ := newNetworkedManager(t)
	f := NewPortForwarder(m, "")

	r := httptest.NewRequest(http.MethodGet, "http://example/", nil)
	r.Host = "8000-sbx_absent.sandbox.ai"
	w := httptest.NewRecorder()
	f.ServeHTTP(w, r)

	if w.Code == http.StatusUnauthorized {
		t.Fatal("a forwarder with no token configured demanded one")
	}
}

func TestWebSocketUpgradeSurvivesTheNodeHop(t *testing.T) {
	// The node's forwarder wraps its transport in a func to pick h2c or HTTP/1.1 per
	// request, and httputil.ReverseProxy only tunnels a 101 when it can get at an
	// http.Transport. So the wrapping is what this checks: a handshake that returns
	// 101 and then moves no bytes is the failure mode, and it looks like the user's
	// app hanging.
	//
	// The dialer is replaced rather than a sandbox booted: what is under test is the
	// proxy plumbing, and a real guest would test the guest.
	upstream := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
				t.Errorf("upstream saw Upgrade=%q, want websocket", r.Header.Get("Upgrade"))
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			conn, buf, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Fatalf("hijack: %v", err)
			}
			defer conn.Close()
			_, _ = buf.WriteString("HTTP/1.1 101 Switching Protocols\r\n" +
				"Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			_ = buf.Flush()
			line, _ := buf.ReadString('\n')
			_, _ = buf.WriteString("ECHO:" + line)
			_ = buf.Flush()
		}))
	defer upstream.Close()
	upstreamAddr := strings.TrimPrefix(upstream.URL, "http://")

	m, _ := newNetworkedManager(t)
	if _, err := m.Create(context.Background(), &nodev1.SandboxSpec{
		SandboxId: "sbx_ws", Image: "scratch",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	f := NewPortForwarder(m, "")
	// Send the guest-bound connection to the test server instead of into a namespace.
	f.h1 = &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", upstreamAddr)
		},
		DisableKeepAlives: true,
	}

	front := httptest.NewServer(f)
	defer front.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatalf("dial the forwarder: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := fmt.Fprintf(conn, "GET /live HTTP/1.1\r\nHost: 8000-sbx_ws.sandbox.ai\r\n"+
		"Connection: Upgrade\r\nUpgrade: websocket\r\n"+
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"); err != nil {
		t.Fatalf("write the handshake: %v", err)
	}

	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read the status line: %v", err)
	}
	if !strings.Contains(status, "101") {
		t.Fatalf("handshake got %q, want 101", strings.TrimSpace(status))
	}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read headers: %v", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}

	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatalf("write after upgrade: %v", err)
	}
	echoed, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read after upgrade: %v", err)
	}
	if strings.TrimSpace(echoed) != "ECHO:ping" {
		t.Fatalf("after the upgrade got %q, want ECHO:ping -- the 101 arrived but the "+
			"tunnel carries no bytes", strings.TrimSpace(echoed))
	}
}

// The agent's port is the one place the forwarder injects the per-sandbox token,
// so a client dialling through the proxy never handles it. These two tests pin the
// boundary from both sides: the agent port carries the credential, and no other
// port does -- a user's process must never be handed the secret that lets its
// holder impersonate noded to the agent.

func TestTargetForCarriesTheAgentTokenOnTheAgentPort(t *testing.T) {
	m, _ := newNetworkedManager(t)
	if _, err := m.Create(context.Background(), &nodev1.SandboxSpec{
		SandboxId: "sbx_tok", Image: "scratch",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	target, err := m.TargetFor("sbx_tok", runtime.AgentGuestPort)
	if err != nil {
		t.Fatalf("TargetFor: %v", err)
	}
	if target.AgentToken == "" {
		t.Fatal("the agent port resolved without a token; a client dialling through " +
			"the proxy would then reach a fail-closed agent and be denied")
	}
	// The token the forwarder injects must be the plaintext the agent's hash was
	// derived from, or the agent rejects it. Verify against the hash the guest holds.
	m.mu.Lock()
	sb := m.sandboxes["sbx_tok"]
	hash := sbxtoken.Hash(sb.agentToken)
	m.mu.Unlock()
	if !sbxtoken.Verify(hash, target.AgentToken) {
		t.Fatal("the injected token does not verify against the sandbox's hash, so " +
			"the agent would reject the proxied call")
	}
}

func TestTargetForWithholdsTheAgentTokenFromAUserPort(t *testing.T) {
	m, _ := newNetworkedManager(t)
	if _, err := m.Create(context.Background(), &nodev1.SandboxSpec{
		SandboxId: "sbx_userport", Image: "scratch",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}

	target, err := m.TargetFor("sbx_userport", 8000)
	if err != nil {
		t.Fatalf("TargetFor: %v", err)
	}
	if target.AgentToken != "" {
		t.Fatal("a user port carries the agent token; the process listening there " +
			"runs code the sandbox controls, and would receive the secret that keeps " +
			"it from dialling the agent as noded")
	}
}

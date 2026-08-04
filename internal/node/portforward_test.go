package node

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	nodev1 "github.com/garysng/bean/internal/gen/bean/node/v1"
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
	f := NewPortForwarder(m)

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
	f := NewPortForwarder(m)

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

package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSandboxes answers the one question the proxy asks.
type fakeSandboxes struct {
	addr string
	err  error
	// asked records what was looked up, so a test can tell "routed to the right
	// node" from "routed somewhere that happened to answer".
	asked []string
}

func (f *fakeSandboxes) NodeAddrFor(id string) (string, error) {
	f.asked = append(f.asked, id)
	return f.addr, f.err
}

func TestSandboxIDFromHost(t *testing.T) {
	for _, tc := range []struct{ host, want string }{
		{"10001-sbx_abc.sandbox.ai", "sbx_abc"},
		{"8000-sbx_abc.sandbox.ai", "sbx_abc"},
		{"8000-sbx_abc.sandbox.ai:8443", "sbx_abc"},
		{"8000-sbx_abc", "sbx_abc"},
		// A sandbox id containing the separator survives, which is why only the first
		// segment is taken as the port.
		{"8000-sbx-with-dashes.sandbox.ai", "sbx-with-dashes"},
	} {
		got, err := SandboxIDFromHost(tc.host)
		if err != nil {
			t.Errorf("%s: %v", tc.host, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s -> %q, want %q", tc.host, got, tc.want)
		}
	}
}

func TestRequestIsForwardedWithTheClientsHostIntact(t *testing.T) {
	// The Host is the routing information for the *next* hop: the node reads it to
	// decide which port inside the guest to connect to. A reverse proxy normally
	// rewrites it to the upstream's address, and doing that here would erase the port
	// -- leaving the node unable to route and the failure looking like the sandbox not
	// listening.
	var gotHost, gotToken, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			gotHost, gotToken, gotPath = r.Host, r.Header.Get(headerNodeToken), r.URL.Path
			fmt.Fprint(w, "from the node")
		}))
	defer upstream.Close()

	fake := &fakeSandboxes{addr: strings.TrimPrefix(upstream.URL, "http://")}
	s := New(fake, "ntok")

	r := httptest.NewRequest(http.MethodGet, "http://ignored/some/app/path", nil)
	r.Host = "8000-sbx_abc.sandbox.ai"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("got %d: %s", w.Code, w.Body.String())
	}
	if gotHost != "8000-sbx_abc.sandbox.ai" {
		t.Errorf("upstream saw Host %q; the node needs the client's Host to know "+
			"which port inside the guest to reach", gotHost)
	}
	if gotPath != "/some/app/path" {
		t.Errorf("upstream saw path %q, want it untouched: the path belongs to the "+
			"user's application", gotPath)
	}
	if gotToken != "ntok" {
		t.Errorf("upstream saw node token %q, want the proxy's own credential "+
			"presented onward", gotToken)
	}
	if len(fake.asked) != 1 || fake.asked[0] != "sbx_abc" {
		t.Errorf("looked up %v, want exactly [sbx_abc]", fake.asked)
	}
	if body, _ := io.ReadAll(w.Body); string(body) != "from the node" {
		t.Errorf("body %q did not come from the upstream", body)
	}
}

func TestTheClientNeverSeesTheNodeToken(t *testing.T) {
	// The token is the cluster's shared secret. It is added on the way out, and a
	// client that supplied one must not have it echoed or trusted.
	upstream := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get(headerNodeToken); got != "real" {
				t.Errorf("upstream saw token %q, want the proxy's own; a "+
					"client-supplied value must not pass through", got)
			}
		}))
	defer upstream.Close()

	s := New(&fakeSandboxes{addr: strings.TrimPrefix(upstream.URL, "http://")}, "real")
	r := httptest.NewRequest(http.MethodGet, "http://ignored/", nil)
	r.Host = "8000-sbx_abc.sandbox.ai"
	r.Header.Set(headerNodeToken, "forged")
	s.ServeHTTP(httptest.NewRecorder(), r)
}

func TestUnroutableHostIsRejected(t *testing.T) {
	s := New(&fakeSandboxes{addr: "127.0.0.1:1"}, "")
	for _, host := range []string{"sandbox.ai", "-sbx_abc.sandbox.ai", ""} {
		r := httptest.NewRequest(http.MethodGet, "http://ignored/", nil)
		r.Host = host
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("host %q got %d, want 400", host, w.Code)
		}
	}
}

func TestLookupFailuresMapToDistinctStatuses(t *testing.T) {
	// The three cases have different remedies -- correct the id, start the node with a
	// flag, look at the control plane -- so collapsing them into one status would
	// leave an operator with nothing to act on.
	for _, tc := range []struct {
		err  error
		want int
	}{
		{fmt.Errorf("%w: sbx_x", ErrNoSandbox), http.StatusNotFound},
		{fmt.Errorf("%w: node n1", ErrNoForwarding), http.StatusServiceUnavailable},
		{fmt.Errorf("database is locked"), http.StatusInternalServerError},
	} {
		s := New(&fakeSandboxes{err: tc.err}, "")
		r := httptest.NewRequest(http.MethodGet, "http://ignored/", nil)
		r.Host = "8000-sbx_abc.sandbox.ai"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("error %v got %d, want %d", tc.err, w.Code, tc.want)
		}
	}
}

func TestAnUnreachableNodeIsABadGateway(t *testing.T) {
	// The sandbox and node are known; the node is not answering. 502 rather than 500,
	// so a caller can tell "the platform lost track of this" from "the machine holding
	// it is down".
	s := New(&fakeSandboxes{addr: "127.0.0.1:1"}, "")
	r := httptest.NewRequest(http.MethodGet, "http://ignored/", nil)
	r.Host = "8000-sbx_abc.sandbox.ai"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502 for a node that does not answer", w.Code)
	}
}

package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/garysng/bean/internal/gen/bean/agent/v1/agentv1connect"
	commonv1 "github.com/garysng/bean/internal/gen/bean/common/v1"
	"github.com/garysng/bean/internal/node/runtime"
)

func TestDataPlaneForOptsInOnlyWithAProxyURL(t *testing.T) {
	// Unset proxy means the client stays on the bean-api relay, so a single-node
	// or dev deployment is unaffected. Only a set URL opts into the data plane.
	if _, ok := dataPlaneFor("", "example.com"); ok {
		t.Error("empty proxy URL opted into the data plane; the relay must be the default")
	}
	if dp, ok := dataPlaneFor("http://127.0.0.1:7480", ""); !ok || dp == nil {
		t.Fatal("a set proxy URL did not produce a data-plane target")
	}
}

func TestDataPlaneForStripsTheScheme(t *testing.T) {
	// The transport dials a bare host:port; a scheme left on would make it dial a
	// host literally named "http".
	for _, in := range []string{
		"http://proxy.example:7480",
		"https://proxy.example:7480/",
		"proxy.example:7480",
	} {
		dp, ok := dataPlaneFor(in, "")
		if !ok {
			t.Fatalf("%q did not produce a target", in)
		}
		if dp.proxyAddr != "proxy.example:7480" {
			t.Errorf("%q -> proxyAddr %q, want proxy.example:7480", in, dp.proxyAddr)
		}
	}
}

func TestAuthorityIsPortDashSandbox(t *testing.T) {
	// The authority is what the proxy and the node's forwarder route on, and its
	// shape must match the Host a browser preview would send: "{port}-{sandbox}"
	// with the domain as a suffix, or the bare label when there is no domain.
	agent := runtime.AgentGuestPort

	withDomain := &dataPlane{proxyAddr: "p:7480", domain: "sbx.example.com"}
	if got, want := withDomain.authority(agent, "sbx_abc"), "10001-sbx_abc.sbx.example.com"; got != want {
		t.Errorf("authority with domain = %q, want %q", got, want)
	}

	noDomain := &dataPlane{proxyAddr: "p:7480"}
	if got, want := noDomain.authority(agent, "sbx_abc"), "10001-sbx_abc"; got != want {
		t.Errorf("authority without domain = %q, want %q", got, want)
	}

	// A user port is addressed the same way, which is what lets one mechanism
	// serve both the agent and a user's server.
	if got, want := noDomain.authority(8000, "sbx_abc"), "8000-sbx_abc"; got != want {
		t.Errorf("user-port authority = %q, want %q", got, want)
	}
}

// stubAgent is a minimal AgentService that echoes the command back as stdout, so
// a test can assert the exec round-tripped over the wire.
type stubAgent struct {
	agentv1connect.UnimplementedAgentServiceHandler
}

func (stubAgent) Exec(_ context.Context, req *connect.Request[commonv1.ExecRequest]) (*connect.Response[commonv1.ExecResponse], error) {
	return connect.NewResponse(&commonv1.ExecResponse{
		Stdout: []byte(strings.Join(req.Msg.GetCmd(), " ")),
	}), nil
}

func TestExecReachesTheAgentOverConnectThroughTheProxyHost(t *testing.T) {
	// This is the crux of the CLI's switch to Connect: no gRPC stack, the same
	// h2c transport the SDK uses, and the sandbox chosen by the Host header the
	// proxy would route on. The fake proxy stands in for bean-proxy: it records
	// the Host it was addressed with and forwards to the stub agent.
	agentPath, agentHandler := agentv1connect.NewAgentServiceHandler(stubAgent{})
	agentMux := http.NewServeMux()
	agentMux.Handle(agentPath, agentHandler)
	agent := httptest.NewUnstartedServer(h2c.NewHandler(agentMux, &http2.Server{}))
	agent.EnableHTTP2 = true
	agent.Start()
	defer agent.Close()

	var gotHost string
	proxy := httptest.NewUnstartedServer(h2c.NewHandler(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHost = r.Host
			// Forward to the agent unchanged; the point under test is the CLI's
			// transport and addressing, not the proxy's own routing table.
			agentMux.ServeHTTP(w, r)
		}), &http2.Server{}))
	proxy.EnableHTTP2 = true
	proxy.Start()
	defer proxy.Close()

	dp, ok := dataPlaneFor(proxy.URL, "sbx.example.com")
	if !ok {
		t.Fatal("dataPlaneFor did not opt in for a set proxy URL")
	}
	res, err := dp.execViaProxy(context.Background(), "sbx_abc", []string{"echo", "hi"})
	if err != nil {
		t.Fatalf("exec over Connect through the proxy: %v", err)
	}
	if got := string(res.Stdout); got != "echo hi" {
		t.Fatalf("stdout = %q, want %q", got, "echo hi")
	}
	// The proxy saw the routing authority as its Host, which is what selects the
	// sandbox and port -- the whole addressing convention, carried over HTTP.
	if want := "10001-sbx_abc.sbx.example.com"; gotHost != want {
		t.Fatalf("proxy Host = %q, want %q", gotHost, want)
	}
}

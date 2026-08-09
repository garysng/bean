package beand

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/garysng/bean/internal/gen/bean/agent/v1"
	"github.com/garysng/bean/internal/gen/bean/agent/v1/agentv1connect"
	commonv1 "github.com/garysng/bean/internal/gen/bean/common/v1"
	"github.com/garysng/bean/internal/sbxtoken"
)

// The whole point of serving the agent over Connect is that one set of handlers
// answers both a gRPC client (noded's control path, unchanged) and an HTTP/JSON
// client (the SDK, no grpcio). These tests stand up the real Connect handler over
// h2c and dial it both ways against the same server, because a unit test that
// only exercises one transport would not prove the claim that makes the whole
// change worth it.

// connectTestServer starts the agent as a Connect handler over h2c, the same way
// cmd/beand serves it, and returns the base URL and an h2c http client.
func connectTestServer(t *testing.T) (string, *http.Client) {
	t.Helper()
	agent := NewServer("test", t.TempDir())
	path, handler := agentv1connect.NewAgentServiceHandler(NewConnectServer(agent))
	mux := http.NewServeMux()
	mux.Handle(path, handler)

	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.EnableHTTP2 = true
	srv.Start()
	t.Cleanup(srv.Close)

	// srv.Client() is wired to reach this test server; it carries the HTTP/2
	// support httptest configures, which is what the Connect JSON client needs.
	return srv.URL, srv.Client()
}

func TestConnectServerAnswersAnHTTPJSONClient(t *testing.T) {
	// This is the SDK's path: a plain HTTP client speaking Connect's JSON codec,
	// no gRPC stack. If this works, the SDK needs only an HTTP client.
	base, httpClient := connectTestServer(t)

	client := agentv1connect.NewAgentServiceClient(httpClient, base)
	resp, err := client.Exec(context.Background(),
		connect.NewRequest(&commonv1.ExecRequest{Cmd: []string{"echo", "hi-json"}}))
	if err != nil {
		t.Fatalf("HTTP/JSON exec: %v", err)
	}
	if got := strings.TrimSpace(string(resp.Msg.GetStdout())); got != "hi-json" {
		t.Fatalf("stdout = %q, want hi-json", got)
	}
}

func TestConnectServerAnswersAGRPCClient(t *testing.T) {
	// This is noded's control path: a real gRPC client. A Connect server accepts
	// it unchanged, which is what lets the control path stay on gRPC while the SDK
	// moves to HTTP -- no second API, no rewrite of noded.
	base, _ := connectTestServer(t)
	addr := strings.TrimPrefix(base, "http://")

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc dial: %v", err)
	}
	defer conn.Close()

	resp, err := agentv1.NewAgentServiceClient(conn).Exec(context.Background(),
		&commonv1.ExecRequest{Cmd: []string{"echo", "hi-grpc"}})
	if err != nil {
		t.Fatalf("gRPC exec against the Connect server: %v", err)
	}
	if got := strings.TrimSpace(string(resp.GetStdout())); got != "hi-grpc" {
		t.Fatalf("stdout = %q, want hi-grpc", got)
	}
}

// connectAuthServer starts the agent with the auth interceptor and a known
// expected hash, so the fail-closed behaviour can be checked over the Connect
// transport -- the token arriving as a header, exactly as the forwarder injects
// it on the data-plane path.
func connectAuthServer(t *testing.T, expectedHash string) (string, *http.Client) {
	t.Helper()
	agent := NewServer("test", t.TempDir())
	auth := &Authenticator{hashes: fakeHashes{hash: expectedHash}}
	path, handler := agentv1connect.NewAgentServiceHandler(
		NewConnectServer(agent), connect.WithInterceptors(auth.Interceptor()))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.EnableHTTP2 = true
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL, srv.Client()
}

func TestConnectAuthAdmitsTheTokenHolderAndDeniesEveryoneElse(t *testing.T) {
	tok, err := sbxtoken.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	base, httpClient := connectAuthServer(t, sbxtoken.Hash(tok))
	client := agentv1connect.NewAgentServiceClient(httpClient, base)

	// The forwarder sets the token as a header; reproduce that on the request.
	good := connect.NewRequest(&commonv1.ExecRequest{Cmd: []string{"echo", "ok"}})
	good.Header().Set(sbxtoken.MDKey, tok)
	if _, err := client.Exec(context.Background(), good); err != nil {
		t.Fatalf("the token holder was refused over Connect: %v", err)
	}

	// No header at all -- the sandbox's own root reaching the agent without the
	// forwarder's injection. Must be denied, fail-closed.
	none := connect.NewRequest(&commonv1.ExecRequest{Cmd: []string{"echo", "leak"}})
	if _, err := client.Exec(context.Background(), none); err == nil {
		t.Fatal("a call with no token was admitted; the data-plane auth is not fail-closed")
	} else if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("no-token call: got code %v, want permission_denied", connect.CodeOf(err))
	}

	// A wrong token, also denied.
	bad := connect.NewRequest(&commonv1.ExecRequest{Cmd: []string{"echo", "leak"}})
	bad.Header().Set(sbxtoken.MDKey, "not-the-token")
	if _, err := client.Exec(context.Background(), bad); err == nil {
		t.Fatal("a wrong token was admitted")
	}
}

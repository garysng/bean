// Data-plane client: sandbox operations that go straight to the agent through
// bean-proxy, rather than relaying through bean-api.
//
// The relay path (bean-api -> noded's control gRPC -> agent) still exists and is
// the fallback. This path dials bean-proxy directly with a gRPC authority of
// "{port}-{sandbox}", which is how the proxy and the node's forwarder route to a
// port inside a guest. For the agent that port is 10001, and the call is
// AgentService/Exec (and the file RPCs) rather than the SandboxService the
// control plane speaks.
//
// The client presents no per-sandbox credential. The forwarder on the node
// injects the agent token (it holds the plaintext; the client never does), and
// the proxy injects its own node token toward the forwarder. We assume the
// request already cleared the platform's outer API-key layer, exactly as the
// design (docs/exec-via-proxy.md) settles it: auth lives in the node's trust
// domain, not in the caller's hands.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentv1 "github.com/garysng/bean/internal/gen/bean/agent/v1"
	commonv1 "github.com/garysng/bean/internal/gen/bean/common/v1"
	"github.com/garysng/bean/internal/node/runtime"
)

// dataPlane holds what the CLI needs to reach a sandbox's agent through the
// proxy: the proxy's dial address and the domain the authority is built against.
type dataPlane struct {
	// proxyAddr is where the gRPC connection is opened (host:port). It is the
	// proxy's own address, reachable from the client.
	proxyAddr string
	// domain is the suffix the authority carries so the proxy routes correctly
	// when it distinguishes sandboxes by DNS label ("{port}-{sandbox}.{domain}").
	// Empty is valid for a dev proxy that routes on the bare "{port}-{sandbox}"
	// label with no suffix.
	domain string
}

// dataPlaneFor derives the data-plane target for a sandbox, or (nil, false) when
// the client is not configured to use one -- in which case the caller falls back
// to the bean-api relay path.
//
// BEAN_PROXY_URL is the opt-in: unset means "use the relay", so a single-node or
// dev setup with no proxy is unaffected. The domain comes from the sandbox record
// (its Domain field, surfaced by the server) so the client never assembles the
// addressing convention itself.
func dataPlaneFor(proxyURL, domain string) (*dataPlane, bool) {
	if proxyURL == "" {
		return nil, false
	}
	// Accept both a bare host:port and a URL with a scheme; the gRPC dial wants
	// the authority-less host:port, so strip any scheme.
	addr := proxyURL
	if i := strings.Index(addr, "://"); i >= 0 {
		addr = addr[i+3:]
	}
	addr = strings.TrimRight(addr, "/")
	return &dataPlane{proxyAddr: addr, domain: domain}, true
}

// authority builds the gRPC :authority for a port on a sandbox. This is the value
// the proxy routes on, identical in shape to the Host header a browser preview
// would send: "{port}-{sandbox}" optionally followed by ".{domain}".
func (d *dataPlane) authority(port int, sandboxID string) string {
	label := fmt.Sprintf("%d-%s", port, sandboxID)
	if d.domain == "" {
		return label
	}
	return label + "." + d.domain
}

// dialAgent opens a gRPC client to a sandbox's agent through the proxy.
//
// The connection is cleartext HTTP/2 (h2c): the proxy terminates no TLS and the
// hop is expected to sit behind the platform's own edge. insecure credentials are
// what select h2c here, and WithAuthority is what makes the proxy route to this
// sandbox's agent port rather than anywhere else.
func (d *dataPlane) dialAgent(sandboxID string) (agentv1.AgentServiceClient, func() error, error) {
	auth := d.authority(runtime.AgentGuestPort, sandboxID)
	conn, err := grpc.NewClient(d.proxyAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithAuthority(auth),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("dial proxy %s (authority %s): %w", d.proxyAddr, auth, err)
	}
	return agentv1.NewAgentServiceClient(conn), conn.Close, nil
}

// execViaProxy runs a command through the data plane and returns the agent's
// response. The context bounds the call; the caller supplies the timeout.
func (d *dataPlane) execViaProxy(ctx context.Context, sandboxID string, cmd []string) (*execResult, error) {
	client, closeConn, err := d.dialAgent(sandboxID)
	if err != nil {
		return nil, err
	}
	defer closeConn()

	resp, err := client.Exec(ctx, &commonv1.ExecRequest{SandboxId: sandboxID, Cmd: cmd})
	if err != nil {
		return nil, err
	}
	return &execResult{
		ExitCode:  int(resp.GetExitCode()),
		Stdout:    resp.GetStdout(),
		Stderr:    resp.GetStderr(),
		Truncated: resp.GetTruncated(),
	}, nil
}

// execResult is the CLI-facing shape of an exec, independent of whether it came
// from the data plane or the relay, so the command body can print one thing.
type execResult struct {
	ExitCode  int
	Stdout    []byte
	Stderr    []byte
	Truncated bool
}

// execViaDataPlane resolves the sandbox's domain from its record, then execs
// through the proxy. It lives on Client so it can reuse the REST client to GET
// the record; the actual exec is the gRPC call in execViaProxy.
//
// If the record carries no domain the proxy still routes on the bare
// "{port}-{sandbox}" label, so an empty domain is not an error -- it just means
// the authority has no suffix.
func (c *Client) execViaDataPlane(dp *dataPlane, id string, cmd []string) (*execResult, error) {
	if err := c.resolveDomain(dp, id); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), c.HTTP.Timeout)
	defer cancel()
	return dp.execViaProxy(ctx, id, cmd)
}

// resolveDomain fills dp.domain from the sandbox record, so the authority is
// built against the domain the server chose rather than a client convention.
func (c *Client) resolveDomain(dp *dataPlane, id string) error {
	var rec struct {
		Sandbox struct {
			Domain string `json:"domain"`
		} `json:"sandbox"`
	}
	if err := c.doJSON("GET", "/v1/sandboxes/"+id, nil, &rec); err != nil {
		return err
	}
	dp.domain = rec.Sandbox.Domain
	return nil
}

// writeFileViaDataPlane streams a file to the sandbox through the proxy. The
// WriteFile RPC is a client stream: one meta frame naming the path, then data
// frames. mkdirs mirrors the REST path's behaviour.
func (c *Client) writeFileViaDataPlane(dp *dataPlane, id, remote string, data []byte) error {
	if err := c.resolveDomain(dp, id); err != nil {
		return err
	}
	client, closeConn, err := dp.dialAgent(id)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := context.WithTimeout(context.Background(), c.HTTP.Timeout)
	defer cancel()

	stream, err := client.WriteFile(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&commonv1.WriteFileFrame{
		Frame: &commonv1.WriteFileFrame_Meta{Meta: &commonv1.WriteFileMeta{
			SandboxId: id, Path: remote, Mkdirs: true,
		}},
	}); err != nil {
		return err
	}
	if err := stream.Send(&commonv1.WriteFileFrame{
		Frame: &commonv1.WriteFileFrame_Data{Data: data},
	}); err != nil {
		return err
	}
	_, err = stream.CloseAndRecv()
	return err
}

// readFileViaDataPlane streams a file out of the sandbox through the proxy into
// w. ReadFile is a server stream of data chunks.
func (c *Client) readFileViaDataPlane(dp *dataPlane, id, remote string, w io.Writer) error {
	if err := c.resolveDomain(dp, id); err != nil {
		return err
	}
	client, closeConn, err := dp.dialAgent(id)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := context.WithTimeout(context.Background(), c.HTTP.Timeout)
	defer cancel()

	stream, err := client.ReadFile(ctx, &commonv1.ReadFileRequest{SandboxId: id, Path: remote})
	if err != nil {
		return err
	}
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, werr := w.Write(chunk.GetData()); werr != nil {
			return werr
		}
	}
}

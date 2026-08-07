package node

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// What reaches the dialer is not always what the caller wrote.
//
// dial.go picks a transport from the address's prefix, and the existing netns tests
// call it directly -- so they prove the parsing works but say nothing about whether
// gRPC hands it the address intact. It does not always: gRPC strips a scheme it
// recognises, and "netns:" looks like one.
//
// This mattered the first time a runtime actually used that transport. The container
// tier's agent address is netns:<path>|<ip>:<port>, and the create failed with
// "dial tcp 172.31.0.2:8111: network is unreachable" -- a plain TCP dial in the host
// namespace, with the prefix gone. The error named the right address and the wrong
// namespace, which reads as a routing problem rather than a parsing one.
func TestDialerReceivesTheAddressTheRuntimeChose(t *testing.T) {
	for _, target := range []string{
		"netns:/var/run/netns/bean-0|172.31.0.2:8111",
		"unix:///run/bean/agent.sock",
		"vsock:/tmp/vm.vsock:10001",
		"127.0.0.1:9999",
	} {
		t.Run(target, func(t *testing.T) {
			seen := make(chan string, 1)
			conn, err := grpc.NewClient("passthrough:///"+target,
				grpc.WithTransportCredentials(insecure.NewCredentials()),
				grpc.WithContextDialer(func(_ context.Context, addr string) (net.Conn, error) {
					select {
					case seen <- addr:
					default:
					}
					// Never succeeds; the point is what the dialer was handed.
					return nil, errDialProbe
				}))
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			defer conn.Close()
			conn.Connect()

			select {
			case addr := <-seen:
				if addr != target {
					t.Errorf("dialer got %q, runtime chose %q -- the transport prefix "+
						"did not survive, so dial.go will pick the wrong transport",
						addr, target)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("dialer was never called")
			}
		})
	}
}

var errDialProbe = &probeErr{}

type probeErr struct{}

func (*probeErr) Error() string { return "probe: not dialling" }

// And the routing itself: given the address intact, the right branch has to be taken.
// Checked by the error, since none of these can connect in a test -- but each branch
// fails in a way that names it.
func TestTransportChosenByPrefix(t *testing.T) {
	tests := []struct {
		target string
		expect string
	}{
		// A netns target that cannot be opened says so, which proves it went to the
		// namespace layer rather than to a plain TCP dial.
		{"netns:/var/run/netns/nope-" + t.Name() + "|172.31.0.2:8111", "netns"},
		{"netns:|172.31.0.2:8111", "malformed"},
		{"netns:/var/run/netns/x|", "malformed"},
		{"", "empty"},
	}
	for _, tc := range tests {
		t.Run(tc.target, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, err := dialAgentAddr(ctx, tc.target)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.expect) {
				t.Errorf("error %q does not mention %q, so a different branch ran",
					err, tc.expect)
			}
		})
	}
}

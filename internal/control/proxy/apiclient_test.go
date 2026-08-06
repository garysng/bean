package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/garysng/bean/internal/node"
)

// fakeAPI stands in for bean-api, counting requests so the cache is observable.
type fakeAPI struct {
	nodeID    string
	fwdAddr   string
	sbxStatus int
	sbxCalls  atomic.Int32
	nodeCalls atomic.Int32
}

func (f *fakeAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sandboxes/", func(w http.ResponseWriter, r *http.Request) {
		f.sbxCalls.Add(1)
		if f.sbxStatus != 0 && f.sbxStatus != http.StatusOK {
			w.WriteHeader(f.sbxStatus)
			return
		}
		fmt.Fprintf(w, `{"sandbox":{"id":"x","state":"RUNNING","nodeId":%q}}`, f.nodeID)
	})
	mux.HandleFunc("/v1/nodes", func(w http.ResponseWriter, _ *http.Request) {
		f.nodeCalls.Add(1)
		fmt.Fprintf(w, `{"nodes":[{"id":%q,"labels":{%q:%q}}]}`,
			f.nodeID, labelSandboxPortAddr, f.fwdAddr)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestTheLabelNameMatchesWhatNodesPublish(t *testing.T) {
	// This package declares the label itself rather than importing the node package,
	// which would pull the microVM runtime into a process that only speaks HTTP. The
	// cost of that choice is a constant in two places, so the two are compared here --
	// a mismatch would present as every node appearing to lack a forwarding address.
	if labelSandboxPortAddr != node.LabelSandboxPortAddr {
		t.Fatalf("proxy uses %q, nodes publish %q; every lookup would report the node "+
			"as unable to serve forwarding", labelSandboxPortAddr,
			node.LabelSandboxPortAddr)
	}
}

func TestResolvesThroughTheAPI(t *testing.T) {
	f := &fakeAPI{nodeID: "node-1", fwdAddr: "10.0.0.7:17450"}
	srv := f.server(t)

	a := NewAPISandboxes(srv.URL, "devkey", time.Minute)
	addr, err := a.NodeAddrFor("sbx_abc")
	if err != nil {
		t.Fatalf("NodeAddrFor: %v", err)
	}
	if addr != "10.0.0.7:17450" {
		t.Fatalf("got %q, want the node's advertised forwarding address", addr)
	}
}

func TestPlacementIsCachedAcrossRequests(t *testing.T) {
	// Without this, every proxied request costs two control-plane round trips, which
	// is most of what moving the data plane off the control plane was for.
	f := &fakeAPI{nodeID: "node-1", fwdAddr: "10.0.0.7:17450"}
	srv := f.server(t)

	a := NewAPISandboxes(srv.URL, "devkey", time.Minute)
	for i := 0; i < 5; i++ {
		if _, err := a.NodeAddrFor("sbx_abc"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := f.sbxCalls.Load(); got != 1 {
		t.Errorf("asked bean-api about the sandbox %d times for 5 requests, want 1", got)
	}
	if got := f.nodeCalls.Load(); got != 1 {
		t.Errorf("fetched the node list %d times for 5 requests, want 1", got)
	}
}

func TestInvalidateForcesAFreshLookup(t *testing.T) {
	// A sandbox that moved must not be retried against a stale node for the whole
	// cache window.
	f := &fakeAPI{nodeID: "node-1", fwdAddr: "10.0.0.7:17450"}
	srv := f.server(t)

	a := NewAPISandboxes(srv.URL, "devkey", time.Minute)
	if _, err := a.NodeAddrFor("sbx_abc"); err != nil {
		t.Fatal(err)
	}
	a.Invalidate("sbx_abc")
	if _, err := a.NodeAddrFor("sbx_abc"); err != nil {
		t.Fatal(err)
	}
	if got := f.sbxCalls.Load(); got != 2 {
		t.Errorf("asked %d times, want 2: the second lookup must not be served from "+
			"the cache after Invalidate", got)
	}
}

func TestAnUnknownSandboxIsReportedAsSuch(t *testing.T) {
	f := &fakeAPI{sbxStatus: http.StatusNotFound}
	srv := f.server(t)

	a := NewAPISandboxes(srv.URL, "devkey", time.Minute)
	_, err := a.NodeAddrFor("sbx_gone")
	if !errors.Is(err, ErrNoSandbox) {
		t.Fatalf("got %v, want ErrNoSandbox so the proxy answers 404", err)
	}
}

func TestAnUnplacedSandboxIsNotAForwardingTarget(t *testing.T) {
	// Known to the control plane but on no node: transient during a create, permanent
	// for one whose placement failed. Either way there is nowhere to forward to.
	f := &fakeAPI{nodeID: ""}
	srv := f.server(t)

	a := NewAPISandboxes(srv.URL, "devkey", time.Minute)
	_, err := a.NodeAddrFor("sbx_pending")
	if !errors.Is(err, ErrNoSandbox) {
		t.Fatalf("got %v, want ErrNoSandbox", err)
	}
}

func TestANodeWithoutForwardingIsDistinguished(t *testing.T) {
	// The remedy is a node flag rather than a corrected id, so it must not look like a
	// missing sandbox.
	f := &fakeAPI{nodeID: "node-1", fwdAddr: ""}
	srv := f.server(t)

	a := NewAPISandboxes(srv.URL, "devkey", time.Minute)
	_, err := a.NodeAddrFor("sbx_abc")
	if !errors.Is(err, ErrNoForwarding) {
		t.Fatalf("got %v, want ErrNoForwarding so the proxy answers 503", err)
	}
}

func TestAnUnreachableControlPlaneIsNotAMissingSandbox(t *testing.T) {
	// The distinction decides whether a caller retries. A 404 for an unreachable
	// control plane would tell them their sandbox is gone.
	a := NewAPISandboxes("http://127.0.0.1:1", "devkey", time.Minute)
	_, err := a.NodeAddrFor("sbx_abc")
	if err == nil {
		t.Fatal("an unreachable control plane resolved successfully")
	}
	if errors.Is(err, ErrNoSandbox) || errors.Is(err, ErrNoForwarding) {
		t.Fatalf("got %v, which the proxy maps to 404 or 503; an unreachable control "+
			"plane is neither", err)
	}
}

func TestTheAPIKeyIsPresented(t *testing.T) {
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			seen = r.Header.Get("Authorization")
			fmt.Fprint(w, `{"sandbox":{"nodeId":"node-1"}}`)
		}))
	defer srv.Close()

	a := NewAPISandboxes(srv.URL, "devkey", time.Minute)
	_, _ = a.NodeAddrFor("sbx_abc")
	if seen != "Bearer devkey" {
		t.Fatalf("bean-api saw Authorization %q, want the proxy's own key", seen)
	}
}

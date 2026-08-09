package cli

import (
	"testing"

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
	// The gRPC dial wants a bare host:port; a scheme left on would make it dial a
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

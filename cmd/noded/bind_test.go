package main

import "testing"

// The forwarding port resolves a sandbox id from a Host header and connects. It
// applies no user-level authorization, so whatever can reach it can reach every
// sandbox on the node -- including the agent's interface, which runs commands as
// root. A public bind is therefore refused at startup rather than documented.
//
// This is tested because the failure is silent: bound to 0.0.0.0 the port works
// perfectly, and nothing surfaces until someone else is on the network.

func TestPublicBindsAreRefused(t *testing.T) {
	for _, addr := range []string{
		"0.0.0.0:9000", // the one a container image or a hasty unit file uses
		":9000",        // same thing, written shorter
		"[::]:9000",
		"203.0.113.10:9000", // a real public address
		"example.com:9000",  // a name, which can resolve differently later
		"garbage",           // unparseable
	} {
		if !isPubliclyRoutable(addr) {
			t.Errorf("%q was treated as private; bound there, the forwarding port "+
				"offers unauthenticated access to every sandbox on this node", addr)
		}
	}
}

func TestPrivateAndLoopbackBindsAreAllowed(t *testing.T) {
	// A private address has to be permitted: that is where bean-proxy reaches this
	// port in a real deployment, and requiring loopback would make multi-node
	// impossible.
	for _, addr := range []string{
		"127.0.0.1:9000",
		"localhost:9000",
		"[::1]:9000",
		"10.0.0.5:9000",
		"172.16.4.9:9000",
		"192.168.1.20:9000",
		"169.254.3.4:9000",
	} {
		if isPubliclyRoutable(addr) {
			t.Errorf("%q was refused as public; this is where a proxy on a private "+
				"network reaches the node, so refusing it breaks multi-node", addr)
		}
	}
}

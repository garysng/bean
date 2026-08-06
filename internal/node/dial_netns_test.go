package node

import (
	"context"
	"strings"
	"testing"
)

// The netns target is a composite address: a namespace path and a host:port that is
// identical in every sandbox. Parsing it wrong is not a visible error -- it is a dial
// into the host namespace, where the address either answers nothing or belongs to a
// different sandbox. So the malformed cases are rejected rather than best-effort.

func TestMalformedNetnsTargetIsRejected(t *testing.T) {
	for _, target := range []string{
		"netns:",                      // nothing at all
		"netns:|172.31.0.2:10001",     // no namespace
		"netns:/var/run/netns/bean-0", // no address
		"netns:/var/run/netns/bean-0|",
	} {
		conn, err := dialAgentAddr(context.Background(), target)
		if err == nil {
			conn.Close()
			t.Errorf("target %q was accepted; a half-parsed netns address dials the "+
				"host namespace, where this port belongs to another sandbox or to "+
				"nothing", target)
			continue
		}
		if !strings.Contains(err.Error(), "malformed netns target") {
			t.Errorf("target %q failed with %v, want a parse error naming the target",
				target, err)
		}
	}
}

func TestWellFormedNetnsTargetReachesTheNamespaceLayer(t *testing.T) {
	// A namespace that does not exist, so this cannot succeed. What it proves is that
	// the target parsed and the failure came from the namespace rather than from the
	// parser -- which is the difference between "dialled the wrong place" and "did
	// not dial".
	_, err := dialAgentAddr(context.Background(),
		"netns:/var/run/netns/does-not-exist-"+t.Name()+"|172.31.0.2:10001")
	if err == nil {
		t.Fatal("dialling a nonexistent namespace succeeded")
	}
	if strings.Contains(err.Error(), "malformed") {
		t.Fatalf("a well-formed target was rejected by the parser: %v", err)
	}
}

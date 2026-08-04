package proxy

import (
	"fmt"

	"github.com/garysng/bean/internal/control/store"
	"github.com/garysng/bean/internal/node"
)

// StoreSandboxes resolves a sandbox to its node's forwarding address using the
// control plane's store.
//
// Read-only, and that is the point rather than an accident: the proxy sits on the
// data path and must not be able to change placement. A second writer to the sandbox
// ledger would be able to disagree with the scheduler about where a sandbox is.
type StoreSandboxes struct {
	Store *store.Store
}

// NodeAddrFor returns the forwarding address of the node holding sandboxID.
//
// Two hops, both of which can fail in ways worth distinguishing. The sandbox record
// says which node; that node's registration labels say where its forwarding port is.
// A node that never advertised one cannot serve this traffic at all, and reporting
// that separately is what lets an operator see a missing flag rather than guess at a
// network problem.
func (s StoreSandboxes) NodeAddrFor(sandboxID string) (string, error) {
	rec, err := s.Store.GetSandbox(sandboxID)
	if err != nil {
		return "", fmt.Errorf("look up sandbox %s: %w", sandboxID, err)
	}
	if rec == nil {
		return "", fmt.Errorf("%w: %s", ErrNoSandbox, sandboxID)
	}
	if rec.NodeID == "" {
		// A sandbox that exists in the ledger but has not been placed. Transient
		// during a create, and permanent for one that failed placement.
		return "", fmt.Errorf("%w: %s is not placed on a node", ErrNoSandbox, sandboxID)
	}

	n, err := s.Store.GetNode(rec.NodeID)
	if err != nil {
		return "", fmt.Errorf("look up node %s: %w", rec.NodeID, err)
	}
	if n == nil {
		// The sandbox names a node the control plane has forgotten. Reported as a
		// missing sandbox rather than a server fault: from the caller's side the thing
		// they asked for is unreachable, and the node's absence is not their problem.
		return "", fmt.Errorf("%w: %s is on unknown node %s",
			ErrNoSandbox, sandboxID, rec.NodeID)
	}

	addr := n.Labels[node.LabelSandboxPortAddr]
	if addr == "" {
		return "", fmt.Errorf("%w: node %s holding %s was started without "+
			"--sandbox-port-listen", ErrNoForwarding, rec.NodeID, sandboxID)
	}
	return addr, nil
}

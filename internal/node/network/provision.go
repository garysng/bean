package network

import (
	"fmt"
)

// This file joins the two halves that already exist: the index pool in alloc.go
// and the host commands in setup_linux.go. Neither is useful alone, and the
// ordering between them is the part worth having in one tested place rather than
// spelled out at every call site.
//
// The orderings that matter, and what going wrong costs:
//
//   - Reserve before Setup. Setup needs the addresses, and the addresses are
//     derived from the index.
//   - Release when Setup fails. Otherwise a node quietly loses one slot per
//     failed create, and the only symptom is that a host which used to hold N
//     sandboxes eventually refuses at fewer.
//   - Teardown before Release. Releasing first would let the next Reserve hand
//     the index out while the old namespace still stands, and two sandboxes with
//     the same veth addresses is the collision this whole module is arranged to
//     prevent.

// Host is the host-side half: it both reports what namespaces exist and builds
// or removes one sandbox's networking.
//
// It is one interface rather than two because LinuxSetup implements all of it,
// and because the pool's authority argument depends on the listing coming from
// the same place the namespaces are created. A provisioner that created
// namespaces through one object and listed them through another could be wired
// up with the two disagreeing, which would look correct and allocate
// duplicates.
type Host interface {
	Lister
	// RouteLister is here so the startup collision check runs against the same
	// host these namespaces are created on. Reading routes from somewhere other
	// than the machine about to carry the traffic would check nothing.
	RouteLister
	// Setup builds the namespace, links and rules for one layout. It is
	// responsible for cleaning up its own partial work on failure.
	Setup(l *Layout) error
	// Teardown removes what Setup built. It must be idempotent: it runs on the
	// destroy path and again on a retried destroy.
	Teardown(l *Layout) error
}

// Provisioner gives a sandbox its networking and takes it away again.
type Provisioner struct {
	alloc *Allocator
	host  Host
}

// NewProvisioner wires a pool over guestCIDR to a host.
//
// guestCIDR is validated here rather than at the first create. A node
// configured with a subnet the layout cannot use should fail to start on the
// machine somebody typed it on, not accept sandboxes and refuse each one for a
// reason that looks like a per-sandbox fault.
func NewProvisioner(guestCIDR string, host Host) (*Provisioner, error) {
	if host == nil {
		return nil, fmt.Errorf("network: a host is required to provision")
	}
	if _, err := LayoutFor(0, guestCIDR); err != nil {
		return nil, err
	}
	return &Provisioner{alloc: NewAllocator(guestCIDR, host), host: host}, nil
}

// Provision reserves a slot and builds the networking on it.
//
// A returned error means nothing was left behind: the index is back in the pool
// and Setup has undone its own partial work. That matters because the caller's
// response to a failure here is to fail the create, and a create that fails
// while holding an index would make the node's capacity shrink over time
// without anything reporting it.
func (p *Provisioner) Provision(sandboxID string) (*Layout, error) {
	layout, err := p.alloc.Reserve(sandboxID)
	if err != nil {
		return nil, err
	}
	if err := p.host.Setup(layout); err != nil {
		// The index is returned because Setup already removed what it built. Keeping
		// it would leak a slot per failed create -- the loop-device shape of bug
		// (GitHub #16) with a different resource.
		p.alloc.Release(sandboxID)
		return nil, err
	}
	return layout, nil
}

// Deprovision removes a sandbox's networking and returns its slot.
//
// A sandbox this process never assigned an index to is not touched. After a
// restart the pool knows nothing about namespaces created by the previous
// noded, and those may still be serving running sandboxes -- deleting one
// because a destroy arrived for an id this process does not recognise would take
// the network out from under a live guest. Deciding which of those namespaces is
// genuinely an orphan needs the control plane's expected set, which is host
// resource reconciliation's job (internal/node/reclaim, GitHub #17) and
// deliberately not done here. See docs/network.md section 3.
func (p *Provisioner) Deprovision(sandboxID string) error {
	layout, ok := p.alloc.LayoutOf(sandboxID)
	if !ok {
		return nil
	}
	err := p.host.Teardown(layout)
	// Released even when teardown failed, and this is safe rather than sloppy: a
	// namespace still standing on the host is seen by the next Reserve, which
	// adopts the index instead of handing it out. Holding the slot in memory as
	// well would mean a teardown that half worked cost the node a slot until it
	// restarted, while the host is already the authority that prevents reuse.
	p.alloc.Release(sandboxID)
	if err != nil {
		return fmt.Errorf("network: deprovision %s: %w", sandboxID, err)
	}
	return nil
}

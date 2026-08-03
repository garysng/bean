package network

import (
	"fmt"
	"strings"
)

// This file is the packet-filter and NAT policy for one sandbox, expressed as
// data rather than as calls to iptables. The execution lives in setup_linux.go.
//
// The split exists because the part that is easy to get wrong is not running the
// command, it is the *order* of the rules and the *argument list* used to remove
// them again. Both are decidable without a kernel, so both are tested on any
// machine rather than only on a Linux host with root.
//
// See docs/network.md sections 5 and 5a for the design and for what the filter
// is protecting against.

// deniedDestinations are the destination ranges a sandbox may not reach.
//
// Egress to the public internet is a hard requirement (a task cannot run without
// `pip install`), and the two MASQUERADE rules that grant it also grant, as an
// unavoidable side effect of being able to route off the host, reachability of
// the node's own internal network and the cloud metadata service. Denying those
// is therefore part of granting egress, not a later hardening pass:
// architecture.md, security-and-startup.md A4 and noded-design.md section 5 all
// promise they are denied by default.
//
// 169.254.0.0/16 is listed rather than the single 169.254.169.254 address because
// the whole link-local range has no legitimate use from inside a sandbox, and
// naming only the metadata address invites a variant address being reachable.
//
// IPv4 only, deliberately. The guest is given no IPv6 address anywhere in this
// package, so the IPv6 metadata address (fd00:ec2::254) has no route out of the
// namespace. If a guest ever gains an IPv6 address, this list stops being
// sufficient and the v6 half has to be written.
var deniedDestinations = []string{
	"169.254.0.0/16", // cloud metadata and link-local
	"10.0.0.0/8",     // RFC1918
	"172.16.0.0/12",
	"192.168.0.0/16",
}

// Op is how a rule is placed in, or taken out of, a chain.
//
// It is a distinct value rather than a raw string in the call site because insert
// and append are one character apart and produce chains that behave completely
// differently: a DROP appended after an existing ACCEPT never matches. Making the
// choice a named value means a test can assert on it, and a review can see it.
type Op string

const (
	// OpInsert places a rule at the head of its chain, ahead of everything already
	// there. Every DROP uses this. On a host running Docker the FORWARD chain
	// already carries ACCEPT rules, and a DROP appended after them is dead.
	OpInsert Op = "insert"
	// OpAppend places a rule at the tail. Used only for the ACCEPT that permits
	// what the DROPs have not denied, which must be last by construction.
	OpAppend Op = "append"
	// OpDelete removes a rule. Teardown is nothing but deletes.
	OpDelete Op = "delete"
)

// Scope says which network namespace a rule is applied in.
//
// This distinction is the whole correctness argument for the filter, so it is a
// field rather than something implied by which function built the rule. See
// SetupPlan.
type Scope string

const (
	// ScopeNetns is inside the sandbox's own namespace, where a forwarded packet
	// still carries the guest's source address.
	ScopeNetns Scope = "netns"
	// ScopeHost is the host namespace, where that same packet has already been
	// translated to the veth link address.
	ScopeHost Scope = "host"
)

// Rule is one iptables rule, and the unit that teardown reverses.
//
// Match holds the whole match-and-target argument list and nothing about
// placement. That is what makes deletion safe: Delete reuses this exact slice, so
// the removal argument list cannot drift from the one used to install it. A
// mismatch there would leave the rule behind on a shared host forever, and it is
// invisible in review because both call sites look plausible.
type Rule struct {
	Scope Scope
	Table string
	Chain string
	Op    Op
	Match []string
}

// Args renders the full iptables argument list.
//
// Insert is always at position 1. A numbered position further down would be a
// guess about the contents of a chain this process does not own.
func (r Rule) Args() []string {
	args := []string{"-t", r.Table}
	switch r.Op {
	case OpInsert:
		args = append(args, "-I", r.Chain, "1")
	case OpAppend:
		args = append(args, "-A", r.Chain)
	case OpDelete:
		args = append(args, "-D", r.Chain)
	default:
		// Unreachable via the constructors below. Rendering something invalid is
		// better than rendering something that silently means "append".
		args = append(args, "--unknown-op-"+string(r.Op), r.Chain)
	}
	return append(args, r.Match...)
}

// Delete returns the rule that removes r, with the identical match arguments.
//
// Never a chain flush. The host's nat table carries Docker's MASQUERADE rules and
// its filter table carries Docker's FORWARD rules; -F in either would take down
// every container on the machine, and this platform's premise is sharing a host
// with other workloads.
func (r Rule) Delete() Rule {
	r.Op = OpDelete
	return r
}

// String renders a rule for diagnostics.
func (r Rule) String() string {
	return fmt.Sprintf("%s: iptables %s", r.Scope, strings.Join(r.Args(), " "))
}

// SetupPlan returns every rule for one sandbox, in the order it must be applied.
//
// The ordering of the two scopes is the part worth reading carefully, because it
// is what decides whether the filter works at all.
//
// A packet leaving the guest is seen twice, and its source address differs
// between the two:
//
//	in the namespace   src=172.31.0.2   (the guest)      -> FORWARD, then POSTROUTING masquerades it
//	on the host        src=10.a.b.2     (the veth link)  -> FORWARD, then POSTROUTING masquerades it again
//
// Netfilter traverses FORWARD strictly before POSTROUTING, so inside the
// namespace the source is still the guest subnet when the filter runs. That is
// the only place a rule matching -s <guest subnet> can ever match: by the time
// the packet reaches the host, the namespace's MASQUERADE has rewritten the
// source to the link address, and a host rule matching the guest subnet would be
// a rule that is never evaluated against anything. It would look correct in the
// chain and deny nothing.
//
// The mirror of that mistake is the one that fails loudly: the veth link subnet
// lives inside 10.0.0.0/8, which these rules deny. A DROP that matched the
// post-translation source would deny every packet the guest sends, including the
// internet egress this feature exists to provide. It does not, because the denied
// ranges are matched on the *destination* only, and a route to a next hop in
// 10/8 does not rewrite a packet's destination. Internet-bound traffic keeps
// dst=8.8.8.8 in both chains and matches no DROP in either.
//
// The host rules are therefore not a copy of the namespace rules; they match the
// link subnet, which is what the host actually sees. They are defence in depth
// and they are what isolates one sandbox from another's link.
func SetupPlan(l *Layout, uplink string) ([]Rule, error) {
	if l == nil {
		return nil, fmt.Errorf("network: no layout")
	}
	if uplink == "" {
		// Guessing an interface here would produce a MASQUERADE rule pointing at the
		// wrong link, which presents as "egress works on some nodes".
		return nil, fmt.Errorf("network: uplink interface required to build NAT rules")
	}
	guest := l.GuestSubnetCIDR()
	link := l.LinkCIDR()

	var rules []Rule

	// --- inside the namespace -------------------------------------------------
	//
	// Denials first. Applied before the ACCEPT below purely for readability; the
	// resulting order is guaranteed by OpInsert regardless of application order.
	for _, dst := range deniedDestinations {
		rules = append(rules, Rule{
			Scope: ScopeNetns, Table: "filter", Chain: "FORWARD", Op: OpInsert,
			Match: []string{"-s", guest, "-d", dst, "-j", "DROP"},
		})
	}
	// An explicit ACCEPT so the outcome does not depend on the namespace's default
	// policy, which a future change to how namespaces are created could alter
	// without anyone connecting it to sandbox egress. It is appended, so it is
	// always below the DROPs.
	rules = append(rules, Rule{
		Scope: ScopeNetns, Table: "filter", Chain: "FORWARD", Op: OpAppend,
		Match: []string{"-s", guest, "-j", "ACCEPT"},
	})
	// Guest subnet -> veth link. -o is pinned to this sandbox's own veth so the
	// rule cannot translate anything but this sandbox's traffic.
	rules = append(rules, Rule{
		Scope: ScopeNetns, Table: "nat", Chain: "POSTROUTING", Op: OpAppend,
		Match: []string{"-s", guest, "-o", l.NetnsVeth, "-j", "MASQUERADE"},
	})

	// --- on the host ----------------------------------------------------------
	//
	// The host's ACCEPTs come first in *application* order, which puts them behind
	// the DROPs in the resulting chain. Both use insert, and an insert goes to the
	// head, so whichever is applied last ends up on top. Applying the DROPs first
	// would leave the ACCEPT above them and the denied ranges reachable -- the same
	// bug as appending, reached by a different route, and the reason this ordering
	// has a test of its own rather than a comment.
	//
	// They are inserted rather than appended because Docker sets the FORWARD policy
	// to DROP and may hold its own DROP rules; an appended ACCEPT behind those
	// leaves the sandbox with no egress at all on the very hosts this platform is
	// designed to share.
	rules = append(rules,
		Rule{
			Scope: ScopeHost, Table: "filter", Chain: "FORWARD", Op: OpInsert,
			Match: []string{"-s", link, "-j", "ACCEPT"},
		},
		Rule{
			Scope: ScopeHost, Table: "filter", Chain: "FORWARD", Op: OpInsert,
			Match: []string{"-d", link, "-j", "ACCEPT"},
		},
	)
	// Same denials as in the namespace, matched on the source the host actually
	// sees. Inserted last so they sit ahead of everything above, including the
	// ACCEPTs just added and whatever Docker already had.
	for _, dst := range deniedDestinations {
		rules = append(rules, Rule{
			Scope: ScopeHost, Table: "filter", Chain: "FORWARD", Op: OpInsert,
			Match: []string{"-s", link, "-d", dst, "-j", "DROP"},
		})
	}
	// Link subnet -> uplink. Matched on this sandbox's /30 only, never on a wider
	// range, so the delete can name exactly one rule.
	rules = append(rules, Rule{
		Scope: ScopeHost, Table: "nat", Chain: "POSTROUTING", Op: OpAppend,
		Match: []string{"-s", link, "-o", uplink, "-j", "MASQUERADE"},
	})
	return rules, nil
}

// TeardownPlan returns the deletes that reverse SetupPlan.
//
// Reversed order so a rule is removed before anything it was inserted ahead of,
// and derived from the same builder so a rule added to setup cannot be forgotten
// here. Forgetting one leaks a filter rule per sandbox into the host's FORWARD
// chain, which is unbounded growth on a long-lived node.
func TeardownPlan(l *Layout, uplink string) ([]Rule, error) {
	setup, err := SetupPlan(l, uplink)
	if err != nil {
		return nil, err
	}
	out := make([]Rule, 0, len(setup))
	for i := len(setup) - 1; i >= 0; i-- {
		out = append(out, setup[i].Delete())
	}
	return out, nil
}

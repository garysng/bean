package network

import (
	"strings"
	"testing"
)

const testUplink = "eth0"

func planFor(t *testing.T, index int) (*Layout, []Rule) {
	t.Helper()
	l, err := LayoutFor(index, "172.31.0.0/30")
	if err != nil {
		t.Fatal(err)
	}
	rules, err := SetupPlan(l, testUplink)
	if err != nil {
		t.Fatal(err)
	}
	return l, rules
}

// argsOf renders a rule's command line for substring assertions.
func argsOf(r Rule) string { return strings.Join(r.Args(), " ") }

func TestEveryDeniedRangeIsCovered(t *testing.T) {
	// The promise in architecture.md, security-and-startup.md A4 and
	// noded-design.md section 5 is that metadata and the node's internal network
	// are denied by default. Egress works without these rules, so nothing else in
	// the test suite would notice their absence.
	l, rules := planFor(t, 7)
	for _, dst := range []string{
		"169.254.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
	} {
		found := false
		for _, r := range rules {
			if r.Scope != ScopeNetns || r.Chain != "FORWARD" {
				continue
			}
			a := argsOf(r)
			if strings.Contains(a, "-d "+dst) && strings.Contains(a, "-j DROP") &&
				strings.Contains(a, "-s "+l.GuestSubnetCIDR()) {
				found = true
			}
		}
		if !found {
			t.Errorf("no DROP for destination %s from the guest subnet; a sandbox "+
				"could reach it, which three documents promise it cannot", dst)
		}
	}
}

// TestDropsAreInsertedNotAppended is the test the feature depends on.
//
// An appended DROP and an inserted DROP are one character apart in the source and
// produce chains that behave completely differently: a host running Docker has
// ACCEPT rules in FORWARD already, and a DROP behind them is never evaluated. The
// rule would be present, `iptables -S` would list it, and metadata would still be
// reachable.
//
// So this asserts on the placement, not on the rule existing. Changing OpInsert to
// OpAppend in SetupPlan must turn this red.
func TestDropsAreInsertedNotAppended(t *testing.T) {
	_, rules := planFor(t, 3)
	seen := 0
	for _, r := range rules {
		if !strings.Contains(argsOf(r), "-j DROP") {
			continue
		}
		seen++
		if r.Op != OpInsert {
			t.Errorf("DROP rule is %s, not %s: %s\nOn a host running Docker the "+
				"FORWARD chain already accepts, so an appended DROP never matches "+
				"and the sandbox reaches metadata anyway", r.Op, OpInsert, r)
		}
		// Assert on the rendered flag too. A future Op whose rendering is wrong
		// would satisfy the check above while producing an appending command line.
		args := r.Args()
		pos := -1
		for i, a := range args {
			if a == "-I" {
				pos = i
			}
			if a == "-A" {
				t.Errorf("DROP rule renders with -A (append): %s", r)
			}
		}
		if pos < 0 {
			t.Errorf("DROP rule does not render with -I (insert): %s", r)
		} else if pos+2 >= len(args) || args[pos+2] != "1" {
			t.Errorf("DROP inserted somewhere other than position 1: %s\nA numbered "+
				"position further down would be a guess about a chain this process "+
				"does not own", r)
		}
	}
	if seen == 0 {
		t.Fatal("no DROP rules in the plan at all")
	}
}

// TestDropsSitAheadOfTheAcceptInTheSameChain checks the resulting chain order
// rather than the flags, which is what actually decides the outcome.
//
// The plan is replayed the way iptables would apply it: an insert goes to the
// front, an append to the back. Every DROP must end up ahead of the ACCEPT that
// permits the rest of the guest's egress.
func TestDropsSitAheadOfTheAcceptInTheSameChain(t *testing.T) {
	_, rules := planFor(t, 11)
	for _, scope := range []Scope{ScopeNetns, ScopeHost} {
		var chain []Rule
		for _, r := range rules {
			if r.Scope != scope || r.Table != "filter" || r.Chain != "FORWARD" {
				continue
			}
			switch r.Op {
			case OpInsert:
				chain = append([]Rule{r}, chain...)
			case OpAppend:
				chain = append(chain, r)
			}
		}
		lastDrop, firstAccept := -1, -1
		for i, r := range chain {
			a := argsOf(r)
			if strings.Contains(a, "-j DROP") {
				lastDrop = i
			}
			if strings.Contains(a, "-j ACCEPT") && firstAccept < 0 {
				firstAccept = i
			}
		}
		if lastDrop < 0 {
			t.Errorf("%s: no DROP in the resulting FORWARD chain", scope)
			continue
		}
		if firstAccept >= 0 && lastDrop > firstAccept {
			t.Errorf("%s: a DROP lands at position %d, behind the ACCEPT at %d; "+
				"packets to the denied ranges are accepted before the DROP is "+
				"reached", scope, lastDrop, firstAccept)
		}
	}
}

// TestDropCannotMatchPostMasqueradeTraffic covers the mirror mistake, which breaks
// all egress rather than only the denied destinations.
//
// The veth link subnet is inside 10.0.0.0/8, one of the denied ranges. Two things
// must hold for that not to break everything:
//
//  1. No rule matches a denied range as a *source*. Matching on destination only
//     is what keeps internet-bound traffic (dst=8.8.8.8) out of every DROP, in
//     both chains, regardless of the next hop's address.
//  2. No namespace rule matches -s <link subnet>. Inside the namespace the packet
//     still carries the guest's address; the link address only appears after that
//     namespace's MASQUERADE, in the host chain.
func TestDropCannotMatchPostMasqueradeTraffic(t *testing.T) {
	l, rules := planFor(t, 5)
	link := l.LinkCIDR()

	if !strings.HasPrefix(link, "10.") {
		t.Fatalf("link subnet %s is no longer inside 10/8; this test encodes the "+
			"overlap between the link addressing and the denied ranges, and the "+
			"reasoning needs revisiting", link)
	}

	for _, r := range rules {
		args := r.Args()
		for i, a := range args {
			if a != "-s" || i+1 >= len(args) {
				continue
			}
			src := args[i+1]
			for _, denied := range deniedDestinations {
				if src == denied {
					t.Errorf("rule matches a denied range as a source: %s\nThe veth "+
						"link is inside 10/8, so a source match would drop all egress, "+
						"not just the denied destinations", r)
				}
			}
			if r.Scope == ScopeNetns && src == link {
				t.Errorf("namespace rule matches the link subnet as a source: %s\n"+
					"Inside the namespace the packet still has the guest's source "+
					"address; the link address only exists after this namespace's "+
					"MASQUERADE, which runs in POSTROUTING after FORWARD", r)
			}
		}
	}

	// And the converse: a rule matching the guest subnet is only meaningful in the
	// namespace. On the host that source no longer exists, so such a rule would sit
	// in the chain and be evaluated against nothing.
	for _, r := range rules {
		if r.Scope != ScopeHost {
			continue
		}
		if strings.Contains(argsOf(r), "-s "+l.GuestSubnetCIDR()) {
			t.Errorf("host rule matches the guest subnet as a source: %s\nBy the time "+
				"a packet reaches the host chain the namespace MASQUERADE has "+
				"rewritten the source to %s, so this rule can never match", r, link)
		}
	}
}

// TestTeardownRemovesEveryRuleWithIdenticalArguments is the leak check.
//
// A rule installed and not removed accumulates in the host's FORWARD chain for the
// life of the node. Deletion has to name the rule with the exact match arguments
// that installed it, or iptables removes nothing and reports nothing useful.
func TestTeardownRemovesEveryRuleWithIdenticalArguments(t *testing.T) {
	l, setup := planFor(t, 9)
	teardown, err := TeardownPlan(l, testUplink)
	if err != nil {
		t.Fatal(err)
	}
	if len(teardown) != len(setup) {
		t.Fatalf("teardown has %d rules for %d installed; a rule that is installed "+
			"and never removed leaks into the host chain permanently",
			len(teardown), len(setup))
	}
	for _, s := range setup {
		found := false
		for _, d := range teardown {
			if d.Scope != s.Scope || d.Table != s.Table || d.Chain != s.Chain {
				continue
			}
			if strings.Join(d.Match, " ") == strings.Join(s.Match, " ") {
				found = true
				if d.Op != OpDelete {
					t.Errorf("teardown rule is not a delete: %s", d)
				}
			}
		}
		if !found {
			t.Errorf("no teardown entry with the identical match list for: %s", s)
		}
	}
}

// TestNothingFlushesAChain guards the catastrophic case.
//
// The host's nat table carries Docker's MASQUERADE rules and its filter table
// carries Docker's FORWARD rules. A flush in either takes down every container on
// a machine this platform is designed to share. This covers the DROP rules
// automatically because it walks the whole plan rather than a list of rules
// someone has to remember to extend.
func TestNothingFlushesAChain(t *testing.T) {
	l, setup := planFor(t, 2)
	teardown, err := TeardownPlan(l, testUplink)
	if err != nil {
		t.Fatal(err)
	}
	all := append(append([]Rule{}, setup...), teardown...)
	if len(all) == 0 {
		t.Fatal("empty plan")
	}
	for _, r := range all {
		for _, a := range r.Args() {
			switch a {
			case "-F", "--flush", "-X", "--delete-chain", "-Z", "--zero", "-P", "--policy":
				t.Errorf("plan contains %q: %s\nFlushing or repolicing a shared chain "+
					"removes Docker's rules along with ours", a, r)
			}
		}
	}
	// Every deletion must be by rule specification.
	for _, r := range teardown {
		if r.Op != OpDelete {
			t.Errorf("teardown entry is not a delete: %s", r)
		}
		if len(r.Match) == 0 {
			t.Errorf("delete with no match arguments would be ambiguous: %s", r)
		}
	}
}

func TestNatRulesMatchOnlyThisSandbox(t *testing.T) {
	// A MASQUERADE matching wider than one /30 would translate another sandbox's
	// traffic, and the delete could not name a single rule.
	l, rules := planFor(t, 4)
	nat := 0
	for _, r := range rules {
		if r.Table != "nat" {
			continue
		}
		nat++
		a := argsOf(r)
		if !strings.Contains(a, "-s "+l.GuestSubnetCIDR()) &&
			!strings.Contains(a, "-s "+l.LinkCIDR()) {
			t.Errorf("nat rule is not scoped to this sandbox: %s", r)
		}
	}
	if nat != 2 {
		t.Errorf("expected the two MASQUERADE layers, got %d nat rules", nat)
	}
}

func TestSetupPlanRefusesWithoutAnUplink(t *testing.T) {
	// Guessing the interface produces a MASQUERADE pointing at the wrong link,
	// which presents as egress working on some nodes and not others.
	l, err := LayoutFor(0, "172.31.0.0/30")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SetupPlan(l, ""); err == nil {
		t.Error("SetupPlan accepted an empty uplink")
	}
}

func TestPlanHasNoIPv6Rules(t *testing.T) {
	// The v4 rules say nothing about fd00:ec2::254. That is sound only while the
	// guest has no IPv6 address at all, which is the current state. If an address
	// is ever configured, this package needs the v6 half.
	_, rules := planFor(t, 1)
	for _, r := range rules {
		for _, a := range r.Args() {
			if strings.Contains(a, "::") {
				t.Errorf("IPv6 address in a plan built for IPv4 only: %s", r)
			}
		}
	}
}

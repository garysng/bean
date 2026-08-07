package network

import (
	"net"
	"strings"
	"testing"
)

// Batching changes how rules reach the kernel, not which rules or in what order. The
// failures worth catching here are the ones that leave a working-looking sandbox:
// a script missing --noflush wipes Docker's rules, and a reordered one leaves the
// egress policy installed but ineffective.

func TestBatchesSplitByScopeAndNothingElse(t *testing.T) {
	rules := []Rule{
		{Scope: ScopeNetns, Table: "filter", Chain: "FORWARD", Op: OpInsert, Match: []string{"-s", "a"}},
		{Scope: ScopeNetns, Table: "nat", Chain: "POSTROUTING", Op: OpAppend, Match: []string{"-s", "b"}},
		{Scope: ScopeHost, Table: "filter", Chain: "FORWARD", Op: OpInsert, Match: []string{"-s", "c"}},
		{Scope: ScopeHost, Table: "nat", Chain: "POSTROUTING", Op: OpAppend, Match: []string{"-s", "d"}},
	}
	batches := batchRules(rules)

	// Two batches, not four: one restore script carries several tables, and splitting
	// per table would give back the lock acquisitions this exists to remove.
	if len(batches) != 2 {
		t.Fatalf("got %d batches, want 2 (one per scope)", len(batches))
	}
	if batches[0].Scope != ScopeNetns || batches[1].Scope != ScopeHost {
		t.Errorf("batch scopes are %s then %s, want netns then host",
			batches[0].Scope, batches[1].Scope)
	}
	// Scope must never be mixed inside a batch: a host rule applied in a namespace,
	// or the reverse, is the failure iptArgs was shaped to prevent.
	for i, b := range batches {
		for _, r := range b.Rules {
			if r.Scope != b.Scope {
				t.Errorf("batch %d has scope %s but contains a %s rule", i, b.Scope, r.Scope)
			}
		}
	}
}

// Every rule has to survive the grouping. A batch that silently dropped one would
// produce a sandbox missing a DROP -- reachable where it should not be, with nothing
// failing.
func TestBatchingKeepsEveryRuleAndItsOrder(t *testing.T) {
	// Built inline rather than through layoutForTest, which lives in a linux-only test
	// file. The batching is pure string work, so tying its test to linux would stop a
	// developer on a Mac from running the part most likely to be got wrong.
	rules, err := SetupPlan(planLayoutForTest(), "eth0")
	if err != nil {
		t.Fatal(err)
	}

	var flat []Rule
	for _, b := range batchRules(rules) {
		flat = append(flat, b.Rules...)
	}
	if len(flat) != len(rules) {
		t.Fatalf("batching turned %d rules into %d", len(rules), len(flat))
	}
	// Order is load bearing: -I inserts at position 1, so two inserts land in the
	// chain in reverse, and the plan's order already accounts for that.
	for i := range rules {
		if flat[i].String() != rules[i].String() {
			t.Errorf("rule %d changed: %s became %s", i, rules[i], flat[i])
		}
	}
}

func TestRestoreScriptShape(t *testing.T) {
	b := restoreBatch{
		Scope: ScopeHost,
		Rules: []Rule{
			{Scope: ScopeHost, Table: "filter", Chain: "FORWARD", Op: OpInsert,
				Match: []string{"-s", "10.0.0.0/30", "-d", "10.0.0.0/8", "-j", "DROP"}},
			{Scope: ScopeHost, Table: "filter", Chain: "FORWARD", Op: OpAppend,
				Match: []string{"-s", "10.0.0.0/30", "-j", "ACCEPT"}},
			{Scope: ScopeHost, Table: "nat", Chain: "POSTROUTING", Op: OpAppend,
				Match: []string{"-s", "10.0.0.0/30", "-o", "eth0", "-j", "MASQUERADE"}},
		},
	}
	script := restoreScript(b)

	// One section per table, each committed. A missing COMMIT applies nothing while
	// iptables-restore still exits zero on some versions.
	if got := strings.Count(script, "COMMIT"); got != 2 {
		t.Errorf("%d COMMIT lines for 2 tables:\n%s", got, script)
	}
	for _, want := range []string{"*filter", "*nat"} {
		if !strings.Contains(script, want) {
			t.Errorf("no %s section:\n%s", want, script)
		}
	}

	// The table belongs in the header, not on the rule line: `-t filter` inside a
	// *filter section is rejected by iptables-restore.
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(line, "-") && strings.Contains(line, "-t ") {
			t.Errorf("rule line still carries -t, which restore rejects: %q", line)
		}
	}

	// filter before nat, because that is the order the rules came in. Deterministic
	// output matters for reading a diff of two failures.
	if strings.Index(script, "*filter") > strings.Index(script, "*nat") {
		t.Errorf("table order does not follow the rules:\n%s", script)
	}

	// Position semantics survive: the DROP must still be an insert at 1, or it lands
	// behind the ACCEPT and never matches.
	if !strings.Contains(script, "-I FORWARD 1 -s 10.0.0.0/30 -d 10.0.0.0/8 -j DROP") {
		t.Errorf("insert position lost:\n%s", script)
	}
}

// The single most damaging thing this change could get wrong. Without --noflush,
// iptables-restore replaces the table, and the host's filter and nat tables carry
// Docker's rules -- every container on the machine would lose its networking.
func TestRestoreAlwaysPassesNoflush(t *testing.T) {
	for _, scope := range []Scope{ScopeHost, ScopeNetns} {
		name, args := restoreArgs(scope, "bean-0")
		line := name + " " + strings.Join(args, " ")
		if !strings.Contains(line, "--noflush") {
			t.Errorf("%s: no --noflush, which replaces the table and takes Docker's "+
				"rules with it: %s", scope, line)
		}
		if !strings.Contains(line, "-w ") {
			t.Errorf("%s: no -w, so a contended lock fails instead of waiting: %s", scope, line)
		}
		// A netns batch has to run inside the namespace; a host batch must not.
		switch scope {
		case ScopeNetns:
			if !strings.HasPrefix(line, "ip netns exec bean-0 iptables-restore") {
				t.Errorf("netns batch does not enter the namespace: %s", line)
			}
		case ScopeHost:
			if strings.Contains(line, "netns") {
				t.Errorf("host batch entered a namespace: %s", line)
			}
		}
	}
}

// Both paths render rules through Rule.Args, so a change to one cannot silently
// diverge from the other -- which is what keeps the -D symmetry teardown relies on.
func TestRestoreLineMatchesArgsMinusTable(t *testing.T) {
	r := Rule{Scope: ScopeHost, Table: "filter", Chain: "FORWARD", Op: OpInsert,
		Match: []string{"-s", "10.0.0.0/30", "-j", "DROP"}}
	args := r.Args()
	if args[0] != "-t" || args[1] != "filter" {
		t.Fatalf("Args no longer starts with -t <table>: %v -- restoreLine strips two "+
			"elements on that assumption", args)
	}
	if got, want := restoreLine(r), strings.Join(args[2:], " "); got != want {
		t.Errorf("restoreLine = %q, want %q", got, want)
	}
}

// planLayoutForTest is a layout with every field SetupPlan reads, built without the
// linux-only helper so the batching tests run on any platform.
func planLayoutForTest() *Layout {
	_, guest, _ := net.ParseCIDR("172.31.0.0/30")
	_, link, _ := net.ParseCIDR("10.0.0.0/30")
	return &Layout{
		Index:        0,
		Netns:        "bean-0",
		TapName:      "beantap0",
		GuestIP:      net.IPv4(172, 31, 0, 2),
		GuestGateway: net.IPv4(172, 31, 0, 1),
		GuestSubnet:  guest,
		HostVeth:     "bnh0",
		NetnsVeth:    "bnp0",
		HostLinkIP:   net.IPv4(10, 0, 0, 1),
		NetnsLinkIP:  net.IPv4(10, 0, 0, 2),
		LinkSubnet:   link,
	}
}

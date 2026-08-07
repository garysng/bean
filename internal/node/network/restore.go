package network

import (
	"fmt"
	"strings"
)

// Applying rules one iptables call at a time takes the xtables lock once per rule.
// iptables serialises through that lock, it is per-table and shared with everything
// else on the host -- Docker included -- and a sandbox needs thirteen rules.
//
// Under fan-out that dominated: network_setup cost 0.165s for a single create and 5.1s
// with thirty running at once, 78% of the batch's wall clock. Waiting for the lock
// (-w) is what stopped concurrent creates failing outright, but queueing thirteen
// times per sandbox is still queueing.
//
// iptables-restore applies a table in one transaction, so the same rules cost one
// acquisition instead of one each. Measured uncontended: 13 calls 18ms, one batch 3ms.
// The win under contention is larger, because what is saved there is queueing rather
// than process startup.
//
// # Why --noflush is not optional
//
// Without it, iptables-restore *replaces* the table. The host's filter and nat tables
// carry Docker's rules, and this platform's premise is sharing a host, so a flush would
// take down every container on the machine. Teardown has carried a comment saying there
// must never be a flush anywhere in this package; this is the same hazard arriving
// through a different door, and the reason restoreScript never emits a table line
// without it being paired with --noflush at the call site.
//
// Verified rather than assumed (hack/iptables-restore-probe.sh, against a private
// chain so the check could not damage anything): --noflush leaves existing rules
// alone, and -I position semantics survive -- which matters because bean's DROP rules
// must precede its blanket ACCEPT or egress policy silently does not apply.

// iptablesLockWaitSeconds bounds how long an iptables invocation waits for the xtables
// lock. Shared by the batched and per-rule paths so they cannot disagree.
//
// Bounded rather than unbounded: a create that blocks forever on a stuck lock is worse
// than one that fails, because the scheduler can retry a failure and can do nothing with
// a hang. Five seconds is far above the milliseconds a rule insert takes and far below
// the create timeout.
const iptablesLockWaitSeconds = "5"

// restoreBatch is one iptables-restore invocation: a set of rules that share a scope
// and can therefore be applied together.
type restoreBatch struct {
	Scope Scope
	// Rules in the order they must be applied.
	Rules []Rule
}

// batchRules groups rules into the fewest iptables-restore invocations that preserve
// their meaning.
//
// Grouped by scope only, not by table: one restore script can carry several tables,
// and the tables in it are applied in one transaction. Scope has to split them because
// a netns rule runs inside the sandbox's namespace and a host rule does not -- mixing
// those is the failure iptArgs exists to make impossible.
//
// Order within a scope is preserved exactly as the plan produced it. That is load
// bearing: -I inserts at position 1, so two inserts arrive in the chain in reverse,
// and the plan's order already accounts for that. Regrouping by table would reorder
// them relative to each other and change which packet a rule sees first.
func batchRules(rules []Rule) []restoreBatch {
	var batches []restoreBatch
	for _, r := range rules {
		if n := len(batches); n > 0 && batches[n-1].Scope == r.Scope {
			batches[n-1].Rules = append(batches[n-1].Rules, r)
			continue
		}
		batches = append(batches, restoreBatch{Scope: r.Scope, Rules: []Rule{r}})
	}
	return batches
}

// restoreScript renders a batch as iptables-restore input.
//
// The format is one `*table` header per table, its rules, then `COMMIT`. Rules within
// a table keep the order they were given; tables appear in the order first seen, so a
// batch mixing filter and nat produces both sections without reordering either.
//
// A rule's own arguments come from Rule.Args minus the `-t <table>` prefix, since the
// table is the section header here. Reusing Args rather than re-rendering means the
// batched path and the one-at-a-time path cannot drift: the same builder produces both,
// which is what makes the Delete/-D symmetry teardown depends on still hold.
func restoreScript(b restoreBatch) string {
	// Table order recorded separately from the map, so the output is deterministic --
	// a script whose sections reordered between runs would make a diff of two failures
	// impossible to read.
	var tables []string
	byTable := map[string][]string{}
	for _, r := range b.Rules {
		line := restoreLine(r)
		if _, seen := byTable[r.Table]; !seen {
			tables = append(tables, r.Table)
		}
		byTable[r.Table] = append(byTable[r.Table], line)
	}

	var sb strings.Builder
	for _, t := range tables {
		fmt.Fprintf(&sb, "*%s\n", t)
		for _, line := range byTable[t] {
			sb.WriteString(line)
			sb.WriteByte('\n')
		}
		sb.WriteString("COMMIT\n")
	}
	return sb.String()
}

// restoreLine renders one rule for a restore script: its Args with the leading
// `-t <table>` removed, because the table is the section header.
func restoreLine(r Rule) string {
	args := r.Args()
	// Args always starts with -t <table>; dropping two elements rather than searching
	// for the flag keeps this tied to that shape, and the test asserts it.
	if len(args) >= 2 && args[0] == "-t" {
		args = args[2:]
	}
	return strings.Join(args, " ")
}

// restoreArgs is the command line for applying a batch.
//
// --noflush for the reason at the top of this file: without it the table is replaced
// and Docker's rules go with it. -w for the same reason the per-rule path passes it:
// the lock is shared, and failing rather than waiting is what made concurrent creates
// lose before.
func restoreArgs(scope Scope, netns string) (string, []string) {
	args := []string{"--noflush", "-w", iptablesLockWaitSeconds}
	if scope == ScopeNetns {
		return "ip", append([]string{"netns", "exec", netns, "iptables-restore"}, args...)
	}
	return "iptables-restore", args
}

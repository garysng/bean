//go:build linux

package network

import (
	"strings"
	"testing"
)

// recorder captures commands instead of running them. The failures worth catching
// in setup_linux.go are a rule applied in the wrong namespace and a teardown that
// flushes, and both are visible in the command list without a kernel.
type recorder struct {
	cmds []string
	// scripts holds what was fed to iptables-restore, in call order.
	scripts []string
	failOn  string
	out     []byte
}

func (r *recorder) Run(name string, args ...string) error {
	line := name + " " + strings.Join(args, " ")
	r.cmds = append(r.cmds, line)
	if r.failOn != "" && strings.Contains(line, r.failOn) {
		return errFake
	}
	return nil
}

// RunInput records the script alongside the command, because with batching the script
// *is* the rules -- a test that only saw the command line would no longer be able to
// tell a rule applied in the wrong namespace from one not applied at all.
func (r *recorder) RunInput(stdin, name string, args ...string) error {
	line := name + " " + strings.Join(args, " ")
	r.cmds = append(r.cmds, line)
	r.scripts = append(r.scripts, stdin)
	if r.failOn != "" && (strings.Contains(line, r.failOn) || strings.Contains(stdin, r.failOn)) {
		return errFake
	}
	return nil
}

func (r *recorder) Output(name string, args ...string) ([]byte, error) {
	r.cmds = append(r.cmds, name+" "+strings.Join(args, " "))
	return r.out, nil
}

type fakeErr struct{}

func (fakeErr) Error() string { return "fake failure" }

var errFake = fakeErr{}

func layoutForTest(t *testing.T) *Layout {
	t.Helper()
	l, err := LayoutFor(6, "172.31.0.0/30")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestSetupAppliesNamespaceRulesInsideTheNamespace(t *testing.T) {
	// A rule meant for the namespace but applied on the host matches the guest
	// subnet, which no host-side packet carries. It would appear in the chain and
	// deny nothing at all.
	l := layoutForTest(t)
	rec := &recorder{}
	s := &LinuxSetup{Uplink: testUplink, Cmd: rec}
	if err := s.Setup(l); err != nil {
		t.Fatal(err)
	}
	guest := l.GuestSubnetCIDR()
	for _, c := range rec.cmds {
		if !strings.Contains(c, "iptables") {
			continue
		}
		if strings.Contains(c, "-s "+guest) &&
			!strings.HasPrefix(c, "ip netns exec "+l.Netns+" iptables") {
			t.Errorf("a guest-subnet rule was applied outside the namespace: %s", c)
		}
		if strings.Contains(c, "-s "+l.LinkCIDR()) &&
			strings.HasPrefix(c, "ip netns exec ") {
			t.Errorf("a link-subnet rule was applied inside the namespace, where the "+
				"source is still the guest address: %s", c)
		}
	}
}

func TestSetupInsertsDropsAtPositionOne(t *testing.T) {
	// The exec-layer mirror of TestDropsAreInsertedNotAppended: what actually runs
	// has to carry -I, not -A. Switching OpInsert to OpAppend must fail here too.
	l := layoutForTest(t)
	rec := &recorder{}
	s := &LinuxSetup{Uplink: testUplink, Cmd: rec}
	if err := s.Setup(l); err != nil {
		t.Fatal(err)
	}
	// The rules now arrive as iptables-restore scripts rather than as command lines,
	// so this walks what was fed to stdin. The property is unchanged and is the one
	// that matters: a DROP behind Docker's ACCEPT never matches.
	drops := 0
	for _, script := range rec.scripts {
		for _, line := range strings.Split(script, "\n") {
			if !strings.Contains(line, "-j DROP") {
				continue
			}
			drops++
			if !strings.Contains(line, "-I FORWARD 1") {
				t.Errorf("DROP not inserted at the head of FORWARD: %s\nBehind Docker's "+
					"ACCEPT it never matches", line)
			}
		}
	}
	if drops != len(deniedDestinations)*2 {
		t.Errorf("applied %d DROP rules, expected %d (each denied range in both "+
			"scopes)", drops, len(deniedDestinations)*2)
	}
}

func TestTeardownNeverFlushes(t *testing.T) {
	// Docker's rules live in the same tables. This walks whatever teardown runs, so
	// rules added later are covered without anyone remembering to extend it.
	l := layoutForTest(t)
	rec := &recorder{}
	s := &LinuxSetup{Uplink: testUplink, Cmd: rec}
	if err := s.Teardown(l); err != nil {
		t.Fatal(err)
	}
	for _, c := range rec.cmds {
		for _, bad := range []string{" -F", " --flush", " -X", " -Z", " -P "} {
			if strings.Contains(c, bad) {
				t.Errorf("teardown command contains %q: %s", bad, c)
			}
		}
	}
	// And it must actually delete the DROP rules, not just the MASQUERADE ones.
	for _, dst := range deniedDestinations {
		found := false
		for _, c := range rec.cmds {
			if strings.Contains(c, "-D FORWARD") && strings.Contains(c, "-d "+dst) {
				found = true
			}
		}
		if !found {
			t.Errorf("teardown never deletes the DROP for %s; it accumulates in the "+
				"host chain for the life of the node", dst)
		}
	}
}

func TestTeardownDeletesWithTheInstallArguments(t *testing.T) {
	// -D matches by specification. If the argument list drifts from the one used to
	// install, iptables removes nothing and says little about why.
	l := layoutForTest(t)
	setupRec := &recorder{}
	if err := (&LinuxSetup{Uplink: testUplink, Cmd: setupRec}).Setup(l); err != nil {
		t.Fatal(err)
	}
	downRec := &recorder{}
	if err := (&LinuxSetup{Uplink: testUplink, Cmd: downRec}).Teardown(l); err != nil {
		t.Fatal(err)
	}
	for _, c := range setupRec.cmds {
		if !strings.Contains(c, "iptables") {
			continue
		}
		// Reduce an install command to its match arguments by dropping the placement
		// flag, then require a delete carrying exactly those.
		want := matchPart(c)
		if want == "" {
			continue
		}
		found := false
		for _, d := range downRec.cmds {
			if strings.Contains(d, "-D ") && matchPart(d) == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no delete with the identical match arguments for: %s", c)
		}
	}
}

// matchPart returns everything after the chain name, which is the part -D has to
// reproduce exactly.
func matchPart(cmd string) string {
	for _, flag := range []string{"-I ", "-A ", "-D "} {
		i := strings.Index(cmd, flag)
		if i < 0 {
			continue
		}
		rest := strings.Fields(cmd[i+len(flag):])
		if len(rest) < 2 {
			return ""
		}
		// Skip the chain, and the position argument an insert carries.
		rest = rest[1:]
		if flag == "-I " && len(rest) > 0 && rest[0] == "1" {
			rest = rest[1:]
		}
		return strings.Join(rest, " ")
	}
	return ""
}

func TestSetupCleansUpWhenARuleFails(t *testing.T) {
	// A half-built network is the worst outcome here: the sandbox boots and its
	// connectivity is intermittent, which docs/network.md section 7 calls out as
	// the reason this module cannot ship half done.
	l := layoutForTest(t)
	rec := &recorder{failOn: "MASQUERADE"}
	s := &LinuxSetup{Uplink: testUplink, Cmd: rec}
	if err := s.Setup(l); err == nil {
		t.Fatal("Setup reported success despite a failing rule")
	}
	deleted := false
	for _, c := range rec.cmds {
		if strings.Contains(c, "ip netns del "+l.Netns) {
			deleted = true
		}
	}
	if !deleted {
		t.Error("a failed Setup left the namespace behind, so its index stays " +
			"occupied in the allocator forever")
	}
}

func TestListNamespacesReadsOnlyTheFirstField(t *testing.T) {
	rec := &recorder{out: []byte("bean-0 (id: 1)\nbean-3\ndocker-thing (id: 2)\n")}
	s := &LinuxSetup{Uplink: testUplink, Cmd: rec}
	names, err := s.ListNamespaces()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bean-0", "bean-3", "docker-thing"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("got %v, want %v", names, want)
		}
	}
}

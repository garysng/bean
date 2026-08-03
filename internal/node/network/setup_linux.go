//go:build linux

package network

import (
	"fmt"
	"os/exec"
	"strings"
)

// This is the execution half of the plan built in rules.go: it creates the
// namespace, the tap and the veth pair, and applies the NAT and filter rules.
//
// Everything that decides whether the rules are correct -- their order, their
// scope, and the argument list used to remove them -- lives in rules.go and is
// tested without root. What is here is only the running of commands.

// Commander runs a command. It exists so Setup and Teardown can be exercised
// against a recorder in tests: the interesting failures in this file are wrong
// commands and wrong ordering, and both are observable without a kernel.
type Commander interface {
	Run(name string, args ...string) error
	Output(name string, args ...string) ([]byte, error)
}

// execCommander runs commands for real.
type execCommander struct{}

// Run folds stderr into the error. "exit status 2" from iptables does not say
// whether the chain was missing, the rule was already absent, or the kernel
// module is not loaded, and those need different responses.
func (execCommander) Run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), msg)
		}
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func (execCommander) Output(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).Output()
}

// LinuxSetup builds and destroys one sandbox's networking.
type LinuxSetup struct {
	// Uplink is the host interface egress leaves by. Required: see SetupPlan.
	Uplink string
	// Cmd runs the commands. Nil means run them for real.
	Cmd Commander
}

func (s *LinuxSetup) cmd() Commander {
	if s.Cmd == nil {
		return execCommander{}
	}
	return s.Cmd
}

// ipt renders the iptables invocation for a rule, entering the namespace when the
// rule's scope calls for it.
//
// The scope is read from the rule rather than passed in separately. A host rule
// applied inside a namespace, or the reverse, is the failure this whole design
// hinges on avoiding, and it should not be possible to introduce it by calling
// this function with the wrong argument.
func iptArgs(netns string, r Rule) (string, []string) {
	if r.Scope == ScopeNetns {
		return "ip", append([]string{"netns", "exec", netns, "iptables"}, r.Args()...)
	}
	return "iptables", r.Args()
}

// Setup creates the namespace, links and rules for one sandbox.
//
// On any failure it tears down what it built. A half-created sandbox network is
// the worst outcome available here: it presents as a sandbox that boots and then
// has intermittent connectivity, which is the failure mode docs/network.md
// section 7 singles out as the reason this module cannot be shipped half done.
func (s *LinuxSetup) Setup(l *Layout) error {
	rules, err := SetupPlan(l, s.Uplink)
	if err != nil {
		return err
	}
	if err := s.build(l, rules); err != nil {
		// Best effort. The original error is what the caller needs; a failure while
		// cleaning up would only obscure it.
		_ = s.Teardown(l)
		return err
	}
	return nil
}

func (s *LinuxSetup) build(l *Layout, rules []Rule) error {
	c := s.cmd()
	ns := l.Netns

	steps := [][]string{
		{"ip", "netns", "add", ns},
		// The tap the VMM attaches to. Same name in every namespace, which is what
		// lets a restored snapshot find the device it recorded.
		{"ip", "netns", "exec", ns, "ip", "tuntap", "add", "name", l.TapName, "mode", "tap"},
		{"ip", "netns", "exec", ns, "ip", "addr", "add", l.GatewayCIDR(), "dev", l.TapName},
		{"ip", "netns", "exec", ns, "ip", "link", "set", l.TapName, "up"},
		{"ip", "netns", "exec", ns, "ip", "link", "set", "lo", "up"},

		// veth pair: created in the host namespace, then one end moved in.
		{"ip", "link", "add", l.HostVeth, "type", "veth", "peer", "name", l.NetnsVeth},
		{"ip", "link", "set", l.NetnsVeth, "netns", ns},
		{"ip", "addr", "add", l.HostLinkCIDR(), "dev", l.HostVeth},
		{"ip", "link", "set", l.HostVeth, "up"},
		{"ip", "netns", "exec", ns, "ip", "addr", "add", l.NetnsLinkCIDR(), "dev", l.NetnsVeth},
		{"ip", "netns", "exec", ns, "ip", "link", "set", l.NetnsVeth, "up"},
		{"ip", "netns", "exec", ns, "ip", "route", "add", "default", "via", l.HostLinkIP.String()},

		// Forwarding inside the namespace, without which the guest's packets are
		// delivered locally and dropped instead of crossing to the veth.
		{"ip", "netns", "exec", ns, "sysctl", "-q", "-w", "net.ipv4.ip_forward=1"},
	}
	for _, step := range steps {
		if err := c.Run(step[0], step[1:]...); err != nil {
			return fmt.Errorf("network: setup %s: %w", l.Netns, err)
		}
	}

	for _, r := range rules {
		name, args := iptArgs(ns, r)
		if err := c.Run(name, args...); err != nil {
			return fmt.Errorf("network: apply %s: %w", r, err)
		}
	}
	return nil
}

// Teardown removes everything Setup created.
//
// Every step runs even if an earlier one failed, and the first error is returned.
// Stopping at the first failure would leave the rest behind, and what is left
// behind here is not inert: a stale filter rule accumulates in the host's FORWARD
// chain for the life of the node, and a stale namespace makes its index look
// occupied to the allocator forever.
//
// Rule removal is by -D with the same argument list that installed it, produced
// by the same builder. There is no flush anywhere in this function and there must
// never be: the host's nat and filter tables carry Docker's rules, and -F in
// either would take down every container on a machine this platform shares by
// design.
func (s *LinuxSetup) Teardown(l *Layout) error {
	c := s.cmd()
	var first error
	note := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}

	// Host-scope rules are deleted explicitly because they outlive the namespace.
	// Namespace-scope rules go away with the namespace, but they are deleted too:
	// Teardown also runs on the reconciliation path where the namespace may be
	// kept, and a delete of an absent rule is a harmless non-zero exit.
	rules, err := TeardownPlan(l, s.Uplink)
	if err != nil {
		return err
	}
	for _, r := range rules {
		name, args := iptArgs(l.Netns, r)
		if err := c.Run(name, args...); err != nil && r.Scope == ScopeHost {
			// Only host-scope failures are reported. A namespace-scope delete fails
			// routinely once the namespace is gone, and treating that as an error
			// would make every successful teardown look broken.
			note(fmt.Errorf("network: remove %s: %w", r, err))
		}
	}

	// The host end of the veth goes when the namespace does, since its peer is
	// inside. It is deleted first anyway, so a namespace that was already gone
	// does not leave the interface behind.
	if err := c.Run("ip", "link", "del", l.HostVeth); err != nil {
		// Absent is the expected case on a repeat teardown, so this is not an error.
		_ = err
	}
	note2 := c.Run("ip", "netns", "del", l.Netns)
	if note2 != nil && s.namespaceExists(l.Netns) {
		// Only a failure that left the namespace in place matters. Deleting one that
		// was already gone is the normal idempotent path.
		note(fmt.Errorf("network: delete namespace %s: %w", l.Netns, note2))
	}
	return first
}

// namespaceExists reports whether a namespace is still present.
func (s *LinuxSetup) namespaceExists(ns string) bool {
	names, err := s.ListNamespaces()
	if err != nil {
		// Unknown. Reported as absent so a listing failure does not turn every
		// teardown into an error; the allocator consults the host again anyway.
		return false
	}
	for _, n := range names {
		if n == ns {
			return true
		}
	}
	return false
}

// ListNamespaces implements Lister, so the allocator rebuilds its pool from the
// host rather than from memory.
func (s *LinuxSetup) ListNamespaces() ([]string, error) {
	out, err := s.cmd().Output("ip", "-o", "netns", "list")
	if err != nil {
		return nil, fmt.Errorf("network: ip netns list: %w", err)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Lines read "<name>" or "<name> (id: 0)"; the name is the first field.
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		names = append(names, fields[0])
	}
	return names, nil
}

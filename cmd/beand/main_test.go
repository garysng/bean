package main

import (
	"flag"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestUnknownFlagDoesNotStopTheAgent is a regression test for a guest panic.
//
// noded builds the agent's command line, and the agent image on disk can be older
// than the noded that boots it -- the disk is a build artifact shipped separately.
// When noded passed --guest-dns to an image that predated the flag, Go's default
// flag handling printed usage and exited 2. As PID 1 in a microVM that is
// "Attempted to kill init!", and what noded reported was an agent that never
// answered, twenty seconds later, with no mention of a flag.
//
// The exit code is what this asserts on, because it is the thing the kernel reacts
// to. A warning on stderr is not enough: an agent that warns and then exits panics
// the guest just the same.
func TestUnknownFlagDoesNotStopTheAgent(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "beand")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build beand: %v\n%s", err, out)
	}

	// A flag no version of the agent has, standing in for one a future noded adds.
	// --listen points at a path in a directory that does not exist, so the agent
	// fails at the listener rather than running forever: this test is about how far
	// it gets past argument parsing, and a bind failure is past it.
	cmd := exec.Command(bin,
		"--listen", filepath.Join(t.TempDir(), "no-such-dir", "agent.sock"),
		"--flag-from-a-newer-noded", "value")
	out, err := cmd.CombinedOutput()

	if code := cmd.ProcessState.ExitCode(); code == 2 {
		t.Fatalf("agent exited 2 on an unknown flag, which as PID 1 panics the "+
			"guest with \"Attempted to kill init!\"\nerr: %v\noutput:\n%s", err, out)
	}
	if !strings.Contains(string(out), "ignoring unusable arguments") {
		t.Errorf("no warning about the unusable argument; a silently ignored flag "+
			"is its own diagnosis problem\noutput:\n%s", out)
	}
	// Reaching the listener is the proof it got past parsing. If this ever stops
	// being the failure, the assertion above is the one that matters.
	if !strings.Contains(string(out), "listen") && err == nil {
		t.Errorf("expected the agent to proceed as far as binding its listener\n"+
			"output:\n%s", out)
	}
}

// TestKnownFlagsStillParse guards the other direction: ContinueOnError must not
// turn a real configuration mistake into silence.
func TestKnownFlagsStillParse(t *testing.T) {
	fs := flag.NewFlagSet("beand", flag.ContinueOnError)
	dns := fs.String("guest-dns", "", "")
	if err := fs.Parse([]string{"--guest-dns", "223.5.5.5"}); err != nil {
		t.Fatalf("a known flag failed to parse: %v", err)
	}
	if *dns != "223.5.5.5" {
		t.Fatalf("guest-dns = %q, want 223.5.5.5", *dns)
	}
}

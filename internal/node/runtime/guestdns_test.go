package runtime

import "testing"

// TestGuestDNSBootArgsEmptyWhenUnset pins the promise that a node with no
// resolver configured boots its guests with an unchanged kernel command line.
// An empty --guest-dns would be a new argument on every guest on every node,
// which is a change to the default deployment rather than an opt-in feature.
func TestGuestDNSBootArgsEmptyWhenUnset(t *testing.T) {
	if got := GuestDNSBootArgs(""); got != "" {
		t.Errorf("GuestDNSBootArgs(\"\") = %q, want the command line untouched", got)
	}
}

// TestGuestDNSBootArgsAppendsFlag checks the leading space, since this is
// concatenated onto a command line whose last token is otherwise run together
// with the flag name.
func TestGuestDNSBootArgsAppendsFlag(t *testing.T) {
	got := GuestDNSBootArgs("10.0.0.53")
	if got != " --guest-dns 10.0.0.53" {
		t.Errorf("GuestDNSBootArgs = %q; without a leading space the flag merges "+
			"into the preceding argument", got)
	}
}

// TestLocalRuntimeOmitsGuestDNSWhenUnset is the same promise on the dev tier:
// the agent is spawned with the arguments it had before this flag existed.
func TestLocalRuntimeOmitsGuestDNSWhenUnset(t *testing.T) {
	r := NewLocalRuntime("beand", t.TempDir())
	args := r.agentArgs("/tmp/a.sock", "/tmp/root")
	for _, a := range args {
		if a == "--guest-dns" {
			t.Fatalf("agentArgs = %v; no resolver is configured, so the agent must "+
				"not be asked to write one", args)
		}
	}
}

func TestLocalRuntimePassesGuestDNS(t *testing.T) {
	r := NewLocalRuntime("beand", t.TempDir())
	r.GuestDNS = "10.0.0.53"
	args := r.agentArgs("/tmp/a.sock", "/tmp/root")

	found := false
	for i, a := range args {
		if a != "--guest-dns" {
			continue
		}
		found = true
		if i+1 >= len(args) || args[i+1] != "10.0.0.53" {
			t.Fatalf("agentArgs = %v, --guest-dns has the wrong value", args)
		}
	}
	if !found {
		t.Errorf("agentArgs = %v, want --guest-dns", args)
	}
}

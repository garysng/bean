package image

import "strings"

// dmPrefix marks bean's device-mapper mappings on a host it shares with other
// workloads. Device-mapper names are one flat namespace for the whole machine,
// shared with Docker's thin pools and anything else using dm, so the prefix is
// both collision avoidance and the only evidence available later that a mapping
// is ours to touch.
const dmPrefix = "bean-"

// DMName derives the mapping name for a sandbox.
func DMName(sandboxID string) string {
	return dmPrefix + sandboxID
}

// SandboxIDFromDMName recovers a sandbox id from a mapping name, reporting false
// for any name bean did not create.
//
// This is the inverse of DMName rather than a prefix test written a second time,
// because the two must not be able to disagree: reconciliation decides what to
// destroy from this answer, and a mismatch would either leak mappings forever or
// remove another workload's.
func SandboxIDFromDMName(name string) (string, bool) {
	id, ok := strings.CutPrefix(name, dmPrefix)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

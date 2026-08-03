package image

import "testing"

// TestDMNameRoundTrips keeps the two directions from drifting. Reconciliation
// decides what to destroy from SandboxIDFromDMName, so a name DMName produces and
// the inverse does not recognise would leak that mapping forever.
func TestDMNameRoundTrips(t *testing.T) {
	for _, id := range []string{"sbx_a", "sbx-with-dashes", "sbx.dots", "bean-nested"} {
		got, ok := SandboxIDFromDMName(DMName(id))
		if !ok || got != id {
			t.Errorf("round trip of %q = %q,%v", id, got, ok)
		}
	}
}

// TestSandboxIDFromDMNameRejectsForeignNames is the safety boundary for
// device-mapper. These are the shapes seen on the shared hosts this runs on, plus
// the near-miss that contains the prefix without starting with it.
func TestSandboxIDFromDMNameRejectsForeignNames(t *testing.T) {
	for _, name := range []string{
		"docker-253:1-pool",
		"nexus-pod-7f3a",
		"vg0-lv_root",
		"nexus-bean-sbx_x",
		"Bean-sbx_a",
		// The bare prefix names no sandbox, so it must not yield an empty id that
		// an expected-set lookup would then miss.
		"bean-",
		"",
	} {
		if id, ok := SandboxIDFromDMName(name); ok {
			t.Errorf("SandboxIDFromDMName(%q) accepted, yielding %q", name, id)
		}
	}
}

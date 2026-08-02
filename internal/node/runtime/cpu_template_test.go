package runtime

import (
	"strings"
	"testing"
)

func TestParseCPUTemplate(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    CPUTemplate
		wantErr bool
	}{
		{in: "", want: CPUTemplateNone},
		{in: "none", want: CPUTemplateNone},
		{in: "portable", want: CPUTemplatePortable},
		{in: "PORTABLE", wantErr: true},
		{in: "t2", wantErr: true},
	} {
		got, err := ParseCPUTemplate(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseCPUTemplate(%q) = %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseCPUTemplate(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("ParseCPUTemplate(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestClearBitsBitmapPositions pins the bit ordering.
//
// Firecracker's bitmap is written most-significant-bit first, so bit n lands at
// string index 31-n. Getting this backwards still produces a well-formed request
// that Firecracker accepts — it would just mask unrelated features and leave the
// intended ones exposed, with nothing to indicate it happened.
func TestClearBitsBitmapPositions(t *testing.T) {
	got := clearBitsBitmap([]uint{0})
	if !strings.HasPrefix(got, "0b_") {
		t.Fatalf("bitmap %q lacks the 0b_ prefix", got)
	}
	body := strings.TrimPrefix(got, "0b_")
	// 31, not 32: Firecracker rejects a 32-character bitmap as too long even
	// though the registers are 32 bits wide. Sending 32 fails the request
	// outright, which is how this was found — on real hardware, after the unit
	// tests passed against the wrong width.
	if len(body) != cpuBitmapWidth {
		t.Fatalf("bitmap body is %d chars, want %d: %q", len(body), cpuBitmapWidth, body)
	}
	if body[cpuBitmapWidth-1] != '0' {
		t.Errorf("bit 0 should be the last character, got %q", body)
	}

	// The highest addressable bit is the first character.
	body = strings.TrimPrefix(clearBitsBitmap([]uint{cpuBitmapWidth - 1}), "0b_")
	if body[0] != '0' {
		t.Errorf("bit %d should be the first character, got %q", cpuBitmapWidth-1, body)
	}

	// Everything not named must stay 'x' so the host's value survives.
	body = strings.TrimPrefix(clearBitsBitmap([]uint{5}), "0b_")
	if body[cpuBitmapWidth-1-5] != '0' {
		t.Errorf("bit 5 not cleared at the right index: %q", body)
	}
	if strings.Count(body, "0") != 1 {
		t.Errorf("expected exactly one cleared bit, got %q", body)
	}
	if strings.Count(body, "x") != cpuBitmapWidth-1 {
		t.Errorf("unnamed bits must stay x, got %q", body)
	}
}

// TestUnmaskableBitsAreNotClaimedAsMasked covers the one bit Firecracker cannot
// address. Reporting avx512vl as masked when the request cannot express it would
// put a false claim in the snapshot manifest, and the restore that trusted it
// would be the thing that discovered the truth.
func TestUnmaskableBitsAreNotClaimedAsMasked(t *testing.T) {
	body := strings.TrimPrefix(clearBitsBitmap([]uint{cpuBitmapWidth}), "0b_")
	if strings.Contains(body, "0") {
		t.Errorf("an unaddressable bit produced a cleared position: %q", body)
	}

	masked := map[string]bool{}
	for _, f := range MaskedCPUFeatures(CPUTemplatePortable) {
		masked[f] = true
	}
	unmaskable := UnmaskableCPUFeatures(CPUTemplatePortable)
	if len(unmaskable) == 0 {
		t.Fatal("expected at least avx512vl to be reported unmaskable")
	}
	for _, f := range unmaskable {
		if masked[f] {
			t.Errorf("%s is reported both masked and unmaskable", f)
		}
	}
}

// TestCPUConfigMergesRegisters is the property that makes the config mean what
// it says. Two features in the same leaf/register have to become one bitmap: as
// separate entries, the second one's 'x' at the first one's bit position would
// restore the host value, silently unmasking a feature.
func TestCPUConfigMergesRegisters(t *testing.T) {
	cfg := cpuConfigFor(CPUTemplatePortable)
	if cfg == nil {
		t.Fatal("portable template produced no config")
	}

	seen := map[string]int{}
	for _, m := range cfg.CPUIDModifiers {
		for _, r := range m.Modifiers {
			key := m.Leaf + "/" + m.Subleaf + "/" + r.Register
			seen[key]++
			if seen[key] > 1 {
				t.Errorf("%s appears %d times; masks must be merged into one bitmap",
					key, seen[key])
			}
		}
	}

	// Leaf 7 EBX carries several AVX-512 bits plus avx2, so its bitmap must
	// clear more than one position — the case a non-merging implementation
	// gets wrong.
	var found bool
	for _, m := range cfg.CPUIDModifiers {
		if m.Leaf != "0x7" {
			continue
		}
		for _, r := range m.Modifiers {
			if r.Register != "ebx" {
				continue
			}
			found = true
			if n := strings.Count(r.Bitmap, "0"); n < 2 {
				t.Errorf("leaf 7 ebx clears %d bits, want several: %q", n, r.Bitmap)
			}
		}
	}
	if !found {
		t.Error("no leaf 7 ebx modifier; the AVX-512 bits are not masked")
	}
}

func TestCPUConfigNoneIsNil(t *testing.T) {
	if cfg := cpuConfigFor(CPUTemplateNone); cfg != nil {
		t.Errorf("template none produced a config: %+v", cfg)
	}
	if got := MaskedCPUFeatures(CPUTemplateNone); got != nil {
		t.Errorf("template none masks %v, want nothing", got)
	}
}

// TestPortableMasksVectorExtensions guards the selection itself. avx and avx2
// are the ones that actually break a migration, because runtime libraries pick
// code paths from them during startup and cache the choice.
func TestPortableMasksVectorExtensions(t *testing.T) {
	masked := map[string]bool{}
	for _, f := range MaskedCPUFeatures(CPUTemplatePortable) {
		masked[f] = true
	}
	for _, want := range []string{"avx", "avx2", "avx512f"} {
		if !masked[want] {
			t.Errorf("portable template does not mask %s", want)
		}
	}
	// Baseline features must not be masked: every Firecracker-capable host has
	// them, so hiding them costs performance and buys no portability.
	//
	// xsave is in this list for a sharper reason. Masking leaf 1 ECX bit 26 does
	// hide "xsave", but the XSAVE sub-features live in leaf 0xD and remained
	// visible in a real guest — leaving a CPUID that advertises xsaveopt without
	// xsave, which describes no actual processor.
	for _, unwanted := range []string{"sse2", "fpu", "cmov", "xsave"} {
		if masked[unwanted] {
			t.Errorf("portable template masks baseline feature %s", unwanted)
		}
	}
}

package runtime

import "fmt"

// CPU templates mask instruction-set features out of what a guest sees, so a
// memory snapshot does not bind itself to the machine that produced it.
//
// A guest that boots on a host with AVX2 will use AVX2 — glibc selects string
// routines from CPUID at startup and caches the choice. Restoring that guest's
// memory on a host without AVX2 does not fail at restore; it faults later, in
// whatever code happens to run next. Masking is only effective before the guest
// boots, which is why this cannot be added at snapshot time: by then the guest
// has already made its choices.
//
// What masking cannot hide is the vendor and family: CPUID leaf 0 carries the
// vendor string, and a guest kernel branches on it for errata workarounds and
// MSR access. So a template makes snapshots portable *within* a vendor, and
// cross-vendor restore has to be refused by the scheduler instead.
//
// Measured on the AMD EPYC 7542 (family 23, Zen 2) verification host, none of
// Firecracker's built-in static templates can even start a VM:
//
//	T2, C3, T2S, T2CL -> "CPU vendor mismatched" (all Intel-only)
//	T2A               -> "current CPU model is not permitted" (Milan/Zen 3 only)
//
// Hence a custom template through /cpu-config rather than a named one. It also
// avoids tying the platform's portability story to whichever CPU models AWS
// chose to support.

// cpuFeatureMask describes one CPUID bit to clear.
type cpuFeatureMask struct {
	// Name is the feature as /proc/cpuinfo spells it, for logs and tests.
	Name string
	// Leaf and Subleaf identify the CPUID leaf.
	Leaf, Subleaf uint32
	// Register is "eax", "ebx", "ecx" or "edx".
	Register string
	// Bit is the bit position to clear within that register.
	Bit uint
}

// portableCPUMask lists the features cleared by the "portable" template.
//
// The selection is deliberately narrow: every masked feature is one a guest
// cannot use, so masking too much makes every sandbox slower for the benefit of
// a migration that may never happen. These are the wide-vector extensions,
// which are both the most variable across CPU generations and the ones runtime
// libraries dispatch on at startup.
//
// Baseline features (SSE2, and on x86-64 everything in the original AMD64 spec)
// are not masked: they exist on every host that can run Firecracker, so hiding
// them would cost performance and buy no portability.
var portableCPUMask = []cpuFeatureMask{
	// Leaf 7 subleaf 0, EBX.
	{Name: "avx2", Leaf: 0x7, Subleaf: 0x0, Register: "ebx", Bit: 5},
	{Name: "avx512f", Leaf: 0x7, Subleaf: 0x0, Register: "ebx", Bit: 16},
	{Name: "avx512dq", Leaf: 0x7, Subleaf: 0x0, Register: "ebx", Bit: 17},
	{Name: "avx512ifma", Leaf: 0x7, Subleaf: 0x0, Register: "ebx", Bit: 21},
	{Name: "avx512cd", Leaf: 0x7, Subleaf: 0x0, Register: "ebx", Bit: 28},
	{Name: "avx512bw", Leaf: 0x7, Subleaf: 0x0, Register: "ebx", Bit: 30},
	// avx512vl is leaf 7 EBX bit 31, which Firecracker's bitmap cannot address
	// (see cpuBitmapWidth). It is listed so the gap is visible here rather than
	// discovered from a guest, and MaskedCPUFeatures omits it — reporting a
	// feature as masked when it is not would be worse than not masking it.
	{Name: "avx512vl", Leaf: 0x7, Subleaf: 0x0, Register: "ebx", Bit: 31},
	// Leaf 1, ECX. AVX itself goes too: a guest that saw AVX will use it.
	{Name: "avx", Leaf: 0x1, Subleaf: 0x0, Register: "ecx", Bit: 28},
	{Name: "fma", Leaf: 0x1, Subleaf: 0x0, Register: "ecx", Bit: 12},
	{Name: "f16c", Leaf: 0x1, Subleaf: 0x0, Register: "ecx", Bit: 29},
	// XSAVE is deliberately not masked. Clearing leaf 1 ECX bit 26 does hide
	// "xsave", but the XSAVE sub-features live in leaf 0xD and stayed visible —
	// a guest that sees xsaveopt without xsave is looking at a CPUID that
	// describes no real processor. XSAVE is also present on every host that can
	// run Firecracker, so masking it buys no portability for that risk.
}

// CPUTemplate names a masking policy.
type CPUTemplate string

const (
	// CPUTemplateNone passes the host CPU through. Fastest, and a memory
	// snapshot taken under it is only safe to restore on the same CPU model.
	CPUTemplateNone CPUTemplate = "none"
	// CPUTemplatePortable masks wide-vector extensions so a memory snapshot
	// can be restored across CPU generations from the same vendor.
	CPUTemplatePortable CPUTemplate = "portable"
)

// ParseCPUTemplate validates a template name.
func ParseCPUTemplate(s string) (CPUTemplate, error) {
	switch CPUTemplate(s) {
	case "", CPUTemplateNone:
		return CPUTemplateNone, nil
	case CPUTemplatePortable:
		return CPUTemplatePortable, nil
	default:
		return "", fmt.Errorf("unknown cpu template %q (want none or portable)", s)
	}
}

// fcCPUConfig is the body of Firecracker's PUT /cpu-config.
type fcCPUConfig struct {
	CPUIDModifiers []fcCPUIDModifier `json:"cpuid_modifiers"`
}

type fcCPUIDModifier struct {
	Leaf    string `json:"leaf"`
	Subleaf string `json:"subleaf"`
	// Flags selects which subleaves the entry applies to; 1 means the
	// subleaf field is significant, which is what a specific mask needs.
	Flags     uint32                    `json:"flags"`
	Modifiers []fcCPUIDRegisterModifier `json:"modifiers"`
}

type fcCPUIDRegisterModifier struct {
	Register string `json:"register"`
	// Bitmap is cpuBitmapWidth characters of '0', '1' or 'x', most significant
	// bit first, prefixed with "0b_". 'x' preserves the host's bit.
	Bitmap string `json:"bitmap"`
}

// cpuBitmapWidth is how many bit positions Firecracker's bitmap parser accepts.
//
// It is 31, not 32, despite the registers being 32 bits wide: a 32-character
// bitmap is rejected as "string is too long" (verified against Firecracker
// v1.15.1). The consequence is that bit 31 cannot be masked at all, which the
// mask table has to work around rather than silently mis-align around.
const cpuBitmapWidth = 31

// cpuConfigFor renders a template as a Firecracker /cpu-config body, or nil when
// there is nothing to mask.
//
// Masks for the same leaf/register are merged into one bitmap. Sending one entry
// per bit would let a later entry's 'x' positions overwrite an earlier entry's
// cleared bit, so the merge is what makes the result mean what it says.
func cpuConfigFor(t CPUTemplate) *fcCPUConfig {
	if t != CPUTemplatePortable {
		return nil
	}
	type key struct {
		leaf, subleaf uint32
		register      string
	}
	bits := map[key][]uint{}
	var order []key
	for _, m := range portableCPUMask {
		k := key{m.Leaf, m.Subleaf, m.Register}
		if _, seen := bits[k]; !seen {
			order = append(order, k)
		}
		bits[k] = append(bits[k], m.Bit)
	}

	byLeaf := map[[2]uint32]*fcCPUIDModifier{}
	cfg := &fcCPUConfig{}
	for _, k := range order {
		lk := [2]uint32{k.leaf, k.subleaf}
		entry, ok := byLeaf[lk]
		if !ok {
			cfg.CPUIDModifiers = append(cfg.CPUIDModifiers, fcCPUIDModifier{
				Leaf:    fmt.Sprintf("0x%x", k.leaf),
				Subleaf: fmt.Sprintf("0x%x", k.subleaf),
				Flags:   1,
			})
			entry = &cfg.CPUIDModifiers[len(cfg.CPUIDModifiers)-1]
			byLeaf[lk] = entry
		}
		entry.Modifiers = append(entry.Modifiers, fcCPUIDRegisterModifier{
			Register: k.register,
			Bitmap:   clearBitsBitmap(bits[k]),
		})
	}
	return cfg
}

// clearBitsBitmap renders a mask that zeroes the given bit positions and leaves
// the rest as the host reports them.
//
// Positions at or above cpuBitmapWidth are dropped: Firecracker cannot express
// them. maskableBit is what keeps such a feature from being listed as masked
// when it is not.
func clearBitsBitmap(positions []uint) string {
	b := make([]byte, cpuBitmapWidth)
	for i := range b {
		b[i] = 'x'
	}
	// Index 0 is the most significant expressible bit, so bit n sits at
	// (cpuBitmapWidth-1)-n.
	for _, p := range positions {
		if maskableBit(p) {
			b[cpuBitmapWidth-1-p] = '0'
		}
	}
	return "0b_" + string(b)
}

// maskableBit reports whether Firecracker's bitmap can address a bit position.
func maskableBit(p uint) bool { return p < cpuBitmapWidth }

// MaskedCPUFeatures returns the feature names a template hides, for the
// snapshot manifest and for tests that assert against a guest's /proc/cpuinfo.
func MaskedCPUFeatures(t CPUTemplate) []string {
	if t != CPUTemplatePortable {
		return nil
	}
	names := make([]string, 0, len(portableCPUMask))
	for _, m := range portableCPUMask {
		if !maskableBit(m.Bit) {
			continue
		}
		names = append(names, m.Name)
	}
	return names
}

// UnmaskableCPUFeatures returns features the template intends to hide but cannot,
// because Firecracker's bitmap cannot address their bit. A snapshot taken under
// this template still binds to these, so the list belongs in the manifest and in
// the node's startup log rather than only in a comment.
func UnmaskableCPUFeatures(t CPUTemplate) []string {
	if t != CPUTemplatePortable {
		return nil
	}
	var names []string
	for _, m := range portableCPUMask {
		if !maskableBit(m.Bit) {
			names = append(names, m.Name)
		}
	}
	return names
}

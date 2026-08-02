#!/usr/bin/env bash
# Probes what CPU masking a host actually supports.
#
# Every fact bean's CPU template code relies on was established by hand on one
# AMD EPYC 7542, and none of it is documented anywhere authoritative: which of
# Firecracker's built-in templates a given CPU accepts, how wide a custom bitmap
# may be, and which features a mask really removes from a guest. On a different
# CPU the answers differ, and the failures are quiet — a rejected /cpu-config
# leaves guests unmasked, and a mis-aligned bitmap masks the wrong features.
#
# So this exists to re-derive those answers per host rather than to trust the
# ones recorded in docs/decisions.md §6.
#
# Usage:
#   hack/cpu-template-probe.sh [--firecracker PATH] [--kernel PATH]
#
# Requires: firecracker binary, a guest kernel, /dev/kvm, curl, python3.
# Read-only with respect to bean: it starts throwaway VMMs in a temp directory
# and touches no bean state.
set -uo pipefail

FC_BIN="${FC_BIN:-/var/lib/bean/assets/firecracker}"
KERNEL="${KERNEL:-}"
WORK="$(mktemp -d /tmp/cpu-probe.XXXXXX)"
FAILED=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --firecracker) FC_BIN="$2"; shift 2 ;;
    --kernel) KERNEL="$2"; shift 2 ;;
    -h|--help) sed -n '2,20p' "$0" | sed 's/^# \?//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 64 ;;
  esac
done

cleanup() {
  # Kill only the VMMs this script started, identified by their socket paths
  # under our own temp directory — a pkill on "firecracker" would take out
  # every sandbox on the node.
  for sock in "$WORK"/*.sock; do
    [[ -e "$sock" ]] || continue
    pkill -f -- "$sock" 2>/dev/null
  done
  rm -rf "$WORK"
}
trap cleanup EXIT

say() { printf '%s\n' "$*"; }
hr() { printf '%s\n' "------------------------------------------------------------"; }

require() {
  command -v "$1" >/dev/null 2>&1 || { say "missing required tool: $1"; exit 69; }
}
require curl
require python3

[[ -x "$FC_BIN" ]] || { say "firecracker not executable: $FC_BIN"; exit 69; }
[[ -r /dev/kvm ]] || { say "/dev/kvm not readable; probing needs KVM"; exit 69; }

if [[ -z "$KERNEL" ]]; then
  # Boot probes need a kernel; the config probes do not. Pick the newest asset
  # rather than pinning a version that may have been replaced.
  KERNEL="$(ls -1t /var/lib/bean/assets/vmlinux* 2>/dev/null | grep -v '\.config$' | head -1)"
fi

# start_vmm launches a throwaway VMM and sets VMM_SOCK to its API socket.
#
# The socket comes back in a global rather than on stdout: capturing this
# function with $(...) would run it in a subshell, and the backgrounded VMM
# started there does not reliably outlive it.
VMM_SOCK=""
start_vmm() {
  local name="$1"
  VMM_SOCK="$WORK/$name.sock"
  setsid "$FC_BIN" --api-sock "$VMM_SOCK" >"$WORK/$name.log" 2>&1 &
  # Poll for the socket instead of sleeping a fixed amount: the wait is
  # milliseconds, and a fixed sleep is either wasteful or flaky.
  for _ in $(seq 1 50); do
    [[ -S "$VMM_SOCK" ]] && return 0
    sleep 0.1
  done
  return 1
}

# fault extracts Firecracker's error text from a JSON response.
fault() {
  python3 -c '
import json, sys
raw = sys.argv[1]
try:
    print(json.loads(raw).get("fault_message", raw)[:110])
except Exception:
    print(raw[:110])' "$1"
}

# api PUTs a body to a VMM endpoint and echoes the response body.
api() {
  local sock="$1" path="$2" body="$3"
  curl -s --unix-socket "$sock" -X PUT "http://localhost$path" \
    -H "Content-Type: application/json" -d "$body" 2>&1
}

say "host CPU"
hr
python3 - <<'PY'
import re
info = {}
for line in open("/proc/cpuinfo"):
    if ":" not in line:
        continue
    k, v = (s.strip() for s in line.split(":", 1))
    if k in ("vendor_id", "cpu family", "model", "model name", "stepping") and k not in info:
        info[k] = v
for k in ("vendor_id", "cpu family", "model", "model name", "stepping"):
    print(f"  {k:12} {info.get(k, '(absent)')}")
PY
say ""

say "built-in templates: does the VM start?"
hr
say "  A template is only usable if InstanceStart succeeds. PUT /machine-config"
say "  accepts every name regardless of the host CPU, so the vendor check"
say "  happens at start and testing only the config would report false support."
say ""
BOOTABLE=""
for T in T2 C3 T2S T2CL T2A; do
  start_vmm "builtin-$T" || { say "  $T: could not start a VMM"; continue; }
  cfg="$(api "$VMM_SOCK" /machine-config \
    "{\"vcpu_count\":1,\"mem_size_mib\":128,\"cpu_template\":\"$T\"}")"
  if [[ -n "$cfg" ]]; then
    say "  $T: rejected at config: $(fault "$cfg")"
    continue
  fi
  if [[ -z "$KERNEL" || ! -r "$KERNEL" ]]; then
    say "  $T: config accepted (no kernel available to test the boot)"
    continue
  fi
  api "$VMM_SOCK" /boot-source \
    "{\"kernel_image_path\":\"$KERNEL\",\"boot_args\":\"quiet reboot=k panic=-1 pci=off\"}" >/dev/null
  start="$(api "$VMM_SOCK" /actions '{"action_type":"InstanceStart"}')"
  if [[ -z "$start" ]]; then
    say "  $T: BOOTS"
    BOOTABLE="$BOOTABLE $T"
  else
    say "  $T: $(fault "$start")"
  fi
done
say ""
if [[ -z "$BOOTABLE" ]]; then
  say "  RESULT: no built-in template can start a VM here, which is why bean"
  say "  uses a custom /cpu-config template instead of a named one."
else
  say "  RESULT: built-in templates usable here:$BOOTABLE"
  say "  bean still uses a custom template — a built-in one ties portability"
  say "  to whichever CPU models AWS chose to support."
fi
say ""

say "custom template: widest accepted bitmap"
hr
say "  bean masks features through PUT /cpu-config, whose bitmap width is not"
say "  documented. It is narrower than the 32-bit register on the reference"
say "  host, which means the top bit cannot be masked at all — a fact worth"
say "  re-deriving rather than assuming."
say ""
widest=0
for N in 30 31 32 33; do
  start_vmm "width-$N" || continue
  bm="0b_$(python3 -c "print('x'*($N-1)+'0')")"
  resp="$(api "$VMM_SOCK" /cpu-config \
    "{\"cpuid_modifiers\":[{\"leaf\":\"0x1\",\"subleaf\":\"0x0\",\"flags\":1,\"modifiers\":[{\"register\":\"ecx\",\"bitmap\":\"$bm\"}]}]}")"
  if [[ -z "$resp" ]]; then
    say "  $N bits: accepted"
    (( N > widest )) && widest=$N
  else
    say "  $N bits: rejected — $(fault "$resp")"
  fi
done
say ""
if (( widest == 0 )); then
  say "  RESULT: /cpu-config rejected every width — custom masking unavailable here."
  FAILED=1
else
  say "  RESULT: widest accepted bitmap is $widest bits (bean uses cpuBitmapWidth)."
  if (( widest != 31 )); then
    say "  MISMATCH: bean's cpu_template.go hardcodes 31. Update cpuBitmapWidth"
    say "  and its comment, then re-run the runtime tests."
    FAILED=1
  fi
fi
say ""

say "what a guest actually sees"
hr
say "  The only proof that masking worked is the guest's own view. Comparing"
say "  an unmasked boot against a masked one is what catches a bitmap that was"
say "  accepted but aligned wrongly: it would mask real features, just not the"
say "  intended ones."
say ""
say "  This compares the host's own flags against what bean's mask would remove."
say "  It reads /proc/cpuinfo rather than booting a guest, so it says which"
say "  masked features this host even has — masking one it lacks proves nothing,"
say "  and a guest check that passes for that reason is a false positive."
say ""
python3 - <<'PY'
# The list mirrors portableCPUMask in internal/node/runtime/cpu_template.go.
# It is duplicated rather than imported because this script has to run on a host
# that has firecracker but not a Go toolchain.
MASKED = ["avx", "avx2", "avx512f", "avx512dq", "avx512ifma",
          "avx512cd", "avx512bw", "fma", "f16c"]
# Masking these would be a bug: every Firecracker-capable host has them, and
# xsave specifically cannot be masked coherently — its sub-features live in
# CPUID leaf 0xD and stay visible, describing a CPU that does not exist.
KEEP = ["sse2", "xsave", "fpu", "cmov"]

flags = set()
for line in open("/proc/cpuinfo"):
    if line.startswith("flags"):
        flags = set(line.split(":", 1)[1].split())
        break

present = [f for f in MASKED if f in flags]
absent = [f for f in MASKED if f not in flags]
print(f"  masked features this host has:  {' '.join(present) or '(none)'}")
print(f"  masked features it lacks:       {' '.join(absent) or '(none)'}")
print(f"  must survive masking:           {' '.join(f for f in KEEP if f in flags)}")
print()
if not present:
    print("  WARNING: this host has none of the features bean masks, so a guest")
    print("  check here cannot tell a working mask from a broken one. Verify on")
    print("  a host with at least AVX before trusting --cpu-template.")
else:
    print("  To confirm end to end, boot a sandbox under each setting and compare:")
    print("    noded --cpu-template none      -> bean exec SBX -- grep -m1 flags /proc/cpuinfo")
    print(f"      expect to see: {' '.join(present)}")
    print("    noded --cpu-template portable -> same command")
    print("      expect those gone, and sse2/xsave still present")
PY
say ""

hr
if (( FAILED )); then
  say "PROBE FOUND A MISMATCH — see the notes above before trusting"
  say "--cpu-template on this host."
  exit 70
fi
say "Probe complete. No contradictions with bean's recorded assumptions."

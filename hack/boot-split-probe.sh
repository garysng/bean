#!/usr/bin/env bash
# Splits a cold create's 770ms into kernel time and userspace time.
#
# Why this is worth measuring before optimising anything: the 770ms figure covers
# `InstanceStart` → a reachable agent, and it has been loosely described as "kernel
# boot". That is almost certainly wrong. Firecracker's own SPECIFICATION.md
# promises ≤125ms from InstanceStart to the guest executing /sbin/init, and a
# published experiment reaches an 8.5ms kernel boot with a trimmed config. If the
# kernel is ~100ms of our 770ms, then trimming it — which needs a self-built kernel
# toolchain we deliberately do not have (decisions.md §1.3) — attacks the small
# part, and the remaining ~650ms of guest userspace is where the cheap wins are.
#
# Method: boot with the serial console on and read the kernel's own timestamps.
# `[    0.123456]` prefixes give kernel time directly, and the agent's first log
# line marks the point the platform actually cares about. The console costs ~493ms
# of boot on its own (measured), so the absolute total here is NOT comparable to a
# production create — but the split between kernel and userspace is.
#
# Usage: boot-split-probe.sh [--runs N]
set -uo pipefail

RUNS=${RUNS:-3}
IMAGE=${IMAGE:-alpine:3.20}
BASE_URL=${BEAN_BASE_URL:-http://127.0.0.1:18080}
API_KEY=${BEAN_API_KEY:-devkey}
BEAN=${BEAN:-/tmp/bean}
SBXDIR=${SBXDIR:-/var/lib/bean/sandboxes}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --runs) RUNS="$2"; shift 2 ;;
    -h|--help) sed -n '2,20p' "$0" | sed 's/^# \?//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 64 ;;
  esac
done

export BEAN_BASE_URL="$BASE_URL" BEAN_API_KEY="$API_KEY"
say() { printf '%s\n' "$*"; }
hr() { printf -- '------------------------------------------------------------\n'; }

[[ -x "$BEAN" ]] || { say "bean CLI not executable: $BEAN"; exit 69; }

say "This needs the node started with --debug-console, otherwise the guest emits"
say "no kernel log and there is nothing to read timestamps from."
hr

create_small() {
  curl -s -X POST "$BASE_URL/v1/sandboxes" \
    -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
    -d "{\"image\":\"$IMAGE\",\"resources\":{\"diskMiB\":2048}}" 2>/dev/null |
    python3 -c 'import json,sys; print(json.load(sys.stdin).get("sandbox",{}).get("id",""))' 2>/dev/null
}

ids=()
for i in $(seq 1 "$RUNS"); do
  sbx=$(create_small)
  if [[ -z "$sbx" ]]; then say "create $i failed"; continue; fi
  ids+=("$sbx")
  say "run $i: $sbx"
done
hr

for sbx in "${ids[@]}"; do
  log="$SBXDIR/$sbx/console.log"
  if [[ ! -s "$log" ]]; then
    say "$sbx: no console output — was the node started with --debug-console?"
    continue
  fi
  say "$sbx"
  # Kernel timestamps are relative to the kernel's own start, so the last one
  # before userspace hands off is the kernel's share. "Run /sbin/init" or the
  # freeing of unused kernel memory are the conventional markers.
  python3 - "$log" <<'PY'
import re, sys

stamped = re.compile(r'^\[\s*(\d+\.\d+)\]\s*(.*)$')
first = last = None
init_at = None
agent_at = None
for line in open(sys.argv[1], errors='replace'):
    m = stamped.match(line.strip())
    if not m:
        continue
    t, msg = float(m.group(1)), m.group(2)
    if first is None:
        first = t
    last = t
    # The handoff to userspace. Different kernels word this differently, so
    # several markers are accepted rather than one exact string.
    if init_at is None and (
            'Run /sbin/init' in msg
            or 'Freeing unused kernel' in msg
            or 'Kernel command line' in msg and False):
        init_at = t
    if agent_at is None and ('beand' in msg or 'bean' in msg.lower()):
        agent_at = t

if first is None:
    print("  no timestamped kernel lines found")
    sys.exit(0)

print(f"  first kernel line   {first*1000:8.1f} ms")
if init_at is not None:
    print(f"  handoff to init     {init_at*1000:8.1f} ms   ← kernel's share")
else:
    print("  handoff to init      not found (no 'Run /sbin/init' or "
          "'Freeing unused kernel' line)")
print(f"  last kernel line    {last*1000:8.1f} ms")
if agent_at is not None:
    print(f"  agent mentioned     {agent_at*1000:8.1f} ms")
if init_at is not None:
    print(f"  → userspace so far  {(last-init_at)*1000:8.1f} ms "
          f"(to the last kernel message, not to a reachable agent)")
PY
done

hr
say "Reading this: if the handoff to init is a small fraction of the create's"
say "measured 770ms, the kernel is not the problem and a trimmed kernel would buy"
say "little. The remainder is guest userspace — the agent's own startup, the mount"
say "matrix and the pivot — which is ours to shorten without a kernel toolchain."
say ""
say "Either way the conclusion for task #44 holds: not booting at all removes"
say "both parts, which is what e2b does (real boot only at template-build time)."

for sbx in "${ids[@]}"; do
  "$BEAN" kill "$sbx" >/dev/null 2>&1 &
done
wait

#!/usr/bin/env bash
# Drives N concurrent sandbox creates against a running stack and reports the
# latency distribution, then checks that tearing them down leaves nothing behind.
#
# Every performance number bean quotes — 952 ms create, 950 ms restore — was
# measured one sandbox at a time. That says nothing about what happens when a
# batch arrives, which is the only way this platform is ever used. This script
# exists to produce that number.
#
# Usage:
#   hack/stress-fc.sh [--count N] [--image REF] [--keep]
#
# Requires a stack already running (hack/dev-fc-stack.sh start) and the bean CLI.
# Safety: it only ever destroys sandboxes it created, and only inspects host
# resources — a pkill on firecracker would take out every sandbox on the node.
set -uo pipefail

COUNT=${COUNT:-20}
IMAGE=${IMAGE:-alpine:3.20}
# The per-sandbox disk request is what limits density, not max_creates: the
# scheduler reserves the nominal size while the sparse layer actually costs
# kilobytes, so the default 20 GiB request exhausts a 100 GiB node after five
# sandboxes. Overriding it is how this script reaches a concurrency worth
# measuring on a node whose disk figure has not been tuned.
DISK_MIB=${DISK_MIB:-}
BASE_URL=${BEAN_BASE_URL:-http://127.0.0.1:18080}
API_KEY=${BEAN_API_KEY:-devkey}
BEAN=${BEAN:-/tmp/bean}
KEEP=0
WORK="$(mktemp -d /tmp/bean-stress.XXXXXX)"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --count) COUNT="$2"; shift 2 ;;
    --image) IMAGE="$2"; shift 2 ;;
    --disk-mib) DISK_MIB="$2"; shift 2 ;;
    --keep) KEEP=1; shift ;;
    -h|--help) sed -n '2,16p' "$0" | sed 's/^# \?//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 64 ;;
  esac
done

export BEAN_BASE_URL="$BASE_URL" BEAN_API_KEY="$API_KEY"

say() { printf '%s\n' "$*"; }
hr() { printf '%s\n' "------------------------------------------------------------"; }

cleanup_workdir() { rm -rf "$WORK"; }
trap cleanup_workdir EXIT

[[ -x "$BEAN" ]] || { say "bean CLI not executable: $BEAN"; exit 69; }

# ---- baseline: what the host holds before we start -------------------------
# Recorded so the leak check compares against reality rather than against zero:
# the host runs other projects' containers, and the shared base image is
# legitimately attached.
before_dm=$(dmsetup ls 2>/dev/null | grep -c '^bean-' || true)
before_fc=$(pgrep -c firecracker 2>/dev/null || true)
before_loop=$(losetup -a 2>/dev/null | grep -c . || true)

say "stress: $COUNT concurrent creates of $IMAGE"
say "baseline: dm=$before_dm fc=$before_fc loop=$before_loop"
hr

# ---- fire N creates at once ------------------------------------------------
# Each worker writes "<millis> <exit> <id-or-error>" so a failure is as visible
# as a slow success. curl does the timing rather than the shell, since a shell
# subprocess launch is tens of milliseconds and would blur a 952 ms figure.
create_one() {
  local i="$1" out="$WORK/create.$1"
  local body="{\"image\":\"$IMAGE\"}"
  if [[ -n "$DISK_MIB" ]]; then
    body="{\"image\":\"$IMAGE\",\"resources\":{\"diskMiB\":$DISK_MIB}}"
  fi
  local resp
  resp=$(curl -s -o "$WORK/body.$i" -w '%{time_total} %{http_code}' \
    -X POST "$BASE_URL/v1/sandboxes" \
    -H "Authorization: Bearer $API_KEY" \
    -H "Content-Type: application/json" \
    -d "$body" 2>/dev/null)
  local secs code
  secs=$(printf '%s' "$resp" | cut -d' ' -f1)
  code=$(printf '%s' "$resp" | cut -d' ' -f2)
  local ms
  ms=$(python3 -c "print(int(float('${secs:-0}')*1000))" 2>/dev/null || echo 0)
  local id
  id=$(python3 -c '
import json,sys
try:
    d=json.load(open(sys.argv[1]))
    print(d.get("sandbox",{}).get("id") or d.get("code") or "?")
except Exception:
    print("parse-error")' "$WORK/body.$i" 2>/dev/null || echo "?")
  printf '%s %s %s\n' "$ms" "$code" "$id" > "$out"
}

started=$(date +%s)
for i in $(seq 1 "$COUNT"); do
  create_one "$i" &
done
wait
elapsed=$(( $(date +%s) - started ))

# ---- results ---------------------------------------------------------------
cat "$WORK"/create.* > "$WORK/all" 2>/dev/null
python3 - "$WORK/all" "$COUNT" "$elapsed" <<'PY'
import sys, collections
rows = []
for line in open(sys.argv[1]):
    parts = line.split(None, 2)
    if len(parts) < 3:
        continue
    rows.append((int(parts[0]), parts[1], parts[2].strip()))
total, elapsed = int(sys.argv[2]), int(sys.argv[3])

ok = [r for r in rows if r[1] == "201"]
bad = [r for r in rows if r[1] != "201"]

def pct(values, p):
    if not values:
        return 0
    s = sorted(values)
    # Nearest-rank: with 20 samples an interpolated p99 is invented precision.
    k = min(len(s) - 1, int(round(p / 100 * (len(s) - 1))))
    return s[k]

lat = [r[0] for r in ok]
print(f"requests   {len(rows)}/{total}   wall {elapsed}s")
print(f"succeeded  {len(ok)}")
print(f"failed     {len(bad)}")
if lat:
    print(f"p50        {pct(lat,50)} ms")
    print(f"p95        {pct(lat,95)} ms")
    print(f"p99        {pct(lat,99)} ms")
    print(f"max        {max(lat)} ms")
    print(f"min        {min(lat)} ms")
if bad:
    print("\nfailures by code and reason:")
    for (code, reason), n in collections.Counter(
            (r[1], r[2][:60]) for r in bad).most_common():
        print(f"  {n:4d}  HTTP {code}  {reason}")
PY

# ---- teardown and leak check ----------------------------------------------
ids=$(awk '$2 == "201" { print $3 }' "$WORK/all" 2>/dev/null)
count_ids=$(printf '%s\n' "$ids" | grep -c . || true)

if [[ "$KEEP" == "1" ]]; then
  hr
  say "--keep: leaving $count_ids sandboxes running"
  exit 0
fi

hr
say "destroying $count_ids sandboxes"
destroy_start=$(date +%s)
for id in $ids; do
  "$BEAN" kill "$id" >/dev/null 2>&1 &
done
wait
say "destroy wall: $(( $(date +%s) - destroy_start ))s"

# Settle: destroy is asynchronous on the node side, so an immediate check would
# report mappings that are on their way out.
sleep 3

after_dm=$(dmsetup ls 2>/dev/null | grep -c '^bean-' || true)
after_fc=$(pgrep -c firecracker 2>/dev/null || true)
after_loop=$(losetup -a 2>/dev/null | grep -c . || true)
deleted_loop=$(losetup -a 2>/dev/null | grep -c deleted || true)

hr
say "resource check (baseline → after)"
say "  dm mappings   $before_dm → $after_dm"
say "  firecracker   $before_fc → $after_fc"
say "  loop devices  $before_loop → $after_loop"
say "  loop holding deleted files: $deleted_loop"

leaked=0
[[ "$after_dm" -gt "$before_dm" ]] && { say "LEAK: $((after_dm - before_dm)) dm mappings"; leaked=1; }
[[ "$after_fc" -gt "$before_fc" ]] && { say "LEAK: $((after_fc - before_fc)) firecracker processes"; leaked=1; }
[[ "$deleted_loop" -gt 0 ]] && { say "LEAK: $deleted_loop loop devices hold deleted files"; leaked=1; }

if [[ "$leaked" == "1" ]]; then
  say ""
  say "Leaked resources are listed below; they are NOT cleaned up automatically"
  say "because a wrong guess here would break sandboxes belonging to other work."
  dmsetup ls 2>/dev/null | grep '^bean-' || true
  losetup -a 2>/dev/null | grep deleted || true
  exit 70
fi

say "no leaks"

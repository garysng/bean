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
# noded's metrics, for reading the phase histograms it already keeps rather than
# timing the same things again from outside.
METRICS_URL=${METRICS_URL:-http://127.0.0.1:17444/metrics}
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

# cpu_seconds sums host CPU across noded and every firecracker on the host.
#
# Including the VMMs is the whole point. Firecracker runs as a separate process, so
# the guest kernel's boot is spent there and noded's own utime+stime cannot see it:
# measuring only noded reported 0.01 s for a create whose real cost is closer to
# 0.6 s, three orders of magnitude low, and made a restore look *more* expensive than
# a boot because noded does more orchestration on the restore path. That mistake was
# made in hack/warm-snapshot-probe.sh first; this is the corrected form.
#
# Summed across all firecracker processes rather than tracked per sandbox, because the
# pid of a given sandbox's VMM is not knowable from here. Sandboxes belonging to other
# work inflate before and after equally, so a delta is still attributable.
cpu_seconds() {
  local tck total=0 p
  tck=$(getconf CLK_TCK)
  for p in $(pgrep -f 'noded' 2>/dev/null; pgrep -x firecracker 2>/dev/null); do
    [[ -r "/proc/$p/stat" ]] || continue
    total=$(awk -v acc="$total" -v t="$tck" \
      '{printf "%.4f", acc + ($14 + $15) / t}' "/proc/$p/stat" 2>/dev/null || echo "$total")
  done
  echo "$total"
}

# phase_p50 reads a phase's median from noded's own histogram, so the script reports
# where the time went without adding timing code of its own. The phases are already
# instrumented in internal/node/manager.go (runtime_create, agent_ready,
# network_setup, total).
#
# Nearest bucket rather than an interpolation: a Prometheus histogram bounds the
# median between two edges and inventing a value inside them would be false precision,
# which is the same reason the latency percentiles below use nearest-rank.
phase_p50() {
  local phase="$1"
  curl -sf "$METRICS_URL" 2>/dev/null | python3 -c '
import sys, re
phase = sys.argv[1]
buckets = []
total = None
for line in sys.stdin:
    if "phase=\"%s\"" % phase not in line:
        continue
    m = re.search(r"_bucket\{.*le=\"([^\"]+)\".*\} ([0-9.e+]+)", line)
    if m:
        buckets.append((float(m.group(1)), float(m.group(2))))
        continue
    m = re.search(r"_count\{.*\} ([0-9.e+]+)", line)
    if m:
        total = float(m.group(1))
if not buckets or not total:
    print("n/a")
    sys.exit()
buckets.sort()
for edge, count in buckets:
    if count >= total / 2:
        print("<=%gs" % edge)
        break
else:
    print(">%gs" % buckets[-1][0])
' "$phase" 2>/dev/null || echo "n/a"
}

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
  # imageRef, not image: the field was renamed when templates landed, and the old name is
  # silently ignored rather than rejected -- every create then fails for want of an image
  # reference, which reads like a broken node rather than a stale script.
  local body="{\"imageRef\":\"$IMAGE\"}"
  if [[ -n "$DISK_MIB" ]]; then
    body="{\"imageRef\":\"$IMAGE\",\"resources\":{\"diskMiB\":$DISK_MIB}}"
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

# Wall clock in nanoseconds, because a 100-create burst on a large host can finish
# in under a second and integer seconds would round the throughput figure to
# infinity or zero.
cpu_before=$(cpu_seconds)
started_ns=$(date +%s%N)
for i in $(seq 1 "$COUNT"); do
  create_one "$i" &
done
wait
elapsed_ns=$(( $(date +%s%N) - started_ns ))
cpu_after=$(cpu_seconds)
elapsed=$(( elapsed_ns / 1000000000 ))
elapsed_s=$(python3 -c "print(f'{$elapsed_ns/1e9:.3f}')")
cpu_used=$(python3 -c "print(f'{$cpu_after - $cpu_before:.2f}')")
cores=$(nproc 2>/dev/null || echo 0)

# ---- results ---------------------------------------------------------------
cat "$WORK"/create.* > "$WORK/all" 2>/dev/null
python3 - "$WORK/all" "$COUNT" "$elapsed_s" <<'PY'
import sys, collections
rows = []
for line in open(sys.argv[1]):
    parts = line.split(None, 2)
    if len(parts) < 3:
        continue
    rows.append((int(parts[0]), parts[1], parts[2].strip()))
total, elapsed = int(sys.argv[2]), float(sys.argv[3])

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
print(f"requests   {len(rows)}/{total}   wall {elapsed:.3f}s")
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

# ---- throughput attribution ------------------------------------------------
# This is the section the script was extended for. The latency figures above say how
# long one create took under load; they cannot say what bounds the rate, and the claim
# under test -- "throughput is about cores / CPU-seconds-per-create" -- is about the
# rate.
hr
succeeded=$(awk '$2 == "201"' "$WORK/all" 2>/dev/null | grep -c . || echo 0)
say "throughput attribution"
say "  cores available      $cores"
say "  wall                 ${elapsed_s}s"
say "  host cpu consumed    ${cpu_used}s  (noded + every firecracker)"
if [[ "$succeeded" -gt 0 ]]; then
  python3 - "$succeeded" "$elapsed_s" "$cpu_used" "$cores" <<'PYA'
import sys
ok, wall, cpu, cores = int(sys.argv[1]), float(sys.argv[2]), float(sys.argv[3]), int(sys.argv[4])
per = cpu / ok if ok else 0
rate = ok / wall if wall > 0 else 0
print(f"  cpu per create       {per:.3f}s")
print(f"  observed throughput  {rate:.2f} creates/s")
if per > 0 and cores > 0:
    # The claim, stated as a prediction so the run can contradict it. If a create
    # costs P CPU-seconds and nothing else binds, C cores should sustain C/P per
    # second.
    predicted = cores / per
    print(f"  predicted by cpu     {predicted:.2f} creates/s   (cores / cpu-per-create)")
    if predicted > 0:
        ratio = rate / predicted
        print(f"  observed / predicted {ratio:.2f}")
        if ratio < 0.5:
            print()
            print("  The run is far below what its own CPU cost predicts, so host CPU")
            print("  is NOT the binding constraint here. Something else serialises the")
            print("  create path -- a lock, the SQLite write, or a per-node limit other")
            print("  than create concurrency. The 'cores / cpu-per-create' claim in")
            print("  README.md, docs/status.md and --create-wait's help text is wrong as")
            print("  stated and needs correcting rather than explaining away.")
        elif ratio > 2:
            print()
            print("  Faster than the CPU cost predicts, which means the per-create CPU")
            print("  figure is not the whole story: creates overlap on something that is")
            print("  not CPU (disk, or waiting on the agent), so the cost is not additive.")
        else:
            print()
            print("  Consistent with host CPU being the binding constraint.")
PYA
fi
say ""
say "  where the time went (noded's own histograms, median bucket)"
for phase in total runtime_create agent_ready network_setup; do
  printf '    %-18s %s\n' "$phase" "$(phase_p50 "$phase")"
done

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

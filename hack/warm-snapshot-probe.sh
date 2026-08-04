#!/bin/bash
# Checks the one claim warm snapshots are for: that a create stops booting a kernel.
#
# The reason this needs a script rather than a unit test is that "it was faster" is
# not the claim and cannot establish it. A second create of the same image is faster
# anyway -- page cache is warm, the image file is local, the device-mapper base is
# already set up. So this asserts on the *counters* that say which path ran:
#
#   bean_node_warm_lookups_total{outcome="hit"}    a create restored
#   bean_node_warm_lookups_total{outcome="miss"}   a create booted
#
# and separately on host CPU, because CPU-seconds are what bounds throughput and
# they are the thing a warm snapshot removes. Wall-clock latency is reported but is
# not what passes or fails this probe.
#
# Cleans up only the sandboxes it created. Other sandboxes and the shared assets on
# this host belong to other work.
set -u

REPO=${REPO:-/root/bean-net}
# The image must have a digest recorded in its sidecar, or it cannot be warmed at
# all and every check below passes vacuously -- which is exactly what happened on
# the first run of this probe against busybox:1.35, converted before digests were
# recorded. So the probe converts its own image under a fresh tag rather than
# trusting whatever the host already holds. IMAGE names what to pull; PROBE_TAG is
# the local name it is registered under.
IMAGE=${IMAGE:-busybox:1.35}
PROBE_TAG=${PROBE_TAG:-}
ASSETS=${ASSETS:-/var/lib/bean/assets}
API_PORT=${API_PORT:-18080}
METRICS_PORT=${METRICS_PORT:-17444}
API_KEY=${API_KEY:-devkey}
BIN=${BIN:-/tmp}
RUN=${RUN:-/tmp/beanrun}
BEAN="$BIN/bean"

CREATED=()
FAILED=0

note() { printf '\n=== %s\n' "$1"; }

check() {
  local label=$1 want=$2 got=$3 proves=$4
  if [ "$got" = "$want" ]; then
    printf 'PASS  %-36s %s\n' "$label" "$proves"
  else
    printf 'FAIL  %-36s want=%s got=%s  (%s)\n' "$label" "$want" "$got" "$proves"
    FAILED=$((FAILED + 1))
  fi
}

cleanup() {
  if [ ${#CREATED[@]} -gt 0 ]; then
    note "killing the ${#CREATED[@]} sandbox(es) this probe created"
    for s in "${CREATED[@]}"; do
      "$BEAN" kill "$s" >/dev/null 2>&1 || true
    done
  fi
}
trap cleanup EXIT

# counter reads one labelled sample from noded's metrics endpoint, defaulting to 0
# when the series does not exist yet -- a counter that has never been incremented is
# absent rather than zero in the Prometheus text format.
counter() {
  local outcome=$1
  local v
  v=$(curl -sf "http://127.0.0.1:$METRICS_PORT/metrics" 2>/dev/null \
    | awk -v o="outcome=\"$outcome\"" \
        '$0 ~ /^bean_node_warm_lookups_total/ && $0 ~ o {print $2; f=1}
         END {if (!f) print 0}' | head -1)
  echo "${v:-0}"
}

# cpu_seconds sums host CPU across noded *and every firecracker on the host*.
#
# Including the VMMs is the whole point and was got wrong on the first attempt.
# Firecracker runs as a separate process, a child of noded, so the guest kernel's
# boot is spent there and noded's own utime+stime cannot see it: measuring only
# noded reported 0.01s for a cold create, which is three orders of magnitude below
# the ~5 CPU-seconds a boot actually costs, and made a warm create look *more*
# expensive because noded does more orchestration work on the restore path.
#
# Sums all firecracker processes rather than tracking one, because the pid of the
# VMM for a given sandbox is not knowable from here, and any VMM running during the
# window is work this node is doing. Other sandboxes on a shared host inflate both
# measurements equally, so the comparison holds even though the absolute figures
# include them.
cpu_seconds() {
  local tck total=0 p
  tck=$(getconf CLK_TCK)
  for p in $(pgrep -f "$BIN/noded"; pgrep -x firecracker); do
    [ -r "/proc/$p/stat" ] || continue
    total=$(awk -v acc="$total" -v t="$tck" \
      '{printf "%.4f", acc + ($14 + $15) / t}' "/proc/$p/stat" 2>/dev/null || echo "$total")
  done
  echo "$total"
}

cd "$REPO" || exit 1

note "building"
for c in bean bean-api noded; do
  go build -o "$BIN/$c" "./cmd/$c" || exit 1
done

# The probe's first assertion is that a create with nothing warm *misses*, so a
# bundle left by a previous run makes it a hit and the run reports failure against a
# working implementation -- which is what happened on the second attempt here.
#
# Only this directory is touched, and only the bundles in it: they are derived state
# that a prewarm rebuilds in seconds, unlike the snapshot cache next door, which
# holds the node's copy of user checkpoints.
note "clearing warm bundles so the first create is genuinely a miss"
WARM_DIR=${WARM_DIR:-/var/lib/bean/sandboxes/.warm}
if [ -d "$WARM_DIR" ]; then
  found=$(find "$WARM_DIR" -maxdepth 1 -name '*.warm' | wc -l)
  find "$WARM_DIR" -maxdepth 1 -name '*.warm' -delete
  echo "removed $found bundle(s) from $WARM_DIR"
else
  echo "$WARM_DIR does not exist yet"
fi

note "starting the stack with warm snapshots on"
ASSETS="$ASSETS" \
NODED_FLAGS="--fc-warm-snapshots" \
  bash hack/dev-fc-stack.sh >/tmp/warm-stack.log 2>&1 || {
  tail -20 /tmp/warm-stack.log
  exit 1
}
grep -q 'warm snapshots on' "$RUN/noded.log" || {
  echo "noded did not report warm snapshots on; every create below would boot and"
  echo "the probe would be measuring nothing:"
  grep -i warm "$RUN/noded.log" | tail -3
  exit 1
}
echo "noded reports: $(grep 'warm snapshots on' "$RUN/noded.log" | tail -1)"

export BEAN_API_KEY="$API_KEY"
export BEAN_BASE_URL="http://127.0.0.1:$API_PORT"

# An image with no digest in its sidecar cannot be warmed, and the checks below
# would then all pass while proving nothing. Verified here rather than assumed,
# because that is what the first run of this probe did wrong.
note "confirming the image has a digest, without which it cannot be warmed"
sidecar_has_digest() {
  local ref=$1
  for f in /var/lib/bean/images/*.ref; do
    [ -f "$f" ] || continue
    if grep -q "\"ref\":\"$ref\"" "$f" 2>/dev/null; then
      grep -q '"digest"' "$f" && return 0
      return 1
    fi
  done
  return 1
}

if [ -z "$PROBE_TAG" ]; then
  if sidecar_has_digest "$IMAGE"; then
    PROBE_TAG="$IMAGE"
    echo "$IMAGE already has a digest recorded"
  else
    echo "$IMAGE has no digest (converted before digests were recorded, or built)."
    echo "Looking for any image that does, since a fresh conversion needs a registry."
    PROBE_TAG=$(for f in /var/lib/bean/images/*.ref; do
      grep -l '"digest"' "$f" 2>/dev/null >/dev/null && \
        sed -n 's/.*"ref":"\([^"]*\)".*/\1/p' "$f"
    done | head -1)
    if [ -z "$PROBE_TAG" ]; then
      echo "No image on this node has a digest. Pull one through the platform first;"
      echo "a conversion records the digest, an image converted before that change"
      echo "does not, and this probe cannot prove anything without one."
      exit 1
    fi
    echo "using $PROBE_TAG instead"
  fi
fi
IMAGE="$PROBE_TAG"
echo "probing with: $IMAGE"

# run creates a sandbox and records it for cleanup. Echoes the id.
run_sandbox() {
  local out id
  out=$("$BEAN" run --image "$IMAGE" 2>&1)
  id=$(printf '%s\n' "$out" | grep -oE 'sbx_[0-9a-f]{20}' | head -1)
  if [ -z "$id" ]; then
    printf '%s\n' "$out" | tail -3 >&2
    return 1
  fi
  CREATED+=("$id")
  echo "$id"
}

note "first create: no warm snapshot exists yet, so this must boot"
miss_before=$(counter miss)
hit_before=$(counter hit)
cpu_before=$(cpu_seconds)
t0=$(date +%s.%N)
first=$(run_sandbox) || exit 1
t1=$(date +%s.%N)
cold_cpu=$(echo "$(cpu_seconds) - $cpu_before" | bc)
cold_wall=$(echo "$t1 - $t0" | bc)
echo "sandbox: $first"
echo "wall ${cold_wall}s, host cpu ${cold_cpu}s"

check "the first create was a miss" "1" \
  "$(echo "$(counter miss) - $miss_before" | bc)" \
  "nothing was warm, so it booted"
check "the first create was not a hit" "0" \
  "$(echo "$(counter hit) - $hit_before" | bc)" \
  "there was nothing to hit"

note "prewarming, which boots one guest and checkpoints it"
prewarm_start=$(date +%s.%N)
if ! curl -sf -X POST -H "Authorization: Bearer $API_KEY" \
  -H 'Content-Type: application/json' \
  -d "{\"refs\":[\"$IMAGE\"]}" \
  "http://127.0.0.1:$API_PORT/v1/images/prewarm" >/tmp/warm-prewarm.json 2>&1; then
  echo "prewarm request rejected:"
  cat /tmp/warm-prewarm.json
  FAILED=$((FAILED + 1))
  exit 1
fi

# Prewarm is asynchronous, so wait for the bundle rather than for the request.
for _ in $(seq 1 120); do
  if grep -q 'warm snapshot stored' "$RUN/noded.log" 2>/dev/null; then break; fi
  sleep 1
done
echo "took $(echo "$(date +%s.%N) - $prewarm_start" | bc)s"
if ! grep -q 'warm snapshot stored' "$RUN/noded.log"; then
  echo "no warm snapshot was stored. Everything below would be a miss, which would"
  echo "pass a weaker version of this probe while proving nothing:"
  grep -iE 'warm|prewarm' "$RUN/noded.log" | tail -5
  FAILED=$((FAILED + 1))
  exit 1
fi
ls -la /var/lib/bean/sandboxes/.warm/ 2>/dev/null | tail -3

note "second create: a warm snapshot exists, so this must restore"
miss_before=$(counter miss)
hit_before=$(counter hit)
cpu_before=$(cpu_seconds)
t0=$(date +%s.%N)
second=$(run_sandbox) || exit 1
t1=$(date +%s.%N)
warm_cpu=$(echo "$(cpu_seconds) - $cpu_before" | bc)
warm_wall=$(echo "$t1 - $t0" | bc)
echo "sandbox: $second"
echo "wall ${warm_wall}s, host cpu ${warm_cpu}s"

check "the second create was a hit" "1" \
  "$(echo "$(counter hit) - $hit_before" | bc)" \
  "it restored the warm snapshot instead of booting"
check "the second create was not a miss" "0" \
  "$(echo "$(counter miss) - $miss_before" | bc)" \
  "the lookup found the bundle"

note "the guest a warm create produced has to be usable"
if out=$("$BEAN" exec "$second" -- sh -c 'echo warm-guest-ok' 2>&1); then
  check "the restored guest runs commands" "warm-guest-ok" \
    "$(echo "$out" | tr -d '[:space:]')" \
    "a restored guest is a working sandbox, not just a fast one"
else
  check "the restored guest runs commands" "warm-guest-ok" "exec failed" \
    "a restored guest is a working sandbox, not just a fast one"
fi

note "cpu attribution -- this is the number the feature exists for"
printf 'cold create: %ss host cpu\n' "$cold_cpu"
printf 'warm create: %ss host cpu\n' "$warm_cpu"
if [ "$(echo "$warm_cpu < $cold_cpu" | bc)" = "1" ]; then
  printf 'PASS  %-36s %s\n' "warm costs less host cpu" \
    "throughput is bounded by cpu, so this is the ceiling moving"
else
  printf 'FAIL  %-36s cold=%s warm=%s  (%s)\n' "warm costs less host cpu" \
    "$cold_cpu" "$warm_cpu" \
    "a restore that costs as much cpu as a boot has not removed the boot"
  FAILED=$((FAILED + 1))
fi
printf 'wall clock: cold %ss, warm %ss (reported, not asserted -- a second create\n' \
  "$cold_wall" "$warm_wall"
printf '            is faster anyway from page cache alone)\n'

note "result"
if [ "$FAILED" -eq 0 ]; then
  echo "ALL CHECKS PASSED: a warm create restores rather than boots"
else
  echo "$FAILED CHECK(S) FAILED"
fi
exit "$FAILED"

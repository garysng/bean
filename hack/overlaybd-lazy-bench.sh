#!/usr/bin/env bash
# Measures the create that lazy pull is actually for: layers already published, and a
# node that has never converted them.
#
# overlaybd-bench.sh compares dm-snapshot against overlaybd with conversion on the
# create path, and found create latency unchanged (12-32s both backends) -- the disk
# and CPU wins were real, the latency win was not, because both arms did the same
# conversion. That is the honest number for a cold fleet, and it is not the number the
# published-layer design claims. This measures the claim: prewarm once, wipe the node's
# layers, create again.
#
# Three arms:
#   dm-snapshot         the baseline, flattening every layer
#   overlaybd cold      conversion on the create path (reproduces the earlier result)
#   overlaybd published prewarmed and published, node's layer dir emptied
#
# The third arm's layer directory is wiped between prewarm and create, deliberately.
# Leaving it would measure a local-file read and pass for the wrong reason -- level 1
# of the lookup, not level 2. A run where the wipe is skipped is not this benchmark.
#
# Requires an S3-compatible store for the published arm. Credentials come from the
# environment, never a flag: a flag puts the secret key in the process command line,
# visible to every local user.
#
# Usage: BEAN_S3_ENDPOINT=http://127.0.0.1:9000 BEAN_S3_ACCESS_KEY=... \
#        BEAN_S3_SECRET_KEY=... overlaybd-lazy-bench.sh
set -uo pipefail

BIN=${BIN:-/tmp/beantest/bin}
STACK=${STACK:-$(dirname "$0")/dev-fc-stack.sh}
RUN=${RUN:-/tmp/beanrun}
IMAGES=${IMAGES:-"python:3.12-slim python:3.11-slim"}
# Docker Hub resets connections on large blob fetches often enough to lose a run: one
# arm of a measured comparison failing to network flake is a wasted 20 minutes, not a
# result. The registry fetch already retries internally; this retries the whole
# operation on top of it.
ATTEMPTS=${ATTEMPTS:-3}
IMAGE_DIR=/var/lib/bean/images
LAYER_DIR=$IMAGE_DIR/layers
OBD_BUCKET=${OBD_BUCKET:-bean-obd-layers}
export BEAN_BASE_URL=http://127.0.0.1:18080
export BEAN_API_KEY=devkey

: "${BEAN_S3_ENDPOINT:?set BEAN_S3_ENDPOINT for the published arm}"
: "${BEAN_S3_ACCESS_KEY:?set BEAN_S3_ACCESS_KEY}"
: "${BEAN_S3_SECRET_KEY:?set BEAN_S3_SECRET_KEY}"

OBD_FLAGS="--fc-overlaybd --fc-overlaybd-lazy-pull \
  --fc-overlaybd-s3-endpoint $BEAN_S3_ENDPOINT \
  --fc-overlaybd-s3-bucket $OBD_BUCKET \
  --fc-overlaybd-s3-path-style \
  --fc-overlaybd-read-url $BEAN_S3_ENDPOINT"

cleanup() { BIN=$BIN bash "$STACK" stop >/dev/null 2>&1; }
trap cleanup EXIT

allocated() { du -s --block-size=1 "$1" 2>/dev/null | awk '{print $1}'; }
human() { numfmt --to=iec-i --suffix=B "${1:-0}" 2>/dev/null || echo "${1}B"; }

start_stack() {  # start_stack <extra noded flags>
  rm -rf "$RUN" "$IMAGE_DIR"
  BIN=$BIN BUILDKIT_ADDR= NODED_FLAGS="$1" \
    bash "$STACK" start >"${TMPDIR:-/tmp}/lazybench-stack.log" 2>&1 || {
      echo "  stack failed to start"; tail -15 "${TMPDIR:-/tmp}/lazybench-stack.log"; return 1; }
}

# time_create runs one create and prints its wall-clock seconds, or "FAILED".
time_create() {  # time_create <image>
  local img=$1 t0 t1 sbx
  t0=$(date +%s.%N)
  sbx=$(timeout 300 "$BIN/bean" run --image "$img" --quiet 2>/dev/null)
  t1=$(date +%s.%N)
  if [ -z "$sbx" ]; then echo FAILED; return 1; fi
  "$BIN/bean" kill "$sbx" >/dev/null 2>&1
  echo "$t1 - $t0" | bc
}

prewarm_once() {  # prewarm_once <image>...
  local refs job
  refs=$(printf '"%s",' "$@" | sed 's/,$//')
  job=$(curl -fsS -X POST "$BEAN_BASE_URL/v1/images/prewarm" \
    -H "Authorization: Bearer $BEAN_API_KEY" -H 'Content-Type: application/json' \
    -d "{\"refs\":[$refs],\"targetNodes\":1}" | python3 -c \
    'import json,sys; print(json.load(sys.stdin).get("jobId",""))' 2>/dev/null)
  [ -n "$job" ] || { echo "  prewarm did not start"; return 1; }
  # Polled rather than assumed complete: the job is asynchronous, and creating
  # against a half-published image would measure a mix of levels 2 and 3.
  local i done_flag
  for i in $(seq 1 120); do
    done_flag=$(curl -fsS "$BEAN_BASE_URL/v1/images/prewarm/$job" \
      -H "Authorization: Bearer $BEAN_API_KEY" | python3 -c \
      'import json,sys; print(json.load(sys.stdin).get("done",False))' 2>/dev/null)
    [ "$done_flag" = "True" ] && return 0
    sleep 5
  done
  echo "  prewarm did not finish in 600s"; return 1
}

# prewarm retries, because a lost blob fetch is a network event rather than a result.
# Retrying is cheap and correct here: layers already converted are skipped, so a second
# attempt only redoes what actually failed.
prewarm() {  # prewarm <image>...
  local i
  for i in $(seq 1 "$ATTEMPTS"); do
    if prewarm_once "$@"; then return 0; fi
    echo "  prewarm attempt $i/$ATTEMPTS did not complete; retrying"
    grep -o 'prewarm failed.*error=.*' "$RUN/api.log" 2>/dev/null | tail -1 | cut -c1-200
  done
  return 1
}

echo "images under test: $IMAGES"
echo

##### arm 1+2: the existing comparison, conversion on the create path
for arm in "dm-snapshot:" "overlaybd-cold:--fc-overlaybd"; do
  label=${arm%%:*}; flags=${arm#*:}
  echo "########## $label ##########"
  start_stack "$flags" || continue
  for img in $IMAGES; do
    for attempt in $(seq 1 "$ATTEMPTS"); do
      secs=$(time_create "$img") && break
      echo "  $img: create attempt $attempt/$ATTEMPTS failed"
      secs=""
    done
    [ -n "$secs" ] || { echo "  $img: CREATE FAILED"; tail -5 "$RUN/noded.log"; continue; }
    printf "  %s: create %.1fs\n" "$img" "$secs"
  done
  echo "  image dir: $(human "$(allocated "$IMAGE_DIR")")"
  cleanup; sleep 2; echo
done

##### arm 3: published layers, node has none
echo "########## overlaybd-published ##########"
start_stack "$OBD_FLAGS" || exit 1

echo "  prewarming (converts and publishes)..."
if ! prewarm $IMAGES; then tail -20 "$RUN/noded.log"; exit 1; fi

published=$(allocated "$LAYER_DIR")
echo "  layers on disk after prewarm: $(human "$published")"

# The whole point of the arm. Without this the create reads local files and the
# measurement says nothing about the published copy.
rm -rf "$LAYER_DIR"
echo "  layer dir wiped: $(human "$(allocated "$LAYER_DIR")") remains"

for img in $IMAGES; do
  # Not retried. A create in this arm should touch no registry blob at all, so a
  # failure here is a finding about the published path rather than a flake to paper
  # over -- and a retry would run against a layer dir the first attempt may have
  # populated, measuring level 1 instead.
  secs=$(time_create "$img") || { echo "  $img: CREATE FAILED"; tail -20 "$RUN/noded.log"; continue; }
  printf "  %s: create %.1fs (published)\n" "$img" "$secs"
done

# A create that silently fell back to converting would look like a fast create with a
# full layer directory, so the residue is reported: on-demand reads leave the cache,
# which is smaller than the sealed layers by however much went untouched.
echo "  layer dir after creates: $(human "$(allocated "$LAYER_DIR")")"
[ -d "$LAYER_DIR/cache" ] && echo "  block cache: $(human "$(allocated "$LAYER_DIR/cache")")"
sealed=$(ls "$LAYER_DIR"/*.obd 2>/dev/null | wc -l | tr -d ' ')
echo "  sealed layers present: $sealed (non-zero means a create converted after all)"

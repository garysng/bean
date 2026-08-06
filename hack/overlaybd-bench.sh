#!/usr/bin/env bash
# Compares the two rootfs backends on the numbers this platform actually cares
# about: real disk for a set of images that share a base, cold create latency, and
# host CPU spent converting.
#
# Written because the overlaybd work was argued from an extrapolation
# (hack/layer-amplification.go reads manifests) and never measured through the
# provider itself. What that tool cannot answer: what overlaybd's own on-disk form
# costs, and what the conversion costs in CPU and wall-clock.
#
# Two images that genuinely share a layer are required or the comparison is
# meaningless. python:3.12-slim and python:3.11-slim share their debian base
# (1.51x by manifest); python:3.12 and python:3.12-slim share nothing at all,
# which is worth knowing before assuming "same family" implies sharing.
#
# Usage: overlaybd-bench.sh
set -uo pipefail

BIN=${BIN:-/tmp/beantest/bin}
STACK=${STACK:-$(dirname "$0")/dev-fc-stack.sh}
RUN=${RUN:-/tmp/beanrun}
IMAGES=${IMAGES:-"python:3.12-slim python:3.11-slim"}
IMAGE_DIR=/var/lib/bean/images
export BEAN_BASE_URL=http://127.0.0.1:18080
export BEAN_API_KEY=devkey

cleanup() { BIN=$BIN bash "$STACK" stop >/dev/null 2>&1; }
trap cleanup EXIT

# du -s --apparent-size would report the provisioned size, which for a sparse ext4
# is orders of magnitude above what is allocated. Only allocated blocks are a cost.
allocated() { du -s --block-size=1 "$1" 2>/dev/null | awk '{print $1}'; }
human() { numfmt --to=iec-i --suffix=B "${1:-0}" 2>/dev/null || echo "${1}B"; }

# cpu_of sums utime+stime for a process tree from /proc, in clock ticks. Measured
# per-backend around the create so conversion CPU is attributed, which is the cost
# the layer-sharing argument is really about.
noded_cpu() {
  local pid; pid=$(pgrep -f "$BIN/noded" | head -1)
  [ -n "$pid" ] || { echo 0; return; }
  awk '{print $14+$15}' "/proc/$pid/stat" 2>/dev/null || echo 0
}

start_stack() {  # start_stack <extra noded flags>
  rm -rf "$RUN" "$IMAGE_DIR"
  BIN=$BIN BUILDKIT_ADDR= NODED_FLAGS="$1" \
    bash "$STACK" start >"${TMPDIR:-/tmp}/bench-stack.log" 2>&1 || {
      echo "  stack failed to start"; tail -15 "${TMPDIR:-/tmp}/bench-stack.log"; return 1; }
}

run_backend() {  # run_backend <label> <noded flags>
  local label=$1 flags=$2
  echo "########## $label ##########"
  start_stack "$flags" || return 1

  local n=0
  for img in $IMAGES; do
    n=$((n+1))
    local cpu0 t0 t1 cpu1
    cpu0=$(noded_cpu)
    t0=$(date +%s.%N)
    local sbx
    sbx=$(timeout 300 "$BIN/bean" run --image "$img" --quiet 2>/dev/null)
    t1=$(date +%s.%N)
    cpu1=$(noded_cpu)

    if [ -z "$sbx" ]; then
      echo "  image $n ($img): CREATE FAILED"
      tail -5 "$RUN/noded.log"
      continue
    fi
    # Ticks to seconds. getconf is the portable way to the kernel's USER_HZ.
    local hz; hz=$(getconf CLK_TCK)
    printf "  image %d (%s): create %.1fs, noded cpu %.1fs\n" \
      "$n" "$img" "$(echo "$t1 - $t0" | bc)" \
      "$(echo "scale=2; ($cpu1 - $cpu0) / $hz" | bc)"
    "$BIN/bean" kill "$sbx" >/dev/null 2>&1
  done

  echo "  --- disk after both images ---"
  local total; total=$(allocated "$IMAGE_DIR")
  echo "  $IMAGE_DIR allocated: $(human "$total")"
  # Broken out so the shared-layer claim is visible rather than inferred from a total.
  if [ -d "$IMAGE_DIR/layers" ]; then
    echo "  sealed layers: $(ls "$IMAGE_DIR/layers" | wc -l | tr -d ' ') files, $(human "$(allocated "$IMAGE_DIR/layers")")"
    ls -la "$IMAGE_DIR/layers" | tail -n +4 | awk '{printf "    %s %s\n", $5, $9}'
  fi
  ls "$IMAGE_DIR"/*.ext4 >/dev/null 2>&1 && {
    echo "  flattened ext4 images:"
    for f in "$IMAGE_DIR"/*.ext4; do
      printf "    apparent=%s allocated=%s %s\n" \
        "$(human "$(stat -c%s "$f")")" "$(human "$(allocated "$f")")" "$(basename "$f")"
    done
  }
  echo "  BENCH_TOTAL $label $total"
  cleanup
  sleep 2
}

echo "images under test: $IMAGES"
echo "(manifest sharing for this pair, from hack/layer-amplification.go: 1.51x)"
echo

run_backend "dm-snapshot (default)" "" | tee "${TMPDIR:-/tmp}/bench-dm.txt"
echo
run_backend "overlaybd" "--fc-overlaybd" | tee "${TMPDIR:-/tmp}/bench-obd.txt"

echo
echo "########## summary ##########"
DM=$(grep BENCH_TOTAL "${TMPDIR:-/tmp}/bench-dm.txt" | awk '{print $NF}')
OBD=$(grep BENCH_TOTAL "${TMPDIR:-/tmp}/bench-obd.txt" | awk '{print $NF}')
echo "dm-snapshot image dir: $(human "${DM:-0}")"
echo "overlaybd   image dir: $(human "${OBD:-0}")"
if [ -n "${DM:-}" ] && [ -n "${OBD:-}" ] && [ "${OBD:-0}" -gt 0 ]; then
  echo "ratio (dm / overlaybd): $(echo "scale=2; $DM / $OBD" | bc)x"
fi

#!/usr/bin/env bash
# Measures what lazy pull is actually for: the cost of a create on a node that has
# never seen the image, when the layers were converted and published earlier.
#
# The comparison that matters is the same image on the same host, three ways:
#   1. dm-snapshot          -- download, flatten
#   2. overlaybd, no store  -- download, seal, assemble        (today's default)
#   3. overlaybd + store    -- publish once, then metadata only
#
# Run 3 twice: the first pays for conversion AND publication, the second is what every
# later node sees. That second number is the point of the whole exercise, so the script
# wipes the node's local state between the two while leaving the bucket intact --
# otherwise it measures a warm cache and says nothing.
#
# Usage: overlaybd-lazy-e2e.sh
set -uo pipefail

BIN=${BIN:-/tmp/beantest/bin}
STACK=${STACK:-$(dirname "$0")/dev-fc-stack.sh}
RUN=${RUN:-/tmp/beanrun}
IMAGE=${IMAGE:-python:3.12-slim}
S3=${S3:-http://127.0.0.1:9000}
BUCKET=${BUCKET:-bean-obd-layers}
export BEAN_S3_ACCESS_KEY=${BEAN_S3_ACCESS_KEY:-beanadmin}
export BEAN_S3_SECRET_KEY=${BEAN_S3_SECRET_KEY:-beansecret123}
export BEAN_BASE_URL=http://127.0.0.1:18080
export BEAN_API_KEY=devkey

cleanup() { BIN=$BIN bash "$STACK" stop >/dev/null 2>&1; }
trap cleanup EXIT

HZ=$(getconf CLK_TCK)
noded_cpu() {
  local pid; pid=$(pgrep -f "$BIN/noded" | head -1)
  [ -n "$pid" ] || { echo 0; return; }
  awk '{print $14+$15}' "/proc/$pid/stat" 2>/dev/null || echo 0
}

# Only the node's own state. The bucket is deliberately left alone between runs, since
# a published layer surviving is the entire mechanism under test.
wipe_node() { rm -rf "$RUN" /var/lib/bean/images /opt/overlaybd/registry_cache/*; }

measure() {  # measure <label> <noded flags>
  local label=$1 flags=$2
  BIN=$BIN BUILDKIT_ADDR= NODED_FLAGS="$flags" \
    bash "$STACK" start >"${TMPDIR:-/tmp}/lazy-stack.log" 2>&1 || {
      echo "  $label: stack failed"; tail -12 "${TMPDIR:-/tmp}/lazy-stack.log"; return 1; }

  local c0 t0 t1 c1 sbx
  c0=$(noded_cpu); t0=$(date +%s.%N)
  sbx=$(timeout 300 "$BIN/bean" run --image "$IMAGE" --quiet 2>/dev/null)
  t1=$(date +%s.%N); c1=$(noded_cpu)

  if [ -z "$sbx" ]; then
    printf "  %-34s CREATE FAILED\n" "$label"
    tail -6 "$RUN/noded.log"
    cleanup; return 1
  fi
  # Proves the sandbox is real rather than just recorded.
  local ok="read-failed"
  "$BIN/bean" exec "$sbx" -- cat /etc/os-release >/dev/null 2>&1 && ok="guest-ok"
  printf "  %-34s %6.1fs wall  %5.2fs cpu  %s\n" "$label" \
    "$(echo "$t1 - $t0" | bc)" "$(echo "scale=2; ($c1 - $c0) / $HZ" | bc)" "$ok"
  "$BIN/bean" kill "$sbx" >/dev/null 2>&1
  cleanup; sleep 2
}

echo "image under test: $IMAGE"
echo "blob store: $S3/$BUCKET"
echo

# Start from nothing at all, bucket included, so run 3a genuinely pays for publication.
docker exec bean-minio mc alias set local "$S3" "$BEAN_S3_ACCESS_KEY" "$BEAN_S3_SECRET_KEY" >/dev/null 2>&1
docker exec bean-minio mc rb --force "local/$BUCKET" >/dev/null 2>&1

LAZY="--fc-overlaybd --fc-overlaybd-lazy-pull \
--s3-endpoint $S3 --s3-bucket $BUCKET"

wipe_node; measure "1. dm-snapshot" ""
wipe_node; measure "2. overlaybd, local conversion" "--fc-overlaybd"
wipe_node; measure "3a. overlaybd + store, first" "$LAZY"

echo "  --- published to the store ---"
docker exec bean-minio mc ls --recursive "local/$BUCKET" 2>/dev/null \
  | awk '{printf "      %s %s\n", $4, $6}' | head -6

# The node forgets everything; the bucket remembers. This is what a second node sees.
wipe_node; measure "3b. overlaybd + store, cached" "$LAZY"

echo
echo "3b is the number lazy pull exists for: no download of the original image, no"
echo "conversion, just a config naming remote digests. Compare it with 1 and 2."

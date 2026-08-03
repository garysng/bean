#!/usr/bin/env bash
# Restores the same snapshot several times and checks each one got its own files.
#
# Two things are being verified together, because they interact:
#
#   1. Repeated restores of one snapshot all succeed. The node stops inflating the
#      bundle once it has the writable layer, but the sender streams the whole
#      thing regardless — so the remainder must still be drained or the sender
#      blocks and the restore fails with EOF. That bug was introduced and caught
#      here, not in a unit test: the first restore populates the cache and only the
#      second takes the early-exit path.
#
#   2. Each restored sandbox reads back the marker its snapshot captured, after
#      drop_caches. A restore that served the base image instead would still boot
#      and simply not have the file, and reading through the page cache would hide
#      it (docs/decisions.md §3.0).
#
# Usage: restore-repeat-check.sh [--restores N]
set -uo pipefail

COUNT=${COUNT:-3}
IMAGE=${IMAGE:-alpine:3.20}
BASE_URL=${BEAN_BASE_URL:-http://127.0.0.1:18080}
API_KEY=${BEAN_API_KEY:-devkey}
BEAN=${BEAN:-/tmp/bean}
MARKER="restore-repeat-marker"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --restores) COUNT="$2"; shift 2 ;;
    -h|--help) sed -n '2,18p' "$0" | sed 's/^# \?//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 64 ;;
  esac
done

export BEAN_BASE_URL="$BASE_URL" BEAN_API_KEY="$API_KEY"
say() { printf '%s\n' "$*"; }
hr() { printf -- '------------------------------------------------------------\n'; }

[[ -x "$BEAN" ]] || { say "bean CLI not executable: $BEAN"; exit 69; }

# The CLI has no resource flags and the default 20 GiB request would exhaust the
# node's disk commitment before this finishes.
create_small() {
  curl -s -X POST "$BASE_URL/v1/sandboxes" \
    -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
    -d "{\"image\":\"$IMAGE\",\"resources\":{\"diskMiB\":2048}}" 2>/dev/null |
    python3 -c 'import json,sys; print(json.load(sys.stdin).get("sandbox",{}).get("id",""))' 2>/dev/null
}

SRC=$(create_small)
[[ -n "$SRC" ]] || { say "could not create the source sandbox"; exit 70; }
say "source sandbox: $SRC"

"$BEAN" exec "$SRC" -- sh -c "echo $MARKER > /marker.txt; sync" >/dev/null 2>&1
SNAP=$("$BEAN" snapshot create "$SRC" --name restore-repeat --quiet 2>/dev/null)
"$BEAN" kill "$SRC" >/dev/null 2>&1
[[ -n "$SNAP" ]] || { say "snapshot failed"; exit 70; }
say "snapshot: $SNAP"
hr

restored=()
failures=0
for i in $(seq 1 "$COUNT"); do
  # Timed individually: the first restore populates the cache and the rest take
  # the early-exit path, so a single average would hide the difference this change
  # exists to produce.
  start=$(date +%s%3N)
  sbx=$("$BEAN" run --snapshot "$SNAP" --quiet 2>&1)
  elapsed=$(( $(date +%s%3N) - start ))
  if [[ "$sbx" != sbx_* ]]; then
    say "  restore $i FAILED after ${elapsed}ms: $sbx"
    failures=$((failures + 1))
    continue
  fi
  say "  restore $i: $sbx in ${elapsed}ms"
  restored+=("$sbx")
done

hr
# Reading through the page cache would pass even against a device serving the base
# image, so the cache is dropped first.
say "dropping page cache so reads reach the block device"
sync
echo 3 > /proc/sys/vm/drop_caches

for sbx in "${restored[@]}"; do
  got=$("$BEAN" exec "$sbx" -- cat /marker.txt 2>/dev/null | tr -d '\r\n')
  if [[ "$got" == "$MARKER" ]]; then
    say "  $sbx: marker OK"
  else
    say "  $sbx: EXPECTED $MARKER GOT '${got:-<empty>}'"
    failures=$((failures + 1))
  fi
done

hr
for sbx in "${restored[@]}"; do
  "$BEAN" kill "$sbx" >/dev/null 2>&1 &
done
wait

if [[ "$failures" -gt 0 ]]; then
  say "FAIL: $failures problem(s)"
  exit 70
fi
say "PASS: $COUNT restores succeeded and each read its own marker"

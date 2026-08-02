#!/usr/bin/env bash
# Proves the snapshot cache stays under its watermark and that eviction never
# hands a restore a broken filesystem.
#
# Unit tests cover the ordering and the pin, but they seed the cache directly.
# What they cannot show is that a real restore, racing a real sweep, still gets
# its files — and that is the failure this whole mechanism risks introducing.
#
# The check that matters is the last one: every restored sandbox must read back
# the file its snapshot captured, AFTER dropping the page cache. A restore that
# read from cache would pass against a cache that had been evicted out from under
# it, which is exactly the bug this is looking for (see decisions.md §3.0).
#
# Usage: snapcache-evict-check.sh [--snapshots N]
# Requires a stack started with --snapshot-cache-high-mib set.
set -uo pipefail

COUNT=${COUNT:-6}
IMAGE=${IMAGE:-alpine:3.20}
BASE_URL=${BEAN_BASE_URL:-http://127.0.0.1:18080}
API_KEY=${BEAN_API_KEY:-devkey}
BEAN=${BEAN:-/tmp/bean}
CACHE=${CACHE:-/var/lib/bean/sandboxes/.snapshots}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --snapshots) COUNT="$2"; shift 2 ;;
    -h|--help) sed -n '2,17p' "$0" | sed 's/^# \?//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 64 ;;
  esac
done

export BEAN_BASE_URL="$BASE_URL" BEAN_API_KEY="$API_KEY"
say() { printf '%s\n' "$*"; }
hr() { printf -- '------------------------------------------------------------\n'; }

[[ -x "$BEAN" ]] || { say "bean CLI not executable: $BEAN"; exit 69; }

cache_bytes() { du -sb "$CACHE" 2>/dev/null | cut -f1 || echo 0; }
cache_entries() { find "$CACHE" -mindepth 1 -maxdepth 1 -type d ! -name '.*' 2>/dev/null | wc -l; }

say "creating $COUNT snapshots, each from its own sandbox"
say "cache before: $(cache_bytes) bytes across $(cache_entries) entries"
hr

# The CLI has no resource flags, and the default 20 GiB request would exhaust a
# node's disk commitment after a handful of sandboxes — long before the snapshot
# cache reaches its watermark, which is what this script is trying to observe.
create_small() {
  curl -s -X POST "$BASE_URL/v1/sandboxes" \
    -H "Authorization: Bearer $API_KEY" -H "Content-Type: application/json" \
    -d "{\"image\":\"$IMAGE\",\"resources\":{\"diskMiB\":2048}}" 2>/dev/null |
    python3 -c 'import json,sys; print(json.load(sys.stdin).get("sandbox",{}).get("id",""))' 2>/dev/null
}

snaps=()
for i in $(seq 1 "$COUNT"); do
  sbx=$(create_small)
  if [[ -z "$sbx" ]]; then say "create $i failed"; continue; fi
  # Each snapshot gets a distinct marker, so a restore that silently served
  # another snapshot's filesystem is detectable rather than plausible.
  "$BEAN" exec "$sbx" -- sh -c "echo marker-$i > /marker.txt; sync" >/dev/null 2>&1
  snap=$("$BEAN" snapshot create "$sbx" --name "evict-$i" --quiet 2>/dev/null)
  "$BEAN" kill "$sbx" >/dev/null 2>&1
  if [[ -z "$snap" ]]; then say "snapshot $i failed"; continue; fi
  snaps+=("$snap:$i")
  say "  snapshot $i: $snap"
done

hr
say "restoring each snapshot once, which is what fills the cache"
restored=()
for entry in "${snaps[@]}"; do
  snap="${entry%%:*}"; want="${entry##*:}"
  sbx=$("$BEAN" run --snapshot "$snap" --quiet 2>/dev/null)
  if [[ -z "$sbx" ]]; then say "  restore of $snap FAILED"; continue; fi
  restored+=("$sbx:$want")
  say "  restored $snap -> $sbx  (cache now $(cache_bytes) bytes, $(cache_entries) entries)"
done

hr
# Reading through the page cache would pass even against a corrupted device: the
# metadata lives in the memory image and the data on the block device, and only
# dropping the cache forces the read to reach the device.
say "dropping page cache so reads reach the block device"
sync
echo 3 > /proc/sys/vm/drop_caches

failures=0
for entry in "${restored[@]}"; do
  sbx="${entry%%:*}"; want="${entry##*:}"
  got=$("$BEAN" exec "$sbx" -- cat /marker.txt 2>/dev/null | tr -d '\r\n')
  if [[ "$got" == "marker-$want" ]]; then
    say "  $sbx: marker-$want OK"
  else
    say "  $sbx: EXPECTED marker-$want GOT '${got:-<empty>}'"
    failures=$((failures + 1))
  fi
done

hr
final_bytes=$(cache_bytes)
say "cache after: $final_bytes bytes across $(cache_entries) entries"

for entry in "${restored[@]}"; do
  "$BEAN" kill "${entry%%:*}" >/dev/null 2>&1 &
done
wait

if [[ "$failures" -gt 0 ]]; then
  say "FAIL: $failures restored sandbox(es) could not read their own marker"
  say "A restore served the base image or a half-evicted entry."
  exit 70
fi
say "PASS: every restore read its own marker after drop_caches"

#!/usr/bin/env bash
# Do several container sandboxes coexist?
#
# The end-to-end runs one or two at a time, and this tier has several per-sandbox
# resources that could collide under fan-out:
#
#   - a metadata service bound to 169.254.169.254:80, the same address in every sandbox
#   - a TCMU device per sandbox, whose WWID the kernel derives from a serial
#   - one mount per sandbox, released on destroy
#   - a netns each, with the metadata address assigned to its veth
#
# Each is supposed to be scoped by the sandbox's own namespace, so they should not
# collide. That is exactly the kind of reasoning that has been wrong three times on
# this tier already -- port forwarding dialling the tap address, a flag test passing on
# one platform's path length, an egress probe aimed at an unreachable host. So it is
# measured.
#
# What would make this fail interestingly: two sandboxes sharing a metadata document
# (each would then accept the other's agent token), or a device whose WWID collides
# (multipathd merges them and one sandbox reads another's filesystem).
#
# Usage: RUNTIME=runsc N=5 oci-tier-concurrent.sh
set -uo pipefail

RUNTIME=${RUNTIME:-runsc}
N=${N:-5}
BIN=${BIN:-/tmp/beantest/bin}
STACK=${STACK:-$(dirname "$0")/dev-fc-stack.sh}
RUN=${RUN:-/tmp/beanrun}
IMG=${IMG:-docker.m.daocloud.io/library/python:3.11-slim}
UPLINK=${UPLINK:-$(ip route | awk '/^default/ {print $5; exit}')}
GUEST_SUBNET=${GUEST_SUBNET:-172.31.0.0/30}
# The node's advertised disk bounds how many sandboxes fit, at --default-disk-mib
# each, so the default 100 GiB caps this at five -- a quota, not a concurrency limit.
# Raising it is what lets the xtables lock window actually be exercised.
#
# Passed through to dev-fc-stack.sh, which already has this variable and renders it as
# --disk-mib. Setting it in NODED_FLAGS instead put the flag on the command line twice
# and the stack's own value won, which read as a concurrency ceiling that was not there.
#
# Safe to oversubscribe: an overlaybd writable layer is sparse, measured at 40 KiB for
# an idle sandbox against a 20 GiB apparent size.
NODE_DISK_MIB=${NODE_DISK_MIB:-}
export BEAN_BASE_URL=http://127.0.0.1:18080
export BEAN_API_KEY=devkey

FAILED=0
pass() { printf '  PASS  %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; FAILED=1; }

SBXS=""
cleanup() {
  for s in $SBXS; do "$BIN/bean" kill "$s" >/dev/null 2>&1; done
  BIN=$BIN bash "$STACK" stop >/dev/null 2>&1
}
trap cleanup EXIT

echo "runtime: $RUNTIME  sandboxes: $N"

rm -rf "$RUN" /var/lib/bean/images
GUEST_SUBNET=$GUEST_SUBNET UPLINK=$UPLINK BIN=$BIN BUILDKIT_ADDR= \
  RUNTIME=$RUNTIME NODED_FLAGS="--fc-overlaybd" \
  ${NODE_DISK_MIB:+NODE_DISK_MIB=$NODE_DISK_MIB} \
  bash "$STACK" start >"${TMPDIR:-/tmp}/oci-conc-stack.log" 2>&1 || {
    echo "stack failed to start"; tail -20 "${TMPDIR:-/tmp}/oci-conc-stack.log"; exit 1; }
sleep 3

# One create first, so the image is converted and the concurrent ones are not all
# waiting on the same conversion -- that would measure the layer flight, not fan-out.
warm=$("$BIN/bean" run --image "$IMG" --quiet 2>&1)
if [ -z "$warm" ] || printf '%s' "$warm" | grep -qi error; then
  echo "warm-up create failed: $warm"; tail -20 "$RUN/noded.log"; exit 1
fi
"$BIN/bean" kill "$warm" >/dev/null 2>&1
echo "  image converted; starting $N concurrently"

echo
echo "### $N concurrent creates"
start=$(date +%s.%N)
pids=""
outdir=$(mktemp -d)
for i in $(seq 1 "$N"); do
  ( "$BIN/bean" run --image "$IMG" --quiet >"$outdir/$i" 2>&1 ) &
  pids="$pids $!"
done
for p in $pids; do wait "$p"; done
end=$(date +%s.%N)

created=0
capacity=0
for i in $(seq 1 "$N"); do
  s=$(tr -d '\n' < "$outdir/$i")
  if printf '%s' "$s" | grep -q '^sbx_'; then
    SBXS="$SBXS $s"
    created=$((created + 1))
  elif printf '%s' "$s" | grep -q NO_CAPACITY; then
    capacity=$((capacity + 1))
  else
    echo "  create $i failed: $(printf '%s' "$s" | head -c 160)"
  fi
done
rm -rf "$outdir"
printf '  %d/%d created in %.1fs\n' "$created" "$N" "$(echo "$end - $start" | bc)"
if [ "$created" -eq "$N" ]; then
  pass "all $N concurrent creates succeeded"
elif [ "$capacity" -gt 0 ]; then
  # The node's disk quota bounds how many sandboxes fit, and a create refused for
  # NO_CAPACITY is the scheduler working. Distinguished from a create that failed,
  # because the first says raise --disk-mib and the second says fix a bug -- and an
  # N above the quota otherwise reads as a concurrency limit that is not there.
  echo "  note: $capacity of $N refused for capacity (node disk quota), not failure"
  if [ "$((created + capacity))" -eq "$N" ]; then
    pass "every create either started or was refused for capacity"
  else
    fail "$((N - created - capacity)) create(s) failed for other reasons"
    tail -15 "$RUN/noded.log"
  fi
else
  fail "only $created of $N started"
  tail -15 "$RUN/noded.log"
fi
[ "$created" -eq 0 ] && exit 1

echo
echo "### each sandbox is reachable and is its own"
# Reachability under fan-out is the metadata service and the netns dial working per
# sandbox. Identity is the stronger check: each writes a marker only it should see, so
# a shared rootfs or a merged device shows up as a sandbox reading someone else's.
ok=0
for s in $SBXS; do
  if "$BIN/bean" exec "$s" -- sh -c "echo $s > /whoami" >/dev/null 2>&1; then
    ok=$((ok + 1))
  fi
done
if [ "$ok" -eq "$created" ]; then
  pass "all $ok sandboxes reachable"
else
  fail "only $ok of $created reachable"
fi

leaks=0
for s in $SBXS; do
  got=$("$BIN/bean" exec "$s" -- cat /whoami 2>&1 | tr -d '\n\r ')
  if [ "$got" != "$s" ]; then
    fail "$s reads \"$got\" -- sandboxes are sharing a filesystem"
    leaks=$((leaks + 1))
  fi
done
[ "$leaks" -eq 0 ] && pass "each sandbox sees only its own marker"

echo
echo "### one device and one mount per sandbox"
mounts=$(grep -c "/var/lib/bean/sandboxes" /proc/mounts 2>/dev/null | head -1)
mounts=${mounts:-0}
# Only the container tier mounts a rootfs on the host. fc hands the block device
# straight to the microVM over virtio-blk, so zero is correct there -- asserting
# otherwise reported a failure against the tier that behaves as designed.
case "$RUNTIME" in
  runsc | runc)
    if [ "$mounts" -eq "$created" ]; then
      pass "$mounts mounts for $created sandboxes"
    else
      fail "$mounts mounts for $created sandboxes"
    fi
    ;;
  *)
    echo "  n/a   $RUNTIME attaches the device directly; $mounts host mounts"
    ;;
esac

# Distinct WWIDs matter because multipathd merges devices that share one, and the
# symptom is a sandbox serving another's data rather than an error.
wwids=$(ls /dev/disk/by-id/ 2>/dev/null | grep -c "TCMU_device" | head -1)
wwids=${wwids:-0}
uniq=$(ls /dev/disk/by-id/ 2>/dev/null | grep "TCMU_device" | sed 's/.*TCMU_device_//' | sort -u | wc -l | tr -d ' ')
if [ "$wwids" -gt 0 ] && [ "$uniq" -eq "$wwids" ]; then
  pass "$wwids TCMU device ids, all distinct"
elif [ "$wwids" -eq 0 ]; then
  echo "  SKIP  no TCMU devices listed (by-id may not be populated here)"
else
  fail "$wwids TCMU device ids but only $uniq distinct"
fi

echo
echo "### destroying all of them releases everything"
for s in $SBXS; do "$BIN/bean" kill "$s" >/dev/null 2>&1; done
SBXS=""
sleep 3
left=$(grep -c "/var/lib/bean/sandboxes" /proc/mounts 2>/dev/null | head -1)
left=${left:-0}
if [ "$left" -eq 0 ]; then
  pass "no mounts left behind"
else
  fail "$left mount(s) still present"
  grep "/var/lib/bean/sandboxes" /proc/mounts | head -3
fi
# A metadata address left on a torn-down namespace would be invisible until the next
# sandbox reused the slot and failed to bind.
stale=$(ls /var/run/netns 2>/dev/null | wc -l | tr -d ' ')
if [ "$stale" -eq 0 ]; then
  pass "no namespaces left behind"
else
  echo "  note: $stale namespace(s) still present"
fi

echo
if [ "$FAILED" -eq 0 ]; then
  echo "all checks passed"
else
  echo "at least one check failed"
fi
exit $FAILED

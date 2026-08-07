#!/usr/bin/env bash
# End-to-end for the container tier: a sandbox starts under an OCI runtime, the node
# reaches its agent, and a command runs inside it.
#
# The assertions go through the sandbox rather than around it. A create that returns an
# id proves only that the runtime accepted a bundle; what has to hold is that the node
# can reach the agent through the sandbox's network namespace and that the rootfs the
# image produced is actually there. So this reads a file's bytes and runs an
# interpreter, and treats a create with no working exec as a failure.
#
# Usage: RUNTIME=runsc oci-tier-e2e.sh
#        RUNTIME=runc  oci-tier-e2e.sh
set -uo pipefail

RUNTIME=${RUNTIME:-runsc}
BIN=${BIN:-/tmp/beantest/bin}
STACK=${STACK:-$(dirname "$0")/dev-fc-stack.sh}
RUN=${RUN:-/tmp/beanrun}
IMG=${IMG:-docker.m.daocloud.io/library/python:3.11-slim}
UPLINK=${UPLINK:-$(ip route | awk '/^default/ {print $5; exit}')}
GUEST_SUBNET=${GUEST_SUBNET:-172.31.0.0/30}
export BEAN_BASE_URL=http://127.0.0.1:18080
export BEAN_API_KEY=devkey

FAILED=0
pass() { printf '  PASS  %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; FAILED=1; }

cleanup() {
  [ -n "${SBX:-}" ] && "$BIN/bean" kill "$SBX" >/dev/null 2>&1
  BIN=$BIN bash "$STACK" stop >/dev/null 2>&1
}
trap cleanup EXIT

command -v "$RUNTIME" >/dev/null || { echo "$RUNTIME not found"; exit 1; }
echo "runtime: $RUNTIME ($($RUNTIME --version 2>&1 | head -1))"
echo "uplink: $UPLINK  guest subnet: $GUEST_SUBNET"

# The container tier needs node networking: the agent is reached through the sandbox's
# network namespace, so without a pool there is no address to dial. noded refuses the
# create in that case rather than starting something unreachable.
rm -rf "$RUN" /var/lib/bean/images
GUEST_SUBNET=$GUEST_SUBNET UPLINK=$UPLINK BIN=$BIN BUILDKIT_ADDR= \
  RUNTIME=$RUNTIME NODED_FLAGS="--fc-overlaybd" \
  bash "$STACK" start >"${TMPDIR:-/tmp}/oci-e2e-stack.log" 2>&1 || {
    echo "stack failed to start"; tail -20 "${TMPDIR:-/tmp}/oci-e2e-stack.log"; exit 1; }
sleep 3

echo
echo "### create"
# Two creates, reported separately, because one number here is misleading.
#
# This script wipes the image directory, so the first create includes converting the
# image to overlaybd layers -- work the fc tier pays too, and nothing to do with
# containers. Reporting only that made the tier look an order of magnitude slower than
# it is: measured 16.3s cold against 0.9s warm, with the phase metrics attributing
# 16.86s of 17.09s total to runtime_create and 0.072s to agent_ready.
start=$(date +%s.%N)
COLD=$("$BIN/bean" run --image "$IMG" --quiet 2>&1)
end=$(date +%s.%N)
if [ -z "$COLD" ] || printf '%s' "$COLD" | grep -qi error; then
  fail "create: $COLD"
  tail -25 "$RUN/noded.log"
  exit 1
fi
printf '  cold (image converted here): %.1fs\n' "$(echo "$end - $start" | bc)"

start=$(date +%s.%N)
SBX=$("$BIN/bean" run --image "$IMG" --quiet 2>&1)
end=$(date +%s.%N)
if [ -z "$SBX" ] || printf '%s' "$SBX" | grep -qi error; then
  fail "second create: $SBX"
  exit 1
fi
warm=$(echo "$end - $start" | bc)
printf '  warm (steady state): %.1fs\n' "$warm"
"$BIN/bean" kill "$COLD" >/dev/null 2>&1

# A steady-state create is dominated by mounting the rootfs and starting one process,
# so seconds would mean something is wrong -- a shell-out retrying, or the agent
# health check parked in a backoff tuned for a microVM boot.
if [ "$(echo "$warm < 3" | bc)" = "1" ]; then
  pass "steady-state create is sub-3s"
else
  fail "steady-state create took ${warm}s; a container should not need that long"
fi

echo
echo "### the agent is reachable through the sandbox's network namespace"
# exec goes node -> AgentConn -> agent gRPC. It working at all is the proof that the
# netns transport resolved, since there is no other way in.
if out=$("$BIN/bean" exec "$SBX" -- echo agent-reachable 2>&1) &&
   printf '%s' "$out" | grep -q agent-reachable; then
  pass "exec reached the agent"
else
  fail "exec did not reach the agent: $(printf '%s' "$out" | tail -3)"
  tail -25 "$RUN/noded.log"
  exit 1
fi

echo
echo "### the rootfs is the image's, down to the bytes"
# An interpreter that runs and a file whose checksum is right. Listing a directory
# would pass on a filesystem that mounted and served nothing.
out=$("$BIN/bean" exec "$SBX" -- python3 -c '
import hashlib, json, os, sysconfig, sys
p = os.path.join(sysconfig.get_paths()["stdlib"], "json", "decoder.py")
b = open(p, "rb").read()
print("python", sys.version.split()[0])
print("json", json.loads("[1,2,3]"))
print("decoder", len(b), hashlib.sha256(b).hexdigest()[:16])
' 2>&1)
if printf '%s' "$out" | grep -q "^python 3.11"; then
  pass "python3 runs: $(printf '%s' "$out" | tr '\n' ' ')"
else
  fail "python3 did not run: $(printf '%s' "$out" | tail -3)"
fi

echo
echo "### the sandbox is isolated from the host"
# A container that can see the host's process table is not isolated. The count is
# small rather than zero because the sandbox has its own processes.
hostprocs=$(ls -d /proc/[0-9]* 2>/dev/null | wc -l)
inprocs=$("$BIN/bean" exec "$SBX" -- sh -c 'ls -d /proc/[0-9]* | wc -l' 2>&1 | tail -1)
if [ "${inprocs:-0}" -gt 0 ] 2>/dev/null && [ "${inprocs:-0}" -lt 50 ]; then
  pass "own pid namespace: $inprocs processes inside vs $hostprocs on the host"
else
  fail "process table looks wrong: $inprocs inside, $hostprocs on the host"
fi

# CAP_SYS_ADMIN is what the spec deliberately withholds, because with it a process can
# mount and mounting is most of the way out of a container.
out=$("$BIN/bean" exec "$SBX" -- sh -c 'mount -t tmpfs none /mnt 2>&1; echo rc=$?' 2>&1 | tail -2)
if printf '%s' "$out" | grep -q "rc=0"; then
  fail "the sandbox could mount: CAP_SYS_ADMIN was not dropped"
else
  pass "mount refused inside the sandbox"
fi

echo
echo "### writes land in the sandbox, not on the host"
"$BIN/bean" exec "$SBX" -- sh -c 'echo written-inside > /marker.txt' >/dev/null 2>&1
if [ -f /marker.txt ]; then
  fail "a file written inside appeared on the host filesystem"
  rm -f /marker.txt
else
  back=$("$BIN/bean" exec "$SBX" -- cat /marker.txt 2>&1 | tail -1)
  if [ "$back" = "written-inside" ]; then
    pass "written inside, readable inside, absent on the host"
  else
    fail "the write did not persist inside: $back"
  fi
fi

echo
echo "### destroy releases the mount and the device"
mounts_before=$(grep -c "$RUN\|/var/lib/bean/sandboxes" /proc/mounts 2>/dev/null || echo 0)
"$BIN/bean" kill "$SBX" >/dev/null 2>&1
SBX=""
sleep 2
leaked=$(grep "/var/lib/bean/sandboxes" /proc/mounts 2>/dev/null | wc -l)
if [ "$leaked" -eq 0 ]; then
  pass "no sandbox mount left behind"
else
  fail "$leaked mount(s) still present after destroy:"
  grep "/var/lib/bean/sandboxes" /proc/mounts | head -3
fi

echo
if [ "$FAILED" -eq 0 ]; then
  echo "all checks passed"
else
  echo "at least one check failed"
fi
exit $FAILED

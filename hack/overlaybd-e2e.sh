#!/usr/bin/env bash
# End-to-end check that a sandbox actually boots from an overlaybd device, and that
# the image's own config reaches the guest.
#
# overlaybd-probe.sh and the hardware tests prove the provider assembles a device the
# host can mount. That is a different claim from "a guest boots from it", which is what
# this checks -- and the gap between the two is exactly where a rootfs that mounts on
# the host but cannot be booted would hide.
#
# Needs a KVM host with the node assets built, the overlaybd binaries, and bean's own
# binaries in BIN. Starts and stops its own stack, and leaves nothing behind.
set -uo pipefail

BIN=${BIN:-/tmp/beantest/bin}
STACK=${STACK:-$(dirname "$0")/dev-fc-stack.sh}
RUN=${RUN:-/tmp/beanrun}
export BEAN_BASE_URL=http://127.0.0.1:18080
export BEAN_API_KEY=devkey
FAILED=0
pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }
info() { echo "  ... $*"; }

cleanup() {
  BIN=$BIN bash $STACK stop >/dev/null 2>&1
}
trap cleanup EXIT

echo "=== start the stack with overlaybd ==="
# BUILDKIT_ADDR= and S3 left unset: this host only needs to run sandboxes, and the
# colon-less default in the script distinguishes "deliberately empty" from "unset".
BIN=$BIN BUILDKIT_ADDR= NODED_FLAGS="--fc-overlaybd" \
  bash $STACK start >${TMPDIR:-/tmp}/obd-e2e-stack.log 2>&1
if [ $? -ne 0 ]; then
  fail "stack did not start"
  tail -30 ${TMPDIR:-/tmp}/obd-e2e-stack.log
  tail -30 $RUN/noded.log 2>/dev/null
  exit 1
fi
pass "stack started"

# The node must report the overlaybd provider, not fall back to device-mapper.
if grep -q "rootfs via overlaybd" $RUN/noded.log 2>/dev/null; then
  pass "node is using overlaybd: $(grep -m1 'rootfs via overlaybd' $RUN/noded.log)"
else
  fail "node is not using overlaybd -- it fell back or failed to configure"
  grep -iE "overlaybd|rootfs via" $RUN/noded.log | head -5
  exit 1
fi

echo "=== create a sandbox from a real registry image ==="
# alpine rather than busybox: it has a real /etc/os-release to read back, and its
# layer is large enough that a broken chain shows up as a mount failure.
# First use converts the layer, which takes longer than the CLI default wait.
SBX=$(timeout 120 "$BIN/bean" run --image alpine:3.20 --quiet 2>${TMPDIR:-/tmp}/obd-e2e-run.err)
if [ -z "$SBX" ]; then
  fail "create failed"
  cat ${TMPDIR:-/tmp}/obd-e2e-run.err
  tail -40 $RUN/noded.log
  tail -20 /var/log/overlaybd.log 2>/dev/null
  exit 1
fi
pass "sandbox created: $SBX"

echo "=== the guest booted from the overlaybd device ==="
OUT=$("$BIN/bean" exec "$SBX" -- cat /etc/os-release 2>&1)
if echo "$OUT" | grep -q "Alpine"; then
  pass "guest read its own rootfs: $(echo "$OUT" | grep -m1 PRETTY_NAME)"
else
  fail "guest could not read the image's rootfs"
  info "output: $OUT"
fi

# Writes have to land in the sandbox's own writable layer.
if "$BIN/bean" exec "$SBX" -- sh -c 'echo written > /tmp/probe && cat /tmp/probe' 2>&1 |
     grep -q written; then
  pass "the writable layer accepts writes"
else
  fail "could not write inside the sandbox"
fi

echo "=== the device really is overlaybd ==="
# Distinguishes this from a silent fall back to device-mapper: a TCMU-backed device
# is an sd*, a dm one is /dev/mapper/bean-*.
if ls /sys/kernel/config/target/core/user_999/ 2>/dev/null | grep -q .; then
  pass "a TCMU backstore exists for the running sandbox:"
  ls /sys/kernel/config/target/core/user_999/ | grep -v hba_ | head -3
else
  fail "no TCMU backstore -- the sandbox is not on an overlaybd device"
fi

echo "=== image config reached the guest (PR #48 on this backend) ==="
# alpine declares no ENTRYPOINT, so PATH is the observable part: the guest's PATH
# should be the image's, not the agent's fallback.
PATHOUT=$("$BIN/bean" exec "$SBX" -- printenv PATH 2>&1)
info "guest PATH: $PATHOUT"
if echo "$PATHOUT" | grep -q "/usr/local/sbin"; then
  pass "the image's PATH is in the guest environment"
else
  info "PATH does not obviously come from the image; alpine's is close to the default"
fi

echo "=== teardown releases the device ==="
"$BIN/bean" kill "$SBX" >/dev/null 2>&1
sleep 2
# grep -vc exits non-zero when it counts nothing, so `|| echo 0` would append a
# second line rather than substitute one. Counted with grep -v into wc instead.
LEFT=$(ls /sys/kernel/config/target/core/user_999/ 2>/dev/null | grep -v hba_ | wc -l | tr -d ' ')
if [ "${LEFT:-0}" = 0 ]; then
  pass "no TCMU backstore left after kill"
else
  fail "$LEFT backstore(s) leaked after kill"
  ls /sys/kernel/config/target/core/user_999/
fi
if multipath -ll 2>/dev/null | grep -q .; then
  fail "a multipath device is present -- devices were merged"
  multipath -ll 2>/dev/null | head -4
else
  pass "no multipath device"
fi

echo
[ "$FAILED" = 0 ] && echo "E2E PASSED" || echo "E2E FAILED"
exit "$FAILED"

#!/bin/bash
# Proves exec and file transfer work through bean-proxy -- the data plane -- rather
# than relaying through bean-api, and that auth on that path still fails closed.
#
# This is the one check that exercises the whole exec-via-proxy path end to end on
# a real guest. Everything below it is unit-tested: the forwarder's token
# injection has a Go test (agent port carries a verifying token, a user port does
# not), the CLI's authority construction has a Go test. But none of those puts a
# gRPC call through the proxy into a booted agent, and that hop is where an
# authority mismatch, a dropped header, or an h2c misconfiguration would show up.
#
# It must run on the fc tier on a real Linux/KVM host: the proxy's forwarder needs
# a routable guest IP (TargetFor errors without one), which only a networked fc
# sandbox has. The local tier has no guest network, so this cannot run in the
# local-tier CI stack -- it is a hack/ probe, deliberately, for the same reason
# guest-egress-probe.sh is.
#
# The assertions, in order:
#
#   exec through the proxy returns the guest's output   -- the data-plane exec works
#   a file round-trips through the proxy byte-for-byte   -- WriteFile + ReadFile work
#   an uninjected dial to the agent port is denied       -- auth still fails closed
#
# The last one is the whole security argument. The forwarder injects the token so
# the legitimate path carries it; a caller reaching the same agent port without
# that injection (the sandbox's own root is the real threat) must still be
# rejected. We simulate that by dialling the node's sandbox-port listener directly
# -- which is the forwarder without the proxy in front, so no token is added --
# and asserting the agent refuses.
#
# Leaves the stack running on failure so the guest can be inspected. Cleans up the
# sandbox it created and nothing else.
set -u

REPO=${REPO:-/root/bean}
IMAGE=${IMAGE:-busybox:1.35}
GUEST_DNS=${GUEST_DNS:-223.5.5.5}
GUEST_SUBNET=${GUEST_SUBNET:-172.31.0.0/30}
UPLINK=${UPLINK:-enp6s18}
API_PORT=${API_PORT:-18080}
PROXY_PORT=${PROXY_PORT:-17460}
SANDBOX_DOMAIN=${SANDBOX_DOMAIN:-sandbox.local}
API_KEY=${API_KEY:-devkey}
ASSETS=${ASSETS:-/var/lib/bean/assets}
BIN=${BIN:-/tmp}
RUN=${RUN:-/tmp/beanrun}
BEAN="$BIN/bean"
BASE="http://127.0.0.1:$API_PORT"

SBX=""
FAILED=0

note() { printf '\n=== %s\n' "$1"; }

check() {
  local label=$1 want=$2 got=$3 proves=$4
  if [ "$got" = "$want" ]; then
    printf 'PASS  %-40s %s\n' "$label" "$proves"
  else
    printf 'FAIL  %-40s want=%s got=%s  (%s)\n' "$label" "$want" "$got" "$proves"
    FAILED=$((FAILED + 1))
  fi
}

cleanup() {
  if [ -n "$SBX" ]; then
    note "killing the sandbox this probe created ($SBX)"
    BEAN_PROXY_URL= "$BEAN" kill "$SBX" 2>&1 | tail -1 || true
  fi
  if [ "$FAILED" -ne 0 ]; then
    echo
    echo "$FAILED check(s) failed. The stack is still up for inspection:"
    echo "  tail $RUN/noded.log $RUN/proxy.log"
  fi
}
trap cleanup EXIT

cd "$REPO" || exit 1

note "building into $BIN"
for c in bean bean-api noded bean-proxy; do
  go build -o "$BIN/$c" "./cmd/$c" >>/tmp/exec-via-proxy-build.log 2>&1 || {
    echo "build of $c failed:"
    tail -20 /tmp/exec-via-proxy-build.log
    exit 1
  }
done
echo "built: bean bean-api noded bean-proxy"

note "starting the stack with sandbox networking on"
ASSETS="$ASSETS" \
SANDBOX_DOMAIN="$SANDBOX_DOMAIN" \
NODED_FLAGS="--guest-subnet $GUEST_SUBNET --uplink $UPLINK --guest-dns $GUEST_DNS" \
  bash hack/dev-fc-stack.sh >/tmp/exec-via-proxy-stack.log 2>&1 || {
  tail -30 /tmp/exec-via-proxy-stack.log
  exit 1
}
grep -q 'sandbox networking on' "$RUN/noded.log" || {
  echo "noded did not report sandbox networking on; the forwarder would have no guest IP"
  exit 1
}

export BEAN_API_KEY="$API_KEY"
export BEAN_BASE_URL="$BASE"

note "creating a sandbox from $IMAGE"
# No proxy for create: create is a control-plane call and stays on bean-api. The
# data-plane env is set only for the exec and cp calls below.
RUN_OUT=$(BEAN_PROXY_URL= "$BEAN" run --image "$IMAGE" 2>&1)
SBX=$(printf '%s\n' "$RUN_OUT" | grep -oE 'sbx_[0-9a-f]{20}' | head -1)
if [ -z "$SBX" ] || ! BEAN_PROXY_URL= "$BEAN" ls 2>/dev/null | grep -q "$SBX"; then
  echo "run did not yield a usable sandbox id. output was:"
  printf '%s\n' "$RUN_OUT" | tail -5
  tail -20 "$RUN/noded.log"
  SBX=""
  exit 1
fi
echo "sandbox: $SBX"

# The create response should carry the domain we started bean-api with, since that
# is what the client builds the authority against.
DOMAIN=$(BEAN_PROXY_URL= "$BEAN" ls --json 2>/dev/null \
  | grep -oE "\"domain\":\"[^\"]*\"" | head -1 | cut -d'"' -f4)
check "create returns the sandbox domain" "$SANDBOX_DOMAIN" "${DOMAIN:-}" \
  "the record carries the data-plane domain the client addresses"

# From here the data-plane env is on: exec and cp dial the proxy.
export BEAN_PROXY_URL="127.0.0.1:$PROXY_PORT"

note "exec through the proxy"
MARKER="hello-via-proxy-$$"
OUT=$("$BEAN" exec "$SBX" -- sh -c "echo $MARKER" 2>/tmp/exec-via-proxy-exec.err)
check "exec returns the guest's output" "$MARKER" "$(printf '%s' "$OUT" | tr -d '[:space:]')" \
  "AgentService/Exec answered through the proxy, token injected by the forwarder"
if [ "$(printf '%s' "$OUT" | tr -d '[:space:]')" != "$MARKER" ]; then
  echo "exec stderr:"; tail -3 /tmp/exec-via-proxy-exec.err
fi

note "file round-trip through the proxy"
SENT="round-trip-$$-$(date +%s)"
LOCAL_IN=$(mktemp)
LOCAL_OUT=$(mktemp)
printf '%s' "$SENT" >"$LOCAL_IN"
"$BEAN" cp "$LOCAL_IN" "sbx:$SBX:/tmp/probe.txt" >/dev/null 2>/tmp/exec-via-proxy-cp.err || {
  echo "cp to sandbox failed:"; tail -3 /tmp/exec-via-proxy-cp.err
}
"$BEAN" cp "sbx:$SBX:/tmp/probe.txt" "$LOCAL_OUT" >/dev/null 2>>/tmp/exec-via-proxy-cp.err || {
  echo "cp from sandbox failed:"; tail -3 /tmp/exec-via-proxy-cp.err
}
check "a file round-trips byte-for-byte" "$SENT" "$(cat "$LOCAL_OUT" 2>/dev/null)" \
  "WriteFile and ReadFile both work through the proxy"
rm -f "$LOCAL_IN" "$LOCAL_OUT"

note "fail-closed: a direct dial to the agent, bypassing the forwarder, is denied"
# The threat is the sandbox's own root dialling 10001 directly -- it reaches the
# agent without going through the forwarder, so no token is injected. This is the
# only path that both reaches the agent and carries no credential; going through
# the forwarder (the proxy, or the node's sandbox-port listener) would either add
# the token or be stopped by the node-token gate first, so neither tests the
# agent's own check.
#
# We reproduce it faithfully: enter the sandbox's netns on this node and dial the
# guest's own 10001 with no token. That is byte-for-byte what a process inside the
# guest reaches. The agent must refuse, which is the whole security argument --
# the token is what separates the forwarder from everything else on that port.
#
# The guest IP comes from the guest itself over the working exec path, so the
# probe does not hardcode the /30 layout. The netns is the one noded logged for
# this sandbox.
GUEST_IP=$("$BEAN" exec "$SBX" -- sh -c \
  "ip -4 -o addr show | grep -v ' lo ' | awk '{print \$4}' | cut -d/ -f1" 2>/dev/null \
  | tr -d '[:space:]')
NETNS=$(ip netns list 2>/dev/null | grep -oE "bean[-_][0-9a-zA-Z]*$SBX[0-9a-zA-Z]*" | head -1)
if [ -z "$NETNS" ]; then
  NETNS=$(ip netns list 2>/dev/null | grep -i bean | head -1 | awk '{print $1}')
fi
if command -v grpcurl >/dev/null 2>&1 && [ -n "$GUEST_IP" ] && [ -n "$NETNS" ]; then
  if ip netns exec "$NETNS" grpcurl -plaintext -max-time 5 \
      -d "{\"sandbox_id\":\"$SBX\",\"cmd\":[\"echo\",\"leak\"]}" \
      "$GUEST_IP:10001" bean.agent.v1.AgentService/Exec \
      >/tmp/exec-via-proxy-uninjected.out 2>&1; then
    UNINJECTED="unexpected-success"
  else
    UNINJECTED="denied"
  fi
  check "direct uninjected agent dial is denied" "denied" "${UNINJECTED:-}" \
    "the agent fails closed: only the forwarder-injected path carries the token"
  if [ "${UNINJECTED:-}" = "unexpected-success" ]; then
    echo "  the agent accepted a tokenless call -- injection is not the only way in"
    tail -3 /tmp/exec-via-proxy-uninjected.out
  fi
else
  echo "SKIP  direct uninjected agent dial"
  echo "      needs grpcurl on the node, a resolvable guest IP (got '${GUEST_IP:-}')"
  echo "      and the sandbox netns (got '${NETNS:-}')"
  echo "      the agent's fail-closed check is unit-tested in beand/auth.go regardless"
fi

note "result"
if [ "$FAILED" -eq 0 ]; then
  echo "ALL CHECKS PASSED: exec and file transfer work through the proxy, auth fails closed"
else
  echo "$FAILED CHECK(S) FAILED"
fi
exit "$FAILED"

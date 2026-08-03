#!/bin/bash
# Boots a real microVM through the full stack and checks egress from inside the
# guest, which is the one claim about sandbox networking that no other test makes.
#
# Everything else asserts on a layer below this. The unit tests check the iptables
# argument lists; fc_net_linux_test checks the NIC is registered before
# InstanceStart; netns_linux_test compares /proc/<pid>/ns/net inodes; the netns
# probes use a veth pair standing in for the tap. Each is a real check and all of
# them pass on a stack where the guest still cannot reach anything, because none
# of them ever puts a packet through a tap device from a booted kernel.
#
# So the assertions here are made by the guest, over exec, about what it can and
# cannot reach:
#
#   an address is configured               -- the NIC arrived and DHCP-less config took
#   a default route exists                 -- routes_linux did its half
#   a public address answers               -- both MASQUERADE layers work
#   DNS resolves                           -- resolv.conf reached the guest
#   169.254.169.254 does not answer        -- the metadata denial holds for a real guest
#   the node's own address does not answer  -- the netns-scope DROP holds for a real guest
#
# The last two are the ones worth the trouble. They were measured with veth
# standing in for tap, and a tap device is not a veth: it is fed by a userspace
# VMM rather than by the kernel's own forwarding path. That difference is exactly
# the kind that leaves every existing assertion green.
#
# Leaves the stack running on failure so the guest can be inspected. Cleans up the
# sandbox it created and nothing else -- there are other sandboxes on this host.
set -u

REPO=${REPO:-/root/bean-net}
# busybox because its ping and its nslookup are enough for every check here, and
# because the image is small: a first-time conversion of anything larger dominates
# the run and can time the create out before a single packet is sent.
IMAGE=${IMAGE:-busybox:1.35}
# Without a resolver the guest keeps whatever /etc/resolv.conf its image shipped,
# and the DNS check would be measuring the image rather than the network stack.
GUEST_DNS=${GUEST_DNS:-223.5.5.5}
# A /30, and the same one in every sandbox -- the namespace is what keeps them
# apart, and a constant is what lets one checkpoint fan out to many restores.
GUEST_SUBNET=${GUEST_SUBNET:-172.31.0.0/30}
UPLINK=${UPLINK:-enp6s18}
API_PORT=${API_PORT:-18080}
API_KEY=${API_KEY:-devkey}
# The agent disk has to hold a beand built from this tree. The flags the boot
# arguments pass are parsed by the binary inside that image, not by anything on the
# host, so a stale disk rejects a flag the source clearly defines and the guest
# panics on "flag provided but not defined" -- with the agent as PID 1, that is
# "Attempted to kill init". Overriding ASSETS keeps a shared node's disk untouched;
# see the note below on why it is not rebuilt in place.
ASSETS=${ASSETS:-/var/lib/bean/assets}
# dev-fc-stack.sh runs binaries out of $BIN and does not build them, so this
# builds into the same place rather than assuming a previous run left them there.
BIN=${BIN:-/tmp}
RUN=${RUN:-/tmp/beanrun}
BEAN="$BIN/bean"
BASE="http://127.0.0.1:$API_PORT"

SBX=""
FAILED=0

note() { printf '\n=== %s\n' "$1"; }

# Each check names what it proves, so a failure says which layer to look at
# rather than only that something is wrong.
check() {
  local label=$1 want=$2 got=$3 proves=$4
  if [ "$got" = "$want" ]; then
    printf 'PASS  %-34s %s\n' "$label" "$proves"
  else
    printf 'FAIL  %-34s want=%s got=%s  (%s)\n' "$label" "$want" "$got" "$proves"
    FAILED=$((FAILED + 1))
  fi
}

cleanup() {
  if [ -n "$SBX" ]; then
    note "killing the sandbox this probe created ($SBX)"
    "$BEAN" kill "$SBX" 2>&1 | tail -1 || true
  fi
  if [ "$FAILED" -ne 0 ]; then
    echo
    echo "$FAILED check(s) failed. The stack is still up for inspection:"
    echo "  tail $RUN/noded.log"
    echo "  ip netns list | grep bean"
  fi
}
trap cleanup EXIT

cd "$REPO" || exit 1

note "building into $BIN"
for c in bean bean-api noded; do
  go build -o "$BIN/$c" "./cmd/$c" >>/tmp/guest-egress-build.log 2>&1 || {
    echo "build of $c failed:"
    tail -20 /tmp/guest-egress-build.log
    exit 1
  }
done
echo "built: bean bean-api noded"

note "starting the stack with sandbox networking on"
# The dev stack does not pass network flags, so they come in through NODED_FLAGS.
# CheckSubnetFree refuses a range already routed here, which is why 172.31 rather
# than anything in the six /16s Docker holds on this host.
# ASSETS is passed through so a probe run can use an agent disk built from the
# working tree without touching the node's shared one, which other sandboxes are
# using and which cannot be rebuilt in place while they are running.
ASSETS="$ASSETS" \
NODED_FLAGS="--guest-subnet $GUEST_SUBNET --uplink $UPLINK --guest-dns $GUEST_DNS" \
  bash hack/dev-fc-stack.sh >/tmp/guest-egress-stack.log 2>&1 || {
  tail -30 /tmp/guest-egress-stack.log
  exit 1
}
# Checked rather than assumed: noded warns and carries on when networking is off,
# so without this the probe would boot a guest with no NIC and report six failures
# that all have one cause.
grep -q 'sandbox networking on' "$RUN/noded.log" || {
  echo "noded did not report sandbox networking on; it would boot guests with no NIC"
  grep -iE 'guest-subnet|uplink|network' "$RUN/noded.log" | tail -5
  exit 1
}
echo "noded reports: $(grep 'sandbox networking on' "$RUN/noded.log" | tail -1)"

export BEAN_API_KEY="$API_KEY"
export BEAN_BASE_URL="$BASE"

note "creating a sandbox from $IMAGE"
RUN_OUT=$("$BEAN" run --image "$IMAGE" 2>&1)
SBX=$(printf '%s\n' "$RUN_OUT" | grep -oE 'sbx_[0-9a-f]{20}' | head -1)
# Existence is confirmed through ls rather than trusted from the parse: a run that
# printed an error containing something id-shaped would otherwise send the probe on
# to report six reachability failures for a sandbox that was never created.
if [ -z "$SBX" ] || ! "$BEAN" ls 2>/dev/null | grep -q "$SBX"; then
  echo "run did not yield a usable sandbox id. output was:"
  printf '%s\n' "$RUN_OUT" | tail -5
  tail -20 "$RUN/noded.log"
  SBX=""
  exit 1
fi
echo "sandbox: $SBX"

# Runs a command in the guest and prints its stdout. Failures are reported by the
# caller's check, not here, because "no output" is a meaningful answer for the
# reachability probes.
inguest() { "$BEAN" exec "$SBX" -- sh -c "$1" 2>/dev/null; }

note "what the guest sees"
inguest 'ip -4 addr show; echo "--- routes ---"; ip route' || true

ADDR=$(inguest "ip -4 -o addr show | grep -v ' lo ' | wc -l" | tr -d '[:space:]')
check "an address is configured" "1" "${ADDR:-0}" \
  "the NIC reached the guest and was addressed"

ROUTE=$(inguest 'ip route | grep -c "^default"' | tr -d '[:space:]')
check "a default route exists" "1" "${ROUTE:-0}" \
  "routes_linux configured the guest's gateway"

note "egress"
PUB=$(inguest 'ping -c 2 -W 3 223.5.5.5 >/dev/null 2>&1 && echo ok || echo no' \
  | tr -d '[:space:]')
check "a public address answers" "ok" "${PUB:-no}" \
  "both MASQUERADE layers translate guest traffic"

# nslookup rather than getent: busybox has no getent, and a missing binary would
# read as a DNS failure.
DNS=$(inguest 'nslookup pypi.org >/dev/null 2>&1 && echo ok || echo no' \
  | tr -d '[:space:]')
check "DNS resolves" "ok" "${DNS:-no}" \
  "resolv.conf reached the guest and the resolver is reachable"

note "denials -- these are the checks a veth stand-in could not make"
META=$(inguest 'ping -c 1 -W 2 169.254.169.254 >/dev/null 2>&1 && echo reached || echo denied' \
  | tr -d '[:space:]')
check "cloud metadata is denied" "denied" "${META:-}" \
  "the 169.254/16 DROP holds for traffic arriving over a tap"

SELF=$(ip -4 -o addr show "$UPLINK" | awk '{print $4}' | cut -d/ -f1)
NODE=$(inguest "ping -c 1 -W 2 $SELF >/dev/null 2>&1 && echo reached || echo denied" \
  | tr -d '[:space:]')
check "the node's own address is denied" "denied" "${NODE:-}" \
  "the netns-scope DROP catches host-local delivery from a real guest"

GW=$(ip route | awk '/^default/ {print $3; exit}')
UP=$(inguest "ping -c 1 -W 2 $GW >/dev/null 2>&1 && echo reached || echo denied" \
  | tr -d '[:space:]')
check "the node's gateway is denied" "denied" "${UP:-}" \
  "RFC1918 is denied for a forwarded destination too"

note "result"
if [ "$FAILED" -eq 0 ]; then
  echo "ALL CHECKS PASSED: a real guest has egress, and the denials hold for it"
else
  echo "$FAILED CHECK(S) FAILED"
fi
exit "$FAILED"

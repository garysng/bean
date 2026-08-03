#!/usr/bin/env bash
# Builds the network plumbing for one sandbox by hand and proves a guest-side
# address can reach the internet through it.
#
# This exists to validate the design in docs/network.md before any Go is written.
# The plumbing is four objects (netns, tap, veth pair, two NAT rules) and getting
# their order or their teardown wrong is the kind of mistake that shows up as
# "networking works but only sometimes" — much cheaper to find here than inside
# a runtime.
#
# It does NOT start a microVM. The tap stands in for one: an address on the tap
# inside the netns is routed exactly as a guest's address would be, so if that
# address can reach the internet, a guest on the same tap can too.
#
# Safety: every object is prefixed and removed on exit. The host runs other
# projects' containers with their own iptables rules, so NAT rules here are
# inserted with an exact -s match and deleted with the same match — never -F.
#
# Usage: netns-egress-probe.sh [--guest-subnet 172.31.0.0/30] [--index N]
set -uo pipefail

GUEST_NET=${GUEST_NET:-172.31.0.0/30}
INDEX=${INDEX:-0}
TARGET=${TARGET:-8.8.8.8}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --guest-subnet) GUEST_NET="$2"; shift 2 ;;
    --index) INDEX="$2"; shift 2 ;;
    --target) TARGET="$2"; shift 2 ;;
    -h|--help) sed -n '2,18p' "$0" | sed 's/^# \?//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 64 ;;
  esac
done

say() { printf '%s\n' "$*"; }
hr() { printf -- '------------------------------------------------------------\n'; }

[[ $EUID -eq 0 ]] || { say "needs root"; exit 77; }

NS="bean-probe-$INDEX"
TAP="beantap0"
VETH_HOST="bean-veth$INDEX"
VETH_NS="bean-vpeer$INDEX"

# Guest side is identical for every sandbox: that is the whole point, since a
# restored snapshot comes back with the address it had.
GUEST_GW="${GUEST_NET%.*}.1"
GUEST_IP="${GUEST_NET%.*}.2"

# Host side must be unique per sandbox because both ends live in the host's
# namespace. /30 steps of 4 give one point-to-point link each.
LINK_A=$((INDEX / 64))
LINK_B=$(((INDEX % 64) * 4))
LINK_NET="10.$LINK_A.$LINK_B.0/30"
LINK_HOST="10.$LINK_A.$LINK_B.1"
LINK_NS="10.$LINK_A.$LINK_B.2"

UPLINK=$(ip -j route list default 2>/dev/null |
  python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["dev"])' 2>/dev/null)
[[ -n "$UPLINK" ]] || { say "cannot determine the uplink interface"; exit 69; }

# Teardown in reverse order of setup, every step tolerant of already-gone: the
# probe deliberately exercises failure paths, so cleanup must not stop at the
# first non-zero status and leave NAT rules behind.
cleanup() {
  ip netns exec "$NS" iptables -t nat -D POSTROUTING -s "$GUEST_NET" \
    -o "$VETH_NS" -j MASQUERADE 2>/dev/null || true
  iptables -t nat -D POSTROUTING -s "$LINK_NET" -o "$UPLINK" -j MASQUERADE 2>/dev/null || true
  ip link del "$VETH_HOST" 2>/dev/null || true
  ip netns del "$NS" 2>/dev/null || true
}
trap cleanup EXIT

# A subnet already routed on this host would have its traffic captured by
# whatever owns it — the host runs Docker with six MASQUERADE rules over
# 172.17-172.22, so this check is not theoretical.
if ip route list | grep -q "^${GUEST_NET%/*}"; then
  say "refusing: $GUEST_NET is already routed on this host"
  ip route list | grep "^${GUEST_NET%/*}" | sed 's/^/  /'
  exit 65
fi

say "guest side (same for every sandbox): $GUEST_IP/30 via $GUEST_GW"
say "link side (unique per sandbox):      $LINK_NS/30 via $LINK_HOST"
say "uplink: $UPLINK"
hr

ip netns add "$NS"

# The tap keeps the same name in every namespace, which is what lets a restored
# snapshot find the device it recorded without renumbering the guest.
ip netns exec "$NS" ip tuntap add name "$TAP" mode tap
ip netns exec "$NS" ip addr add "$GUEST_GW/30" dev "$TAP"
ip netns exec "$NS" ip link set "$TAP" up
ip netns exec "$NS" ip link set lo up

ip link add name "$VETH_HOST" type veth peer name "$VETH_NS" netns "$NS"
ip addr add "$LINK_HOST/30" dev "$VETH_HOST"
ip link set "$VETH_HOST" up
ip netns exec "$NS" ip addr add "$LINK_NS/30" dev "$VETH_NS"
ip netns exec "$NS" ip link set "$VETH_NS" up
ip netns exec "$NS" ip route add default via "$LINK_HOST"

# Two translations, so two rules: guest subnet → veth subnet → uplink.
ip netns exec "$NS" iptables -t nat -A POSTROUTING -s "$GUEST_NET" \
  -o "$VETH_NS" -j MASQUERADE
iptables -t nat -A POSTROUTING -s "$LINK_NET" -o "$UPLINK" -j MASQUERADE

say "plumbing up:"
ip netns exec "$NS" ip -brief addr | sed 's/^/  /'
hr

# The tap address stands in for the guest: it is routed identically, so reaching
# the internet from it proves a guest on this tap could.
say "can the guest's gateway address reach $TARGET?"
if ip netns exec "$NS" ping -c 2 -W 3 -I "$GUEST_GW" "$TARGET" 2>&1 | tail -3; then
  say "PASS: egress works from the guest subnet"
else
  say "FAIL: no egress from the guest subnet"
  exit 70
fi

hr
say "does DNS resolve through it?"
if ip netns exec "$NS" getent hosts github.com >/dev/null 2>&1; then
  say "  yes"
else
  say "  no — the netns has no resolver configured, which is expected here:"
  say "  the design has the agent write /etc/resolv.conf inside the guest"
fi

hr
say "NAT rules added (these must delete precisely; the host has Docker's own):"
iptables -t nat -S POSTROUTING | grep "$LINK_NET" | sed 's/^/  /'

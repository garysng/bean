#!/bin/bash
# Asks whether a sandbox can reach the node's own listening addresses, using a
# topology that is faithful to bean's two-scope filter.
#
# An earlier version of this probe pinged from inside the sandbox namespace and
# concluded the filter had a hole. That conclusion was unfounded, for two reasons
# that are worth stating because both are easy to repeat:
#
#   1. It applied rules only in the host namespace. bean's denials exist in both
#      scopes, and the netns-side rule matches -s <guest subnet>, which is a
#      source the host never sees. Testing one scope says nothing about the pair.
#   2. A ping generated inside the sandbox namespace is locally generated there,
#      so it traverses OUTPUT and never the namespace's FORWARD chain at all. A
#      real guest sits behind a tap device and its packets are genuinely
#      forwarded, which is a different path through netfilter.
#
# So this builds three namespaces: a guest, the sandbox that forwards for it, and
# the host. Traffic from the guest is forwarded twice, exactly as a microVM's is,
# and both scopes' rules are installed. veth stands in for the tap: netfilter
# treats both as an ingress interface into the forwarding path, and using veth
# means the probe needs no microVM.
#
# Every claim is read off a rule's own packet counter. Reachability alone cannot
# say which chain acted, and that distinction is the whole question here.
#
# Deletes only by exact match, never by flush -- the host's nat table holds
# Docker's and nexus's rules. Prints counts before and after so a leak is visible.
set -u

GUEST_NS="bean-probe-guest"
SBX_NS="bean-probe-sbx"

GUEST_SUBNET="172.31.240.0/30"
GUEST_IP="172.31.240.2"
GUEST_GW="172.31.240.1"

LINK="10.140.168.0/30"
LINK_HOST_IP="10.140.168.1"
LINK_SBX_IP="10.140.168.2"

VH="vhl-h"       # host end of the host <-> sandbox pair
VS="vhl-s"       # sandbox end of the same pair
VT="vhl-t"       # sandbox end of the sandbox <-> guest pair (stands in for tap0)
VG="vhl-g"       # guest end

UPLINK="${UPLINK:-enp6s18}"
DENIED="169.254.0.0/16 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16"
RANGE="192.168.0.0/16"

fwd_count() { iptables -S FORWARD | wc -l; }
nat_count() { iptables -t nat -S POSTROUTING | wc -l; }

BEFORE_FWD=$(fwd_count)
BEFORE_NAT=$(nat_count)
echo "baseline: FORWARD=$BEFORE_FWD nat/POSTROUTING=$BEFORE_NAT"

cleanup() {
  ip netns del "$GUEST_NS" 2>/dev/null
  ip netns del "$SBX_NS" 2>/dev/null
  ip link del "$VH" 2>/dev/null
  iptables -t nat -D POSTROUTING -s "$LINK" -o "$UPLINK" -j MASQUERADE 2>/dev/null
  for d in $DENIED; do
    iptables -D FORWARD -s "$LINK" -d "$d" -j DROP 2>/dev/null
  done
  iptables -D FORWARD -d "$LINK" -j ACCEPT 2>/dev/null
  iptables -D FORWARD -s "$LINK" -j ACCEPT 2>/dev/null

  AFTER_FWD=$(fwd_count)
  AFTER_NAT=$(nat_count)
  echo
  echo "after teardown: FORWARD=$AFTER_FWD nat/POSTROUTING=$AFTER_NAT"
  if [ "$AFTER_FWD" = "$BEFORE_FWD" ] && [ "$AFTER_NAT" = "$BEFORE_NAT" ]; then
    echo "TEARDOWN CLEAN"
  else
    echo "TEARDOWN LEAKED: expected $BEFORE_FWD/$BEFORE_NAT"
    iptables -S FORWARD | grep -E "10.140.168|172.31.240" || true
  fi
}
trap cleanup EXIT

# --- topology ---------------------------------------------------------------
ip netns add "$SBX_NS" || exit 1
ip netns add "$GUEST_NS" || exit 1

ip link add "$VH" type veth peer name "$VS" || exit 1
ip link set "$VS" netns "$SBX_NS"
ip addr add "$LINK_HOST_IP/30" dev "$VH"
ip link set "$VH" up

ip link add "$VT" type veth peer name "$VG" || exit 1
ip link set "$VT" netns "$SBX_NS"
ip link set "$VG" netns "$GUEST_NS"

ip -n "$SBX_NS" link set lo up
ip -n "$SBX_NS" addr add "$LINK_SBX_IP/30" dev "$VS"
ip -n "$SBX_NS" link set "$VS" up
ip -n "$SBX_NS" addr add "$GUEST_GW/30" dev "$VT"
ip -n "$SBX_NS" link set "$VT" up
ip -n "$SBX_NS" route add default via "$LINK_HOST_IP"
ip netns exec "$SBX_NS" sysctl -qw net.ipv4.ip_forward=1

ip -n "$GUEST_NS" link set lo up
ip -n "$GUEST_NS" addr add "$GUEST_IP/30" dev "$VG"
ip -n "$GUEST_NS" link set "$VG" up
ip -n "$GUEST_NS" route add default via "$GUEST_GW"

sysctl -qw net.ipv4.ip_forward=1

# --- bean's rules, both scopes, in bean's application order -----------------
# Netns scope: denials matched on the guest subnet, then an appended ACCEPT,
# then MASQUERADE onto the link.
for d in $DENIED; do
  ip netns exec "$SBX_NS" iptables -I FORWARD 1 -s "$GUEST_SUBNET" -d "$d" -j DROP
done
ip netns exec "$SBX_NS" iptables -A FORWARD -s "$GUEST_SUBNET" -j ACCEPT
ip netns exec "$SBX_NS" iptables -t nat -A POSTROUTING -s "$GUEST_SUBNET" -o "$VS" -j MASQUERADE

# Host scope: ACCEPTs inserted first so the DROPs inserted after finish above them.
iptables -I FORWARD -s "$LINK" -j ACCEPT
iptables -I FORWARD -d "$LINK" -j ACCEPT
for d in $DENIED; do
  iptables -I FORWARD -s "$LINK" -d "$d" -j DROP
done
iptables -t nat -A POSTROUTING -s "$LINK" -o "$UPLINK" -j MASQUERADE

# --- counters ---------------------------------------------------------------
netns_drop() {
  ip netns exec "$SBX_NS" iptables -L FORWARD -v -n -x \
    | awk -v s="${GUEST_SUBNET}" -v d="${RANGE}" \
        '$3 == "DROP" && $8 == s && $9 == d {print $1; f=1} END {if (!f) print "NO-RULE"}'
}
host_drop() {
  iptables -L FORWARD -v -n -x \
    | awk -v s="${LINK}" -v d="${RANGE}" \
        '$3 == "DROP" && $8 == s && $9 == d {print $1; f=1} END {if (!f) print "NO-RULE"}'
}

SELF=$(ip -4 -o addr show "$UPLINK" | awk '{print $4}' | cut -d/ -f1)
GW=$(ip route | awk '/^default/ {print $3; exit}')
echo
echo "node's own address on $UPLINK: $SELF   (delivered locally by the host)"
echo "node's default gateway:        $GW   (forwarded by the host)"
echo "both inside $RANGE, both covered by the DROP in each scope"

probe() {
  local target=$1 label=$2
  echo
  echo "--- from the guest to $target ($label) ---"
  local nb hb na ha result
  nb=$(netns_drop); hb=$(host_drop)
  result=$(ip netns exec "$GUEST_NS" ping -c 2 -W 2 "$target" 2>&1 \
    | grep -o '[0-9]*% packet loss')
  na=$(netns_drop); ha=$(host_drop)
  echo "reachability:      ${result:-no reply line}"
  echo "netns DROP count:  $nb -> $na"
  echo "host  DROP count:  $hb -> $ha"
  if [ "$nb" != "$na" ]; then
    echo "VERDICT:           denied in the sandbox namespace, before the host saw it"
  elif [ "$hb" != "$ha" ]; then
    echo "VERDICT:           reached the host and was denied there"
  else
    echo "VERDICT:           NO DROP RULE WAS CONSULTED IN EITHER SCOPE"
  fi
}

probe "$SELF" "the node itself"
probe "$GW" "a forwarded address in the same range"

echo
echo "--- control: egress to a public address must still work ---"
ip netns exec "$GUEST_NS" ping -c 2 -W 3 223.5.5.5 2>&1 | tail -2

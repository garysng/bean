#!/bin/bash
# Verifies on a live kernel what the unit tests structurally cannot: that bean's
# rule ordering produces egress, that the denials actually deny, and that
# teardown returns the host to exactly the state it was found in.
#
# The unit tests assert on the commands that would be run. Whether the kernel
# then behaves as intended is a different claim, and this is the only thing that
# can settle it. It matters most on a host whose FORWARD policy is DROP with no
# general ACCEPT -- measured on 192.168.75.52, where Docker's ACCEPTs all match
# on its own bridges and so do nothing for bean's veth. On such a host the
# host-side ACCEPT is what makes egress work at all, and its position relative to
# the DROPs is what makes the denials hold.
#
# Uses a throwaway index far above anything the allocator would assign, its own
# /30, and its own device names. Deletes only by exact match, never by flush,
# and prints rule counts before and after so a leak is visible rather than
# assumed. Safe to run on a shared host; it is written to be run on one.
set -u

IDX=9001
NS="bean-probe-$IDX"
LINK="10.140.164.0/30"
HOSTIP="10.140.164.1"
NSIP="10.140.164.2"
VH="vprobe-h"
VN="vprobe-n"
UPLINK="${UPLINK:-enp6s18}"
RESOLVER="${RESOLVER:-223.5.5.5}"
DENIED="169.254.0.0/16 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16"

fwd_count() { iptables -S FORWARD | wc -l; }
nat_count() { iptables -t nat -S POSTROUTING | wc -l; }

BEFORE_FWD=$(fwd_count)
BEFORE_NAT=$(nat_count)
echo "baseline: FORWARD=$BEFORE_FWD nat/POSTROUTING=$BEFORE_NAT"

# Every delete names the rule in full. A flush here would take Docker's and
# nexus's rules with it, which is unrecoverable, so it is not in this file at all.
cleanup() {
  ip netns del "$NS" 2>/dev/null
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
    echo "TEARDOWN CLEAN: host is back to its baseline rule counts"
  else
    echo "TEARDOWN LEAKED: expected $BEFORE_FWD/$BEFORE_NAT"
    iptables -S FORWARD | grep "10.140.164" || true
    iptables -t nat -S POSTROUTING | grep "10.140.164" || true
  fi
}
trap cleanup EXIT

ip netns add "$NS" || exit 1
ip link add "$VH" type veth peer name "$VN" || exit 1
ip link set "$VN" netns "$NS"
ip addr add "$HOSTIP/30" dev "$VH"
ip link set "$VH" up
ip netns exec "$NS" ip link set lo up
ip netns exec "$NS" ip addr add "$NSIP/30" dev "$VN"
ip netns exec "$NS" ip link set "$VN" up
ip netns exec "$NS" ip route add default via "$HOSTIP"
sysctl -qw net.ipv4.ip_forward=1

# The ordering under test: ACCEPTs inserted first, DROPs inserted after, so the
# DROPs finish on top. Reversing these two loops is the bug the unit tests exist
# to catch, and this is where the kernel confirms the consequence.
iptables -I FORWARD -s "$LINK" -j ACCEPT
iptables -I FORWARD -d "$LINK" -j ACCEPT
for d in $DENIED; do
  iptables -I FORWARD -s "$LINK" -d "$d" -j DROP
done
iptables -t nat -A POSTROUTING -s "$LINK" -o "$UPLINK" -j MASQUERADE

echo
echo "--- live FORWARD, bean's rules only, in kernel order ---"
iptables -S FORWARD | grep -n "10.140.164"

echo
echo "--- egress to a public address (expect 0% loss) ---"
ip netns exec "$NS" ping -c 2 -W 3 "$RESOLVER" 2>&1 | tail -2

echo
echo "--- denied: cloud metadata 169.254.169.254 (expect 100% loss) ---"
ip netns exec "$NS" ping -c 1 -W 2 169.254.169.254 2>&1 | tail -2

echo
echo "--- denied: the host's own segment (expect 100% loss) ---"
ip netns exec "$NS" ping -c 1 -W 2 192.168.75.52 2>&1 | tail -2

echo
echo "--- DNS through the resolver a guest would be given ---"
ip netns exec "$NS" getent ahostsv4 pypi.org 2>&1 | head -2

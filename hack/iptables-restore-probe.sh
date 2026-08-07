#!/usr/bin/env bash
# Does iptables-restore --noflush do what batching the rules would need?
#
# Applying thirteen rules per sandbox one at a time takes the xtables lock thirteen
# times, and the seven host-scope ones contend with every other writer on the machine
# -- Docker included. Under fan-out that showed up as network_setup costing 5.1s per
# create against 0.165s for a single one, 78% of the wall clock for a batch of thirty.
#
# iptables-restore applies a whole table in one transaction, so the question is whether
# it can do that *without* destroying what is already there. Three things have to hold,
# and the third is the one that would be a disaster to get wrong:
#
#   1. --noflush leaves existing rules alone (no -F of the table)
#   2. -I position semantics survive, since bean's DROP rules must precede its ACCEPT
#   3. an unrelated writer's rules -- Docker's -- are still present afterwards
#
# Nothing here is left behind: every rule this adds is removed, and the probe refuses
# to run if it cannot record the starting state first.
#
# Usage: iptables-restore-probe.sh
set -uo pipefail

CHAIN=BEANPROBE
pass() { printf '  PASS  %s\n' "$1"; }
fail() { printf '  FAIL  %s\n' "$1"; FAILED=1; }
FAILED=0

# A private chain rather than FORWARD, so a mistake here cannot affect anything real.
# The probe's whole point is that --noflush is safe, and testing that claim inside a
# live chain would be betting on the answer.
cleanup() {
  iptables -w 5 -t filter -F "$CHAIN" 2>/dev/null
  iptables -w 5 -t filter -X "$CHAIN" 2>/dev/null
}
trap cleanup EXIT
cleanup

iptables -w 5 -t filter -N "$CHAIN" || { echo "cannot create probe chain"; exit 1; }

echo "iptables-restore: $(iptables-restore --version 2>&1 | head -1)"

# Pre-existing rules, standing in for Docker's.
iptables -w 5 -t filter -A "$CHAIN" -s 10.99.0.1/32 -j ACCEPT
iptables -w 5 -t filter -A "$CHAIN" -s 10.99.0.2/32 -j ACCEPT
before=$(iptables -w 5 -t filter -S "$CHAIN" | grep -c "10.99.0")
echo "  seeded $before pre-existing rules"

##### 1. --noflush preserves what is there
echo
echo "### 1. --noflush leaves existing rules alone"
# The rules bean would add: DROPs inserted at the front, an ACCEPT appended.
iptables-restore -w 5 --noflush <<EOF
*filter
-I $CHAIN 1 -s 172.31.0.0/30 -d 192.168.0.0/16 -j DROP
-I $CHAIN 1 -s 172.31.0.0/30 -d 10.0.0.0/8 -j DROP
-A $CHAIN -s 172.31.0.0/30 -j ACCEPT
COMMIT
EOF
rc=$?
if [ $rc -ne 0 ]; then
  fail "iptables-restore exited $rc"
else
  after=$(iptables -w 5 -t filter -S "$CHAIN" | grep -c "10.99.0")
  if [ "$after" -eq "$before" ]; then
    pass "all $before pre-existing rules survived"
  else
    fail "pre-existing rules went from $before to $after -- --noflush is not safe"
  fi
  added=$(iptables -w 5 -t filter -S "$CHAIN" | grep -c "172.31.0.0/30")
  if [ "$added" -eq 3 ]; then
    pass "all 3 new rules applied in one transaction"
  else
    fail "only $added of 3 new rules applied"
  fi
fi

##### 2. insert position is preserved
echo
echo "### 2. -I puts the DROPs before the ACCEPT"
# This is the ordering bean's egress policy depends on: a packet to a private range
# must hit a DROP before reaching the blanket ACCEPT. If restore reordered them the
# rules would still all be present and the policy would silently not apply.
order=$(iptables -w 5 -t filter -S "$CHAIN" | grep "172.31.0.0/30" | \
  awk '{for (i=1; i<=NF; i++) if ($i == "-j") print $(i+1)}' | tr '\n' ' ')
case "$order" in
  "DROP DROP ACCEPT ")
    pass "order is DROP DROP ACCEPT"
    ;;
  *)
    fail "order is \"$order\", wanted DROP DROP ACCEPT -- egress policy would not apply"
    ;;
esac

##### 3. one transaction, not thirteen locks
echo
echo "### 3. a batch costs one lock acquisition"
# Measured rather than asserted: the point of the change is fewer round trips through
# the lock, and if restore internally took one per line there would be nothing to gain.
iptables -w 5 -t filter -F "$CHAIN"
single_start=$(date +%s%N)
for i in $(seq 1 13); do
  iptables -w 5 -t filter -A "$CHAIN" -s "10.98.$i.0/24" -j ACCEPT
done
single_end=$(date +%s%N)

iptables -w 5 -t filter -F "$CHAIN"
batch_start=$(date +%s%N)
{
  echo "*filter"
  for i in $(seq 1 13); do echo "-A $CHAIN -s 10.98.$i.0/24 -j ACCEPT"; done
  echo "COMMIT"
} | iptables-restore -w 5 --noflush
batch_end=$(date +%s%N)

single_ms=$(( (single_end - single_start) / 1000000 ))
batch_ms=$(( (batch_end - batch_start) / 1000000 ))
echo "  13 separate calls: ${single_ms}ms"
echo "  1 batched call:    ${batch_ms}ms"
if [ "$batch_ms" -lt "$single_ms" ]; then
  pass "batching is faster ($(( single_ms - batch_ms ))ms saved uncontended)"
else
  fail "batching was not faster; the lock is not the cost here"
fi

echo
if [ "$FAILED" -eq 0 ]; then
  echo "all probes passed"
else
  echo "at least one probe failed"
fi
exit $FAILED

#!/usr/bin/env bash
# Does the node have a way to reach an agent inside a gVisor sandbox?
#
# gvisor-probe.sh established that a unix socket does NOT work: a process inside
# binds it successfully, and the host never sees the socket -- gVisor implements
# unix sockets inside the Sentry rather than as nodes on the host filesystem. An
# ordinary file written to the same bind mount does appear on the host, so this is
# specific to sockets, not to the mount.
#
# That rules out the transport the local tier uses. This probe tests the other one
# bean already has: internal/node/dial.go's `netns:<path>|<host>:<port>`, whose
# comment describes exactly this situation -- an address that only exists inside
# one sandbox's network namespace. portforward.go:113 already dials that way for
# proxied ports, so a pass here means the container tier needs no new transport.
#
# gVisor's network stack (netstack) is also a userspace implementation, so this is
# not obvious either: the question is whether a listener inside the sandbox is
# reachable through the veth in a pre-created namespace.
#
# Usage: gvisor-netns-probe.sh
set -uo pipefail

RUNSC=${RUNSC:-runsc}
WORK=${WORK:-/tmp/gvisor-probe}
NS=${NS:-beanprobe}
HOSTIF=vbp0
PEERIF=vbp1
HOSTADDR=172.30.99.1/30
GUESTIP=172.30.99.2
PORT=8111

command -v "$RUNSC" >/dev/null || { echo "runsc not found"; exit 1; }
[ -d "$WORK/plain/rootfs" ] || { echo "run gvisor-probe.sh first (need $WORK/plain/rootfs)"; exit 1; }

cleanup() {
  "$RUNSC" --ignore-cgroups delete --force nstest >/dev/null 2>&1
  ip netns del "$NS" 2>/dev/null
  ip link del "$HOSTIF" 2>/dev/null
}
trap cleanup EXIT

echo "runsc: $($RUNSC --version | head -1)"

##### a namespace and veth pair, the same shape the fc tier builds
ip netns del "$NS" 2>/dev/null
ip link del "$HOSTIF" 2>/dev/null
ip netns add "$NS"
ip link add "$HOSTIF" type veth peer name "$PEERIF"
ip link set "$PEERIF" netns "$NS"
ip addr add "$HOSTADDR" dev "$HOSTIF"
ip link set "$HOSTIF" up
ip netns exec "$NS" ip addr add "$GUESTIP/30" dev "$PEERIF"
ip netns exec "$NS" ip link set "$PEERIF" up
ip netns exec "$NS" ip link set lo up

if ping -c 1 -W 2 "$GUESTIP" >/dev/null 2>&1; then
  echo "  namespace reachable from the host before the sandbox starts"
else
  echo "  FAIL: the namespace itself is not reachable -- probe inconclusive"
  exit 1
fi

##### a bundle that joins the existing namespace rather than creating one
d="$WORK/netnstest"
rm -rf "$d"; mkdir -p "$d/rootfs"
cp -a "$WORK/plain/rootfs/." "$d/rootfs/"

LISTENER=$(cat <<'PY'
import socket, sys, time
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", 8111))
s.listen(1)
print("listening on 0.0.0.0:8111", flush=True)
s.settimeout(30)
try:
    c, a = s.accept()
    c.sendall(b"agent-alive-via-netns")
    c.close()
    print("served", a, flush=True)
except Exception as e:
    print("no client:", e, flush=True)
PY
)

BUNDLE="$d" NSPATH="/var/run/netns/$NS" LISTENER="$LISTENER" python3 - <<'PY'
import json, os
base = os.path.join(os.environ["BUNDLE"], "..", "plain", "config.json")
spec = json.load(open(os.path.normpath(base)))
spec["process"]["args"] = ["python3", "-u", "-c", os.environ["LISTENER"]]
# The point of the probe: join a namespace that already exists, so the host end of
# the veth is one the node created and can dial through.
for ns in spec["linux"]["namespaces"]:
    if ns["type"] == "network":
        ns["path"] = os.environ["NSPATH"]
json.dump(spec, open(os.path.join(os.environ["BUNDLE"], "config.json"), "w"), indent=2)
PY

##### dial from the host while the sandbox is up
(
  # Waited for, not probed for. An earlier version pre-checked with /dev/tcp, which
  # consumed the listener's single accept() -- the real dial then timed out while the
  # sandbox log showed it had served the probe. The failure looked like "gVisor is
  # unreachable" and was entirely this script's doing.
  sleep 8
  reply=$(timeout 8 python3 -c "
import socket
s = socket.create_connection(('$GUESTIP', $PORT), timeout=5)
print(s.recv(64).decode())
" 2>&1 | tail -1)
  if [ "$reply" = "agent-alive-via-netns" ]; then
    echo "  PASS  host reached a listener inside the gVisor sandbox through its netns"
  else
    echo "  FAIL  could not reach it: $reply"
  fi
) &
waiter=$!

out=$(timeout 90 "$RUNSC" --ignore-cgroups run --bundle "$d" nstest 2>&1)
wait $waiter
echo "  sandbox said: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"

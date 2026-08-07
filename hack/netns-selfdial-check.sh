#!/usr/bin/env bash
# Narrows down why the container tier's agent dial fails with "network is unreachable"
# on an address that exists and is UP.
#
# The trace showed the address reaching the dialer intact, prefix and all, so
# dialInNetns is running and failing inside the namespace. gvisor-netns-probe.sh had
# already shown a host process entering a namespace and reaching a listener there -- but
# it dialled the *peer* address from the host side. The container tier dials the
# namespace's own local address from inside it, which is a different case.
#
# Two things are separated here:
#   1. can a process inside the namespace reach a listener bound in that namespace,
#      addressed by the namespace's own veth IP?
#   2. does entering the namespace from the host and dialling that same address work?
#
# Usage: netns-selfdial-check.sh
set -uo pipefail

NS=bnstest
HOSTIF=bnsh0
PEERIF=bnsp0
HOSTIP=10.9.0.1
NSIP=10.9.0.2
PORT=8111

cleanup() {
  ip netns del "$NS" 2>/dev/null
  ip link del "$HOSTIF" 2>/dev/null
}
trap cleanup EXIT
cleanup

ip netns add "$NS"
ip link add "$HOSTIF" type veth peer name "$PEERIF"
ip link set "$PEERIF" netns "$NS"
ip addr add "$HOSTIP/30" dev "$HOSTIF"
ip link set "$HOSTIF" up
ip netns exec "$NS" ip addr add "$NSIP/30" dev "$PEERIF"
ip netns exec "$NS" ip link set "$PEERIF" up
ip netns exec "$NS" ip link set lo up

cat > /tmp/nsdial-listen.py <<'PY'
import socket, time
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", 8111))
s.listen(4)
print("listening", flush=True)
deadline = time.time() + 20
while time.time() < deadline:
    try:
        s.settimeout(2)
        c, _ = s.accept()
        c.sendall(b"ok")
        c.close()
    except Exception:
        pass
PY

cat > /tmp/nsdial-try.py <<'PY'
import socket, sys
target = sys.argv[1]
host, port = target.rsplit(":", 1)
try:
    c = socket.create_connection((host, int(port)), timeout=3)
    print("  OK:", c.recv(8).decode())
except Exception as e:
    print("  FAIL:", type(e).__name__, e)
PY

# The listener runs inside the namespace, as the agent does.
ip netns exec "$NS" python3 /tmp/nsdial-listen.py >/tmp/nsdial-listen.log 2>&1 &
listener=$!
sleep 2

echo "### 1. from inside the namespace, dialling its own veth address"
ip netns exec "$NS" python3 /tmp/nsdial-try.py "$NSIP:$PORT"

echo "### 2. from inside the namespace, dialling 127.0.0.1"
ip netns exec "$NS" python3 /tmp/nsdial-try.py "127.0.0.1:$PORT"

echo "### 3. from the host, entering the namespace (what dialInNetns does)"
ip netns exec "$NS" python3 /tmp/nsdial-try.py "$NSIP:$PORT"

echo "### 4. from the host namespace, dialling the peer address (no setns)"
python3 /tmp/nsdial-try.py "$NSIP:$PORT"

echo "### routing inside the namespace"
ip netns exec "$NS" ip route 2>&1 | sed 's/^/  /'
ip netns exec "$NS" ip -br addr 2>&1 | sed 's/^/  /'

kill $listener 2>/dev/null
wait $listener 2>/dev/null

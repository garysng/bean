#!/usr/bin/env bash
# Which runsc --network mode lets the node reach an agent inside the sandbox?
#
# The agent now listens successfully inside a gVisor sandbox -- "beand listening
# addr=tcp:0.0.0.0:8111" -- and the node still cannot connect: "network is unreachable"
# on a veth address that is UP and reachable four other ways (netns-selfdial-check.sh).
#
# The suspect is runsc's -network default, `sandbox`, which uses gVisor's own userspace
# network stack: it takes over the veth and implements TCP/IP internally, so a listener
# inside is not a listener the host's stack can see on that interface. The earlier probe
# passed because it ran with --network=host, which this script did not appreciate at the
# time.
#
# So: same bundle, same listener, three modes. Whichever the node can reach is the one
# the container tier has to use, and the difference is a real trade -- host networking
# gives up one of gVisor's isolation boundaries.
#
# Usage: gvisor-network-mode-check.sh
set -uo pipefail

RUNSC=${RUNSC:-runsc}
WORK=${WORK:-/tmp/gvisor-netmode}
NS=bnmode
HOSTIF=bnm0
PEERIF=bnm1
HOSTIP=10.8.0.1
NSIP=10.8.0.2
PORT=8111

cleanup() {
  for id in mode-sandbox mode-host; do
    "$RUNSC" --ignore-cgroups delete --force "$id" >/dev/null 2>&1
  done
  ip netns del "$NS" 2>/dev/null
  ip link del "$HOSTIF" 2>/dev/null
}
trap cleanup EXIT
cleanup

[ -d "$WORK/rootfs" ] || {
  mkdir -p "$WORK/rootfs"
  cid=$(docker create docker.m.daocloud.io/library/python:3.11-slim true 2>/dev/null) || {
    echo "docker create failed"; exit 1; }
  docker export "$cid" | tar -C "$WORK/rootfs" -xf - 2>/dev/null
  docker rm "$cid" >/dev/null 2>&1
}

ip netns add "$NS"
ip link add "$HOSTIF" type veth peer name "$PEERIF"
ip link set "$PEERIF" netns "$NS"
ip addr add "$HOSTIP/30" dev "$HOSTIF"
ip link set "$HOSTIF" up
ip netns exec "$NS" ip addr add "$NSIP/30" dev "$PEERIF"
ip netns exec "$NS" ip link set "$PEERIF" up
ip netns exec "$NS" ip link set lo up

LISTENER='
import socket, time
s = socket.socket()
s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
s.bind(("0.0.0.0", 8111))
s.listen(4)
print("listening", flush=True)
end = time.time() + 25
while time.time() < end:
    try:
        s.settimeout(2)
        c, _ = s.accept()
        c.sendall(b"reached")
        c.close()
    except Exception:
        pass
'

BUNDLE="$WORK" NSPATH="/var/run/netns/$NS" LISTENER="$LISTENER" python3 - <<'PY'
import json, os
caps = ["CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_NET_BIND_SERVICE", "CAP_SETUID", "CAP_SETGID"]
spec = {
    "ociVersion": "1.0.2",
    "process": {
        "terminal": False, "user": {"uid": 0, "gid": 0},
        "args": ["python3", "-u", "-c", os.environ["LISTENER"]],
        "env": ["PATH=/usr/local/bin:/usr/bin:/bin"], "cwd": "/",
        "capabilities": {k: caps for k in ("bounding", "effective", "permitted")},
    },
    "root": {"path": "rootfs", "readonly": False},
    "hostname": "probe",
    "mounts": [
        {"destination": "/proc", "type": "proc", "source": "proc"},
        {"destination": "/dev", "type": "tmpfs", "source": "tmpfs",
         "options": ["nosuid", "strictatime", "mode=755", "size=65536k"]},
        {"destination": "/sys", "type": "sysfs", "source": "sysfs",
         "options": ["nosuid", "noexec", "nodev", "ro"]},
    ],
    "linux": {"namespaces": [
        {"type": "pid"}, {"type": "ipc"}, {"type": "uts"}, {"type": "mount"},
        {"type": "network", "path": os.environ["NSPATH"]},
    ]},
}
json.dump(spec, open(os.path.join(os.environ["BUNDLE"], "config.json"), "w"), indent=2)
PY

cat > /tmp/nm-try.py <<'PY'
import socket, sys
host, port = sys.argv[1].rsplit(":", 1)
try:
    c = socket.create_connection((host, int(port)), timeout=4)
    print("  OK:", c.recv(16).decode())
except Exception as e:
    print("  FAIL:", type(e).__name__, e)
PY

try_mode() {  # try_mode <mode>
  local mode=$1
  local id="mode-$mode"
  echo "### --network=$mode"
  "$RUNSC" --ignore-cgroups delete --force "$id" >/dev/null 2>&1
  timeout 40 "$RUNSC" --network="$mode" --ignore-cgroups run --bundle "$WORK" "$id" \
    >"$WORK/$mode.log" 2>&1 &
  local runner=$!
  sleep 7
  # Entering the namespace, which is what dialInNetns does.
  ip netns exec "$NS" python3 /tmp/nm-try.py "$NSIP:$PORT"
  echo "  listener said: $(head -1 "$WORK/$mode.log" 2>/dev/null)"
  echo "  visible in the namespace: $(ip netns exec "$NS" ss -ltn 2>/dev/null | grep -c ":$PORT") listener(s)"
  "$RUNSC" --ignore-cgroups delete --force "$id" >/dev/null 2>&1
  kill $runner 2>/dev/null
  wait $runner 2>/dev/null
}

try_mode sandbox
try_mode host

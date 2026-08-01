#!/usr/bin/env bash
# Runs the whole stack on the microVM tier: gateway plus one noded, with
# snapshot blobs in object storage. Intended for driving the CLI against a real
# fc node during development.
#
# Must run on a KVM host with the node assets built (see build-assets.sh).
set -euo pipefail

RUN=${RUN:-/tmp/beanrun}
ASSETS=${ASSETS:-/var/lib/bean/assets}
KERNEL=${KERNEL:-$ASSETS/vmlinux-6.1.175}
BIN=${BIN:-/tmp}

# Ports are overridable and default off the common ranges, since a development
# host usually has something else on 8080.
API_PORT=${API_PORT:-18080}
NODE_GRPC_PORT=${NODE_GRPC_PORT:-17440}
NODED_PORT=${NODED_PORT:-17443}
NODE_METRICS_PORT=${NODE_METRICS_PORT:-17444}

API_KEY=${API_KEY:-devkey}
NODE_TOKEN=${NODE_TOKEN:-ntok}
BOOTSTRAP_TOKEN=${BOOTSTRAP_TOKEN:-btok}

case "${1:-start}" in
stop)
  pkill -f "$BIN/bean-api" 2>/dev/null || true
  pkill -f "$BIN/noded" 2>/dev/null || true
  echo "stopped"
  exit 0
  ;;
start) ;;
*) echo "usage: $0 [start|stop]" >&2; exit 1 ;;
esac

pkill -f "$BIN/bean-api" 2>/dev/null || true
pkill -f "$BIN/noded" 2>/dev/null || true
sleep 1

mkdir -p "$RUN"
# A fresh database each run: this is a development stack, and carrying over
# records that point at sandboxes from a previous run is only confusing.
rm -f "$RUN/bean.db"

BEAN_S3_ENDPOINT=${BEAN_S3_ENDPOINT:-http://127.0.0.1:9000} \
BEAN_S3_ACCESS_KEY=${BEAN_S3_ACCESS_KEY:-beanadmin} \
BEAN_S3_SECRET_KEY=${BEAN_S3_SECRET_KEY:-beansecret123} \
nohup "$BIN/bean-api" \
  --listen 127.0.0.1:$API_PORT \
  --node-grpc 127.0.0.1:$NODE_GRPC_PORT \
  --db "$RUN/bean.db" \
  --api-key "$API_KEY" \
  --node-token "$NODE_TOKEN" \
  --bootstrap-token "$BOOTSTRAP_TOKEN" \
  --runtime-tier fc \
  --region local \
  >"$RUN/api.log" 2>&1 &

# The node registers outbound, so the gateway has to be listening first.
for _ in $(seq 1 40); do
  curl -sf -o /dev/null "http://127.0.0.1:$API_PORT/metrics" && break
  sleep 0.25
done

nohup "$BIN/noded" \
  --listen 127.0.0.1:$NODED_PORT \
  --runtime fc \
  --control-plane 127.0.0.1:$NODE_GRPC_PORT \
  --node-token "$NODE_TOKEN" \
  --bootstrap-token "$BOOTSTRAP_TOKEN" \
  --region local \
  --firecracker-bin "$ASSETS/firecracker" \
  --kernel "$KERNEL" \
  --agent-disk "$ASSETS/agent.ext4" \
  --base-dir /var/lib/bean/sandboxes \
  --image-dir /var/lib/bean/images \
  --cpu 8 --memory-mib 16384 \
  --labels tier=fc \
  --metrics "127.0.0.1:$NODE_METRICS_PORT" \
  --buildkit-addr "${BUILDKIT_ADDR:-unix:///run/bean/buildkitd.sock}" \
  >"$RUN/noded.log" 2>&1 &

# Registration is what makes the node placeable, so waiting for it here means a
# following CLI call does not race the handshake.
for _ in $(seq 1 40); do
  if curl -sf -H "Authorization: Bearer $API_KEY" \
      http://127.0.0.1:$API_PORT/v1/nodes | grep -q READY; then
    echo "stack up: gateway 127.0.0.1:$API_PORT, node registered"
    exit 0
  fi
  sleep 0.25
done

echo "node did not register; logs:" >&2
tail -20 "$RUN/api.log" "$RUN/noded.log" >&2
exit 1

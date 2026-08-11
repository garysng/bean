#!/usr/bin/env bash
# Proves a guest boots from a layer this node does not have on disk, read over range
# requests, served through a ublk device.
#
# The point is the *absence* of the local file. A create with lazy pull on will happily read
# a layer that is already here, and then nothing about the remote path has been exercised --
# which is exactly how a broken range reader passes an end-to-end test. So the layer is
# published, removed locally, and only then read.
#
# One node for the whole run, and lazy pull on from the start. Two things forced that:
#
#   - dev-fc-stack.sh wipes the control-plane database on every start, by design. Restarting
#     between phases therefore erases the template, and the second create becomes a *first*
#     create -- which is the one the control plane asks to publish.
#   - publishing and reading remotely are mutually exclusive within one create
#     (`resolve` sets `remote: !publish`), because a conversion needs its parents as local
#     files. So the publish has to happen in an earlier create than the remote read.
#
# An earlier version of this script restarted the node between the two phases and reported a
# conversion where a remote read was expected. It was right to report it: the run had not
# demonstrated what it claimed.
set -uo pipefail

BIN=${BIN:-/tmp/beantest/bin}
STACK=${STACK:-$(dirname "$0")/dev-fc-stack.sh}
IMAGE=${IMAGE:-beanreg.local:5443/alpine:3.20}
LAYER_DIR=${LAYER_DIR:-/var/lib/bean/images/layers}
STASH=${STASH:-/tmp/obd-lazy-stash}
S3=${S3_ENDPOINT:-http://127.0.0.1:9000}

export BEAN_BASE_URL=${BEAN_BASE_URL:-http://127.0.0.1:18080}
export BEAN_API_KEY=${BEAN_API_KEY:-devkey}
export BEAN_S3_ACCESS_KEY=${BEAN_S3_ACCESS_KEY:-beanadmin}
export BEAN_S3_SECRET_KEY=${BEAN_S3_SECRET_KEY:-beansecret123}
export NODE_CPU=${NODE_CPU:-32}
export NODE_MEM_MIB=${NODE_MEM_MIB:-32768}
export NODE_DISK_MIB=${NODE_DISK_MIB:-131072}
export KERNEL=${KERNEL:-/var/lib/bean/assets/vmlinux-6.1.175}

say() { printf '%s\n' "$*"; }

restore_layers() {
	if [ -d "$STASH" ]; then
		mkdir -p "$LAYER_DIR"
		mv "$STASH"/* "$LAYER_DIR"/ 2>/dev/null
		rmdir "$STASH" 2>/dev/null
		say "restored stashed layers to $LAYER_DIR"
	fi
}
cleanup() {
	restore_layers
	BIN=$BIN bash "$STACK" stop >/dev/null 2>&1
}
trap cleanup EXIT

kill_all() {
	for s in $("$BIN/bean" ls 2>/dev/null | awk '/^sbx_/ {print $1}'); do
		"$BIN/bean" kill "$s" >/dev/null 2>&1
	done
	sleep 2
}

say "== 0. the blob store must answer anonymously =="
# overlaybd reads the store without credentials, so the published prefix has to allow
# anonymous GET. Without it noded logs "layers will be converted locally instead of read on
# demand" and every later step silently measures a conversion.
code=$(curl -s -o /dev/null -w '%{http_code}' "$S3/bean-obd-layers/probe" || true)
say "anonymous GET on the bucket: HTTP $code"
if [ "$code" = "401" ] || [ "$code" = "403" ]; then
	say "the store rejects anonymous reads, so lazy pull would fall back to converting."
	say "For MinIO: mc anonymous set download <alias>/bean-obd-layers"
	exit 1
fi

say ""
say "== 1. start one node, lazy pull on, and keep it for the whole run =="
BIN=$BIN bash "$STACK" stop >/dev/null 2>&1
sleep 1
NODED_FLAGS="--fc-overlaybd --fc-ublk --fc-overlaybd-lazy-pull --s3-endpoint $S3 --s3-bucket bean-obd-layers" \
	BIN=$BIN bash "$STACK" start >/tmp/lazy-stack.log 2>&1 || {
	say "stack failed to start:"
	tail -20 /tmp/lazy-stack.log
	exit 1
}
sleep 3
grep -oE 'rootfs via overlaybd.{0,120}' /tmp/beanrun/noded.log | tail -1

say ""
say "== 2. first create: converts and publishes =="
if ! "$BIN/bean" run --image-ref "$IMAGE" --disk-mib 2048 >/tmp/lazy-warm.log 2>&1; then
	say "the first create failed, so nothing was published:"
	tail -5 /tmp/lazy-warm.log
	exit 1
fi
kill_all
say "layers on disk: $(ls -1 "$LAYER_DIR"/*.obd 2>/dev/null | wc -l)"
say "in the store:"
curl -s "$S3/bean-obd-layers/?list-type=2&prefix=blobs/" 2>/dev/null |
	grep -oE '<Key>[^<]+</Key>' | head -4 || say "(could not list)"

say ""
say "== 3. remove the local copy, keeping the node and its template =="
mkdir -p "$STASH"
moved=0
for f in "$LAYER_DIR"/*.obd; do
	[ -e "$f" ] || continue
	mv "$f" "$STASH"/
	moved=$((moved + 1))
done
say "stashed $moved layer file(s); $LAYER_DIR now holds $(ls -1 "$LAYER_DIR" 2>/dev/null | wc -l) entries"
if [ "$moved" -eq 0 ]; then
	say "nothing to stash: the layer was never converted locally, so this run cannot tell a"
	say "remote read from a local one. Aborting rather than reporting a pass."
	exit 1
fi

say ""
say "== 4. second create: the layer is only in the store =="
SBX=$("$BIN/bean" run --image-ref "$IMAGE" --disk-mib 2048 2>&1 | awk '/^sbx_/ {print $1}')
if [ -z "$SBX" ]; then
	say "create FAILED with the layer absent:"
	"$BIN/bean" run --image-ref "$IMAGE" --disk-mib 2048 2>&1 | tail -4
	say "--- noded:"
	grep -oE 'level=ERROR.{0,200}' /tmp/beanrun/noded.log | tail -3
	exit 1
fi
say "sandbox: $SBX"

say ""
say "== 5. read inside the guest =="
"$BIN/bean" exec "$SBX" -- sh -c 'cat /etc/alpine-release; uname -m; ls /bin/busybox' 2>&1 | head -4

say ""
say "== 6. did the layer come back to disk? =="
back=$(ls -1 "$LAYER_DIR"/*.obd 2>/dev/null | wc -l)
if [ "$back" -gt 0 ]; then
	say "FAIL: $back layer file(s) reappeared -- the create converted rather than reading"
	say "remotely, so this run does NOT demonstrate lazy pull."
else
	say "PASS: none. The guest read its filesystem without the layer ever being on this disk."
fi

say ""
say "== 7. the device that served it =="
ls -l /dev/ublkb* 2>/dev/null | head -3

kill_all
say ""
say "ublk devices left: $(ls -1 /dev/ublkb* 2>/dev/null | wc -l)"

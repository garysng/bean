#!/usr/bin/env bash
# Checks restore-from-snapshot on the ublk route, which resolves layers through a different
# entry point than a create does.
#
# A create calls lowersFor; a restore calls snapshotFSLowers with the snapshot's manifest
# digest. Both sit above the same layerSources, so a change there passes under both -- and only
# the create path has been exercised. This is the same shape of gap as the one the four-path
# regression closed: one change, two routes, one of them tested.
#
# The check that matters is not that a restore produces a sandbox, but that the restored guest
# reads back a file written *before* the snapshot. A restore that silently loses the filesystem
# still boots, because the base image is intact underneath.
set -uo pipefail

BIN=${BIN:-/tmp/beantest/bin}
STACK=${STACK:-/tmp/beantest/dev-fc-stack.sh}
IMAGE=${IMAGE:-beanreg.local:5443/alpine:3.20}
S3=${S3_ENDPOINT:-http://127.0.0.1:9000}
MARKER="bean-restore-probe-$$"

export BEAN_BASE_URL=${BEAN_BASE_URL:-http://127.0.0.1:18080}
export BEAN_API_KEY=${BEAN_API_KEY:-devkey}
export BEAN_S3_ACCESS_KEY=${BEAN_S3_ACCESS_KEY:-beanadmin}
export BEAN_S3_SECRET_KEY=${BEAN_S3_SECRET_KEY:-beansecret123}
export NODE_CPU=${NODE_CPU:-32}
export NODE_MEM_MIB=${NODE_MEM_MIB:-32768}
export NODE_DISK_MIB=${NODE_DISK_MIB:-131072}
export KERNEL=${KERNEL:-/var/lib/bean/assets/vmlinux-6.1.175}

say() { printf '%s\n' "$*"; }
fail=0

cleanup() { BIN=$BIN bash "$STACK" stop >/dev/null 2>&1; }
trap cleanup EXIT

kill_all() {
	for s in $("$BIN/bean" ls 2>/dev/null | awk '/^sbx_/ {print $1}'); do
		"$BIN/bean" kill "$s" >/dev/null 2>&1
	done
	sleep 2
}

say "== start the node: overlaybd over ublk =="
BIN=$BIN bash "$STACK" stop >/dev/null 2>&1
sleep 1
NODED_FLAGS="--fc-overlaybd --fc-ublk --s3-endpoint $S3 --s3-bucket bean-obd-layers" \
	BIN=$BIN bash "$STACK" start >/tmp/restore-stack.log 2>&1 || {
	say "stack failed:"; tail -8 /tmp/restore-stack.log; exit 1
}
sleep 3
grep -oE 'rootfs via overlaybd.{0,90}' /tmp/beanrun/noded.log | tail -1

say ""
say "== 1. create a sandbox and write a marker =="
SBX=$("$BIN/bean" run --image-ref "$IMAGE" --disk-mib 2048 2>&1 | awk '/^sbx_/ {print $1}')
[ -n "$SBX" ] || { say "create failed"; exit 1; }
say "sandbox: $SBX"
"$BIN/bean" exec "$SBX" -- sh -c "echo $MARKER > /root/marker.txt; sync; cat /root/marker.txt" 2>&1 | head -2

say ""
say "== 2. take a filesystem-only snapshot =="
# --no-memory is the filesystem-only form, which is the one that resolves through
# snapshotFSLowers on restore. A memory snapshot would exercise the UFFD path instead.
SNAP=$("$BIN/bean" snapshot create "$SBX" --no-memory 2>&1 | awk '/^snap/ {print $1}')
if [ -z "$SNAP" ]; then
	say "snapshot failed:"
	"$BIN/bean" snapshot create "$SBX" --no-memory 2>&1 | tail -4 | sed 's/^/  /'
	exit 1
fi
say "snapshot: $SNAP"
kill_all

say ""
say "== 3. create a NEW sandbox from that snapshot =="
# This is the restore path: the filesystem comes from the snapshot's sealed chain, resolved by
# digest, and a fresh writable goes on top.
SBX2=$("$BIN/bean" run --snapshot "$SNAP" --disk-mib 2048 2>&1 | awk '/^sbx_/ {print $1}')
if [ -z "$SBX2" ]; then
	say "FAIL: restore produced no sandbox"
	"$BIN/bean" run --snapshot "$SNAP" --disk-mib 2048 2>&1 | tail -4 | sed 's/^/  /'
	grep -oE 'level=ERROR.{0,170}' /tmp/beanrun/noded.log | tail -2 | sed 's/^/  /'
	exit 1
fi
say "restored sandbox: $SBX2"

say ""
say "== 4. does the restored guest still have the marker? =="
GOT=$("$BIN/bean" exec "$SBX2" -- sh -c 'cat /root/marker.txt' 2>&1 | head -1)
if [ "$GOT" = "$MARKER" ]; then
	say "PASS: restored guest read back '$GOT'"
else
	say "FAIL: restored guest said '${GOT:-<nothing>}', want '$MARKER'"
	say "  a restore that loses the filesystem still boots, because the base image is intact"
	say "  block-io errors in the guest: $(grep -acE 'EXT4-fs error|I/O error' "/var/lib/bean/sandboxes/$SBX2/console.log" 2>/dev/null)"
	fail=1
fi

say ""
say "== 5. the restored sandbox is writable in its own right =="
W=$("$BIN/bean" exec "$SBX2" -- sh -c 'echo second > /root/second.txt && cat /root/second.txt' 2>&1 | head -1)
if [ "$W" = "second" ]; then
	say "PASS: writes to the restored sandbox work"
else
	say "FAIL: write to the restored sandbox gave '${W:-<nothing>}'"
	fail=1
fi

kill_all
say ""
say "ublk devices left: $(ls -1 /dev/ublkb* 2>/dev/null | wc -l)"
[ "$fail" -eq 0 ] && say "RESULT: restore works on the ublk route" || say "RESULT: restore is broken on the ublk route"
exit "$fail"

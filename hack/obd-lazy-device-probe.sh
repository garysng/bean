#!/usr/bin/env bash
# Reads a ublk device backed by a lazily-pulled layer directly from the host, to separate
# "the device serves the right bytes" from "the guest booted".
#
# A create can report RUNNING while the guest never mounts its root: the sandbox exists, the
# device exists, and the agent is simply unreachable. That reads the same from the outside as
# a broken filesystem, so this looks at the device itself -- ext4 magic, then a mount and a
# file read on the host -- before blaming either side.
set -uo pipefail

BIN=${BIN:-/tmp/beantest/bin}
IMAGE=${IMAGE:-beanreg.local:5443/alpine:3.20}
LAYER_DIR=${LAYER_DIR:-/var/lib/bean/images/layers}
STASH=${STASH:-/tmp/obd-probe-stash}
MNT=${MNT:-/tmp/obd-probe-mnt}

export BEAN_BASE_URL=${BEAN_BASE_URL:-http://127.0.0.1:18080}
export BEAN_API_KEY=${BEAN_API_KEY:-devkey}

say() { printf '%s\n' "$*"; }

cleanup() {
	umount "$MNT" 2>/dev/null
	rmdir "$MNT" 2>/dev/null
	if [ -d "$STASH" ]; then
		mv "$STASH"/* "$LAYER_DIR"/ 2>/dev/null
		rmdir "$STASH" 2>/dev/null
		say "restored stashed layers"
	fi
}
trap cleanup EXIT

kill_all() {
	for s in $("$BIN/bean" ls 2>/dev/null | awk '/^sbx_/ {print $1}'); do
		"$BIN/bean" kill "$s" >/dev/null 2>&1
	done
	sleep 2
}

say "== warm the image so it is published, then remove the local copy =="
"$BIN/bean" run --image-ref "$IMAGE" --disk-mib 2048 >/dev/null 2>&1
kill_all
mkdir -p "$STASH"
moved=0
for f in "$LAYER_DIR"/*.obd; do
	[ -e "$f" ] || continue
	mv "$f" "$STASH"/ && moved=$((moved + 1))
done
say "stashed $moved layer(s); local layers now: $(ls -1 "$LAYER_DIR"/*.obd 2>/dev/null | wc -l)"
[ "$moved" -gt 0 ] || { say "nothing published locally to remove; aborting"; exit 1; }

say ""
say "== create with the layer absent =="
SBX=$("$BIN/bean" run --image-ref "$IMAGE" --disk-mib 2048 2>&1 | awk '/^sbx_/ {print $1}')
[ -n "$SBX" ] || { say "create failed"; exit 1; }
say "sandbox: $SBX"
say "local layers after create: $(ls -1 "$LAYER_DIR"/*.obd 2>/dev/null | wc -l) (0 means it was read remotely)"

DEV=$(ls -1 /dev/ublkb* 2>/dev/null | head -1)
[ -n "$DEV" ] || { say "no ublk device appeared"; exit 1; }
say "device: $DEV"

say ""
say "== does the device carry an ext4 superblock? =="
# Magic 0xef53 sits at byte 56 of the superblock, which is at offset 1024.
magic=$(dd if="$DEV" bs=1 skip=1080 count=2 status=none | od -An -tx1 | tr -d ' \n')
say "bytes at 1080: $magic (little-endian 53ef means ext4)"

say ""
say "== what does blkid make of it? =="
blkid "$DEV" 2>&1 | head -2 || say "(blkid found nothing)"

say ""
say "== mount it read-only on the host and read a file =="
mkdir -p "$MNT"
if mount -o ro "$DEV" "$MNT" 2>/tmp/obd-mount.err; then
	say "mounted"
	say "/etc/alpine-release: $(cat "$MNT/etc/alpine-release" 2>&1 | head -1)"
	say "entries in /bin: $(ls -1 "$MNT/bin" 2>/dev/null | wc -l)"
	umount "$MNT"
else
	say "mount failed: $(head -2 /tmp/obd-mount.err)"
fi

kill_all
say ""
say "ublk devices left: $(ls -1 /dev/ublkb* 2>/dev/null | wc -l)"

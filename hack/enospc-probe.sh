#!/usr/bin/env bash
# Answers one question: what does a guest see when its sparse copy-on-write layer
# cannot allocate another block because the HOST filesystem is full?
#
# This decides how much margin a low-disk admission watermark needs. dm-thin has
# two documented behaviours (queue_if_no_space hangs the guest, error_if_no_space
# returns EIO) but bean uses a sparse file on ext4, where the failure happens in
# the write path rather than at the device layer. Nobody has measured which of
# those two shapes it takes, and the difference matters: a guest that hangs
# forever is a much worse outcome than one that gets an error, and it argues for a
# larger margin.
#
# Runs entirely on a small loopback filesystem in a temp directory. It does NOT
# fill the real host disk, and it does not touch any sandbox.
#
# Usage: enospc-probe.sh [--size-mib N]
set -uo pipefail

SIZE_MIB=${SIZE_MIB:-64}
WORK=$(mktemp -d /tmp/bean-enospc.XXXXXX)
NAME="bean-enospc-probe-$$"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --size-mib) SIZE_MIB="$2"; shift 2 ;;
    -h|--help) sed -n '2,17p' "$0" | sed 's/^# \?//'; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 64 ;;
  esac
done

say() { printf '%s\n' "$*"; }
hr() { printf -- '------------------------------------------------------------\n'; }

# Teardown is in reverse order of setup: the mapping holds the loop devices,
# which hold the files.
# Every step is `|| true`: the probe deliberately drives the device into a failed
# state, so several of these are expected to be gone or already detached by the
# time cleanup runs. A cleanup that stopped at the first non-zero status would
# leave the remaining loop devices attached to deleted files.
cleanup() {
  umount "$WORK/guest" 2>/dev/null || true
  umount "$WORK/basecheck" 2>/dev/null || true
  dmsetup remove --retry "$NAME" 2>/dev/null || true
  [[ -n "${COW_LOOP:-}" ]] && { losetup -d "$COW_LOOP" 2>/dev/null || true; }
  [[ -n "${BASE_LOOP:-}" ]] && { losetup -d "$BASE_LOOP" 2>/dev/null || true; }
  umount "$WORK/hostfs" 2>/dev/null || true
  [[ -n "${HOST_LOOP:-}" ]] && { losetup -d "$HOST_LOOP" 2>/dev/null || true; }
  rm -rf "$WORK"
}
trap cleanup EXIT

[[ $EUID -eq 0 ]] || { say "needs root for losetup/dmsetup"; exit 77; }

# A small filesystem standing in for the host's: filling this is what puts the
# copy-on-write layer under the condition being measured.
say "building a ${SIZE_MIB} MiB stand-in for the host filesystem"
mkdir -p "$WORK/hostfs"
truncate -s "${SIZE_MIB}M" "$WORK/host.img"
mkfs.ext4 -q -F "$WORK/host.img"
HOST_LOOP=$(losetup --find --show "$WORK/host.img")
mount "$HOST_LOOP" "$WORK/hostfs"

# The base is read-only and shared, exactly as a real base image is. Its size is
# what the guest believes its disk to be, and it is deliberately larger than the
# host filesystem: that gap is the whole point of thin provisioning, and the
# reason the host can run out first.
say "building a base image larger than the host filesystem can back"
truncate -s 256M "$WORK/base.img"
mkfs.ext4 -q -F "$WORK/base.img"
BASE_LOOP=$(losetup --read-only --find --show "$WORK/base.img")

# The copy-on-write layer lives on the small filesystem, sparse.
truncate -s 256M "$WORK/hostfs/cow.img"
COW_LOOP=$(losetup --find --show "$WORK/hostfs/cow.img")

SECTORS=$(blockdev --getsz "$BASE_LOOP")
dmsetup create "$NAME" --table "0 $SECTORS snapshot $BASE_LOOP $COW_LOOP P 8"
DEV="/dev/mapper/$NAME"
say "assembled $DEV (guest sees 256M, host can back ${SIZE_MIB}M)"
hr

mkdir -p "$WORK/guest"
mount "$DEV" "$WORK/guest" 2>/dev/null || { say "cannot mount the snapshot device"; exit 70; }

df -h "$WORK/hostfs" | tail -1 | sed 's/^/host fs: /'
df -h "$WORK/guest" | tail -1 | sed 's/^/guest fs: /'
hr

# Write until something gives. dd's own report is not trusted: the interesting
# outcome is what the kernel does, so the exit status, stderr and dmesg are all
# captured, and a hang is distinguished from an error by a timeout.
say "writing to the guest filesystem until the host layer cannot allocate"
set +e
timeout 120 dd if=/dev/zero of="$WORK/guest/fill.bin" bs=1M count=512 \
  oflag=direct 2>"$WORK/dd.err"
DD_RC=$?
set -e

hr
case "$DD_RC" in
  124) say "RESULT: the write HUNG (killed at 120s)" ;;
  0)   say "RESULT: the write SUCCEEDED, which means the host never ran out" ;;
  *)   say "RESULT: the write FAILED with exit $DD_RC" ;;
esac
say "dd stderr:"
sed 's/^/  /' "$WORK/dd.err"

hr
say "kernel messages from the probe window:"
dmesg | tail -25 | grep -iE "snapshot|dm-|ext4|I/O error|EIO|no space" |
  sed 's/^/  /' || say "  (nothing matched)"

hr
say "device state after the write:"
dmsetup status "$NAME" | sed 's/^/  /'
say "  (a snapshot target reporting 'Invalid' has run out of copy-on-write space)"

# Whether the filesystem is still usable decides whether a sandbox can recover or
# has to be destroyed.
say ""
say "can the guest filesystem still be read?"
if ls "$WORK/guest" >/dev/null 2>&1; then
  say "  readable"
else
  say "  NOT readable"
fi
say "can it still be written?"
if echo probe > "$WORK/guest/probe.txt" 2>"$WORK/write.err"; then
  say "  the write call SUCCEEDED"
else
  say "  not writable: $(tr -d '\n' < "$WORK/write.err")"
fi

# A successful write() means nothing on its own: it may have landed only in the
# page cache. An invalidated snapshot target fails every write it is given, so the
# question is whether the data is still there once the cache cannot answer.
say ""
say "does that write survive a round-trip through the device?"
sync 2>/dev/null
umount "$WORK/guest" 2>/dev/null
if mount "$DEV" "$WORK/guest" 2>"$WORK/remount.err"; then
  if [[ -f "$WORK/guest/probe.txt" ]]; then
    got=$(cat "$WORK/guest/probe.txt" 2>/dev/null | tr -d '\n')
    if [[ "$got" == "probe" ]]; then
      say "  yes: read back '$got'"
    else
      say "  NO: file present but reads back '${got:-<empty>}' — SILENT DATA LOSS"
    fi
  else
    say "  NO: the file is gone after a remount — the write was silently discarded"
  fi
else
  say "  the device cannot be remounted: $(tr -d '\n' < "$WORK/remount.err")"
  say "  a sandbox in this state is unrecoverable and has to be destroyed"
fi
umount "$WORK/guest" 2>/dev/null || true

hr
# The base is shared by every sandbox using that image on the node, so whether it
# survives decides the blast radius: one sandbox lost, or every sandbox on that
# image.
say "is the SHARED BASE image still intact? (this decides the blast radius)"
# The mapping has to go first: it holds the base loop device, so mounting the base
# while the device is still assembled fails with EBUSY — which reads exactly like
# corruption if the two are not separated.
dmsetup remove --retry "$NAME" 2>/dev/null || true
mkdir -p "$WORK/basecheck"
if mount -o ro "$BASE_LOOP" "$WORK/basecheck" 2>"$WORK/base.err"; then
  say "  yes: the read-only base mounts cleanly, so only this sandbox is lost"
  umount "$WORK/basecheck" 2>/dev/null
else
  say "  NO: $(tr -d '\n' < "$WORK/base.err")"
  say "  every sandbox sharing this image would be affected"
fi

#!/usr/bin/env bash
# Verifies the overlaybd + TCMU path on real hardware, and specifically that the
# two traps recorded in decisions.md 3.1 are handled.
#
# Both traps produce no error: a LUN linked before the nexus leaves a configfs tree
# that looks healthy with no device, and a missing unit serial has multipathd merge
# devices so one image serves another's data. So this script asserts on observable
# outcomes -- a device node appearing, a WWID being distinct -- rather than on exit
# codes.
#
# Usage: overlaybd-probe.sh            # full run
#        overlaybd-probe.sh cleanup    # tear down leftovers from a failed run
set -uo pipefail

OBD=/opt/overlaybd/bin
CORE=/sys/kernel/config/target/core
LOOP=/sys/kernel/config/target/loopback
HBA=user_999
WORK=${WORK:-/var/lib/bean/obdprobe}

pass() { echo "PASS  $*"; }
fail() { echo "FAIL  $*"; FAILED=1; }
info() { echo "  ... $*"; }
FAILED=0

# teardown unwinds in reverse order. Removing a backstore while its LUN still
# references it leaves a device nothing can remove without a reboot, so the LUN
# goes first and every step tolerates already-gone.
teardown_one() {
  local name=$1 wwn=$2
  rm -f "$LOOP/$wwn/tpgt_1/lun/lun_0/virtual_scsi_port" 2>/dev/null
  rmdir "$LOOP/$wwn/tpgt_1/lun/lun_0" 2>/dev/null
  rmdir "$LOOP/$wwn/tpgt_1" 2>/dev/null
  rmdir "$LOOP/$wwn" 2>/dev/null
  echo 0 > "$CORE/$HBA/$name/enable" 2>/dev/null
  rmdir "$CORE/$HBA/$name" 2>/dev/null
}

cleanup_all() {
  for d in "$CORE/$HBA"/probe*; do
    [ -d "$d" ] || continue
    local n; n=$(basename "$d")
    teardown_one "$n" "naa.5001405probe${n##probe}"
  done
  for d in "$LOOP"/naa.5001405probe*; do
    [ -d "$d" ] || continue
    rm -f "$d"/tpgt_1/lun/lun_0/virtual_scsi_port 2>/dev/null
    rmdir "$d"/tpgt_1/lun/lun_0 "$d"/tpgt_1 "$d" 2>/dev/null
  done
  rmdir "$CORE/$HBA" 2>/dev/null
  rm -rf "$WORK"
  echo "cleaned up"
}

[ "${1:-}" = cleanup ] && { cleanup_all; exit 0; }

echo "=== environment ==="
uname -r
for b in overlaybd-create overlaybd-apply overlaybd-commit; do
  [ -x "$OBD/$b" ] || { fail "$b missing"; exit 1; }
done
pass "overlaybd binaries present"
pgrep -f overlaybd-tcmu >/dev/null && pass "overlaybd-tcmu running" \
  || info "overlaybd-tcmu not running (it is started on demand by some setups)"
systemctl is-active --quiet multipathd \
  && info "multipathd is ACTIVE -- the serial trap is live on this host" \
  || info "multipathd inactive"

mkdir -p "$WORK" "$CORE/$HBA"

# A minimal layer to attach. The content does not matter; that a device appears and
# carries a distinct identity does.
echo "=== build a layer ==="
tar -cf "$WORK/layer.tar" -C /etc hostname 2>/dev/null
"$OBD/overlaybd-create" --mkfs "$WORK/base.data" "$WORK/base.index" 2 >/dev/null 2>&1 \
  || { fail "overlaybd-create"; exit 1; }
# Lowers is empty on purpose: a layer being built has nothing underneath it, and
# naming it as its own lower fails with only "failed to create image file".
cat > "$WORK/apply.json" <<EOF
{"lowers":[],"upper":{"data":"$WORK/base.data","index":"$WORK/base.index"}}
EOF
if "$OBD/overlaybd-apply" "$WORK/layer.tar" "$WORK/apply.json" 2>&1 | grep -q "done"; then
  pass "layer built and tar applied"
else
  fail "overlaybd-apply"; exit 1
fi

# Sealing is mandatory, not an optimisation. A lower layer must be a sealed zfile:
# handed a freshly applied (unsealed) layer, the daemon fails with
# "trailer magic ... or sealedness doesn't match" and leaves the backstore
# DEACTIVATED, which surfaces at the configfs write only as ENOENT on enable.
if "$OBD/overlaybd-commit" -z -t "$WORK/base.data" "$WORK/base.index" "$WORK/base.obd" \
     >/dev/null 2>&1 && [ -s "$WORK/base.obd" ]; then
  pass "layer sealed ($(stat -c%s "$WORK/base.obd") bytes)"
else
  fail "overlaybd-commit"; exit 1
fi

# The device config is a separate file: it names the sealed layer as a lower with a
# writable layer on top, which is what a sandbox actually gets.
"$OBD/overlaybd-create" -s "$WORK/w.data" "$WORK/w.index" 2 >/dev/null 2>&1
cat > "$WORK/device.json" <<EOF
{"lowers":[{"file":"$WORK/base.obd"}],"upper":{"data":"$WORK/w.data","index":"$WORK/w.index"},"resultFile":"$WORK/result"}
EOF
pass "device config written"

attach() {  # attach <name> <serial|-> <order: nexus-first|lun-first>
  local name=$1 serial=$2 order=$3
  local wwn="naa.5001405probe${name##probe}"
  local base="$CORE/$HBA/$name"

  # A writable layer per device: two devices sharing one would have each other's
  # writes land in the same index, which is corruption rather than sharing.
  "$OBD/overlaybd-create" -s "$WORK/$name.data" "$WORK/$name.index" 2 >/dev/null 2>&1
  cat > "$WORK/$name.json" <<EOF
{"lowers":[{"file":"$WORK/base.obd"}],"upper":{"data":"$WORK/$name.data","index":"$WORK/$name.index"},"resultFile":"$WORK/$name.result"}
EOF

  mkdir -p "$base" || return 1
  echo "dev_config=overlaybd/$WORK/$name.json,dev_size=$((2*1024*1024*1024))" \
    > "$base/control" 2>/dev/null
  [ "$serial" != - ] && echo "$serial" > "$base/wwn/vpd_unit_serial" 2>/dev/null
  echo 1 > "$base/enable" 2>/dev/null
  mkdir -p "$LOOP/$wwn/tpgt_1" || return 1
  if [ "$order" = nexus-first ]; then
    echo "$wwn" > "$LOOP/$wwn/tpgt_1/nexus" 2>/dev/null
    mkdir -p "$LOOP/$wwn/tpgt_1/lun/lun_0"
    ln -s "$base" "$LOOP/$wwn/tpgt_1/lun/lun_0/virtual_scsi_port" 2>/dev/null
  else
    mkdir -p "$LOOP/$wwn/tpgt_1/lun/lun_0"
    ln -s "$base" "$LOOP/$wwn/tpgt_1/lun/lun_0/virtual_scsi_port" 2>/dev/null
    echo "$wwn" > "$LOOP/$wwn/tpgt_1/nexus" 2>/dev/null
  fi
  sleep 1
}

# device_for <serial> echoes the block device name, or nothing.
#
# The kernel builds a TCMU WWID as naa.6001405 + the HEX DIGITS of the unit serial,
# so a serial this script chose is a key it can search for. Measured: serial
# "beaaaa" -> naa.6001405beaaaa000...
#
# Three other routes were tried on this host and all fail: udev_path stays empty,
# the SCSI model reads "TCMUdevice" for every such device, and scsi_port/dev is a
# global port counter rather than the tcm_loop adapter number (dev=26 was served by
# tcm_loop_adapter_24).
device_for() {
  local serial=$1
  for d in /sys/class/scsi_device/*; do
    local wwid; wwid=$(cat "$d/device/wwid" 2>/dev/null | tr 'A-Z' 'a-z')
    case "$wwid" in
      "naa.6001405$serial"*) ls "$d/device/block" 2>/dev/null | head -1; return 0;;
    esac
  done
  return 1
}

# Trap 1. The failure being tested for is silent: configfs reports enable=1 and
# ACTIVATED either way, and only the presence of a device node distinguishes them.
echo "=== trap 1: LUN linked before the nexus ==="
attach probe1 beaf01 lun-first
if [ -n "$(device_for beaf01)" ]; then
  info "a device appeared even with the wrong order -- this host may rescan;"
  info "the ordering in obdtcmu_linux.go is still required per decisions.md 3.1"
else
  pass "no device with LUN-before-nexus, and configfs reported no error:"
  info "enable=$(cat "$CORE/$HBA/probe1/enable" 2>/dev/null) \
info=$(cat "$CORE/$HBA/probe1/info" 2>/dev/null | head -c 60)"
fi
teardown_one probe1 naa.5001405probe1

echo "=== correct order: nexus before LUN ==="
attach probe2 beaf02 nexus-first
DEV2=$(device_for beaf02)
if [ -n "$DEV2" ]; then
  pass "device appeared with nexus-before-LUN: /dev/$DEV2"
else
  fail "no device even with the correct order (is overlaybd-tcmu running?)"
  info "info=$(cat "$CORE/$HBA/probe2/info" 2>/dev/null | head -c 200)"
fi

# A device existing is not the same as it serving the right bytes. The layer was
# built from a tar of /etc/hostname, so mounting it and finding that file is what
# proves the chain assembled correctly rather than merely appearing.
if [ -n "$DEV2" ]; then
  echo "=== the device serves the layer's contents ==="
  MNT=$(mktemp -d)
  if mount -o ro "/dev/$DEV2" "$MNT" 2>/dev/null; then
    if [ -f "$MNT/hostname" ]; then
      pass "mounted and found the file the layer was built from"
    else
      fail "mounted but the layer's contents are missing: $(ls "$MNT" | head -5)"
    fi
    umount "$MNT" 2>/dev/null
  else
    fail "could not mount /dev/$DEV2 (a merged multipath device reports 'busy')"
  fi
  rmdir "$MNT" 2>/dev/null
fi

# Trap 2. Two devices with no serial both report WWID 36001405 followed by zeros,
# and multipathd merges them. The merged device then serves whichever path it chose
# -- reading another image's data, with no error anywhere.
echo "=== trap 2: unit serial ==="
# Asserted on the WWID rather than on the serial string, because the WWID is what
# multipathd compares. A serial that is set but reduces to the same hex digits as
# another still merges, so checking the serial alone would pass while the bug is live.
wwid_of() {
  for d in /sys/class/scsi_device/*; do
    [ -n "$(ls "$d/device/block" 2>/dev/null)" ] || continue
    if [ "$(ls "$d/device/block" | head -1)" = "$1" ]; then
      cat "$d/device/wwid" 2>/dev/null | tr 'A-Z' 'a-z'
      return
    fi
  done
}

# probe3 gets no serial at all, which is the unfixed case.
attach probe3 - nexus-first
sleep 1
WWID2=$(wwid_of "$DEV2")
info "with a serial:    $WWID2"
ALLZERO=$(cat /sys/class/scsi_device/*/device/wwid 2>/dev/null \
  | tr 'A-Z' 'a-z' | grep -m1 '^naa.6001405000000')
info "without a serial: ${ALLZERO:-<no such device>}"

if [ -n "$WWID2" ] && [ "$WWID2" != "$ALLZERO" ]; then
  pass "the serialled device has a distinct WWID, so multipathd cannot merge it"
else
  fail "WWIDs are not distinct -- multipathd would merge and serve wrong data"
fi

if command -v multipath >/dev/null; then
  if multipath -ll 2>/dev/null | grep -q "3600140500000000"; then
    info "multipath DID merge the serial-less device (this is the trap, reproduced):"
    multipath -ll 2>/dev/null | head -4
  else
    info "no all-zero multipath device present"
  fi
fi
teardown_one probe3 naa.5001405probe3
teardown_one probe2 naa.5001405probe2

echo "=== teardown left nothing behind ==="
left=$(ls "$CORE/$HBA" 2>/dev/null | grep -c probe || true)
[ "${left:-0}" = 0 ] && pass "no configfs objects leaked" || fail "$left backstore(s) leaked"

rmdir "$CORE/$HBA" 2>/dev/null
rm -rf "$WORK"
echo
[ "$FAILED" = 0 ] && echo "ALL CHECKS PASSED" || echo "SOME CHECKS FAILED"
exit "$FAILED"

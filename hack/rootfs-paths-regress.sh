#!/usr/bin/env bash
# Checks that every rootfs path still works, not just the one last changed.
#
# The worker split and the copy-on-write mutex went in to make lazy pull work, but they sit on
# the path every ublk create takes. A change that fixes one route and breaks another is the
# specific risk, and the only way to see it is to exercise the routes that were already
# passing.
#
# Four configurations, each a create plus a read inside the guest:
#   1. dm-snapshot          the default, no flags -- must be untouched by any of this
#   2. ublk alone           flattened ext4 over ublk, no overlaybd
#   3. overlaybd + tcmu     layers, the transport this work did not change
#   4. overlaybd + ublk     layers over ublk, with the layer local
#
# Lazy pull is covered separately by obd-lazy-ublk-e2e.sh, which has to remove the local layer
# to mean anything. This one deliberately leaves it in place: what is under test here is that
# the ordinary paths did not regress.
set -uo pipefail

BIN=${BIN:-/tmp/beantest/bin}
STACK=${STACK:-/tmp/beantest/dev-fc-stack.sh}
IMAGE=${IMAGE:-beanreg.local:5443/alpine:3.20}
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
pass=0
fail=0

cleanup() { BIN=$BIN bash "$STACK" stop >/dev/null 2>&1; }
trap cleanup EXIT

kill_all() {
	for s in $("$BIN/bean" ls 2>/dev/null | awk '/^sbx_/ {print $1}'); do
		"$BIN/bean" kill "$s" >/dev/null 2>&1
	done
	sleep 2
}

# check runs one configuration end to end and reports whether the guest answered.
check() {
	local name=$1 flags=$2

	say ""
	say "=========================================================="
	say "$name"
	say "  flags: ${flags:-<none>}"
	say "=========================================================="

	BIN=$BIN bash "$STACK" stop >/dev/null 2>&1
	sleep 1
	if ! NODED_FLAGS="$flags" BIN=$BIN bash "$STACK" start >/tmp/regress-$$.log 2>&1; then
		say "  FAIL: stack did not start"
		tail -6 /tmp/regress-$$.log | sed 's/^/    /'
		fail=$((fail + 1))
		return
	fi
	sleep 3

	local sbx
	sbx=$("$BIN/bean" run --image-ref "$IMAGE" --disk-mib 2048 2>&1 | awk '/^sbx_/ {print $1}')
	if [ -z "$sbx" ]; then
		say "  FAIL: create produced no sandbox"
		"$BIN/bean" run --image-ref "$IMAGE" --disk-mib 2048 2>&1 | tail -3 | sed 's/^/    /'
		fail=$((fail + 1))
		kill_all
		return
	fi

	# The guest has to answer, not merely exist. A sandbox in RUNNING whose agent is
	# unreachable is the exact shape of this round's bug, and it reads as success from
	# outside.
	local out
	out=$("$BIN/bean" exec "$sbx" -- sh -c 'cat /etc/alpine-release' 2>&1 | head -1)
	if [ "$out" = "3.20.10" ]; then
		say "  PASS: guest answered $out"
		pass=$((pass + 1))
	else
		say "  FAIL: guest said '${out:-<nothing>}', want 3.20.10"
		say "  guest console errors:"
		grep -acE "EXT4-fs error|I/O error" "/var/lib/bean/sandboxes/$sbx/console.log" 2>/dev/null |
			sed 's/^/    block-io errors: /'
		fail=$((fail + 1))
	fi

	# Writes as well as reads: the mutex added this round is on the write path.
	local wout
	wout=$("$BIN/bean" exec "$sbx" -- sh -c 'echo probe > /root/w.txt && cat /root/w.txt' 2>&1 | head -1)
	if [ "$wout" = "probe" ]; then
		say "  PASS: write then read back"
		pass=$((pass + 1))
	else
		say "  FAIL: write did not read back: '${wout:-<nothing>}'"
		fail=$((fail + 1))
	fi

	kill_all
	say "  leaks: ublk=$(ls -1 /dev/ublkb* 2>/dev/null | wc -l) dm=$(dmsetup ls 2>/dev/null | grep -c bean || true)"
}

S3_ARGS="--s3-endpoint $S3 --s3-bucket bean-obd-layers"

check "1. dm-snapshot (default)"        ""
check "2. ublk alone"                   "--fc-ublk"
check "3. overlaybd over tcmu"          "--fc-overlaybd $S3_ARGS"
check "4. overlaybd over ublk"          "--fc-overlaybd --fc-ublk $S3_ARGS"

say ""
say "=========================================================="
say "passed $pass, failed $fail"
say "=========================================================="
[ "$fail" -eq 0 ]

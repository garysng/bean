#!/usr/bin/env bash
# Compares overlaybd's two transports on the number that motivated ublk: teardown.
#
# The claim under test is narrow. Both runs serve the *same* layers, resolved and converted
# by the same code, so image sharing and conversion CPU are held constant -- the only
# difference is how the assembled layer chain reaches the guest: TCMU (a SCSI fabric per
# sandbox, driven by the overlaybd daemon) or ublk (io_uring commands from noded itself).
#
# TCMU takes 4.0 s to tear down 128 devices and takes the same 4.0 s on kernel 5.15 and
# 6.8, because its daemon serialises through one netlink socket upstream warns against using
# concurrently. A cost that does not move with the kernel is a cost in the transport, and
# the only way past it is a different transport. This script is what turns that reasoning
# into a measurement.
#
# Both transports are run in one invocation, alternating nothing else: same host, same
# image, same concurrency, same disk request. A comparison assembled from two separate
# sessions is not one.
#
# Concurrency is stepped rather than jumped to. Going straight to 256 on the ublk path once
# left this host with 141 undeletable kernel objects and needing two reboots: a ublk device
# has no force-remove, so a leak is unrecoverable from userspace. Each step checks for
# leaks before the next one runs, and nothing exceeds ublks_max.
#
# Usage: obd-transport-bench.sh [--steps "4 16 60"] [--image REF]
set -uo pipefail

BIN=${BIN:-/tmp/beantest/bin}
STACK=${STACK:-$(dirname "$0")/dev-fc-stack.sh}
STRESS=${STRESS:-$(dirname "$0")/stress-fc.sh}
IMAGE=${IMAGE:-alpine:3.20}
STEPS=${STEPS:-"4 16 60"}
DISK_MIB=${DISK_MIB:-2048}
METRICS_URL=${METRICS_URL:-http://127.0.0.1:17444/metrics}

# The node has to advertise capacity for the widest step, or the scheduler refuses the
# excess with NO_CAPACITY and the run reports a throughput measured on whatever fraction
# fit. The dev stack defaults to 8 vCPU, which silently caps a 60-way step at 8 -- the
# first run of this script reported "18.65 creates/s" from 8 successes and 52 refusals.
#
# A sandbox reserves 1 vCPU and 512 MiB nominally while costing far less, so these are
# ledger figures rather than a claim about the hardware.
MAX_STEP=0
for n in $STEPS; do [ "$n" -gt "$MAX_STEP" ] && MAX_STEP=$n; done
export NODE_CPU=${NODE_CPU:-$((MAX_STEP * 2))}
export NODE_MEM_MIB=${NODE_MEM_MIB:-$((MAX_STEP * 1024))}
export NODE_DISK_MIB=${NODE_DISK_MIB:-$((MAX_STEP * DISK_MIB * 2))}

export BEAN_BASE_URL=${BEAN_BASE_URL:-http://127.0.0.1:18080}
export BEAN_API_KEY=${BEAN_API_KEY:-devkey}

while [ $# -gt 0 ]; do
	case "$1" in
	--steps) STEPS=$2; shift 2 ;;
	--image) IMAGE=$2; shift 2 ;;
	*) echo "unknown flag: $1" >&2; exit 2 ;;
	esac
done

say() { printf '%s\n' "$*"; }

# ublks_max is a hard kernel ceiling and a step past it does not fail cleanly -- it leaks
# devices that cannot be removed. Read it and refuse rather than discover it.
UBLKS_MAX=64
if [ -r /sys/module/ublk_drv/parameters/ublks_max ]; then
	UBLKS_MAX=$(cat /sys/module/ublk_drv/parameters/ublks_max)
fi
for n in $STEPS; do
	if [ "$n" -gt "$UBLKS_MAX" ]; then
		say "refusing: step $n exceeds ublks_max=$UBLKS_MAX, and a create past it leaks a"
		say "kernel object with no force-remove. Raise it with 'modprobe ublk_drv ublks_max=N'"
		say "-- which disturbs every running ublk device on this host -- or lower --steps."
		exit 1
	fi
done

leaked_ublk() { ls -1 /dev/ublkb* 2>/dev/null | wc -l; }
leaked_tcmu() { ls -1 /sys/kernel/config/target/core/user_999 2>/dev/null | grep -c . || true; }

cleanup() { BIN=$BIN bash "$STACK" stop >/dev/null 2>&1; }
trap cleanup EXIT

# phase reads one histogram's mean from noded's own metrics, which is where the create is
# already decomposed -- timing the same things from outside would measure the CLI too.
phase() {
	local name=$1
	# The series is bean_node_create_phase_seconds_{sum,count} with phase *and* runtime
	# labels, so it is matched by substring rather than by an exact key. Constructing the
	# exact label set is what made the first version print n/a for every phase while the
	# metrics were there all along.
	curl -s "$METRICS_URL" 2>/dev/null |
		awk -v n="phase=\"$name\"" '
			index($1, "bean_node_create_phase_seconds_sum")   && index($1, n) { s = $2 }
			index($1, "bean_node_create_phase_seconds_count") && index($1, n) { c = $2 }
			END { if (c > 0) printf "%.3fs over %d", s / c, c; else print "n/a" }'
}

run_transport() {
	local label=$1 flags=$2

	say ""
	say "############################################################"
	say "# transport: $label"
	say "# noded flags: $flags"
	say "############################################################"

	BIN=$BIN bash "$STACK" stop >/dev/null 2>&1
	sleep 1
	if ! NODED_FLAGS="$flags" BIN=$BIN bash "$STACK" start >/tmp/stack-$label.log 2>&1; then
		say "FAILED to start the stack; last lines:"
		tail -20 /tmp/stack-$label.log
		return 1
	fi
	sleep 3

	# One create first, alone. It converts the image, so leaving it inside the timed run
	# would attribute a one-off conversion to whichever step happened to be first.
	say ""
	say "-- warming the image (conversion happens here, not in the timed steps) --"
	COUNT=1 DISK_MIB=$DISK_MIB IMAGE=$IMAGE bash "$STRESS" --count 1 --image "$IMAGE" \
		>/tmp/warm-$label.log 2>&1 || {
		say "warm create FAILED; last lines:"
		tail -25 /tmp/warm-$label.log
		return 1
	}
	say "warm create ok"

	for n in $STEPS; do
		say ""
		say "-- $label, concurrency $n --"
		local before_ublk before_tcmu
		before_ublk=$(leaked_ublk)
		before_tcmu=$(leaked_tcmu)

		local t0 t1
		t0=$(date +%s)
		COUNT=$n DISK_MIB=$DISK_MIB IMAGE=$IMAGE bash "$STRESS" --count "$n" --image "$IMAGE" \
			>"/tmp/stress-$label-$n.log" 2>&1
		local rc=$?
		t1=$(date +%s)

		if [ $rc -ne 0 ]; then
			say "run FAILED (rc=$rc); last lines:"
			tail -25 "/tmp/stress-$label-$n.log"
		fi

		# A step with any refusal is reported as void rather than as a number. The
		# throughput of the fraction that fit is not the throughput of the step, and
		# printing it next to a failure count invites reading it as one -- which is what
		# happened on this script's first run.
		local failed
		failed=$(grep -oE "^ *failed +[0-9]+" "/tmp/stress-$label-$n.log" | awk '{print $2}' | tail -1)
		failed=${failed:-0}
		if [ "$failed" -gt 0 ]; then
			say "    VOID: $failed of $n creates were refused, so this step measures nothing."
			say "    reason: $(grep -oE "HTTP [0-9]+|NO_CAPACITY|no capacity[^\"]{0,80}" \
				"/tmp/stress-$label-$n.log" | sort -u | head -2 | tr '\n' ' ')"
		fi

		# The stress script prints its own p50/throughput; surface the lines that matter
		# rather than the whole log.
		grep -iE "p50|p95|throughput|created|failed|leftover|leaked" "/tmp/stress-$label-$n.log" |
			sed 's/^/    /' | head -12

		# Seconds from date rather than a float from bc, which is not installed here.
		say "    wall:            $((t1 - t0))s"
		say "    fc_rootfs mean:  $(phase fc_rootfs)"
		say "    runtime_create:  $(phase runtime_create)"
		say "    destroy mean:    $(phase destroy)"
		say "    obd_detach mean: $(phase obd_detach)"
		say "    ublk devices left: $(( $(leaked_ublk) - before_ublk ))"
		say "    tcmu backstores left: $(( $(leaked_tcmu) - before_tcmu ))"
	done

	BIN=$BIN bash "$STACK" stop >/dev/null 2>&1
}

say "== environment =="
say "kernel:    $(uname -r)"
say "cores:     $(nproc)"
say "ublks_max: $UBLKS_MAX"
say "image:     $IMAGE"
say "steps:     $STEPS"
say "disk req:  ${DISK_MIB}MiB"

# The blob store is passed to noded explicitly. The dev stack exports BEAN_S3_* for
# bean-api only, so noded reads no endpoint from the environment and comes up with
# blobStore=none -- and then a create that resolves a published template fails with
# "snapshot filesystem needs an object store", which reads like a broken node rather than
# a store that was never configured.
S3=${S3_ENDPOINT:-http://127.0.0.1:9000}
S3_ARGS="--s3-endpoint $S3 --s3-bucket ${S3_BUCKET:-bean-obd-layers}"
export BEAN_S3_ACCESS_KEY=${BEAN_S3_ACCESS_KEY:-beanadmin}
export BEAN_S3_SECRET_KEY=${BEAN_S3_SECRET_KEY:-beansecret123}

run_transport tcmu "--fc-overlaybd $S3_ARGS"
run_transport ublk "--fc-overlaybd --fc-ublk $S3_ARGS"

say ""
say "== done =="
say "logs: /tmp/stress-{tcmu,ublk}-N.log, /tmp/stack-{tcmu,ublk}.log"

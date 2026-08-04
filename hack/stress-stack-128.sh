#!/usr/bin/env bash
# Brings up a stack sized for the 128-core stress host, where the defaults in
# dev-fc-stack.sh are wrong in three ways that each silently cap the experiment.
#
# It exists as a file rather than a long environment prefix because the settings
# themselves need explaining, and a run whose parameters live in shell history cannot
# be reproduced or reviewed.
set -euo pipefail

cd "$(dirname "$0")/.."

# Node capacity. The dev defaults (8 CPU, 16 GiB, 100 GiB) would each bind long before
# the host's CPU did, which is the thing under test.
export NODE_CPU=${NODE_CPU:-128}
export NODE_MEM_MIB=${NODE_MEM_MIB:-400000}
export NODE_DISK_MIB=${NODE_DISK_MIB:-600000}

# This host has vmlinux-6.1.175 rather than the 6.1.102 the dev stack prefers. The
# difference is ~90 ms per boot, which shifts the absolute numbers and not the
# attribution the run is testing -- so it is recorded here rather than treated as a
# blocker.
export KERNEL=${KERNEL:-/var/lib/bean/assets/vmlinux-6.1.175}

# No buildctl and no MinIO on this host. Both are cleared rather than left to default,
# and both need the value to be empty rather than absent:
#   - buildkitd: noded refuses to start when builds are requested and buildctl is
#     missing, so a stack for running sandboxes has to say it wants no builds.
#   - S3: the gateway treats an unreachable endpoint as fatal, and falls back to a
#     local snapshot directory only when no endpoint is configured at all.
export BUILDKIT_ADDR=
export BEAN_S3_ENDPOINT=

# Queueing on, because the point of the run is to see what the platform sustains
# rather than how a burst is refused. A create that has to wait for a lost race is
# still a create; one rejected at the door is a hole in the measurement.
export API_FLAGS=${API_FLAGS:-"--create-wait 120s"}

# Warm snapshots stay off here: the cold run is the baseline the attribution is stated
# against, and turning them on would measure the optimisation instead. Pass
# NODED_FLAGS="--fc-warm-snapshots" for the warm half of the comparison.
export NODED_FLAGS=${NODED_FLAGS:-}

echo "stack for a $NODE_CPU-core host:"
echo "  kernel        $KERNEL"
echo "  capacity      cpu=$NODE_CPU mem=${NODE_MEM_MIB}MiB disk=${NODE_DISK_MIB}MiB"
echo "  buildkit      ${BUILDKIT_ADDR:-<disabled>}"
echo "  snapshots     ${BEAN_S3_ENDPOINT:-<local directory>}"
echo "  noded flags   ${NODED_FLAGS:-<none>}"
echo

exec bash hack/dev-fc-stack.sh "$@"

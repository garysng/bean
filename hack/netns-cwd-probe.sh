#!/usr/bin/env bash
# Checks whether entering a network namespace preserves the working directory.
#
# This matters more than it sounds. Snapshot portability rests entirely on the
# VMM being started with cmd.Dir set to the sandbox directory while every
# recorded path stays relative: Firecracker saves drive and vsock paths into the
# machine state and re-resolves them from its own working directory, which is what
# lets a snapshot taken by one sandbox restore into another (docs/vm-assembly.md
# §5). If joining a netns changed the cwd, adding networking would silently break
# that — silently, because a snapshot would then resolve paths against the wrong
# sandbox and fail at restore rather than at the point of the mistake.
#
# Verify before building, not after.
set -uo pipefail
NS=bean-cwd-probe-$$
WORK=$(mktemp -d /tmp/bean-cwd.XXXXXX)

cleanup() {
  ip netns del "$NS" 2>/dev/null || true
  rmdir "$WORK" 2>/dev/null || true
}
trap cleanup EXIT

[[ $EUID -eq 0 ]] || { echo "needs root to create a network namespace"; exit 77; }

ip netns add "$NS" || exit 1
cd "$WORK" || exit 1

outside=$(pwd)
inside=$(ip netns exec "$NS" pwd)
printf 'outside netns: %s\n' "$outside"
printf 'inside netns:  %s\n' "$inside"

if [[ "$outside" == "$inside" ]]; then
  echo "PASS: the working directory survives, so relative paths keep resolving"
  exit 0
fi
echo "FAIL: entering a netns changed the cwd; relative device and vsock paths"
echo "would resolve against the wrong sandbox and break snapshot restore"
exit 70

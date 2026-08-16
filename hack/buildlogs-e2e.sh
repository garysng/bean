#!/usr/bin/env bash
# Run the S3 build-log e2e (tests/e2e/buildlogs_test.go) against a local
# MinIO + buildkitd. See docs/build-logs-s3.md §14.
#
# Usage: hack/buildlogs-e2e.sh <creds-env-file>
#
# The creds file must export BEAN_S3_ENDPOINT / BEAN_S3_ACCESS_KEY /
# BEAN_S3_SECRET_KEY (BEAN_S3_REGION optional). It is *sourced*, never echoed,
# so secrets stay in the process environment and out of any log or transcript —
# the same env-only rule the daemons follow (docs/s3-storage.md §6).
set -euo pipefail

ENVFILE="${1:?usage: buildlogs-e2e.sh <creds-env-file>}"
[ -r "$ENVFILE" ] || { echo "cannot read creds file: $ENVFILE" >&2; exit 2; }

set -a
# shellcheck disable=SC1090
source "$ENVFILE"
set +a

: "${BEAN_S3_ENDPOINT:?creds file did not set BEAN_S3_ENDPOINT}"
: "${BEAN_S3_ACCESS_KEY:?creds file did not set BEAN_S3_ACCESS_KEY}"
: "${BEAN_S3_SECRET_KEY:?creds file did not set BEAN_S3_SECRET_KEY}"
export BEAN_S3_REGION="${BEAN_S3_REGION:-us-east-1}"
export BEAN_S3_LOGS_BUCKET="${BEAN_S3_LOGS_BUCKET:-bean-build-logs-e2e}"
export BEAN_BUILDKIT_ADDR="${BEAN_BUILDKIT_ADDR:-unix:///run/bean/buildkitd.sock}"
export BEAN_E2E_BASE_IMAGE="${BEAN_E2E_BASE_IMAGE:-docker.m.daocloud.io/library/busybox}"

# Deliberately print only non-secret config (endpoint/region/bucket are fine;
# access/secret keys are never printed).
echo "endpoint=${BEAN_S3_ENDPOINT} region=${BEAN_S3_REGION} logs-bucket=${BEAN_S3_LOGS_BUCKET}"
echo "buildkit=${BEAN_BUILDKIT_ADDR} base-image=${BEAN_E2E_BASE_IMAGE}"

# Preflight: prove buildkitd can pull the base image and solve a trivial build,
# so an environment problem (no registry access, wrong buildkit addr) surfaces
# here with a clear message instead of as three opaque build FAILEDs.
echo "=== buildkitd preflight ==="
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
printf 'FROM %s\nRUN true\n' "$BEAN_E2E_BASE_IMAGE" > "$tmp/Dockerfile"
if ! buildctl --addr "$BEAN_BUILDKIT_ADDR" build \
      --frontend dockerfile.v0 \
      --local context="$tmp" --local dockerfile="$tmp" \
      --output type=tar,dest=/dev/null >/dev/null 2>"$tmp/err"; then
  echo "buildkitd preflight FAILED (can it reach a registry for ${BEAN_E2E_BASE_IMAGE}?):" >&2
  tail -8 "$tmp/err" >&2
  exit 3
fi
echo "preflight ok"

echo "=== build-log e2e ==="
exec go test -tags=e2e -count=1 -v -timeout 20m \
  -run 'TestBuildLogsLandInS3|TestBuildLogsServedFromOtherReplica|TestBuildCancelFromOtherReplica|TestBuildSurvivesReplicaRestart' \
  ./tests/e2e/

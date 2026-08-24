#!/usr/bin/env bash
# Run the S3 build-log e2e (tests/e2e/buildlogs_test.go) against a local
# MinIO + buildkitd. See docs/build-logs-s3.md §14.
#
# Usage: hack/buildlogs-e2e.sh <creds-env-file>
#
# The creds file must export WIZARD_S3_ENDPOINT / WIZARD_S3_ACCESS_KEY /
# WIZARD_S3_SECRET_KEY (WIZARD_S3_REGION optional). It is *sourced*, never echoed,
# so secrets stay in the process environment and out of any log or transcript —
# the same env-only rule the daemons follow (docs/s3-storage.md §6).
set -euo pipefail

ENVFILE="${1:?usage: buildlogs-e2e.sh <creds-env-file>}"
[ -r "$ENVFILE" ] || { echo "cannot read creds file: $ENVFILE" >&2; exit 2; }

set -a
# shellcheck disable=SC1090
source "$ENVFILE"
set +a

: "${WIZARD_S3_ENDPOINT:?creds file did not set WIZARD_S3_ENDPOINT}"
: "${WIZARD_S3_ACCESS_KEY:?creds file did not set WIZARD_S3_ACCESS_KEY}"
: "${WIZARD_S3_SECRET_KEY:?creds file did not set WIZARD_S3_SECRET_KEY}"
export WIZARD_S3_REGION="${WIZARD_S3_REGION:-us-east-1}"
export WIZARD_S3_LOGS_BUCKET="${WIZARD_S3_LOGS_BUCKET:-wizard-build-logs-e2e}"
export WIZARD_BUILDKIT_ADDR="${WIZARD_BUILDKIT_ADDR:-unix:///run/wizard/buildkitd.sock}"
export WIZARD_E2E_BASE_IMAGE="${WIZARD_E2E_BASE_IMAGE:-docker.m.daocloud.io/library/busybox}"

# Deliberately print only non-secret config (endpoint/region/bucket are fine;
# access/secret keys are never printed).
echo "endpoint=${WIZARD_S3_ENDPOINT} region=${WIZARD_S3_REGION} logs-bucket=${WIZARD_S3_LOGS_BUCKET}"
echo "buildkit=${WIZARD_BUILDKIT_ADDR} base-image=${WIZARD_E2E_BASE_IMAGE}"

# Preflight: prove buildkitd can pull the base image and solve a trivial build,
# so an environment problem (no registry access, wrong buildkit addr) surfaces
# here with a clear message instead of as three opaque build FAILEDs.
echo "=== buildkitd preflight ==="
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
printf 'FROM %s\nRUN true\n' "$WIZARD_E2E_BASE_IMAGE" > "$tmp/Dockerfile"
if ! buildctl --addr "$WIZARD_BUILDKIT_ADDR" build \
      --frontend dockerfile.v0 \
      --local context="$tmp" --local dockerfile="$tmp" \
      --output type=tar,dest=/dev/null >/dev/null 2>"$tmp/err"; then
  echo "buildkitd preflight FAILED (can it reach a registry for ${WIZARD_E2E_BASE_IMAGE}?):" >&2
  tail -8 "$tmp/err" >&2
  exit 3
fi
echo "preflight ok"

echo "=== build-log e2e ==="
exec go test -tags=e2e -count=1 -v -timeout 20m \
  -run 'TestBuildLogsLandInS3|TestBuildLogsServedFromOtherReplica|TestBuildCancelFromOtherReplica|TestBuildSurvivesReplicaRestart' \
  ./tests/e2e/

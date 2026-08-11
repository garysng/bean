#!/usr/bin/env bash
# Runs the sealed-layer reader test against a layer produced by upstream's own tooling.
#
# A script because the env-var-plus-flags inline form gets rejected before it runs, and
# because the fixture path belongs next to the run that needs it rather than in a shell
# history.
set -uo pipefail

LAYER=${LAYER:-/tmp/obd-seal-probe/sealed.lsmt}
BIN=${BIN:-/tmp/image_linux.test}

if [ ! -f "$LAYER" ]; then
	echo "no sealed layer at $LAYER; run hack/obd-seal-a-layer.sh first" >&2
	exit 1
fi

echo "== layer =="
echo "path: $LAYER"
echo "size: $(stat -c %s "$LAYER")"
echo "first 8: $(head -c 8 "$LAYER" | od -An -tx1 | tr -d ' \n')"

echo ""
echo "== reader test =="
BEAN_SEALED_LAYER="$LAYER" "$BIN" -test.run TestOpenRealSealedLayer -test.v 2>&1 | tail -15

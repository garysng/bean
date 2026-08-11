#!/usr/bin/env bash
# Produces a sealed overlaybd layer with real content, using upstream's own tooling, so
# the Go LSMT/ZFile reader can be checked against bytes it had no hand in writing.
#
# This is the verification that matters most for the reader: every other test builds its
# input, which cannot catch the test's idea of the format and the reader's idea of it being
# wrong in the same way. It has already earned its keep -- it found that bean's layers are
# tar-wrapped (so the payload is not at offset 0) and that both CRC variants in the reader
# were wrong.
#
# The content goes in through `overlaybd-apply`, not by writing to the data file directly:
# the data file is an extent store, and bytes poked into it are not registered in the
# index, so sealing produces a valid layer with an empty index and nothing to read.
#
# Writes only under its own work directory and removes nothing outside it.
set -euo pipefail

BIN=${BIN:-/opt/overlaybd/bin}
WORK=${WORK:-/tmp/obd-seal-probe}
SIZE_GB=${SIZE_GB:-1}

say() { printf '%s\n' "$*"; }

rm -rf "$WORK"
mkdir -p "$WORK/content"

say "== building a tar to apply =="
# Recognisable content at more than one size, so the sealed layer has several extents and
# a reader that mixes up an offset produces visibly wrong bytes rather than plausible ones.
printf 'BEAN-PROBE-ALPHA\n' >"$WORK/content/alpha.txt"
printf 'BEAN-PROBE-BETA\n' >"$WORK/content/beta.txt"
head -c 200000 /dev/urandom >"$WORK/content/blob.bin"
mkdir -p "$WORK/content/nested/dir"
printf 'BEAN-PROBE-NESTED\n' >"$WORK/content/nested/dir/gamma.txt"
tar -C "$WORK/content" -cf "$WORK/layer.tar" .
say "tar: $(stat -c %s "$WORK/layer.tar") bytes"

say ""
say "== creating a writable overlaybd layer (${SIZE_GB}GB virtual) =="
# --mkfs because this is a base layer and so has to carry a filesystem. bean passes it for
# the base and omits it for layers that sit over others, where formatting would write an
# empty superblock over the filesystem they hold.
"$BIN/overlaybd-create" --mkfs "$WORK/data" "$WORK/index" "$SIZE_GB"

say ""
say "== applying the tar into the layer =="
# Exactly the config bean writes: empty lowers for a base layer, and no resultFile -- with
# one, overlaybd-apply segfaulted here.
cat >"$WORK/apply.json" <<JSON
{
  "lowers": [],
  "upper": {"data": "$WORK/data", "index": "$WORK/index"}
}
JSON
"$BIN/overlaybd-apply" "$WORK/layer.tar" "$WORK/apply.json"
say "applied; data now $(stat -c %s "$WORK/data") bytes"

say ""
say "== sealing with overlaybd-commit -z -t =="
# The same flags bean uses: -z compresses to a ZFile so blocks are independently
# decompressable, -t wraps the result in a tar so it is a valid OCI blob.
"$BIN/overlaybd-commit" -z -t "$WORK/data" "$WORK/index" "$WORK/sealed.lsmt"
say "sealed: $(stat -c %s "$WORK/sealed.lsmt") bytes"

say ""
say "== layout =="
say "first 8 bytes: $(head -c 8 "$WORK/sealed.lsmt" | od -An -tx1 | tr -d ' \n')"
say "(tar-wrapped, so this is a tar header rather than a magic number)"

say ""
say "sealed layer left at $WORK/sealed.lsmt"

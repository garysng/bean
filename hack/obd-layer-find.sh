#!/usr/bin/env bash
# Lists sealed overlaybd layers on this host, which is what the Go LSMT reader has to be
# able to open.
#
# It matters that these were produced by upstream's `overlaybd-commit`, not by bean's
# tests: a reader verified only against layers its own tests build cannot catch the
# builder and the reader sharing a misreading of the format.
set -uo pipefail

say() { printf '%s\n' "$*"; }

say "== candidate layer directories =="
for d in /var/lib/bean/layers /var/lib/bean/images /var/lib/bean/tplimages \
	/var/lib/bean/tplimages2 /var/lib/bean/p2images /var/lib/bean/p3images; do
	[ -d "$d" ] || continue
	n=$(find "$d" -maxdepth 2 -type f 2>/dev/null | wc -l)
	say "$d: $n files"
done

say ""
say "== files whose first 8 bytes are the LSMT magic =="
# "LSMT\0\1\2" little-endian. Identified by content rather than by name, because the
# naming convention is bean's and the format is not.
found=0
while IFS= read -r f; do
	magic=$(head -c 8 "$f" 2>/dev/null | od -An -tx1 | tr -d ' \n')
	if [ "$magic" = "4c534d5400010200" ]; then
		sz=$(stat -c %s "$f")
		say "LSMT: $f ($sz bytes)"
		found=$((found + 1))
		[ "$found" -ge 12 ] && break
	fi
done < <(find /var/lib/bean -maxdepth 3 -type f -size +8k 2>/dev/null)
[ "$found" -eq 0 ] && say "(none found)"

say ""
say "== files whose first 8 bytes are the ZFile magic =="
found=0
while IFS= read -r f; do
	magic=$(head -c 8 "$f" 2>/dev/null | od -An -tx1 | tr -d ' \n')
	if [ "$magic" = "5a46696c6500010" ] || [ "${magic:0:14}" = "5a46696c650001" ]; then
		sz=$(stat -c %s "$f")
		say "ZFile: $f ($sz bytes)"
		found=$((found + 1))
		[ "$found" -ge 12 ] && break
	fi
done < <(find /var/lib/bean -maxdepth 3 -type f -size +8k 2>/dev/null)
[ "$found" -eq 0 ] && say "(none found)"

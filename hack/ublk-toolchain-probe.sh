#!/usr/bin/env bash
# Reports whether this host can build bean, and what is missing if not.
#
# Separate from ublk-env-probe.sh because that one answers "can this host run the
# benchmark" and this one answers "can it produce the binary to benchmark". The two
# failures need different fixes.
set -uo pipefail

say() { printf '%s\n' "$*"; }

say "== go toolchain =="
for c in /usr/local/go/bin/go /usr/lib/go/bin/go /snap/bin/go; do
	if [ -x "$c" ]; then
		say "found: $c ($("$c" version 2>&1))"
	fi
done
if command -v go >/dev/null 2>&1; then
	say "on PATH: $(go version)"
else
	say "on PATH: absent"
fi
for d in /usr/lib/go-1.2*; do
	[ -d "$d" ] && say "distro package: $d"
done

say ""
say "== what a cross-compiled binary would need =="
say "arch:  $(uname -m)"
say "libc:  $(ldd --version 2>&1 | head -1)"

say ""
say "== bean binaries already here =="
for b in noded bean bean-api bean-proxy beand; do
	p=$(command -v "$b" 2>/dev/null)
	if [ -n "$p" ]; then
		say "$b: $p"
	fi
done
ls -1 /usr/local/bin 2>/dev/null | grep -E '^(bean|noded|beand)' | sed 's/^/local: /'

say ""
say "== previous run artifacts =="
ls -1d /var/lib/bean/* 2>/dev/null | head -10

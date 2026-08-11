#!/usr/bin/env bash
# Reports what a ublk/overlaybd benchmark run needs from this host, and nothing else.
#
# A script rather than an inline ssh command because the long chained form gets rejected
# before it runs, and because a run that reports its own environment is reproducible by
# whoever reads the numbers afterwards.
set -uo pipefail

say() { printf '%s\n' "$*"; }

say "== host =="
say "kernel:  $(uname -r)"
say "cores:   $(nproc)"
say "load:    $(cut -d' ' -f1-3 /proc/loadavg)"

say ""
say "== ublk =="
if [ -e /dev/ublk-control ]; then
	say "control: present"
else
	say "control: ABSENT"
fi
if [ -r /sys/module/ublk_drv/parameters/ublks_max ]; then
	say "ublks_max: $(cat /sys/module/ublk_drv/parameters/ublks_max)"
else
	say "ublks_max: unreadable (module not loaded?)"
fi
say "live ublk block devices: $(ls -1 /dev/ublkb* 2>/dev/null | wc -l)"
say "live ublk char devices:  $(ls -1 /dev/ublkc* 2>/dev/null | wc -l)"

say ""
say "== overlaybd =="
if pgrep -x overlaybd-tcmu >/dev/null 2>&1; then
	say "overlaybd-tcmu: running"
else
	say "overlaybd-tcmu: not running"
fi
for b in overlaybd-create overlaybd-commit overlaybd-apply; do
	if [ -x "/opt/overlaybd/bin/$b" ]; then
		say "$b: present"
	else
		say "$b: MISSING"
	fi
done

say ""
say "== bean source =="
for d in /root/bean-src /root/src/bean /opt/bean /srv/bean /root/go/src/github.com/garysng/bean; do
	if [ -f "$d/go.mod" ]; then
		say "found: $d"
	fi
done
say "go: $(command -v go >/dev/null 2>&1 && go version || echo absent)"

say ""
say "== bean assets =="
if [ -d /var/lib/bean/assets ]; then
	ls -1 /var/lib/bean/assets 2>/dev/null | head -10
else
	say "/var/lib/bean/assets absent"
fi

say ""
say "== stale state =="
say "D-state processes: $(ps -eo stat= | grep -c '^D' || true)"
say "bean dm mappings:  $(dmsetup ls 2>/dev/null | grep -c bean || true)"

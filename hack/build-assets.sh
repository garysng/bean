#!/usr/bin/env bash
# Builds the node-side assets the microVM tier needs: an agent disk holding
# beand, and a base rootfs image.
#
# These are node assets rather than build artefacts, which is why they live
# under /var/lib/bean instead of the repo: the agent disk upgrades with the node
# and every sandbox on the host shares one copy of it.
#
# Must run on the node (needs mkfs and loopback mounts).
set -euo pipefail

ASSETS=${ASSETS:-/var/lib/bean/assets}
IMAGES=${IMAGES:-/var/lib/bean/images}
REPO=${REPO:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}

need_root() {
  if [ "$(id -u)" -ne 0 ]; then
    echo "must run as root (mkfs and mount)" >&2
    exit 1
  fi
}

# build_agent_disk packs a statically linked beand into a small ext4 image.
# Read-only and shared by every sandbox, so one copy serves the whole node.
build_agent_disk() {
  local out="$ASSETS/agent.ext4"
  local staging
  staging=$(mktemp -d)
  trap 'rm -rf "$staging"' RETURN

  echo "building beand (static)"
  ( cd "$REPO" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
      go build -ldflags="-s -w" -o "$staging/beand" ./cmd/beand )

  # 32 MiB leaves room for the agent plus a margin; the image is sparse, so
  # unused space costs nothing on disk.
  rm -f "$out"
  truncate -s 32M "$out"
  mkfs.ext4 -q -F -L bean-agent "$out"

  local mnt
  mnt=$(mktemp -d)
  mount -o loop "$out" "$mnt"
  mkdir -p "$mnt/bean"
  install -m 0755 "$staging/beand" "$mnt/bean/beand"

  # The agent disk is the guest's root and is mounted read-only, so every
  # mountpoint the agent needs during early boot has to exist now: it cannot
  # create them at runtime. /rootfs is where the user image goes.
  mkdir -p "$mnt"/{proc,sys,dev,rootfs}

  umount "$mnt"
  rmdir "$mnt"
  echo "agent disk: $out"
}

# build_base_image produces a bootable rootfs from a container image, which is
# how an OCI reference becomes something Firecracker can attach. The agent runs
# as PID 1, so the image needs no init system.
build_base_image() {
  local ref=${1:?image reference required}
  local size_mib=${2:-512}
  # The filename encoding must match refToFilename in internal/node/image.
  local name
  name=$(python3 - "$ref" <<'PY'
import sys
ref = sys.argv[1]
out = []
for ch in ref:
    if ch.isascii() and (ch.isalnum() or ch in "-_"):
        out.append(ch)
    else:
        out.append("_%x" % ord(ch))
print("".join(out))
PY
)
  local out="$IMAGES/$name.ext4"

  echo "exporting $ref"
  local cid
  cid=$(docker create "$ref" /bin/true)
  local tarball
  tarball=$(mktemp)
  docker export "$cid" > "$tarball"
  docker rm -f "$cid" >/dev/null

  rm -f "$out"
  truncate -s "${size_mib}M" "$out"
  mkfs.ext4 -q -F "$out"

  local mnt
  mnt=$(mktemp -d)
  mount -o loop "$out" "$mnt"
  tar -xf "$tarball" -C "$mnt"

  # The guest mounts the agent disk itself, so the mountpoint must exist. The
  # pseudo-filesystem directories are likewise created here because the agent
  # mounts them before anything else runs.
  mkdir -p "$mnt"/{bean,proc,sys,dev,tmp,run}

  umount "$mnt"
  rmdir "$mnt"
  rm -f "$tarball"
  echo "base image: $out"
}

need_root
mkdir -p "$ASSETS" "$IMAGES"

case "${1:-all}" in
  agent) build_agent_disk ;;
  image) build_base_image "${2:?usage: $0 image <ref> [size_mib]}" "${3:-512}" ;;
  all)
    build_agent_disk
    build_base_image "${2:-alpine:3.20}" "${3:-512}"
    ;;
  *) echo "usage: $0 [agent|image <ref> [size]|all [ref] [size]]" >&2; exit 1 ;;
esac

# Phase 3 design notes — fs layers unified on overlaybd, dedup by digest

> Status: 📐 **design, not implemented.** A snapshot's filesystem is still
> captured as a sparse extent stream inside the one opaque bundle described
> below, so nothing here has landed. The status-marker convention is defined in
> [architecture.md](architecture.md) §0.
> **Authority order: code > [status.md](status.md) > [decisions.md](decisions.md) > design docs.**

## Goal (docs/s3-storage.md 8.5)
An image's fs and a snapshot's fs both become overlaybd layer chains keyed by content
digest. A snapshot taken from an image shares that image's layer digests (S3 stores one
copy). The snapshot's fs member moves off the standalone bundle onto the shared overlaybd
layer space; memory + device state remain a separate blob (the one part no image has).

## Current state (verified via code map)
- Capture (`fc_lifecycle_linux.go:187,219`): bundles `vmstate` + `memory`/`memory.diff` +
  `rootfs` (sparse CoW extent stream of the writable device) into ONE gzip tar, streamed to
  control, stored opaque at `snapshots/<id>/data` (`s3blobs.go:38`). fs is never sealed,
  never digested, never deduped against image layers.
- Restore (`fc_linux.go:474-483`, `snapstage_linux.go:72`): base image fs comes from shared
  overlaybd lowers; snapshot fs is replayed as raw extents into the per-sandbox WRITABLE
  layer (ephemeral). Memory via uffd + `/snapshot/load`.
- Snapshot record (`store/types.go:280`): links base image by ref string only; no layer
  digests. `BaseID`/`ChainDepth` express a memory-diff chain among snapshots.
- Primitives that already do the needed shape: `sealWritable` (`obdbuild_linux.go:210`) seals
  writable.data/.index to a digest-keyed layer; `publish`+`recordInStore`
  (`overlaybd_linux.go`) publish by digest + write a StoredManifest{Layers,Config}+tag;
  `PublishBuiltRootfs` (Phase 2) is the template.
- s3blobs.go:36-42 + docs/snapshot-resume.md 3.1 already ANTICIPATE the split (the `/data`
  suffix leaves room for a manifest + rootfs layer alongside the memory blob).

## Design

### Capture (overlaybd/FC tier only)
Instead of writing the `rootfs` sparse member, seal the sandbox writable layer into a
digest-keyed overlaybd layer (reuse `sealWritable`), compute `sha256` of the sealed bytes,
`publish` to `blobs/<digest>` (idempotent, so a re-seal of unchanged fs dedups), and build a
StoredManifest = base-image `StoredLayer`s ++ [sealed snapshot layer]. The snapshot fs layer
chain shares the base image's layer digests verbatim -> S3 stores one copy.

Memory + vmstate keep going into the separate blob (still `snapshots/<id>/data`), now WITHOUT
the rootfs member. This is the "memory/device state remain a separate blob" requirement.

### Storage / record
`store.Snapshot` grows an fs manifest reference (manifest digest, or the layer-chain digests)
distinct from the memory blob. New field must default-compat: absent = legacy opaque-bundle
snapshot (has `rootfs` member in blob).

### Restore
New-style: snapshot fs layer(s) become additional read-only LOWERS in `Prepare`'s chain, with
a fresh empty writable on top -- no extent replay. Memory restore (uffd) unchanged.
Old-style (blob has `rootfs` member): keep the current extent-replay seed path. Dispatch on
member presence (`readSnapshotBundle` already skips unknown members).

### Migration / compat
- `readSnapshotBundle` (`fc_lifecycle_linux.go:653`) already dispatches on member name -> old
  bundles keep restoring. Branch new vs old on presence of the fs-manifest record field +
  absence of `rootfs` member. Do NOT delete the extent-replay path; branch it.
- Local (non-overlaybd) tier keeps its tar format (`checkpoint.go`); layer-chain fs is
  overlaybd/FC-only.
- Reclaim lifetimes: fs-layer refcount (digest dedup) vs snapshot BaseID memory-diff chain
  must not disagree about when a base is reclaimable. Keep memory-diff chain as-is; fs dedup
  is orthogonal (digest-keyed idempotent publish, shared with image layers).

## Decisions (user, 2026-08-09)
1. **Both at once** — memory snapshots AND filesystem-only snapshots move to the layer-chain
   format together (not phased within Phase 3).
2. **No backward compat** — not launched yet, so there are no old-format snapshots to preserve.
   REMOVE the extent-replay seed path and the `rootfs` sparse bundle member outright rather
   than branching legacy restore. The bundle becomes memory + vmstate only; fs is always the
   sealed layer chain. This simplifies capture, storage, and restore (no dual-path dispatch).

## e2e (needs a real KVM host)
Take a snapshot from a built/pulled image, confirm the snapshot fs layer lands in
`blobs/<digest>` and SHARES the base image's layer digests (one copy in S3); restore on a
cache-cleared node and confirm the fs comes from the shared lowers; memory snapshot restores
guest state.
(buildctl on PATH, docker.m.daocloud.io mirror, vhost_vsock).

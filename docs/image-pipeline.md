# The Image Path: OCI ref → a mountable block device

> 中文版:[zh/image-pipeline.md](zh/image-pipeline.md)

> The status-marker convention is defined in [architecture.md](architecture.md) §0.
> Implementation: `internal/node/image/` (registry / convert / devmapper / pulling).
> "How images get built" is in [image-build.md](image-build.md); this document is "how an existing image becomes something bootable".

What the user hands the platform is an ordinary OCI ref like `python:3.12`. What the fc tier
needs is a block device. This document is every step in between, plus where the 2m45s cold
start goes.

## 1. Three layers of Provider ✅

```
PullingProvider          triggers conversion on a cache miss, deduplicates concurrency
  └── DevMapperProvider  shared read-only base + one CoW per sandbox → /dev/mapper/bean-<id>
      (or FileProvider)  a full copy per sandbox, the fallback when dm is unavailable
```

Why it is layered rather than one large provider: **"where the image comes from" and "how the
block device is assembled" are two different things**. `PullingProvider` wraps any inner
implementation, so the behaviour "pull on first use" does not have to be reimplemented in
every block-device backend; and it will not need to change when the backend becomes overlaybd.

The `Provider` interface has only four methods:

```go
Name() string
Prepare(ctx, sandboxID, imageRef string, opts PrepareOptions) (*Rootfs, error)
Prewarm(ctx, imageRef string) error
Cached() (map[string]int64, error)
```

`Cached()` exists because **the node is the only authority on what it holds** — the heartbeat
reports that manifest, and the scheduler scores image affinity from it while a prewarm job
shows progress from it. Having the control plane infer "roughly what the node has" would
diverge from reality immediately.

## 2. Conversion: tar.gz layers → ext4 ✅

```
① ParseReference        parse the ref
② look in ImageDir      return directly if it exists ← immutable semantics, see below
③ FetchManifest         registry authentication + manifest parsing
④ sizeFor(manifest)     estimate the filesystem size from the compressed layer sizes
⑤ writeBaseImage        create a sparse file → mkfs.ext4 → mount
⑥ applyLayer per layer  unpack in order, handle whiteouts
⑦ add the directories the guest requires   mount points such as /proc /sys /dev
⑧ unmount → rename into place              atomic publication
⑨ write a sidecar recording the ref        remember which ref this file came from
```

**The sidecar is written after the image**, and the order is deliberate: it is the basis for
what `Cached()` reports, so writing the sidecar first would have the node claim to hold an
image that is not usable yet — the scheduler would send work over on that basis, and then
create would fail.

`refToFilename` encodes a ref into a filename (non-alphanumerics become separators), and
**different separators must not all map to the same character**, otherwise `a:b` and `a/b`
would collide. The sidecar is the answer to the reverse lookup: the original ref cannot be
derived back from the filename, so it is recorded separately.

### Immutable semantics ✅

```go
if _, err := os.Stat(final); err == nil {
    // Already converted. Images are immutable once written — a tag that
    // moves is a different digest and so a different file.
    return final, nil
}
```

The filename is derived from the ref, and **a tag that has moved is a different digest and
therefore a different file**. So "use it directly if it exists" cannot hand back stale
content. This is what makes conversion naturally idempotent, and why no cache-invalidation
logic is needed.

### Why nothing is visible until the rename ✅

The work directory and `ImageDir` have to sit on the same filesystem, because the last step is
a `rename`:

```
WorkDir/<tmp>.ext4  →  ImageDir/<name>.ext4
```

The consequence of not doing this is very concrete: a concurrent `Prepare` would see a
**half-written ext4**, mount it and fail — and the failure looks like "corrupt image" rather
than "race". This is the kind of bug that costs a day before you realise it is a race, so
requiring the same filesystem is the better trade.

### How the filesystem size is estimated ⚠️

```go
sizeMiB := (compressed >> 20) * 3      // sum of the compressed layers × 3
if sizeMiB < floor { sizeMiB = floor }  // not below DefaultSizeMiB
if sizeMiB < 256 { sizeMiB = 256 }      // hard floor
```

**This is an estimate, not a calculation.** The manifest carries only compressed sizes, and
the expansion ratio depends on the content (a text layer can reach 5×, already-compressed
binaries are close to 1×). × 3 is a middle value, with the floor covering small images.

This estimate **fails if it guesses low** (ENOSPC during conversion), while guessing high only
wastes the nominal size of a sparse file (actual occupancy follows what is written). So it
leans towards guessing high. **The hit rate of × 3 has never been measured** — if conversion
ENOSPC shows up, this is the first place to look.

### Whiteouts: OCI's deletion semantics ✅

Layers stack, so "delete" has to be expressed with marker files:

| Marker | Meaning | Handling |
|---|---|---|
| `.wh.<name>` | delete `<name>` from the layer below | `os.RemoveAll(victim)` |
| `.wh..wh..opq` | clear everything below this directory | `clearDir(dir)` |

The consequence of missing whiteout handling is that **files deleted in an upper layer
reappear inside the guest** — the typical symptom is a deleted key file or a cleaned-up build
cache coming back, while the image looks fine.

### Path-escape protection ✅

```go
func safeJoin(root, name string) (string, error) {
    clean := filepath.Clean("/" + name)
    joined := filepath.Join(root, clean)
    if joined != root && !strings.HasPrefix(joined, root+string(os.PathSeparator)) {
        return "", fmt.Errorf("layer entry %q escapes the image root", name)
    }
    return joined, nil
}
```

A malicious or malformed image can contain an entry like `../../etc/cron.d/x`.
**Conversion runs as root on the node**, so this is not "a privilege violation inside the
guest" but "writing the host filesystem directly" — it has to be refused during unpacking and
cannot be left to a later stage.

Doing `Clean("/" + name)` before the join is the key part: it reduces the `..` away **under
absolute-path semantics**, which is what makes the prefix check that follows meaningful.

### The gzip decision does not trust the media type ✅

```go
// Most layers are gzipped; the media type says so, but some registries are
// loose about it, so the magic bytes decide.
```

Some registries' media types do not match the actual content. Trusting the media type ends in
unpacking a gzip stream as tar (an immediate failure) or the reverse. Deciding by magic bytes
is defensive, and it costs a few bytes of reading.

## 3. Concurrency deduplication ✅

A batch of evals starting at once, all using the same uncached image, is the **default
scenario** rather than an edge case.

```go
func (p *PullingProvider) Prepare(...) (*Rootfs, error) {
    rootfs, err := p.Inner.Prepare(ctx, sandboxID, imageRef, opts)
    if !errors.Is(err, ErrNotCached) {
        return rootfs, err        // a hit, or some other error
    }
    if err := p.ensure(ctx, imageRef); err != nil {   // ← deduplication happens here
        return nil, err
    }
    return p.Inner.Prepare(ctx, sandboxID, imageRef, opts)
}
```

`ensure` uses an in-flight map to collapse concurrent requests for the same ref into one
conversion, with the rest waiting on the result. **Waiters share the result but keep their own
cancellation** — a client that has given up should not be held back by someone else's pull,
and the comment says so explicitly.

The consequence of not deduplicating is not just waste: N processes simultaneously `mkfs` and
unpack the same image into different temporary files, saturating disk IO, and in the end only
one of their outputs is useful.

## 4. Where the cold-start time goes ⚠️

Measured:

```
busybox   5–10 s
alpine    2m45s (on an unstable network)
```

**The 2m45s is almost entirely network** — pulling the compressed layers. The conversion
itself (mkfs + unpack) is seconds. So:

- **prewarm is a requirement, not an optimisation**. The images have to be spread across the
  target nodes before a batch of evals starts, otherwise the whole first batch of sandboxes
  sits waiting on downloads
- This is also **where overlaybd lazy-pull's value lies**: it trades "download every layer"
  for "read the blocks actually used, on demand". Measured at 7ms to mount, with only 19.6% of
  the layer bytes transferred before it mounts and files can be read
  (decisions §3.1). **But it is not wired in yet** — see §6

Image-affinity scheduling is the other face of the same problem: repeated eval runs on the
same image should land on nodes that already have it. That part is shipped (`Cached()` →
heartbeat → scheduling score).

## 5. commit: the reverse path ✅

`commit` seals the filesystem of a running sandbox into a new base image.

The current implementation **reads a complete ext4 out of the composite device under
`/dev/mapper`** rather than "sealing an incremental layer". The reason is that dm-snapshot's
CoW layer is not in OCI layer format and cannot be used as a layer directly.

The cost: a commit produces a full image rather than an increment. Once overlaybd is wired in
this can become `overlaybd-commit` sealing the LSMT writable layer — that is the genuinely
zero-conversion form (image-build §2).

## 6. Relationship to overlaybd ⚠️

**The capability is measured working, but it is not wired into the code.** The difference
between the current state and the target form:

| | Current (dm-snapshot) | Target (overlaybd) |
|---|---|---|
| First use | pull the whole thing + convert, minutes | read blocks on demand, 7ms to mount |
| Per-sandbox cost | 8 KiB (measured) | comparable, CoW stores only changes in both |
| Layer format | converted into one ext4, layer structure lost | LSMT layers retained, sealable directly |
| commit | read out a full ext4 | seal the writable layer, zero conversion |

**The key realisation: overlaybd's value is in "the wait when a large image is used for the
first time", not in "the per-sandbox cost" — dm-snapshot's CoW already solved the latter.**
So it is an optimisation rather than infrastructure, which is also why it is sequenced after
the snapshot capability.

The work to wire it in is writing an `OverlaybdProvider` implementing the same `Provider`
interface: configfs orchestration (TCMU backstore → tpgt → nexus → LUN, **the order cannot be
wrong**), registry push, lifecycle. Two traps only a real machine can reveal are recorded in
decisions §3.1: the LUN has to be linked after the nexus, and every backstore has to be given
a unique `vpd_unit_serial`, otherwise the host's `multipathd` will merge the devices of
different images and **silently return wrong data**.

## 7. What does not exist yet 📐

- **Cache reclamation**: base images in `ImageDir` are never cleaned up automatically. The
  design has image-granularity LRU + chunk LRU (noded-design §4.2) with zero implementation.
  Running long enough will fill the disk
- **Uploading build outputs to S3**: a built image lands on the node's local disk, so it is
  **only usable on the node that built it** (GitHub #22). In a multi-node cluster that is
  essentially unusable
- **digest verification**: layer digests are not verified on pull. Under the premise that the
  registry is trusted this is not a big problem, but it is one link in supply-chain protection

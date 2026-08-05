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

OverlaybdProvider        alternative, --fc-overlaybd; layers shared by digest (section 7)
```

Why it is layered rather than one large provider: **"where the image comes from" and "how the
block device is assembled" are two different things**. `PullingProvider` wraps any inner
implementation, so the behaviour "pull on first use" does not have to be reimplemented in
every block-device backend.

`OverlaybdProvider` sits **beside** that stack rather than inside it, which is the one place
the original layering did not anticipate. It resolves an image to its layers itself, because
deciding which layers to fetch is the same decision as deciding whether to fetch them at all
(lazy pull fetches none). Wrapping it in `PullingProvider` would run the ext4 converter first
and produce an artifact overlaybd has no use for.

The `Provider` interface is small — assembling a device, plus reporting what the node holds:

```go
Name() string
Prepare(ctx, sandboxID, imageRef string, opts PrepareOptions) (*Rootfs, error)
Prewarm(ctx, imageRef string) error
Cached() (map[string]CachedImage, error)
Config(imageRef string) (*Config, error)   // §5
Digest(imageRef string) (string, error)
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

### Deciding gzip by magic bytes rather than the media type 📐

The code branches on `layer.MediaType` alone (`convert_linux.go`, `applyLayer`); nothing in the
package sniffs the stream. The comment there describes an intent that was never implemented:

```go
// Most layers are gzipped; the media type says so, but some registries are
// loose about it, so the magic bytes decide.
```

The plan is to decide by magic bytes instead. Some registries' media types do not match the actual
content, and trusting the media type ends in unpacking a gzip stream as tar (an immediate failure)
or the reverse. Sniffing would be defensive at the cost of reading a few bytes. Whether to do it is
a separate decision and is not taken here.

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
  (decisions §3.1). **Now wired in** — see §7

Image-affinity scheduling is the other face of the same problem: repeated eval runs on the
same image should land on nodes that already have it. That part is shipped (`Cached()` →
heartbeat → scheduling score).

## 5. Image configuration: ENV / ENTRYPOINT / CMD / WORKDIR ✅

An image is two things: layers, and a configuration blob describing how to start them.
The conversion above handles the first and **drops the second** — flattening layers into an
ext4 carries no metadata. So the config has to travel by a separate route, and without one
an image that depends on its own `ENV` or `WORKDIR` starts wrong with no error anywhere.

The route has two halves, because the registry is reachable at conversion time and not at
create time:

```
convert  FetchConfig(manifest.Config.Digest)  → Config{Env,Entrypoint,Cmd,WorkingDir,User}
             │                                    written into the .ref sidecar
             ▼
create   Provider.Config(ref) → *Config    ┐
         spec.Cmd / spec.Env               ├→ MergeConfig → Process{Argv,Env,Workdir,User}
                                           ┘        │
                                    StartUserProcessRequest → beand exec
```

Recorded in the same `.ref` sidecar as the reference and digest rather than a second file, so
one atomic write publishes everything the node knows about an image and no reader can catch
the two disagreeing.

### Field-by-field correspondence

| OCI config | Where it goes | Status |
|---|---|---|
| `Env` | merged under the request's env, per key | ✅ |
| `Entrypoint` | head of `Argv`, always preserved | ✅ |
| `Cmd` | tail of `Argv`, replaced by the request's cmd if it has one | ✅ |
| `WorkingDir` | `Workdir`, used when the request names none | ✅ |
| `User` | carried on `Process`, **not applied** | 📐 |
| `VOLUME` / `EXPOSE` / `HEALTHCHECK` | ignored / metadata / not executed | ➖ same as a container runtime |

### The merge rules, and the one that is easy to get wrong

```
Argv     = Entrypoint ++ (request.Cmd if non-empty else image.Cmd)
Env      = image.Env, then request.Env overriding per key
Workdir  = request.Workdir, else image.WorkingDir, else the agent's default
```

**A request's command replaces `Cmd` and leaves `Entrypoint` alone.** This is what makes
`docker run python:3.12 -c 'print(1)'` pass an argument to the interpreter rather than trying
to exec `-c`. Overriding both together looks right for every image whose `Entrypoint` is empty
— which is most of what anyone tests with — and breaks precisely the images that declare one.
The table test in `config_test.go` pins this case specifically.

Env merges per key rather than wholesale for a related reason: an image's `PATH` and a caller's
one extra variable both have to survive, and a caller cannot be expected to restate the image's
environment in order to add to it.

### Where a config comes from, by image source

| Source | Config | Why |
|---|---|---|
| registry pull | from the config blob | fetched during conversion |
| `commit` | carried forward from the source image | committing changes a filesystem, not the way the environment starts |
| `build` | **none** 📐 | buildctl is asked for `type=tar`, a flat rootfs with no image metadata; capturing the Dockerfile's `ENV`/`ENTRYPOINT` means exporting an OCI image from the builder instead |

An absent config reads back as `nil`, never as an empty `Config`, and a `nil` means "start from
the request alone" — which is what every image did before configs were recorded. Distinguishing
the two matters: an empty `Config` would claim the image genuinely declares no entrypoint.

### `User` is recorded but not enforced 📐

The value is stored and reaches `Process`, and everything runs as root regardless. It cannot be
applied where the rest of this is applied: beand is PID 1, so lowering its own uid would cost it
the ability to exec anything afterwards — it has to happen in the child. Resolving a name like
`nobody` also needs the guest's `/etc/passwd`, which only exists after the pivot to the image's
rootfs. So this is a separate change rather than a missing line.

## 6. commit: the reverse path ✅

`commit` seals the filesystem of a running sandbox into a new base image.

The current implementation **reads a complete ext4 out of the composite device under
`/dev/mapper`** rather than "sealing an incremental layer". The reason is that dm-snapshot's
CoW layer is not in OCI layer format and cannot be used as a layer directly.

The cost: a commit produces a full image rather than an increment. Once overlaybd is wired in
this can become `overlaybd-commit` sealing the LSMT writable layer — that is the genuinely
zero-conversion form (image-build §2).

## 7. The overlaybd path ✅

An alternative to flattening, selected with `--fc-overlaybd`. Off by default; the
dm-snapshot path above is still what a node uses unless asked otherwise.

| | dm-snapshot (default) | overlaybd |
|---|---|---|
| First use | pull the whole thing + convert, minutes | read blocks on demand, 7ms to mount |
| Per-sandbox cost | 44 KiB (measured) | comparable, both store only changes |
| Layer sharing | **none** — each image is its own ext4 | shared by digest, one copy per layer |
| Conversion CPU | paid per image, including for shared layers | paid once per distinct layer |
| commit | read out a full ext4 | seal the writable layer in place |

The gain that made this worth building is not the one originally written down here. The
earlier note said the value was first-use latency and that prewarm shadows it — true, but
it omits the two that prewarm does **not** shadow: layers shared across images are stored
once instead of once per image (3.1x less disk for a SWE-bench-shaped set, measured with
`hack/layer-amplification.go`), and the CPU to convert a shared base is paid once per node
rather than once per image. For a set of 2000 task images on one base, the flattening path
gunzips and rewrites that base 2000 times.

### The layer pipeline

```
registry layer (tar.gz)
  │  decompress -- overlaybd-apply reads tar, not tar.gz
  ▼
overlaybd-create --mkfs data index <GB>     empty layer with a filesystem
overlaybd-apply  layer.tar apply.json      write the tar into it
overlaybd-commit -z -t data index out      seal to a zfile blob
  ▼
<layerDir>/sha256-<digest>.obd             named by OCI digest, so shared
```

Named by digest rather than by image: that is the whole mechanism behind layer sharing,
and it means a second image referencing a layer already here converts nothing.

A per-sandbox writable layer is `overlaybd-create -s` (sparse), costing the blocks written
rather than its virtual size — 40 KiB measured for an idle sandbox against a 1.1 GB
apparent size.

### Exposing it as a block device

overlaybd has no block-device interface of its own; the kernel's SCSI target subsystem
provides one, driven entirely by writing files under configfs. The order is not
stylistic:

```
1. mkdir  core/user_999/<name>              create the TCMU backstore
2. write  control = dev_config=overlaybd/<config.json>,dev_size=N
3. write  wwn/vpd_unit_serial = <hex>       BEFORE enable, see below
4. write  enable = 1
5. mkdir  loopback/<wwn>/tpgt_1
6. write  tpgt_1/nexus = <wwn>              MUST precede the LUN
7. mkdir  tpgt_1/lun/lun_0
8. symlink lun_0/virtual_scsi_port -> the backstore
```

### Four constraints, each learned from hardware

None of these produce an error message that names the cause, which is why they are
enforced in code with the reasoning attached rather than left to documentation.

**1. The nexus must precede the LUN.** The SCSI host scans for LUNs when the fabric
registers, which happens on the nexus write; a LUN linked afterwards is never scanned and
writing the nexus later does not trigger a rescan. Verified: with the wrong order, no
device appears while configfs reports `enable=1` and `Status: ACTIVATED`, and overlaybd's
own result file says success. Nothing reports a problem.

**2. Every backstore needs a unique unit serial, written before `enable=1`.** TCMU
provides none by default, so devices report WWID `36001405` followed by zeros. multipathd
sees identical WWIDs, concludes they are two paths to one LUN, and merges them —
**serving one sandbox another's data**, with the original device then reporting "already
mounted or mount point busy". Reproduced live: a serial-less device was merged into
`mpatha` while a serialled one stayed distinct.

**3. The serial must be hex digits only.** The kernel builds the WWID from the
*hex-digit characters* of the serial and discards the rest: `bean-aaa` became
`naa.6001405beaaaa000...`. So `bean-sbx-alpha` and `bean-probe-2` both reduce to `beabaa`
— two serials that look unique and collide, which is constraint 2 all over again but
harder to see. `deviceSerial` hashes the sandbox id into hex, and `attachTCMU` **refuses**
a non-hex serial rather than sanitising one, so the value that reaches the kernel is the
value the caller chose.

**4. A lower layer must be sealed.** Handed a freshly applied (unsealed) layer, the
daemon fails with `trailer magic, trailer type, file type or sealedness doesn't match` and
leaves the backstore DEACTIVATED — which reaches the caller only as `ENOENT` on the
`enable` write, with the real reason in `/var/log/overlaybd.log`. Relatedly, the throwaway
config handed to `overlaybd-apply` while *building* a layer must have **empty** lowers:
naming the layer as its own lower asks overlaybd to open one file as both read-only parent
and writable target, and fails with only `failed to create image file`.

Two further notes on details that look like they should work and do not. Teardown does
**not** write `enable=0` — the kernel rejects it (`For dev_enable ops, only valid value
is 1`), so a backstore is removed by removing its directory. And finding a LUN's block
device goes by WWID, because `udev_path` stays empty, the SCSI model reads `TCMUdevice`
for every such device, and `statistics/scsi_port/dev` is a global port counter rather than
the tcm_loop adapter number (a LUN reporting 26 was served by `tcm_loop_adapter_24`).

### What is verified, and what is not

`internal/node/image/overlaybd_hw_linux_test.go` runs against real binaries, real
configfs and real block devices, skipping where the host cannot support it. On the
verification host (kernel 5.15, TCMU, multipathd active) it builds and seals a layer,
attaches it, **mounts the device and reads back the file the layer was built from**, and
confirms two devices get distinct WWIDs.

Not yet exercised: lazy pull against a registry (`--fc-overlaybd-lazy-pull` is
implemented and untested — the measured 7 ms mount and 19.6% transfer come from the
earlier manual verification in decisions §3.1, not from this code), `commit` through
`CommitSandbox`, and behaviour under concurrent fan-out. ublk would be a faster backend
than TCMU but needs kernel ≥ 6.0; TCMU is functionally complete.

## 8. What does not exist yet 📐

- **Cache reclamation**: base images in `ImageDir` are never cleaned up automatically. The
  design has image-granularity LRU + chunk LRU (noded-design §4.2) with zero implementation.
  Running long enough will fill the disk
- **Uploading build outputs to S3**: a built image lands on the node's local disk, so it is
  **only usable on the node that built it** (GitHub #22). In a multi-node cluster that is
  essentially unusable
- **digest verification**: layer digests are not verified on pull. Under the premise that the
  registry is trusted this is not a big problem, but it is one link in supply-chain protection

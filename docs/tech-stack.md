# The stack, and why each piece is in it

> 中文版:[zh/tech-stack.md](zh/tech-stack.md)

> Section status convention: [architecture.md](architecture.md) §0.
> **Authority order: code > [status.md](status.md) > [decisions.md](decisions.md) > design docs.**

This is a survey of every technology `bean` depends on, what it does here, and
what was rejected to get to it. Where a choice has measured data behind it the
number is quoted; where it is a judgment call with no measurement, it says so
rather than borrowing authority it does not have.

Two things this document is not. It is not a claim of delivery — the markers
distinguish what runs from what is designed, and the network stack, the jailer,
the container tiers and Postgres are all in the second group. And it is not a
list of libraries: `go.mod` has **four direct requires**, and most of the
interesting choices below are decisions *not* to take a dependency.

Measurement host throughout: AMD EPYC 7542 (Zen 2), 16 physical cores, 24 GB,
guest kernel 6.1.102, Alpine 3.20.

---

## 1. Isolation: Firecracker microVMs ✅

Firecracker is the VMM. `internal/node/runtime/fc_linux.go` spawns one process
per sandbox and drives it over its Unix-socket HTTP API — a config file could
describe the initial machine, but pause, resume and snapshot are only reachable
through the API, so one client covers the whole lifecycle (`fc_api.go`).

The workload decides this. Sandboxes run AI-generated code from an evaluation
harness, which is untrusted by construction, and the cheapest correct answer to
"untrusted code" is a hardware virtualisation boundary rather than a shared
kernel. On top of that Firecracker natively supports the two things the platform
is organised around: memory snapshots and on-demand page serving.

**What was rejected:**

- **Plain containers (runc, or a Pod per task).** One shared kernel, so a
  container escape is a host compromise. The boundary is a syscall filter, not a
  hardware one. For a harness whose whole purpose is running code nobody
  reviewed, that is the wrong default.
- **gVisor (runsc).** It is kept in the design as the fallback for hosts without
  `/dev/kvm` (architecture D3), not as the main tier. A syscall emulation layer
  is a compatibility surface, and eval images are arbitrary — anything that
  builds a kernel module, uses an unusual syscall, or probes `/proc` in an
  uncommon way becomes a support question. A real guest kernel has no such
  surface. **📐 Unimplemented**: there is no runsc runtime in the repo.
- **Kata Containers.** Superseded by driving Firecracker directly. Kata's value
  is a CRI-compatible VM runtime; the platform does not speak CRI and does not
  want containerd on the hot path, so Kata would be a layer that only
  translates.
- **QEMU.** Feature-complete and therefore large: a full device model, a much
  bigger attack surface, and boot time measured in seconds rather than hundreds
  of milliseconds. Firecracker exists precisely because that trade is wrong for
  short-lived sandboxes. There is no head-to-head measurement here — this one
  rests on Firecracker's published rationale rather than our own data.

**The isolation that is actually there, stated honestly** (⚠️): the VMM runs as
root in the host mount namespace, with only Firecracker's built-in seccomp.
**The jailer and the host-side cgroup wrapper are unimplemented** (GitHub #20).
The hardware boundary holds, but defence in depth is thinner than it should be:
a Firecracker or KVM escape lands on host root rather than on a chrooted,
deprivileged user.

**A third tier exists and is not isolation at all.** `LocalRuntime`
(`internal/node/runtime/local.go`) runs the sandbox as a host process tree with
the real `beand` binary confined to a directory. It exists so that development
and CI on macOS exercise the same agent gRPC surface without KVM. It is not a
security boundary and is not offered to callers as one.

### The cost of the boundary, measured

A create is **952 ms** to a reachable agent (234 ms runtime + 770 ms agent
wait), and under load each `firecracker` process burns **5 CPU-seconds** to
boot, then nothing. During a 16-way burst `vmstat` reported `r=16 / id=0 / wa=0`
on 16 cores. So **throughput ≈ cores ÷ 5 CPU-seconds ≈ 2.3 creates/s**, and it
is guest boot, not our code: `runtime_create` (device-mapper assembly plus VMM
spawn) only moves 241 ms → 369 ms between concurrency 1 and 16, while
`agent_ready` goes 627 ms → 5710 ms.

That number is the reason snapshot restore matters. Restoring skips kernel init
entirely, which is the only way around those 5 CPU-seconds short of trimming the
guest kernel.

---

## 2. Rootfs: device-mapper snapshots ✅

Every sandbox needs a writable root filesystem derived from an image. The live
path is device-mapper's `snapshot` target: one read-only loop device per image,
shared by every sandbox on the node, plus a sparse copy-on-write store per
sandbox (`internal/node/image/devmapper_linux.go`). The table is
`0 <base_sectors> snapshot <base_loop> <cow_loop> P 8` — `P` for persistent, so
exceptions land in the CoW's metadata area and the device can be torn down and
reassembled, which is the precondition for capturing the CoW layer in a snapshot
and replaying it elsewhere; `8` sectors of chunk size, i.e. 4 KiB, so a single
block write copies 4 KiB instead of tens.

**Measured: 44 KiB of actual disk per sandbox** against a 20 GiB nominal
request. (Earlier revisions quoted 8 KiB in some places and 80 KiB in a code
comment; those measured at different points in a sandbox's life — an empty CoW
layer versus one after the sandbox has run and written. 44 KiB is the figure to
use, and the order of magnitude is the point — `status.md` has the detail.)
Fanning out a hundred clones of one image
costs a hundred sparse files, which is the batch-evaluation case the platform
exists for.

The providers are layered rather than fused: `PullingProvider` wraps an inner
block-device backend and owns "fetch on first use" with concurrent
deduplication, `DevMapperProvider` or `FileProvider` owns "how the device is
assembled." Where the image comes from and how the device is built are separate
concerns, so a new backend does not reimplement the pull.

**What was rejected:**

- **A full copy per sandbox.** That is `FileProvider`, kept only as a fallback
  for hosts without `dm_snapshot`. A 512 MiB image costs 512 MiB of disk and the
  time to write it, per sandbox. At a hundred clones this is the difference
  between a hundred sparse files and 50 GB.
- **overlayfs.** Filesystem-level union, so what it produces is a directory
  tree, not a block device. A microVM needs a block device on virtio-blk; the
  alternative is virtiofs, which Firecracker supports weakly. Keeping the
  composition at block level means one image path serves both the microVM tier
  and (in the design) the container tier.
- **dm-thin.** More capable — real per-device sizing, real quotas — but its
  failure modes are worse than dm-snapshot's here. Under the kernel default
  `queue_if_no_space` a guest that fills the pool **hangs**, and metadata
  exhaustion needs offline `thin_check`/`thin_repair` where adding data space
  does not repair anything. Measured, dm-snapshot fails harder but more
  legibly (§7 below).
- **TCMU/SCSI per sandbox.** A whole SCSI fabric (loopback nexus) per sandbox:
  fragile and slow, against `dm_snapshot` needing one kernel module.

### overlaybd: wired in behind a flag, not the default ⚠️

This one deserves its own explanation, because the ordering that kept it out of the
live path for a while was deliberate rather than an oversight — and the reasoning
below was also **wrong about where the value is**, which is worth preserving rather
than quietly editing.

overlaybd (DADI, Alibaba) is block-level lazy-pull: layers are block-device
diffs in a registry, and a mount range-reads only the blocks touched. Measured
on 2026-08-02 (Ubuntu 20.04 / kernel 5.15 / tcmu backend / alpine 3.20):

```
mount time                       7 ms
mount + read /etc/os-release     1014 KiB / 5175 KiB = 19.6% of the layer
read the entire filesystem       1270 KiB (zfile compression)
registry responses               8 × HTTP 206 Partial Content
writable upper layer, actual     40 KiB (1.1 GB nominal, genuinely sparse)
```

The overlaybd log's `__open_ro_remote` confirms it opens an HTTP URL rather than
a local file. Ready in 25 ms with no full-layer download.

**Why it was not the live path, and where that reasoning was wrong.** The argument
used to be that **overlaybd's value is wait time on first use of a large image, not
per-sandbox cost — CoW already solved the latter at 44 KiB**, making it an
optimisation on the cold path rather than infrastructure, ranked below snapshot
capability accordingly.

The ranking was right and the stated reason was not. Measuring the implementation
(`hack/overlaybd-bench.sh`) showed the cold path **unchanged** — this backend still
downloads and converts every layer before assembling a device, so a first use does
strictly more work than flattening. The real wins are **conversion CPU and disk**:
three images sharing a base go from 392 MiB to 118 MiB, and the shared base is
converted once per node instead of once per image (2.2 s falling to 0.49 s and 0.44 s).
For a SWE-bench-shaped set of 2000 images on one base, the flattening path rewrites
that base 2000 times, and prewarm cannot hide it — prewarm pays the cost earlier, not
less. The other win is concurrency: at 256 simultaneous creates it is 4.2× faster on
rootfs setup, because dm-snapshot forks `losetup`/`dmsetup` per sandbox while attaching
a TCMU device is configfs writes with no fork.

`OverlaybdProvider` now exists behind the same `Provider` interface, enabled with
`--fc-overlaybd`, with dm-snapshot still the default. Two traps from the verification
are encoded in that code, because they will reproduce in production and documentation
will not remember them:

1. **The LUN must be linked after the nexus.** Wrong order and the kernel says
   `TCM_Loop I_T Nexus does not exist` — the SCSI host scans for LUNs at
   registration time when the nexus is still empty, and writing the nexus
   afterwards triggers no rescan. The device never appears while configfs looks
   entirely healthy. Correct order: backstore → tpgt → nexus → LUN link.
2. **`wwn/vpd_unit_serial` must be set per backstore.** TCMU supplies no unique
   serial, so two overlaybd devices with completely different contents both get
   WWID `36001405` + zeros, and `multipathd` merges them into one `mpatha`. The
   symptom is not an error, it is **reading another image's data**, plus a busy
   device that will not mount directly.

The tcmu backend is functionally complete, so **the host kernel did not need upgrading
first**. But "ublk (≥ 6.0) is only faster" turned out to understate it: tcmu takes 4.0 s
to tear down 128 devices and does so identically on 5.15 and 6.8, because the daemon
serialises on one netlink socket. That is a property of the transport, not of the kernel,
so ublk became the intended replacement rather than an optimisation ([status.md](status.md)).

overlaybd also changes the layer story: the dm-snapshot path flattens the image into a
single ext4 and loses the layer structure, so `commit` reads out a full image rather than
sealing an incremental layer. With overlaybd, `overlaybd-commit` seals the LSMT writable
layer and the promise of zero conversion becomes literal — though `CommitSandbox` on this
backend is implemented and still unexercised.

**Nydus was rejected for the same reason overlayfs was**: it is file-level, its
filesystem semantics cannot get into a microVM, and the fc tier would need
virtiofs. It is kept as a fallback for the (unimplemented) container tier.

---

## 3. Snapshot and restore

> Everything in this section is **restore** — building a new sandbox from a snapshot
> on disk. Resume, which unfreezes the vCPUs of a process that never stopped running,
> shares none of this machinery. See
> [snapshot-resume.md](snapshot-resume.md) §0.

### 3.1 UFFD (userfaultfd) for memory restore ✅

Firecracker offers two memory backends at `/snapshot/load`. `File` reads the
whole memory image before the VM runs; `Uffd` maps guest memory anonymously and
asks a handler process for pages as the guest faults them in.

**Measured, restoring a 512 MiB guest:**

```
restore total     1400 ms
├─ restore_load   1303 ms   ← pull blob + gunzip + write memory/rootfs to disk
└─ agent wait       97 ms   ← memory already restored, process still alive
```

Against a 1040 ms cold boot, **the restore was slower than booting from
scratch**, and 93 % of it was `restore_load`. With `Uffd`, `/snapshot/load` went
**1303 ms → 7 ms**, and a restore writes nothing to disk. The cost scales with
pages the guest actually touches rather than with guest size.

`File` was rejected on that measurement. Two other options were rejected on
analysis:

- **Cache the unpacked memory file, keyed by snapshot id.** This removes repeated
  decompression but the first restore still writes 512 MB and it consumes disk
  permanently. It was the original plan; UFFD removes the cost outright. The two
  do not conflict, and the system now has both (§3.2).
- **Pool restore-ready VMs.** Every pool member holds a copy of memory, and the
  measurement shows the bottleneck is unpacking and writing rather than the VM
  restore itself — the agent only waits 97 ms. Pooling would spend memory to
  solve something that was not the problem.

UFFD is also consensus rather than a bet: e2b ships a complete handler
(`packages/orchestrator/pkg/sandbox/uffd/`, with cgo), agentenv ships
`storage/uffd-core/` in Rust, and tensorlake publishes sub-second cold starts.

**Sharing is safe because Firecracker maps the memory file `MAP_PRIVATE`.**
Verified rather than assumed: after writing 64 MB of random data inside the
guest, the md5 of the host's memory file is unchanged. That is what lets one
unpacked memory image serve arbitrarily many restores.

**Two protocol details that only appear when you run it** (both in
`internal/node/runtime/uffd_linux.go`):

1. **The fd and the region layout are not necessarily in the same datagram.** One
   `ReadMsgUnix` can return the fd with an empty body → the JSON parse fails →
   the handler dies → Firecracker blocks forever on the first page fault. The
   read must loop until both have arrived. agentenv's Rust implementation loops
   too.
2. **The fd Firecracker hands over is non-blocking.** A direct `read` returns
   `EAGAIN` immediately and the fault loop exits on the spot. It must `poll` for
   readability first.

Both mistakes present as "`snapshot/load` hangs forever", which is
indistinguishable from the handler having crashed — which is why the handler
carries an `Err()` channel. From Firecracker's own documentation: if the handler
dies, **the VM hangs on the next fault**, so liveness monitoring is mandatory.
The balloon's `MADV_DONTNEED` raises `UFFD_EVENT_REMOVE`, and the handler must
zero those pages rather than re-read the file, or it resurrects stale data.

### 3.2 The unpack cache, and why the rootfs is excluded ✅

With load down to 7 ms, the remaining ~1060 ms was all unpack (gunzip plus
writing 512 MB). Unpacking the same snapshot is byte-identical every time, so it
is cached by snapshot id (`snapcache_linux.go`): **first restore 1617 ms, later
restores ~950 ms.**

**The writable rootfs is deliberately not cached.** Two sandboxes restored from
one snapshot diverge on their first write, so each needs its own device. It is
affordable because it is a sparse extent list and a fresh sandbox has written
almost nothing — that asymmetry is exactly what makes "share the memory, split
the rootfs" work.

Cache eviction is high/low watermarks with LRU, accounted by **allocated blocks
(`st_blocks * 512`), not nominal size** — a merged memory image is sparse
wherever no ancestor wrote, and accounting nominally would evict entries to
reclaim zero bytes. The pair of watermarks is copied from kubelet's image GC:
one threshold makes every restore after the trigger pay for reclamation, a pair
keeps it an occasional batch. Measured at a 600/300 MiB watermark, six restores
of different snapshots went from **4.83 GB / 9 entries to 537 MB / 1 entry**, and
after `drop_caches` every sandbox still read back its own marker.

One belief here was measured and found wrong, which is worth recording: the
assumed risk was "an entry currently mmapped by UFFD must not be deleted." A C
program disproved it — mmap a file, unlink it, read every byte, data intact,
because the inode survives until the last mapping goes. The real window is
narrower: a restore `Lookup`s the path and only later opens the image, so an
unlink between those two points gives `ENOENT` after that restore's stream has
already been consumed, with nothing left to rebuild from. The pin therefore spans
only `stageSnapshot` → `loadSnapshot`.

### 3.3 Diff snapshots, materialised at restore ✅

Firecracker supports diff snapshots natively; the platform exposes them as
`--base SNAP`, measured at **298 KB against a 15.5 MB full snapshot (52×)**, with
depth 2 verified to restore all three expected files and `uptime 57` proving it
resumed rather than rebooted.

A Firecracker diff memory file is **not self-contained** — it is sparse and must
be layered onto a base. So the real question is not how to produce a diff but
**when and where to merge**, and the industry split on exactly this:

- **E2B** does layered lookup at fault time: the UFFD handler goes through
  `block.Slicer` over base plus each layer, so after K pause/resume cycles one
  read chases K `BuildId` references. No cap on depth, only `NormalizeMappings`
  merging adjacent segments from the same build. Their own public analysis states
  that cross-build fragmentation grows over time, with read amplification
  proportional to depth.
- **Cognition's blockdiff** keeps the chain as lineage only and flattens to raw
  before running. `apply` is pure metadata via XFS reflink — a 128 GB
  `cp --reflink=always` measured **0.008 s against 24.5 s**. Their flatten is
  essentially free, which is why their write-up never discusses read
  amplification: at run time there is no chain to walk.
- **Firecracker upstream** ships `snapshot-editor edit-memory rebase`, which is
  flatten, and requires layering in creation order.

**We flatten, and the reason is more than following the majority.** There is a
structural advantage E2B does not have: `snapCache` already caches unpacked
results by snapshot id, so the merge is paid **once per leaf per node** and every
later restore on that node reuses it. Fan-out is precisely "the same leaf
restored many times," so the merge amortises to nothing on the case diffs exist
for.

The stronger reason is that **the page-fault path does not change at all**.
`fill()` is the hottest and most insidious code in the system — a mistake there
is one page of wrong memory with no error signal of any kind. Flattening keeps
`uffd_linux.go` serving one flat image, the same code the full-snapshot path has
always used.

**Chain depth is capped at 8**, past which a checkpoint silently becomes full.
E2B setting no limit and taking on growing fragmentation is the evidence for
setting one. It bounds restore cost, lets ancestors be reclaimed, and means
callers never reason about depth: a diff request always succeeds, occasionally
more expensively.

Three things here must not be silent, and are not:

1. `track_dirty_pages` must be set before boot and is not stored in the snapshot,
   so it is node configuration (`--track-dirty-pages`, **off by default** because
   its overhead has never been measured). A guest without it that requests a diff
   gets an **explicit error**, never a downgrade to full — the caller would
   believe they saved space, and size alone does not explain why.
2. Diff memory uses a **separate member name** `memory.diff` rather than a flag on
   `memory`. Confusing the two is bad in both directions: layering a full as a
   diff wipes pages the base never touched, loading a diff as a full hands the
   guest memory full of holes. Dispatching on the member name makes both
   impossible.
3. Deleting a base with descendants is refused (409). Otherwise the failure is
   distant in time and space: the delete succeeds now and a restore fails later
   on another machine.

**Order is the caller's contract and is not recoverable from the data** — later
layers legitimately overwrite earlier ones, so a reversed chain yields an image
that is structurally intact but assembled from stale pages, which nothing
downstream can detect. So `store.SnapshotChain()` fixes the order once and the
chain is declared in the spec rather than discovered from the stream.

### 3.4 Ordering: the CoW must be seeded before device assembly ✅

Not a technology choice but the reason to trust the ones above, so it belongs
here. dm-snapshot reads the exception table into kernel memory at `dmsetup
create` and never reads it back. Writing into the CoW backing store of an
**already active** device leaves the kernel unaware of those chunks, and the
device keeps serving the base image.

The original restore did exactly that, and **on a full snapshot the bug is
completely silent:**

```
read immediately after restore:  cat /root/marker  →  survives   ← page cache from the memory snapshot
after drop_caches:               cat /root/marker  →  9 × \0     ← actually reads the block device
                                 ls -la            →  size = 9   ← metadata is in memory, and correct
                                 dmesg             →  nothing at all
```

Metadata lives in the memory image, data lives on the block device, the two
disagree, and ext4 has no reason to be suspicious. The fix is a
`PrepareOptions.SeedWritable` callback the provider invokes between "CoW created"
and "device assembled," which forced restore to land the bundle in a staging
directory first and decode the extent stream exactly once, when it is written
into the device.

Nobody else writes into the CoW of an active device: firecracker-containerd's
devmapper snapshotter derives from a thin pool and activates afterwards, so the
ordering is inherently right; Lambda SnapStart supplies a chunked lazily loaded
block device; E2B's rootfs is a host file with CoW at the filesystem layer.
Firecracker's upstream documentation simply makes disk state the caller's
problem — this is the class of problem it was warning about.

### 3.5 CPU templates ✅

A guest reads CPUID once at boot and caches the answer — glibc picks its string
routines from it. Restoring that guest on a host lacking a feature does **not**
fail at restore; it faults later, inside whatever runs next. So masking is only
effective before boot and cannot be retrofitted at snapshot time.

**Firecracker's five built-in static templates were rejected on measurement.** On
the verification host (EPYC 7542, family 23) **none of them will even start:**

```
T2 / C3 / T2S / T2CL  →  "CPU vendor mismatched"            (all Intel-only)
T2A                   →  "current CPU model is not permitted" (Milan/Zen 3 only)
```

Worth noting how this hides: `PUT /machine-config` returns success for **every**
template name, and vendor validation happens at `InstanceStart`. Testing
configuration alone yields the false conclusion that all five are supported.

So: a custom template through `/cpu-config`. That also detaches the platform's
portability story from whichever CPU models AWS chose to support. Two details
only real hardware revealed:

- **The bitmap width is 31, not 32.** 32 bits reports `string is too long`. Unit
  tests all passed at 32; the first create on real hardware failed. The
  consequence is that **bit 31 cannot be masked**, and `avx512vl` sits there — so
  it is listed in `UnmaskableCPUFeatures` and written to the startup log rather
  than falsely claimed as masked.
- **Do not mask xsave.** Masking leaf 1 ECX bit 26 does hide `xsave`, but the
  XSAVE sub-features live in leaf 0xD and stay visible, so the guest would see a
  CPUID matching no real processor. Every host capable of running Firecracker has
  xsave anyway.

**Vendor and family cannot be masked** — leaf 0 carries the vendor string and the
guest kernel branches on it for errata and MSR access. So a template buys
portability *within* a vendor and family, and crossing that boundary has to be
refused by the scheduler (`409 INCOMPATIBLE_CPU`) rather than placed and left to
misbehave. **Model is deliberately not recorded**: masking instruction-set
features is what makes a snapshot usable across models, so matching on model
would erase the template's entire value.

`hack/cpu-template-probe.sh` freezes this probing into a script and exits 70 when
it disagrees with `cpuBitmapWidth` in the code. **It must be re-run on every new
machine** — these answers are per-host and failure is silent, since a rejected
`/cpu-config` leaves the guest with no mask at all. It also exposed a boundary of
the verification: this host has no AVX-512, so the five avx512 bits in the mask
table have never been exercised; only `avx avx2 fma f16c` are measured as masked.

---

## 4. The guest

### 4.1 Kernel: Firecracker's CI prebuilt, no fork, no build pipeline ✅

`hack/build-assets.sh kernel` downloads
`firecracker-ci/v1.11/x86_64/vmlinux-6.1.102` and keeps the published `.config`
alongside it, so "use the prebuilt" and "have our own config in hand" are not
mutually exclusive. It verifies the download is an ELF, because that bucket has
been seen to serve truncated files and a short kernel presents as "boot hangs,"
not as a download error.

**What the survey found:**

| repo | contents | forked |
|---|---|---|
| `e2b-dev/firecracker` | VMM source | **yes** (added a gdb feature, among others) |
| `e2b-dev/fc-versions` | pipeline that builds the VMM | no |
| `e2b-dev/fc-kernels` | kernel config + patch + build.sh | **no** |

`fc-kernels` clones `amazonlinux/linux` at run time — the same source
Firecracker's own `rebuild.sh` uses — and the repo holds only a config plus one
virtio_balloon patch. So **e2b's kernel maintenance surface is one config file,
with no rebase burden**, and that is the surface worth copying. e2b forked the
VMM but not the kernel; we fork neither.

Building our own was rejected on cost-before-evidence grounds: a container build
means paying for a toolchain, a source fetch and a 20-minute build before getting
the first data point, and at the time it was not established that changing the
kernel helped at all.

**Then measured** (quiet, VMM start to agent connectable, three runs each):

```
vmlinux-6.1.175   690 / 689 / 715 ms   (from the agentenv R2 site, config unknown)
vmlinux-6.1.102   603 / 613 / 601 ms   (Firecracker CI, config known)
```

~90 ms (13 %), taking end-to-end create from 1040 ms to 952 ms, with
snapshot/restore unaffected. **But note where the gain is not coming from**: in
the CI config `CONFIG_SCSI_ISCSI_ATTRS`, `CONFIG_BPFILTER`, `CONFIG_SQUASHFS`,
`CONFIG_XFS_FS` and `CONFIG_NFS_FS` are all `=y`. The CI kernel did not trim
those either; the difference is mostly image size (40.8 MB vs 44.5 MB) and the
version. **So the ceiling on a hand-trimmed config is lower than it looks** —
gains on the order of `quiet` (−493 ms) or the gRPC backoff (−800 ms) are not
hiding in kernel trimming. No compile pipeline, and the config is in hand if that
changes.

### 4.2 Boot arguments ✅

```
quiet reboot=k panic=-1 pci=off init=/bean/beand -- --listen vsock:1024 --pivot /dev/vdb
```

`quiet` is the measured one. **Dropping the serial console saves 493 ms (41 %)**:

```
console=ttyS0    1193 / 1195 / 1210 ms
quiet             700 /  700 /  711 ms
```

8250 UART writes are synchronous — the kernel waits on hardware for every line it
logs. The rest: `reboot=k` because Firecracker has no ACPI and keyboard reset is
the minimal working path; `panic=-1` so a crashed guest stays inspectable instead
of entering a reboot loop; `pci=off` because there is no PCI bus to enumerate.

**The trade is taken the way e2b takes it**: their `fc-kernels` config has
`CONFIG_SERIAL_8250=y` but their boot args carry no `console=`, so one kernel both
boots fast and debugs. Here `--debug-console` adds `console=ttyS0` back. A failed
boot has no other source of evidence, so that capability cannot be given up — but
it should not be paid for on every boot. **The cost is real and has to be said
plainly**: with the console off by default, anything the guest writes to stderr,
including the agent's log line carrying the trace id, is invisible.

### 4.3 The agent as PID 1 on its own disk ✅

`beand` is a statically linked Go binary (`CGO_ENABLED=0`, `-ldflags="-s -w"`) on
a 32 MiB read-only ext4 image, attached as the guest's **root** device:

```
/drives/agent   agent.ext4      IsRootDevice: true,  ReadOnly: true   → /dev/vda
/drives/rootfs  <cow device>    IsRootDevice: false                   → /dev/vdb
```

The kernel execs init from whatever it mounted as root, so putting the agent
there means **the user image carries no obligation whatsoever** — no embedded
`beand`, no init system, no modified entrypoint. The agent then pivots to
`/dev/vdb` itself. That is the whole of "zero image conversion" on the agent side.

**Firecracker names drives in attachment order** and `--pivot /dev/vdb` is
constant, so the agent disk must be registered first. Registering them backwards
shows up as a guest mount failure with, under the default no-console config,
**no output at all** — which is the concrete reason `--debug-console` exists.

The disk is symlinked into each sandbox directory rather than copied: one inode,
zero copies, and it lets the drive path be relative.

**All paths are relative**, with the VMM's working directory set to the sandbox's
own directory. This is not tidiness, it is snapshot portability: Firecracker
stores device paths and the vsock UDS path **inside machine state** and re-resolves
them at load, and refuses to let the vsock path be overridden at load. Absolute
paths would send a restored VM looking for the *source* sandbox's files, which may
be destroyed. Relative paths resolve to whichever sandbox directory the VMM was
started in. Cross-machine restore therefore falls out of one decision — cwd plus
relative paths — with no path-rewriting logic anywhere.

Rejected: **CRI streaming exec** for the exec/PTY surface. Poor performance, no
file API, and a long chain of components to depend on.

### 4.4 vsock as the control channel ✅

A microVM has no host-reachable network before the guest configures one, and
giving every sandbox a tap device just so the host can talk to its agent would
make the control path depend on host networking. `AF_VSOCK` exists as soon as the
VM boots, so the agent is reachable during early boot and stays reachable if guest
networking is broken or absent — which, today, it always is.

Both identifiers are constants: `agentVsockPort = 1024`, `guestCID = 3`. Neither
needs allocating, because **each VM has its own vsock namespace**, so there is
nothing to collide with (CID 3 is the lowest guest value; 0–2 are reserved).
Constants also keep the guest cmdline independent of host state, so it is
identical across a snapshot boundary — one fewer thing to reconcile at restore.

The same `AgentService` gRPC surface is designed to run over a unix socket on the
container tier, which is why transport is abstracted rather than assumed
(`internal/node/vsock/`).

---

## 5. Language and runtime: Go ✅

Four binaries, one language: `bean` (CLI), `bean-api` (gateway with scheduler,
image and snapshot modules embedded), `noded` (node daemon), `beand`
(in-sandbox agent). Go 1.26.1.

The reasons are specific rather than general. `beand` ships on a disk attached to
**every** microVM, so its size is paid per boot and a static binary with no libc
dependency is exactly what that requires — `CGO_ENABLED=0` makes it work on any
guest image, glibc or musl. The node daemon is I/O-concurrency-heavy: many
sandboxes, many gRPC streams, a fault-handling goroutine per restore, and
goroutines fit that shape. And the whole system is one build, so the agent, the
node and the control plane share the generated protobuf types rather than
maintaining three views of them.

**Build tags carry the Linux-only code.** device-mapper, userfaultfd,
Firecracker and `SEEK_HOLE` sparse-file walking are Linux kernel interfaces, so
those files are `//go:build linux` with `_other.go` counterparts. This is not
portability theatre: it is what makes `go build ./...` and the whole unit-test
suite run on a macOS laptop, with the `LocalRuntime` tier exercising the same
agent gRPC surface. `golang.org/x/sys/unix` is the one direct dependency this
implies, since userfaultfd and its ioctls are not in the standard library.

### The standard library is the default, and `go.mod` shows it

Four direct requires: `golang.org/x/sys`, `google.golang.org/grpc`,
`google.golang.org/protobuf`, `modernc.org/sqlite`. Everything else in the file
is transitive.

Things that are hand-written rather than pulled in:

- **The Prometheus exposition format** (`internal/obs/metrics.go`) — counters,
  gauges and histograms rendered directly, so the binaries stay dependency-free
  and the same registry can be wrapped by an OTLP exporter later.
- **The OCI distribution client** (`internal/node/image/registry.go`) — manifests,
  layer blobs, and token auth over `net/http`.
- **Structured logging** on `log/slog` from the standard library. Not a
  third-party logger; `log.Printf` is down to one remaining call site against 92
  `slog` uses. (`decisions.md` §4 and `status.md` both still list "71
  `log.Printf` calls, unstructured" as an open gap — **that gap is closed and
  those documents are stale**.)
- **The Python SDK** on `urllib.request` from the standard library, not `httpx`
  or `requests`, so installing it pulls nothing. (architecture.md §7 describes it
  as "hand-written httpx" — also stale.)
- **S3 and SigV4**, which is the clearest case and gets its own section.

### The hand-rolled SigV4, and the reasoning ✅

`aws-sdk-go-v2` is dozens of modules and hundreds of transitive dependencies. The
platform uses GET, PUT, DELETE, HEAD and multipart upload — five operations.

The honest cost of not taking it is that SigV4 has to be right, and that is not
"compute an HMAC." **Compatibility is almost always lost at canonicalisation**,
especially against non-AWS implementations (MinIO, Ceph RGW, every cloud's S3
layer). The algorithm is fully specified by AWS; the value in `sign.go` is
getting canonicalisation right.

The benefits beyond dependency weight are concrete: no hidden retries, connection
pooling or region resolution on the request path, so a failure is fully visible;
and exact control over which headers are signed, which is the thing most often in
need of adjustment against non-AWS servers. Only `host`, `content-type` and
`x-amz-*` are signed — the more headers are signed, the easier it is for a proxy
adding `X-Forwarded-For` or normalising `Accept-Encoding` to invalidate the
signature.

Details where getting it wrong yields a signature mismatch rather than a clear
error: header names lowercased, sorted and deduplicated (Go's `http.Header` is
canonical-MIME form); values `TrimSpace`d because the server trims; `host` pulled
from `req.Host` because Go does not keep it in the header map; the SHA-256 of the
empty string hardcoded, because S3 **requires** `X-Amz-Content-Sha256` even on an
empty body; `EscapedPath()` rather than the raw path, or keys containing spaces or
non-ASCII fail.

**No clock correction, deliberately.** Skew beyond ±15 minutes gets
`RequestTimeTooSkewed`. A host that far off has worse problems (TLS, leases, log
ordering) and compensating inside the S3 client would only hide them.

Multipart upload and range reads are implemented on the same client. Failure must
not leave a readable half-object, so an aborted upload leaves nothing behind —
verified against a real MinIO in CI rather than a fake server, because
`ErrBlobNotFound` mapping, abort semantics and range-read boundaries are server
behaviours and a fake would only confirm our own assumptions.

⚠️ **Credentials are the gap**: nodes take S3 credentials from environment
variables. The design calls for presigned URLs issued by the control plane so
nodes hold no long-lived credentials; that is unimplemented.

---

## 6. Control plane

### 6.1 SQLite or Postgres, chosen by a flag ✅

Hot state — sandbox metadata, leases, scheduling commitments — lands in a
relational database rather than in S3, because commitments need transactions:
a scheduling decision, the resource deduction and the command record have to
commit atomically or two replicas double-place.

Today that is `modernc.org/sqlite`: **pure Go, no cgo**, which matters because
`CGO_ENABLED=0` is a requirement elsewhere in the build and a cgo SQLite would
split the toolchain story. `SetMaxOpenConns(1)` enforces the single writer.

Postgres is now a flag rather than a project: `bean-api --postgres <dsn>`. That is
what allows more than one replica, since SQLite is a single file two replicas
cannot share.

**A dialect, not a second implementation.** One body of statements written with
`?`, rewritten per engine — sized by measurement (103 placeholders plus a few DDL
constructs; all eight `ON CONFLICT` clauses port unchanged) rather than by taste.
Two bodies of SQL that must agree, checked by a suite that can only say afterwards
which one drifted, is the worse position.

**What actually made the swap safe was atomicity, not the interfaces.** Earlier
revisions of this section worried about extracting a `Store` interface, and that
turned out to be the easy half. The hard half was that 37 of 39 methods relied on
a process-local mutex — which could never have ordered writes from a second
replica, and which hid a real lost-update bug (194 of 200 updates lost once it was
removed). Every operation's conditions now live in its statement, and the store
holds no mutex at all.

**Reading the SQL was not sufficient to port it.** Running against a real Postgres
found four more differences than the survey did, including `INTEGER` — 64 bits in
SQLite, 32 in Postgres, with every timestamp in Unix milliseconds — which is
invisible to inspection because the spelling is identical and the meaning is not.
It also found a genuine bug on both engines: `Reserve` had eight placeholders and
nine arguments, so its GPU guard did not exist and SQLite silently ignored the
extra. See `status.md` for the full list and for why there is now a per-method
smoke test with a drift guard.

Rejected: **etcd or a K8s API server as the store**. The scheduler is deliberately
ours (architecture D7) because evaluation scheduling is simple enough that writing
it allows optimisations K8s cannot do — image affinity scored on the byte fraction
of an image already in a node's cache, and anti-affinity within one eval run so a
single node failure does not swallow a batch. Once the scheduler is ours, a
general-purpose distributed store buys nothing a transaction does not already give.

### 6.2 gRPC between components, REST at the edge ✅

Two internal surfaces: `NodeService` + `SandboxService` between control plane and
`noded`, and `AgentService` between `noded` and the in-sandbox agent. The `.proto`
files in `proto/` are the single source of truth, generated with `protoc-gen-go`
and `protoc-gen-go-grpc` **pinned in the Makefile** so generated code is
reproducible across machines and CI.

gRPC earns its place on the things this system actually does: streaming exec with
interleaved stdout/stderr, file transfer, long-lived heartbeat streams, and a
generated client for both the agent and the node from one definition. Doing
bidirectional streaming over hand-rolled HTTP would mean writing a framing
protocol.

One measured tuning decision. The agent was listening at ~700 ms but
`agent_ready` reported 1493 ms, because the agent cannot listen until the guest
has booted, so **the first dial always fails** — and gRPC's default `BaseDelay` of
1 s then parks the connection in backoff for a full second while the poll above it
spins. Set to `BaseDelay` 20 ms / `MaxDelay` 1 s, with poll granularity 50 ms →
10 ms: **create went 2.2 s → 1.04 s**. The reasoning generalises: a retry interval
should match the timescale of "one boot," not the timescale of "a remote service
is down."

**REST at the edge**, hand-written on `net/http` (`internal/control/api/`). The
callers are an evaluation harness in Python and a CLI, so `curl` and
`urllib.request` need to work without codegen. No grpc-gateway: it appears in
`go.sum` only because `hack/tracedump` pulls the OTLP proto packages, and
`go mod why` confirms the main module does not need it.

### 6.3 Scheduling and capacity, and why the numbers had to be attributed

Not a dependency, but it is where several technology decisions were forced.

**`max_creates=16` was never the real limit.** Three configurations, the same
30-concurrent burst:

| Node configuration | Succeeded | Actual limiter |
|---|---|---|
| disk 100 GiB, cpu 8 | **5** | `102400 / 20480` = nominal disk accounting |
| same, requesting 2 GiB disk | **8** | `cpuAllocatable 8 / 1 vCPU` |
| disk 1 TiB, cpu 32 | **16** | this one really is `max_creates` |

The limiter moves to whichever resource is accounted most coarsely, and under the
default configuration that is disk. Attributing the original "16 succeeded" to
`max_creates` was a coincidence — that host happened to have 16 cores. So a
rejection now names the resource that ran out, how many nodes it blocked, and how
far short the closest node was, because the most likely reaction to an
unattributed capacity error is adjusting the wrong limit.

**No disk overcommit factor.** A factor asks an operator to guess a multiplier,
and a sparse file's nominal size was never a sound accounting input. Instead
`statfs` reports actual occupancy — the gap is stark, one node measured
`diskCommittedMiB: 0` against `diskUsedMiB: 76200`. Placement still uses the
commitment ledger, because a ledger cannot be oversold by a burst whereas measured
occupancy lags, and placing against a lagging number walks a batch into a wall
when they all start writing.

**Create concurrency is queued, everything else is refused.** Queueing took a
30-way burst from **16/30 to 30/30**, with wall time 8 s → 13 s — throughput
unchanged at ≈2.3 creates/s, rejections converted into latency. That is the
correct trade for a workload that is a burst by construction and whose rejected
callers retry as another burst. The distinction from CPU/memory/disk is duration,
not severity: create concurrency drains in seconds, while resource commitments are
held for a sandbox's whole life, so waiting on those returns the same rejection
later having also held a client. A timeout answers **504 `QUEUE_TIMEOUT`**, not
503, because the request was admissible and the node was merely busy longer than
the caller would wait.

---

## 7. What happens when the disk fills, and why it constrains the stack ✅

`hack/enospc-probe.sh` assembles a dm-snapshot whose CoW lands on a 64 MiB
loopback filesystem and writes from the guest until the host disk is full:

```
RESULT: the write FAILED with exit 1
  dd: error writing '...': Input/output error
kernel: device-mapper: snapshots: Invalidating snapshot: Error reading/writing.
dmsetup status: 0 524288 snapshot Invalid
```

| | Measured |
|---|---|
| does the guest hang or error | **errors** (EIO) — better than dm-thin's default `queue_if_no_space` |
| device state | the target turns **`Invalid`**, unrecoverably |
| can it be written afterwards | `write()` **still returns success** ← the dangerous part |
| did that write survive | **no** — remount goes straight to `can't read superblock` |
| the shared base | **intact**, mounts cleanly; blast radius is one sandbox |

**The "`write()` succeeds and the data is gone" line settles the design.** This is
the same class of silent failure as the CoW seeding bug: nothing above sees an
anomaly until the page cache is invalidated. So there is no point planning
remediation after the disk fills — by then the sandbox is unrecoverable and the
only correct action is to destroy it. The defence has to sit before the line: the
node refuses admission below `--min-free-disk-mib` / `--min-free-disk-percent`
(off by default), answering **503 `NO_CAPACITY`** with the path, the current free
space, the floor and the consequence, leaving no VM, mapping or directory behind.
The base surviving is what makes that trade obviously cheap — the cost is refusing
a few creates, not losing a batch of running evals.

The layering order is copied from kubelet: **the reclamation trigger must sit
below the stop-accepting line**, or entering pressure means refusing service
without giving reclamation a chance.

⚠️ **Still unverified**: what the *guest* sees when its layer cannot allocate. The
host side is measured; nobody has watched from inside. If the guest does not report
an error, a caller gets a sandbox that looks healthy while losing data — worse than
a refused create, and the answer decides whether such a sandbox should be marked
FAILED proactively.

---

## 8. Image pipeline ✅

```
① ParseReference     resolve the ref
② check ImageDir     present → return  ← immutable semantics
③ FetchManifest      registry auth + manifest parse
④ sizeFor(manifest)  estimate filesystem size from compressed layer sizes ⚠️
⑤ writeBaseImage     sparse file → mkfs.ext4 → mount
⑥ applyLayer × N     unpack in order, honouring whiteouts
⑦ add guest dirs     /proc /sys /dev and the rest of the mountpoints
⑧ unmount → rename   atomic publish
⑨ write metadata     record which ref this file came from
```

**The node pulls images itself** rather than shelling out to a container runtime.
A sandbox's rootfs is a filesystem image, not a container snapshot, so the useful
parts of a runtime — its snapshotter, its content store, its daemon — are not
reusable; what is needed is the manifest, the layer blobs, and somewhere to unpack
them. Doing it directly also means a node has no dependency on docker or
containerd being installed and healthy. This is the same conclusion architecture
D2 states as "no containerd on the hot path," and unlike the "overlaybd driven
directly" half of that claim, **this half is delivered**.

Two orderings that are deliberate: the **metadata file is written after the image**,
because `Cached()` reports from it and writing it first would have the node claim
an image that cannot yet be used — the scheduler would send work and the create
would fail. And **rename is what makes an image visible**, so an interrupted
conversion cannot leave a partial image that looks complete.

Layer paths are checked for escape before extraction, and `refToFilename` maps
distinct separators to distinct characters so `a:b` and `a/b` cannot collide.

⚠️ One documented behaviour does not match the code. `image-pipeline.md` §2
states that gzip detection distrusts the media type and lets the magic bytes
decide, quoting the comment above `applyLayer` — but the code branches on
`layer.MediaType` alone (`convert_linux.go`:110) and there is no magic-byte
sniffing anywhere in the package. The comment describes an intent the
implementation does not carry out, so a registry with a mislabelled layer would
fail rather than be tolerated.

⚠️ **Filesystem sizing is an estimate** from compressed layer sizes, which is a
heuristic rather than a measurement. **📐 No cache reclamation**: base images in
`ImageDir` are never cleaned up, so a long-running node fills its disk. **📐 No
digest verification** on pull — fine while the registry is trusted, but it is a
missing link in supply-chain defence.

### BuildKit for builds ✅

`bean build` shells out to `buildctl` against a `buildkitd` socket. The reasoning
is stated in the code: COPY and ADD semantics, multi-stage builds, ARG
interpolation, build caching, `.dockerignore` and heredocs add up to months of
work and would still be an incomplete imitation. e2b and Daytona reach the same
conclusion.

What the platform keeps is the output shape: BuildKit can export a **flat rootfs
tar**, which is exactly what a base image needs, so there is no layer assembly, no
registry round trip, and the result goes through the same writer as a pulled
image. `commit` is the reverse path — reading a full ext4 back off the composed
device — and it produces a full image rather than an incremental layer, because a
dm-snapshot CoW layer is not an OCI layer format.

⚠️ Build logs report no progress and a build cannot be cancelled. 📐 Build output
stays on the node that built it (GitHub #22), which in a multi-node cluster is
close to unusable.

Private registry credentials are encrypted at rest with **AES-256-GCM** from the
standard library, and the node receives them from the control plane rather than
holding long-lived secrets on disk.

---

## 9. Observability

### 9.1 OpenTelemetry with W3C traceparent ✅

A request id can answer "what happened during this request" but not "which layer
did the 1.2 seconds go to" — that needs parent-child relationships, and those
exist **between** processes, so no single process's logs contain them.

The first tree measured produced a number nobody had:

```
POST /v1/sandboxes            bean-api   1196.0ms
  CreateSandbox               noded      1110.2ms   ← 86ms gap
    runtime.Create            noded       324.2ms
    agent.WaitHealthy         noded       785.8ms
```

Those 86 ms are scheduling plus the database write, and no metric covered them.
That is the value of tracing: it exposes the segment nobody thought to measure.

**The request id *is* the trace id.** Two sets of ids means a join for every
correlation, and they diverge exactly at the cross-process hop, which is the only
place correlation is needed.

**The agent deliberately does not link the tracing SDK.** e2b's `envd` can reach a
collector directly; `beand` has one inbound vsock connection and no outbound path,
so adding a reverse channel would either break "zero inbound exposure" or require
an OTLP relay inside `noded`. It extracts `traceparent` and adopts the trace id
for its own log lines, and nothing more, because the agent ships on a disk
attached to every microVM — its size is paid per boot, and the telemetry an
exporter would serve cannot leave the guest anyway.

**⚠️ One number in `decisions.md` §3.5 is now wrong.** It states
`go list -deps ./cmd/beand` returns 0 OpenTelemetry packages; it returns **12**.
The substance of the decision holds and is more precise than the claim: `beand`
links `otel/trace`, `otel/propagation`, `attribute`, `baggage`, `codes` and
`semconv` — the API and context-propagation packages — and links **zero** of
`otel/sdk` or the OTLP exporters. Extracting a `traceparent` requires the
propagation API, so zero was never achievable; what was achieved is no SDK and no
exporter, which is where the weight lives.

A bug only real hardware exposed:
`resource.Merge(resource.Default(), ...)` returns an error outright when the
pinned semconv version does not match the SDK, and the process fails to start. All
unit tests passed, because they left `Endpoint` empty and returned before reaching
that line. The regression test sets an endpoint deliberately — the exporter
connects lazily, so no real collector is needed.

⚠️ Also worth flagging: the five OTel modules are marked `// indirect` in `go.mod`
while `internal/obs` and `internal/beand` import them directly. `go mod tidy`
moves them to the direct block. Cosmetic, but it makes the file misleading about
what the project depends on.

### 9.2 Prometheus format, no client library ✅

`internal/obs/metrics.go` implements the text exposition format directly —
counters, gauges, histograms — so the binaries stay dependency-free, and the same
registry can be wrapped by an OTLP exporter later. The scrape surface includes
`bean_node_disk_{free,used}_bytes` and the snapshot cache size.

One tool exists because of a measurement trap worth recording:
`hack/phase-delta.py`. A cumulative histogram's `_sum/_count` gives a lifetime
average, which cannot attribute a single run — 26 fast creates hide 16 slow ones.
Differencing two scrapes gives the average over just the interval.

### 9.3 Logging: `log/slog` ✅

Structured logging on the standard library, text for a person and JSON for a
collector, with the trace id carried as the request field. An unrecognised level
falls back to info rather than refusing to start, since a typo in a log level
should not keep a node out of the cluster.

---

## 10. Testing

The suite splits by what a machine can do, which is a deliberate consequence of
the build-tag layout:

| Runs | What | How |
|---|---|---|
| Anywhere, including macOS | unit tests, the `local` runtime tier, the full agent gRPC surface | `make test` |
| Linux with root | device-mapper assembly and CoW seeding | skipped via `os.Geteuid() != 0` |
| Real KVM host | Firecracker create/snapshot/restore, UFFD against a live VMM | `-tags=fcintegration` |
| Wherever a gateway is reachable | 7 end-to-end tests on the local tier | `-tags=e2e` |
| CI with MinIO | SigV4 against a real S3 server | env-gated, skips silently |

The S3 integration tests are the only check that the hand-rolled SigV4 produces
signatures a server accepts — unit tests can only show the canonicalisation is
self-consistent with itself. They are gated on `BEAN_S3_ENDPOINT` so
`go test ./...` stays green without infrastructure. ⚠️ There is no scale or load
testing in `tests/e2e`; the numbers in §1 come from `hack/stress-fc.sh`.

### The two rules this project earned the hard way

**Verify through the real persistence layer.** When state exists in memory and on
disk at once, a test that reads memory proves nothing. The silent
filesystem-corruption bug passed three layers: unit tests checked the tar round
trip (correct — the data *was* written into the file, the bug was below the file),
end-to-end tests read the file from inside the guest (page-cache hit), and
`dmsetup status` inspected the device that was the snapshot *source*. **No layer
read the restored block device.** So any assertion of the form "the data is still
there after restore" must `drop_caches` first, and
`TestDevMapperSeedIsVisibleThroughDevice` mounts `/dev/mapper/...` directly,
bypassing the guest entirely.

**Then break the fix and confirm the test fails.** For that bug every file-level
assertion was green against the broken implementation, so this was the only way to
learn whether the new test was worth anything — moving the seed back after
`dmsetup create` makes it fail immediately. Applied since to the loop-device leak,
the snapshot merge ordering, snapshot cache pinning (short-circuiting the pin
check turned two tests red), and the queue's transient-vs-lifetime distinction.

---

## 11. The boot optimisation ledger

Ordered by contribution, all measured on real hardware:

```
gRPC reconnect backoff     -800 ms   BaseDelay 1s → 20ms
serial console off (quiet) -493 ms   synchronous 8250 UART writes
CI kernel swap              -90 ms   6.1.175 → 6.1.102
health poll granularity     -40 ms   50ms → 10ms
─────────────────────────────
create   2200 ms → 952 ms

UFFD on-demand paging     -1296 ms   /snapshot/load 1303ms → 7ms
unpack cache               -550 ms   unpack once per snapshot rather than per restore
─────────────────────────────
restore  1500 ms → 950 ms (1617ms on the first)
```

**The two largest items were not in the virtualisation layer, they were in our own
code.** The gRPC backoff and the serial console together are 1293 ms, 96 % of the
cold-start work. The initial assumption was that guest kernel boot was the
bottleneck; after attribution the kernel was 90 ms of it. That is the strongest
argument in this document for the order of operations: measure, attribute, then
choose the technology.

---

## 12. Summary of what is and is not built

| Layer | Choice | Status |
|---|---|---|
| VMM | Firecracker (upstream, unforked) | ✅ |
| Jailer / host cgroups | — | 📐 |
| Container tiers | runc / gVisor | 📐 |
| Dev/CI tier | `local` process tree, no isolation | ✅ |
| Rootfs | device-mapper snapshot, shared base + CoW | ✅ 44 KiB/sandbox |
| Rootfs lazy-pull | overlaybd | ⚠️ wired in behind `--fc-overlaybd` over TCMU; lazy pull itself untested against a registry |
| Memory restore | Firecracker UFFD backend | ✅ 7 ms load |
| Diff snapshots | Firecracker diff + merge at restore | ✅ 298 KB, depth capped at 8 |
| `--track-dirty-pages` | | ⚠️ implemented, off by default, overhead unmeasured |
| CPU portability | custom `/cpu-config` template + scheduler filter | ✅ within vendor+family |
| Guest kernel | Firecracker CI `vmlinux-6.1.102` + checked-in config | ✅ |
| Agent | static Go on a read-only ext4 root device | ✅ |
| Control channel | vsock + gRPC | ✅ |
| Sandbox networking | veth + netns + nftables | 📐 address pool built, plumbing not |
| Language | Go, standard library first | ✅ |
| State store | SQLite (`modernc.org/sqlite`, pure Go) | ✅ |
| State store | Postgres | ✅ SQLite or Postgres by flag; requirements run against a real Postgres 16 |
| Internal RPC | gRPC + protobuf, pinned generators | ✅ |
| External API | REST on `net/http` | ✅ |
| Object storage | S3-compatible, hand-rolled SigV4, multipart, range reads | ✅ |
| S3 credentials | presigned / STS rotation | 📐 env vars today |
| Image pull | own OCI distribution client | ✅ |
| Image build | BuildKit via `buildctl` | ✅ logs and cancellation ⚠️ |
| Tracing | OpenTelemetry + W3C traceparent | ✅ agent adopts id, emits no spans |
| Metrics | hand-written Prometheus exposition | ✅ |
| Logging | `log/slog` | ✅ |

## References

- [decisions.md](decisions.md) — every choice with its measurements
- [status.md](status.md) — what is actually built
- [architecture.md](architecture.md) — components and their relationships
- [noded-design.md](noded-design.md), [vm-assembly.md](vm-assembly.md),
  [image-pipeline.md](image-pipeline.md), [s3-storage.md](s3-storage.md),
  [snapshot-resume.md](snapshot-resume.md),
  [competitive-analysis.md](competitive-analysis.md)
- [firecracker: handling page faults on snapshot resume](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/handling-page-faults-on-snapshot-resume.md)
- [e2b-dev/fc-kernels](https://github.com/e2b-dev/fc-kernels)
- [tensorlake: Firecracker disk snapshots in O(changed bytes)](https://tensorlake.ai/blog/firecracker-disk-snapshots-o-changed-bytes)
- [Restoring Uniqueness in MicroVM Snapshots (AWS)](https://arxiv.org/pdf/2102.12892)

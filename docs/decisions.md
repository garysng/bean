# Technology choices and comparisons

> 中文版:[zh/decisions.md](zh/decisions.md)

> Every decision records: measured data, what the competition does (e2b / tensorlake / agentenv), and why this option was chosen.
> Entries with no measured data behind them are marked "unverified" and are not treated as conclusions.

## 1. Boot optimisation

### 1.1 Serial console: off by default

**Measured** (real KVM machine, alpine 3.19, VMM start to agent connectable):

```
console=ttyS0    1193 / 1195 / 1210 ms
quiet             700 /  700 /  711 ms
```

Dropping the serial console saves 493ms (41%). 8250 UART writes are synchronous — the kernel waits on hardware for every line it logs.

**Competition**: in e2b's `fc-kernels` config, `CONFIG_SERIAL_8250=y` is **on** —
compiled into the kernel, but boot args do not carry `console=`. It is attached only when debugging is needed, so one kernel both boots fast and debugs.

**Choice**: follow e2b. The kernel keeps the driver; `--debug-console` controls whether it is attached.
Reasoning: a failed boot has no other source of evidence, so that capability cannot be given up — but it should not be paid for on every boot.

### 1.2 gRPC reconnect backoff

**Measured**: the agent is listening at ~700ms, but `agent_ready` reported 1493ms.

Cause: the agent cannot listen until the guest has finished booting, so the **first dial always fails**.
gRPC's default `BaseDelay` is 1s, so after that failure the connection sits in backoff for a full second while the 50ms poll above it spins doing nothing.

**Choice**: `BaseDelay` to 20ms, `MaxDelay` 1s. Poll granularity 50ms → 10ms.
Reasoning: the retry interval should match the time scale of "one boot", not the time scale of "a remote service is down".

**Result**: create 2.2s → 1.04s (`runtime_create` 234ms + `agent_ready` 770ms).

### 1.3 Guest kernel: use the CI prebuilt, do not fork, do not build our own compile pipeline

**Investigation**:
| repo | contents | forked |
|---|---|---|
| `e2b-dev/firecracker` | VMM source | **yes** (added a gdb feature, among others) |
| `e2b-dev/fc-versions` | pipeline that builds the VMM | no |
| `e2b-dev/fc-kernels` | kernel config + patch + build.sh | **no** |

`fc-kernels` does a `git clone amazonlinux/linux` at run time (the same source Firecracker's official `rebuild.sh` uses); the repo itself only holds a config (3094 lines) plus one virtio_balloon patch.
**e2b's kernel maintenance surface = one config file, with no rebase burden.**

**Choice**: use `firecracker-ci/v1.11/x86_64/vmlinux-6.1.102`, with the `.config` checked in alongside it
(CI publishes the config separately, so "use the prebuilt" and "have our own config in hand" are not mutually exclusive).
`hack/build-assets.sh kernel` downloads it and verifies it is an ELF — that bucket has been seen to serve truncated files, and a short kernel presents as "boot hangs", not as a download error.

Reasoning: building in a container means paying the cost up front (toolchain + fetching source + a 20min build) before getting the first data point, and at the time it was not even established that changing the kernel helped at all. Measure first, build the compile pipeline if the payoff justifies it — the config is already in hand.

**Measured** (quiet, VMM start to agent connectable, three runs each):
```
vmlinux-6.1.175   690 / 689 / 715 ms   (from the agentenv R2 site, config unknown)
vmlinux-6.1.102   603 / 613 / 601 ms   (Firecracker CI, config known)
```
~90ms faster (13%). End-to-end create went from 1040ms → 952ms, and snapshot/restore work normally.

**But note where the gain comes from**: in the CI config, `CONFIG_SCSI_ISCSI_ATTRS`, `CONFIG_BPFILTER`,
`CONFIG_SQUASHFS`, `CONFIG_XFS_FS` and `CONFIG_NFS_FS` are **all =y**.
So those 90ms are not "useless drivers were trimmed" — the CI kernel did not trim them either.
The difference is mostly the smaller image (40.8MB vs 44.5MB) and the version itself.

**Inference**: the ceiling on building our own trimmed config is lower than expected. Those iSCSI / bpfilter probes in the kernel log are present in the CI kernel too, and it is already faster.
If boot time really needs to come down further, gains on the order of `quiet` (-493ms) and the gRPC backoff (-800ms) are not to be found in kernel trimming. **So no compile pipeline for now.**

## 2. snapshot restore: UFFD rather than caching the unpacked result

**Measured** (restoring a sandbox with 512MiB of memory):

```
restore total     1400 ms
├─ restore_load   1303 ms   ← pull blob + gunzip + write memory/rootfs to disk
└─ agent wait       97 ms   ← memory already restored, process still alive
```

Against a 1040ms cold boot: **restore is slower than a cold boot**. 93% of the cost is in `restore_load`.
The memory file written to disk actually occupies 513MB (not sparse), and every restore rewrites the whole thing.

**One fact that has been verified**: Firecracker maps the memory file `MAP_PRIVATE` (copy-on-write).
Measured: after writing 64MB of random data inside the guest, the md5 of the memory file on the host is unchanged.
So multiple restores **can share a single unpacked memory file**.

**What the competition does** (all three agree):
- **e2b**: `packages/orchestrator/pkg/sandbox/uffd/` — a complete UFFD handler, including `memory/`, `prefetch/`, `userfaultfd/` (cgo).
- **agentenv**: `storage/uffd-core/` (Rust) — and it also wires the UFFD backend into overlaybd, so a page fault reads straight from the image.
- **tensorlake**: public blog posts on sub-second cold start, and they made disk snapshots O(changed bytes) (single-file change, 167ms / 105MB).

**Choice**: UFFD. Firecracker's `snapshot/load` supports `backend_type: Uffd` plus a UDS path; the VM does not read the memory file, and the handler process supplies pages on demand when they fault. **Zero disk writes at restore.**

Rejected option: "cache the unpacked memory file by snapshot ID". It removes repeated decompression, but the first time still writes 512MB to disk, and it consumes disk; UFFD eliminates that cost outright, and it is the choice all three competitors made.

Rejected option: "pool restore-ready VMs". Every pool member holds a copy of memory, and measurement shows the bottleneck is unpacking and writing to disk rather than VM restore (the agent only waits 97ms) — pooling does not solve the real problem.

**Preconditions confirmed**: FC v1.15.1-patch-v1 supports it; the host has `CONFIG_USERFAULTFD=y`;
`unprivileged_userfaultfd=0` but noded runs as root, so it is usable.
5.15 goes through the `userfaultfd` syscall, 6.1+ through `/dev/userfaultfd`.

### 2.1 Measured results, and two protocol details that only show up when you run it

`/snapshot/load` went from **1303ms → 7ms**. Verified on real hardware, pages supplied on demand.

Two traps, neither documented clearly:

1. **The fd and the region layout are not necessarily in the same datagram.** A single `ReadMsgUnix`
   returns the fd but with an empty body → JSON parse fails → the handler dies, and Firecracker
   blocks forever on the page fault. You must loop until you have both. agentenv's Rust implementation loops too.
2. **The fd Firecracker hands over is non-blocking.** A direct `read` returns EAGAIN immediately and
   the fault loop exits on the spot. You must `poll` for readability.
   The symptom of this mistake is "`snapshot/load` hangs forever", which is indistinguishable from the handler crashing —
   which is why the handler must have an `Err()` channel, or all you can see is a hang.

### 2.2 unpack cache: once per snapshot per node

With UFFD removing the load cost, the remaining 1060ms is all unpack (gunzip + writing 512MB of memory to disk).
Every restore of the same snapshot unpacks to byte-identical output, so cache it by snapshot ID.

**Safety verified**: Firecracker maps the memory file `MAP_PRIVATE`.
After writing 64MB inside the guest the host file's md5 is unchanged → one unpacked memory file can serve arbitrarily many restores.

**The writable rootfs is deliberately not cached**: two sandboxes restored from the same snapshot diverge on the first write, so each must have its own device. Fortunately it is a sparse extent list, and a new sandbox has barely written anything, so it is very small — which is what makes the "share memory + keep rootfs separate" split work.

**Measured**:
```
first restore   1617 ms   (pays the unpack cost)
later restores   ~950 ms
```

Concurrency correctness: concurrent restores of the same snapshot unpack only once.
Writing the test turned up a real race — between `wg.Done()` and re-checking the cache itself, a waiter could observe "not on disk yet" and unpack all over again. Changed so that waiters read the in-flight result directly.
Publish uses "write to a temp directory + rename", so an interrupted unpack cannot leave a partial entry behind.

### 2.3 Not done yet: the remaining ~950ms

Even on a cache hit, the whole 16MB bundle is still **streamed from the gateway and gunzipped**,
purely to extract the rootfs member. The right fix is for the node to tell the control plane "don't send it" on a hit, or to split the rootfs into its own object. **Not implemented.**

**Known risk** (from Firecracker's own documentation): if the handler process dies, Firecracker **hangs forever** on the next page fault, so liveness monitoring is mandatory. The balloon's `MADV_DONTNEED` produces `UFFD_EVENT_REMOVE`, and the handler must zero the corresponding pages rather than re-reading the file (otherwise it resurrects stale data).

### 2.4 Comparison against the three competitors

| Dimension | e2b | agentenv | tensorlake | bean (today) |
|---|---|---|---|---|
| VMM | forked firecracker (private, added gdb feature) | upstream FC | not public | upstream FC 1.15.1 |
| guest kernel | own config + patch, source from `amazonlinux/linux`, **no fork** | prebuilt (R2 site) | not public | **FC CI prebuilt + config checked in** |
| memory restore | UFFD (`uffd/` + `prefetch/`, cgo) | UFFD (`uffd-core/`, Rust) | details not public, claims sub-second | **UFFD (measured 7ms load)** |
| rootfs on demand | not seen | UFFD backend wired to overlaybd | disk snapshots O(changed bytes), single-file change 167ms | dm-snapshot CoW (44 KiB/sandbox), **lazy-pull not done** |
| disk snapshot deltas | not seen | not seen | **yes** (their differentiator) | none (full snapshot) |

**Three judgements out of that comparison:**

1. **UFFD is consensus, not an option.** All three did it, and both e2b and agentenv wrote a complete
   handler package of their own. Our original plan of "cache the unpacked memory file" only moves the cost
   from "every time" to "once per snapshot"; UFFD is what moves it to "every page actually touched".
   The two do not conflict — we now have both.
2. **Do not fork the kernel.** e2b forked the VMM but did **not** fork the kernel, maintaining just one config.
   That is the smallest maintenance surface, and we follow it.
3. **Delta disk snapshots are our biggest gap.** tensorlake treats it as the core selling point
   (O(changed bytes) vs O(disk size)). Our rootfs already goes through a sparse extent list,
   so cost tracks "how much was written" rather than "how much was provisioned" — the direction is right,
   but it is still a full snapshot, with no delta against the previous one. Firecracker natively supports
   diff snapshots, and the interface would not have to change.

## 3. rootfs: dm-snapshot rather than overlaybd/TCMU

**Measured**: 44 KiB of disk per sandbox (shared read-only base + per-sandbox CoW).

TCMU needs a whole SCSI fabric per sandbox (loopback nexus), which is fragile and slow;
dm-snapshot only needs the `dm_snapshot` module.

**overlaybd's real value** is in "read blocks on demand on first pull", not in "per-sandbox cost" —
CoW already solved the latter. So overlaybd is worth doing, but the reason is **wait time on first use of a large image**, not disk usage.

agentenv's `uffd-core/src/overlaybd.rs` shows the two can be combined: a UFFD page fault reads straight from the overlaybd image. That is a step further than where we are.

### 3.0 restore must seed the CoW **before** device assembly

dm-snapshot reads the exception table into kernel memory at the moment of `dmsetup create` and never reads it back afterwards. Writing into the CoW backing store of an **already active** device means the kernel does not acknowledge those chunks — the device keeps serving the base image.

The original restore did exactly that: `Prepare()` assembled the device, then wrote the snapshot extents into `cow.img`.

**On a full snapshot this bug is silent.** Measured (Zen 2 / 6.1.102):

```
read immediately after restore:  cat /root/marker  →  survives      ← hits page cache brought back by the memory snapshot
after drop_caches:               cat /root/marker  →  (9 × \0)      ← actually goes to the block device
                                 ls -la            →  size = 9      ← metadata is in memory, and correct
                                 dmesg             →  no errors at all
```

The filesystem gives no abnormal signal whatsoever: size is right, no EIO, nothing in dmesg. Only the contents are zeros. Metadata lives in the memory image, data lives on the block device, the two disagree and ext4 has no reason to be suspicious.
A memoryless snapshot has no page cache to lean on, so it exposes it **immediately** as "the file is gone" — that is what surfaced a defect that had been there all along.

**Fix**: a `PrepareOptions.SeedWritable` callback, which the provider invokes between "CoW created" and "device assembled". Restore therefore changed to land the bundle in a staging directory first and then hand it to `Prepare`, keeping the extent stream verbatim and decoding it exactly once, when it is written into the device.

Competitor comparison: **nobody writes into the CoW of an already active device.** firecracker-containerd's devmapper snapshotter derives from a thin-pool first and activates after, so the ordering is inherently correct; Lambda SnapStart supplies a chunked, lazily loaded block device; E2B's rootfs is just a host file, with CoW at the filesystem layer. Firecracker's upstream documentation simply throws disk state back to the caller to guarantee — what we hit is the class of problem it warned about.

**Why the tests did not catch it**: all three layers of verification were at the wrong abstraction level. Unit tests tested tar in and out (the data really was written into the file; the bug is below the file); e2e read a file inside the guest (hits page cache); `dmsetup status` looks at the device that is the snapshot **source**. No layer read **the restored block device itself**.

`TestDevMapperSeedIsVisibleThroughDevice` now mounts `/dev/mapper/...` and reads back, bypassing the guest. Verified that moving the seed back after `dmsetup create` makes that test fail immediately.

**A general rule follows**: when state exists in memory and on disk at the same time, a test that only reads memory is fake. Any assertion of the form "the data is still there after restore" must `drop_caches` first. The same applies to diff snapshots below.

### 3.0.1 diff snapshot: materialise at restore, do not layer in the page-fault path

Firecracker's diff memory file is **not self-contained** — it is a sparse file that must be layered onto a base. So the real design question is not "how to produce a diff" but "when and where to merge".

**The competition took the two opposite paths, and both run in production:**

- **E2B**: layered lookup at fault time. The UFFD handler goes through `block.Slicer` across base plus each layer, and after K pause/resume cycles a single read has to "chase K different BuildId references".
  No cap on chain depth, only `NormalizeMappings` merging adjacent segments from the same build.
  Public analysis states explicitly that **cross-build fragmentation grows over time**, with read amplification proportional to depth.
- **Cognition blockdiff**: the chain is lineage only, flattened to raw before running.
  `apply` is a pure metadata operation (XFS reflink); a 128 GB `cp --reflink=always` measured
  0.008s vs 24.5s. Their flatten is essentially free, which is why the article never discusses read amplification —
  **there is no chain to walk at run time**.
- **Firecracker upstream**: `snapshot-editor edit-memory rebase` is exactly flatten, and requires layering in creation order.

**We chose flatten, and the reason is more than "go with the majority":**

We have a structural advantage E2B does not — `snapCache` already caches unpacked results by snapshot id.
E2B walks the chain itself on every resume; we pay the merge once, **the first time a given leaf is restored on a given node**, and every restore on that node afterwards reuses it. Fan-out is precisely "the same leaf restored many times", so the merge is amortised away entirely.

More importantly, **the UFFD page-fault path does not change at all**. `fill()` is the hottest and most insidiously error-prone code in the whole system —
a bug there is one page of wrong memory, with no error signal of any kind. The full snapshot path runs the same code.

**Chain depth over 8 automatically converts to full.** E2B sets no limit and did in fact take on growing fragmentation, which is evidence for setting one. Automatic conversion bounds restore cost, lets ancestors be reclaimed, and means callers never have to think about chain depth — a diff request always succeeds, it is just occasionally more expensive.

**Three things that must not be silent:**

1. `track_dirty_pages` must be set before boot and must not be stored in the snapshot. A guest without it
   that requests a diff gets an **explicit error**, not a downgrade to full — the caller would believe space was
   saved when it was not, and the size alone does not explain why.
2. Diff memory uses a **separate member name** `memory.diff`, not a flag added to `memory`.
   The consequences of confusing them are asymmetric and both bad: layering a full as a diff wipes out pages the
   base never touched; loading a diff as a full hands the guest memory full of holes. Dispatching by member name
   makes both errors impossible.
3. Deleting a base invalidates the whole chain, so deletion is refused while descendants exist (reusing `ErrInUse` → 409).
   Otherwise the failure is far away in both time and space: the delete succeeds now, and a restore fails later on another machine.

**Order is the caller's contract and cannot be recovered from the data** — later layers legitimately overwrite earlier ones, so reversed order produces an image that is "structurally intact but assembled from stale pages", which downstream cannot detect.
So `store.SnapshotChain()` fixes the order in one shot, and the chain is declared in the spec rather than discovered from the stream (a node must build a reader for every layer before it can start reading: each layer is an independent gzip stream).

**Measured (Zen 2 / 6.1.102, after drop_caches):**

```
base (full)    15,586,720 B   depth 0
diff #1           298,778 B   depth 1   ← 52×
diff #2           241,917 B   depth 2
```

After restoring the depth-2 chain all three files a/b/c are present, and `uptime 57` confirms it was a resume rather than a reboot.

### 3.1 overlaybd lazy-pull: verified working on the tcmu backend

**Measured** (2026-08-02, Ubuntu 20.04 / kernel 5.15 / tcmu backend / alpine 3.20):

```
mount time                       7 ms
mount + read /etc/os-release     1014 KiB / 5175 KiB = 19.6% of layer
read the entire filesystem       1270 KiB (zfile compression, only the blocks accessed are transferred)
registry responses               8 × HTTP 206 Partial Content
writable upper layer, actual     40 KiB (1.1 GB on the surface, genuinely sparse)
```

The path: `overlaybd-create --mkfs` creates an empty layer → `overlaybd-apply` writes the OCI tar into it →
`overlaybd-commit -z -t` seals it into a zfile blob → push to registry → tcmu mounts it via
`repoBlobUrl`. `__open_ro_remote` in the overlaybd log confirms that what it opens is
**an HTTP URL and not a local file**; ready in 25ms, with no full-layer download.

**Verification hit two traps, both of which will reproduce in production:**

**(1) The LUN must be created after the nexus.** Get the order wrong and the kernel reports
`TCM_Loop I_T Nexus does not exist`: the SCSI host scans for LUNs at registration time, when the nexus
is still empty, and **writing the nexus afterwards does not trigger a rescan** — the device never appears, while configfs
looks completely fine (`enable=1`, `info` shows ACTIVATED, `result=success` on the overlaybd side).
Correct order: backstore → tpgt → **nexus** → LUN link.

**(2) `wwn/vpd_unit_serial` must be set, or multipathd silently merges devices.**
TCMU does not provide a unique serial number by default, so two overlaybd devices with completely different contents both get WWID
`36001405` + all zeros, and multipathd treats them as two paths to the same LUN, merging them into `mpatha`.
The consequence is not an error but **reading another image's data**, and on top of that the original device becomes busy and cannot be mounted directly
(mount reports "already mounted or mount point busy", which is thoroughly misleading).
Writing a unique serial per backstore is enough.

**Conclusion**: the tcmu backend is functionally complete; there is no need to upgrade the kernel first. ublk (≥6.0) is only better performing.
Both of these must be encoded into the `image.Provider` implementation — documentation will not remember them for us.

## 3.5 trace: OTel + W3C traceparent, but the agent does not link the SDK

**Why a request id is not enough**: structured logs can answer "what happened during this request",
but not "which layer did the 1.2 seconds go to". The latter needs parent-child relationships, and those relationships exist
**between processes** — no single process's logs contain them.

The very first tree measured produced a number nobody had known before:

```
POST /v1/sandboxes            bean-api   1196.0ms
  CreateSandbox               noded      1110.2ms   ← 86ms gap
    runtime.Create            noded       324.2ms
    agent.WaitHealthy         noded       785.8ms
```

Those 86ms are scheduling plus the database write, and no metric had covered it before. That is exactly the value of tracing:
what it exposes is **the segment nobody thought to measure**.

**Competitor comparison**:

| | trace approach | inside the guest |
|---|---|---|
| e2b | OTel, `traceparent` throughout | agent emits spans (envd has an outbound path) |
| agentenv | OTel | same |
| tensorlake | in-house timing reporting | — |
| **bean** | OTel + W3C traceparent | **adopts the trace id only, emits no spans** |

**The difference between bean and e2b here is deliberate**: e2b's envd can reach a collector directly, whereas our
beand has only one inbound vsock and no outbound path. Adding a reverse channel would either break
"zero inbound exposure" or require an OTLP relay inside noded — the latter is feasible but
not the current bottleneck. So the choice is: beand adopts the caller's trace id and writes it into its own logs,
and **deliberately does not link the OTel SDK**. `go list -deps ./cmd/beand` returns 12 OTel packages, and all
of them are the API and propagation side (`otel/trace`, `otel/propagation`, `otel/attribute`, `otel/baggage`,
`otel/codes`, `otel/semconv` and their internals) — which is what parsing and forwarding a `traceparent` needs.
Zero SDK packages and zero exporters. The reasoning is that the
agent disk is attached to every single microVM, so its size is priced per boot, and the telemetry that SDK would serve
cannot leave the guest at all.

**The cost has to be stated plainly**: the guest's stderr only comes out over the serial console under `--debug-console`,
and the serial console is off by default (§1.1, saving 493ms). So that log line carrying the trace id is
**invisible by default**. Closing the loop properly means collecting guest logs to the node over vsock — not done.

**The request id is the trace id; no separate id is issued.** Two sets of ids means a join for every correlation,
and they will inevitably diverge at the cross-process hop — which is precisely the only place correlation is needed.

**A bug only real hardware exposes**: `resource.Merge(resource.Default(), ...)`
returns an error outright when the pinned semconv version does not match the SDK, and the process fails to start.
All unit tests passed, because they left `Endpoint` empty and returned before reaching that line.
The test added for it deliberately exercises the path with an endpoint set (the exporter connects lazily, so no real collector is needed).

## 3.6 CPU binding of memory snapshots: custom template + scheduler filter

**Problem**: the guest reads CPUID once at boot and caches it (glibc picks its string routines from it), and moving to
a machine without that feature **does not fail at restore** — it crashes later inside some piece of code.
So masking can only be done before the guest boots — it cannot be patched up at snapshot time.

### Why not Firecracker's static templates

Measured on the verification machine (AMD EPYC 7542, family 23 / Zen 2), **none** of the five built-in templates
would even start:

```
T2 / C3 / T2S / T2CL  →  "CPU vendor mismatched" (all Intel-only)
T2A                   →  "current CPU model is not permitted" (Milan/Zen 3 only)
```

Note that `PUT /machine-config` returns success for **every** template name —
vendor validation happens at `InstanceStart`. Testing configuration alone yields the false conclusion that "all of them are supported".

Switching to a `/cpu-config` custom template also means portability is no longer tied to which CPU models AWS chose to support.

### Two details only real hardware reveals

**(1) The bitmap width is 31, not 32.** 32 bits reports `string is too long`.
Unit tests all passed with 32 bits; the first create on real hardware failed. The consequence is that **bit 31 cannot be masked** —
`avx512vl` sits in that position, so it is listed separately in `UnmaskableCPUFeatures` and written to the startup log,
rather than being falsely claimed as masked.

**(2) Do not mask xsave.** Masking leaf 1 ECX bit26 does make `xsave` disappear, but the XSAVE
sub-features are in leaf 0xD and remain visible in practice — the guest would see a CPUID with `xsaveopt`
but no `xsave`, which corresponds to no real processor. And every machine capable of running FC has xsave.

### What cannot be masked: vendor and family

The vendor string and family in CPUID leaf 0 cannot be masked; the guest kernel needs them for errata
handling and MSR access. So **a template only provides portability within the same vendor and same family**,
and crossing that boundary has to be refused by the scheduler — see `scheduler.CPUConstraint`.

**Model is deliberately not recorded**: masking instruction-set features is exactly what makes a snapshot usable across models,
and matching on model would erase the template's value.

### Competitor comparison

| | CPU handling for memory snapshots |
|---|---|
| e2b | CPU template pinned to a baseline, node pools grouped by CPU model |
| agentenv | same; mainly single-node fork (16 child instances), cross-node relies on same-model pools |
| tensorlake | disk deltas are the main selling point, memory snapshots limited to the same machine/same model |
| **bean** | custom template + scheduler hard-filters on vendor/family, incompatible returns 409 |

### Probe script

`hack/cpu-template-probe.sh` freezes all of the above probing into a script: which built-in templates
can start, the upper bound on bitmap width, and which features on this host would be masked.
**It must be re-run on a different machine** — these answers are all per-host, and failure is silent
(a rejected `/cpu-config` leaves the guest with no mask set at all).
The script exits 70 when it disagrees with `cpuBitmapWidth` in the code.

It also revealed a boundary of the verification: **this verification machine has no AVX-512**,
so the 5 avx512 bits in the mask table have never actually been verified —
only `avx avx2 fma f16c` have been measured as masked.

## 3.7 Capacity accounting: nominal vs actual, and why concurrency does not go up

Two problems both look like "concurrency is only 16", but they are in fact **independent** of each other,
and once attributed separately the conclusions are completely different (measured data in status.md, scale load test).

### Slow concurrency: 5 CPU-seconds per boot, nothing to do with our code ✅

`agent_ready` accounts for 94% of a create, while `runtime_create` (dm-snapshot assembly + VMM spawn)
only goes from 241ms to 369ms between concurrency 1 and 16. During the load test `vmstat` gave
`r=16 / id=0 / us+sy=100% / wa≈0 / b=0`, and per-process inspection confirmed each firecracker burns
**5 CPU-seconds** and stops growing after boot.

**So the throughput ceiling ≈ core count ÷ 5 CPU-seconds, which is the intrinsic cost of guest boot.**
Raising `max_creates` only makes each request slower without raising throughput — this is queueing theory, not tuning.

The real levers are **reducing CPU per boot**, or **not booting**:

- restoring from a snapshot skips kernel init, which is the real value of restore relative to create
  (and the reason both e2b and Morph treat resume as the primary path)
- trimming the guest kernel would reduce those 5 seconds, but requires our own compile pipeline (§1.3 decided against it)

**Inference**: the correct semantics for `max_creates` is "queue depth", not "rejection threshold".
A batch eval caller that gets a 503 can only retry itself, and the retry storm makes things worse;
queueing makes latency predictable. Implemented (`--create-wait`, default 0), and measured: at 30 concurrent,
16/30 became **30/30**, while wall time went from 8s to 13s — **throughput unchanged, rejection simply traded for latency**,
which is exactly what the analysis above predicted. GitHub #19.

**Queue only for create concurrency.** The difference is duration rather than severity: create concurrency frees up on its own within seconds,
whereas CPU/memory/disk are held for the sandbox's lifetime — waiting ten seconds is still not enough,
and waiting only turns a fast, clear rejection into a slow one with identical content.

**A rejection must say which resource it was.** This one was learned the hard way: the same burst under three configurations
was limited to 5/8/16 by disk, CPU, and create concurrency respectively, and the error was identical —
which is how "16" got misread as `max_creates` when it was actually the core count. The most likely reaction to an unattributed
capacity error is to go adjust the wrong limit, and that does not work. So it now reports how many nodes each resource blocked,
and by how much the closest node fell short.

### Disk accounting: 20 GiB nominal vs 44 KiB actual ⚠️

The limiter migrates with configuration, and **whatever is accounted most coarsely is always what you hit first** —
under the default configuration that is disk (`102400 / 20480 = 5`), not `max_creates`.

The conclusion from surveying peers: **there is no public evidence of any platform doing scheduling accounting by "blocks actually allocated".**
The industry approach is a combination of three things, rather than making the nominal value accurate:

| Mechanism | Who uses it | Key numbers |
|---|---|---|
| Overcommit + pool-level accounting | containerd devmapper snapshotter, Kata | `base_image_size` is a virtual size; documented examples open 8–10 GB/device straight onto a 100 GB pool, so overcommit is the default posture |
| Hard quota per sandbox | dm-thin per-device size, XFS project quota | relies on quota to contain a single sandbox writing the disk full |
| Node watermark stops accepting work | Kubernetes kubelet | `nodefs.available<10%` triggers DiskPressure; `imageGCHighThresholdPercent=85` is **deliberately below** the eviction line, so reclamation happens before eviction |

**e2b does the same as us**: `dd if=/dev/zero ... count=5120` creates a 5 GB sparse overlay
on top of a read-only squashfs, and their public writing contains **no discussion of quota, accounting, or overcommit policy** at all.
So "sparse layer + imprecise accounting" is not an oversight on our part, it is the norm for this approach;
the difference is that we treated the nominal value as a scheduling input.

### What happens when the host disk fills up: measured ✅

`hack/enospc-probe.sh`. Assemble a dm-snapshot on a 64 MiB loopback filesystem
(base 256M, CoW landing on that small disk), then write from the guest side until the host disk is full:

```
RESULT: the write FAILED with exit 1
  dd: error writing '...': Input/output error
kernel: blk_update_request: I/O error, dev loop9, sector 116032 op 0x1:(WRITE)
kernel: device-mapper: snapshots: Invalidating snapshot: Error reading/writing.
dmsetup status: 0 524288 snapshot Invalid
```

**The conclusion is harder than either of dm-thin's two modes:**

| | Measured result |
|---|---|
| does the guest hang or error | **errors** (EIO), not queueing forever — better than dm-thin's default `queue_if_no_space` |
| device state | the dm-snapshot target turns **`Invalid`**, unrecoverably |
| can it be written afterwards | `write()` **still returns success** ← the dangerous part |
| did that write survive | **no**. remount goes straight to `can't read superblock` |
| what about the shared base | **intact**, the read-only base mounts cleanly — blast radius is a single sandbox |

**The "`write()` succeeds but the data is gone" line is what settles the design.** Once the device is `Invalid`,
write calls on the guest side still return 0 and the data only lands in page cache; by the time the device actually has to be read,
even the superblock cannot be read back. **This is the same class of error as the silent corruption in §3.0** —
nothing above sees any anomaly, until page cache is invalidated.

So **there is no point counting on remediation after the disk fills**: by then the sandbox is unrecoverable,
and the only correct action is to destroy it. The line of defence has to be before it fills.

The good news is the base is intact, so the blast radius is one sandbox rather than "every sandbox on that image" —
which is what makes the tradeoff "better to refuse a create than to fill the disk" work: the cost is refusing a few requests,
not losing a batch of running evals.

### The decisions that follow

**No "disk overcommit factor".** An overcommit factor asks operators to guess a multiplier, and
**a sparse file's nominal size should never have been an accounting input in the first place** — rather than guess a multiplier, report real usage.
So: the heartbeat reports actual usage, and the scheduler judges on the real watermark.

**A watermark that stops accepting work is mandatory, not optional.** Once accounting is by actual usage, overcommit is implicitly unbounded,
and the failure mode measured above is unrecoverable and silent. dm-thin's precedent is just as ugly:
under `queue_if_no_space` (the kernel default) the guest hangs, and metadata exhaustion requires offline
`thin_check`/`thin_repair` — adding data space does not repair it.

Copy kubelet's layering order: **the reclamation (cache LRU) trigger line must be below the stop-accepting line**,
otherwise entering pressure means immediately refusing service without giving reclamation a chance.

### Unbounded growth of the snapshot cache ✅ (fixed)

`sandboxes/.snapshots/` measured **4.6 GB / 9 entries** (each roughly the size of guest memory),
**entirely outside accounting and with no reclamation at all**. That is more dangerous than overestimating in accounting:

- overestimating is **conservative** — fewer sandboxes placed, but the disk will not blow up
- an unbounded cache is **uncontrolled** — it takes no quota so the scheduler cannot see it, yet it really occupies disk

High/low watermarks + LRU are implemented (`--snapshot-cache-high-mib` / `--snapshot-cache-low-mib`,
off by default). Measured on real hardware: at a 600/300 MiB watermark, 6 restores of different snapshots took
**4.83 GB / 9 entries down to 537 MB / 1 entry**, and after `drop_caches` every sandbox
read back its own marker.

#### Three decisions worth recording

**The watermark is a pair, not a single value**, copied from kubelet image GC (85/80):
a single threshold makes **every** restore after the trigger pay for reclamation, while a pair keeps reclamation an occasional batch job.
The low watermark defaults to 80% of the high one — the ratio is the part operators have no basis to choose,
while "how large the cache may get" is the part they do.

**Accounting uses allocated blocks, not nominal size** (`st_blocks * 512`). A merged memory image
is sparse wherever no ancestor wrote, and accounting by nominal size would evict entries in order to reclaim "zero bytes" —
the same mistake as the disk accounting in §3.7, just in the opposite direction.

**Eviction only needs to protect the lookup→open window, not the VM's whole lifetime.**
Initially I thought "an entry currently mmapped by UFFD must not be deleted" was the main risk, and wrote it into GitHub #25.
**That judgement was wrong** — a C program measured it: mmap a file, unlink it,
then read every byte back, and the data is intact (the inode lives until the last mapping goes away).

The real window is much narrower and much more real: a restore first `Lookup`s to get the path,
and only later does `newUffdHandler` open the memory image. **Unlink between those two points and
open is ENOENT, while that restore's stream has already been consumed — there is nothing left to rebuild from.**
So the pin only spans `stageSnapshot` to `loadSnapshot`, released at `stage.Close()`,
which is safe even while the VM is still running.

Pins are counted, because concurrent restores hold the same leaf at the same time;
`unpin` is idempotent, because `Close()` is reached via both the error return and the defer.
Deliberately short-circuiting the pin check turned two tests red immediately — that is the only way to confirm the tests are effective.

## 4. Open / to be verified

- **Wiring overlaybd lazy-pull into `image.Provider`**: the capability itself has been measured working (§3.1),
  what remains is writing `OverlaybdProvider` — configfs orchestration + registry push + lifecycle.
  It is no longer a question of "can this work", it is the engineering effort of "wire it in".
- **Upgrading the host kernel to 6.8**: 20.04's apt has no 6.x (HWE tops out at 5.15).
  It needs the mainline PPA or a distribution upgrade. The payoff is ublk (overlaybd's faster backend).
  This machine is a VM (`/dev/vda2`), and whether nested KVM stays usable after a kernel change is unverified.
  **Priority has been lowered** — the tcmu backend is functionally complete, and ublk is only a performance optimisation.
- **AVX-512 masking is unmeasured**: the verification machine (Zen 2) has no AVX-512,
  so the 5 avx512 bits in the mask table are only "written correctly according to the CPUID specification";
  the effect of masking them has never been verified on real hardware. It needs a machine with AVX-512 to run
  `hack/cpu-template-probe.sh` plus a guest comparison.
- **Cross-model restore within the same family is unmeasured**: there is only one fc machine,
  so there is no way to verify whether the template really does make snapshots usable across models — which is the entire reason it exists.
  It is logically correct (the source of model differences is masked away), but there is no empirical evidence.
- **The overhead of `--track-dirty-pages` is unmeasured**: diff snapshots are implemented (§3.0.1) but that switch is
  off by default, because the cost of KVM dirty-page accounting has never been quantified. It needs the same image and same kernel,
  N runs each with it on and off, comparing boot-to-agent plus the exec throughput of one CPU-bound and one memory-bound workload.
  Default flips to on if the regression is < 2%.
- **What the inside of the guest sees when the sparse CoW layer fills up**: the host side is measured (§3.7: EIO +
  dm-snapshot turning `Invalid` + `write()` still returning success while all data is lost), but it has **never been
  observed from inside the microVM**. The host-side conclusion is enough to set the accept watermark, but the behaviour inside the guest
  (does ext4 go read-only? or keep pretending to succeed?) determines whether we should proactively mark such a sandbox
  FAILED — if the guest does not report an error itself, the caller gets a sandbox that looks healthy while actually losing data,
  which is a far worse outcome than refusing the create.

**Closed since this list was written**: standardising logging and CLI output ✅ (fixed). Logging is
`slog` throughout (92 call sites, one `log.Printf` left and it is in `hack/tracedump`, a dev tool),
with levels, a text/json handler switch and a request id carried on the context
(`internal/logging`). The CLI has `--json`, `--quiet`, and five exit codes rather than two
(0 ok, 64 not found, 69 unavailable, 70 failed, 125 usage — `cli/exit.go`).

## 5. Boot optimisation ledger

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

**Lesson: the two biggest items were not in the "kernel/virtualisation" layer, they were in our own code.**
The gRPC backoff and the serial console together are 1293ms, 96% of the cold-start optimisation.
I initially assumed the bottleneck was guest kernel boot; after attribution the kernel turned out to be only 90ms of it.
Measure before changing — this time that was decisive.

## References

- [firecracker: handling page faults on snapshot resume](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/handling-page-faults-on-snapshot-resume.md)
- [firecracker: guest_configs](https://github.com/firecracker-microvm/firecracker/tree/main/resources/guest_configs)
- [e2b-dev/fc-kernels](https://github.com/e2b-dev/fc-kernels)
- [tensorlake: Firecracker disk snapshots in O(changed bytes)](https://tensorlake.ai/blog/firecracker-disk-snapshots-o-changed-bytes)
- [Restoring Uniqueness in MicroVM Snapshots (AWS)](https://arxiv.org/pdf/2102.12892)


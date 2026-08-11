# What is actually built

> 中文版:[zh/status.md](zh/status.md)
> Section status convention: [architecture.md](architecture.md) §0.
>
> **Authority order: code > this file > [decisions.md](decisions.md) > design docs.**
> When they disagree, the one on the left is right and the others are stale.

Every number here was measured on hardware. Where something is projected rather
than measured, it says so.

Measurement host unless noted: AMD EPYC 7542 (Zen 2), 16 physical cores, 24 GB,
guest kernel 6.1.102, Alpine 3.20. Host kernel was 5.15 for everything measured up to
2026-08-06 and **6.8 from the ublk work onward** — the concurrency numbers below that
say "128-core host" are a different, larger machine. Where a kernel version changes a
result the row says so, and one result explicitly did not change: TCMU teardown is 4.0 s
for 128 devices on both.

## Delivered

| Area | | Notes |
|---|---|---|
| Lifecycle | ✅ | create → exec → cp → pause → resume → snapshot → create-from-snapshot → destroy. Resume wakes **this** sandbox; creating from a snapshot builds a **new** one (the internal restore/Fork path), and N such creates from one snapshot are N independent sandboxes ([snapshot-resume.md](snapshot-resume.md) §0) |
| Images | ✅ | OCI pull and conversion to ext4, private registries (AES-256-GCM at rest), prewarm with image-affinity scheduling |
| Rootfs | ✅ | Shared read-only base + per-sandbox copy-on-write through device-mapper. **44 KiB of actual disk per sandbox** (see the note below on why other figures were quoted) |
| Snapshots | ✅ | Three kinds with different semantics — see below |
| Scheduler | ✅ | Two-level placement, hard filters, scoring, **commitments persisted** so replicas cannot double-place and a restart does not lose the ledger |
| Create queueing | ✅ | A burst larger than a node's create concurrency waits instead of being refused |
| Snapshot blobs on S3 | ✅ | SigV4 against the standard library, no AWS SDK; multipart upload and range reads |
| Tracing | ✅ | OpenTelemetry with W3C `traceparent` across gateway → noded → in-sandbox agent |
| Builds | ✅ | Dockerfile through BuildKit into a base image |
| Snapshot cache eviction | ✅ | High/low watermarks with LRU, and the cache's size is reported |
| Disk pressure | ✅ | Actual occupancy reported; a node stops admitting sandboxes below a floor |
| Sandbox networking | ✅ | Per-sandbox namespace, tap, NAT egress. Metadata and RFC1918 denied by default, verified by rule counters on a live guest. `pip install` works |
| Port exposure and the data plane | ✅ | One mechanism, not two: `{port}-{sandbox}` in the Host reaches that port in that guest, whether it is a user's server or the agent. No registration call and no host-port pool — noded enters the namespace and connects |
| Per-sandbox agent credential | ✅ | The agent is on TCP so one addressing scheme covers it, which means the sandbox can dial it. A per-sandbox token whose hash reaches the guest through MMDS is what replaces the vsock guarantee; verified on hardware that the readable hash is not usable as a token (security-and-startup.md A7) |
| VMM host cgroups | ✅ | `--fc-cgroups`: memory ceiling, CPU quota and pid cap per sandbox, from its own spec. **v2 only** -- a v1 node refuses to start rather than run unlimited, because v1 cannot cap swap and a guest could thrash the host instead of stopping at its ceiling |
| VMM dropped uid | ✅ | `--fc-vmm-uid`: the VMM does not run as root |
| VMM pid namespace | ✅ | `--fc-pid-namespace`, **on by default**: the VMM cannot see or signal any host process. Verified by inode on a live VMM, simultaneously with the sandbox's network namespace -- the two compose because the netns is joined before the fork and the clone flags apply during it |
| VMM mount namespace | ✅ | `--fc-mount-namespace`, **on by default**. Held back at first on the expectation that bean's device-mapper rootfs would stop being openable inside one. That was wrong: a booted guest has a working `eth0` and its own mnt, pid and net namespaces at once |
| VMM killed if noded dies | ✅ | `--fc-kill-on-exit`, **on by default**. Reconciliation already reclaimed such a VMM, but only at the next startup, and until then it holds memory promised elsewhere. Measured with a negative control: with the flag the VMM is gone after `kill -9` on noded, without it it survives |
| Container tier (runc/gVisor) | ✅ | `--runtime runc` or `--runtime runsc`: noded drives the OCI runtime directly (`NewOCITier`), **no containerd** — same bundle and subcommands for both, sharing the fc tier's rootfs providers. This is the third real runtime tier alongside `fc` and `local` |
| Postgres | ✅ | `bean-api --postgres`, which is what allows more than one replica: SQLite is one file, so two replicas cannot share it. A dialect rather than a second implementation, sized by measurement — 103 placeholders plus a few DDL constructs, with all eight `ON CONFLICT` clauses porting unchanged. `hack/postgres-conformance.sh` runs the requirements against a real Postgres 16; the suite skips loudly rather than reporting a pass earned by SQLite. **Reading the SQL was not enough** — see below |

## Not delivered

| feature | status | notes |
|---|---|---|
| Cross-node sandbox networking | 📐 | A non-goal, not a gap. Sandbox-to-sandbox traffic does not cross nodes |
| Per-port access control | 📐 | Any port on a sandbox is reachable by anything that can reach bean-proxy. A sandbox must not be given a port it would not want its caller to see (api-design.md §3.4) |
| jailer chroot | 📐 | Not done, and probably not the right shape. What jailer adds over what is now in place is a chroot and a device allowlist, and it needs the device-mapper node `mknod`'d into a per-sandbox jail (docs/jailer.md). The namespace isolation it is usually wanted for is delivered without it |
| Volumes | 📐 | |
| Host resource reconciliation | 📐 | A crashed noded leaves dm mappings and sandbox directories behind |
| overlaybd over ublk | ⚠️ | `--fc-overlaybd` with `--fc-ublk` resolves layers exactly as the tcmu route does -- shared by digest, converted when absent, same directory -- and serves them from a ublk device this process reads. That means the layer format is read in Go: `lsmt.go` parses the trailer and its bit-packed index, `zfile.go` the block-compressed data, `lz4block.go` the blocks themselves (~100 lines, and it is on every guest read, which is why it is not a dependency), `lsmtstack.go` merges a chain newest-wins, `lsmtcow.go` puts a sparse overlay over it. **The reason is teardown**: removing 128 tcmu devices takes 4.0 s and it is 4.0 s on both kernel 5.15 and 6.8, because the daemon serialises through one netlink socket its own upstream warns against using concurrently -- a cost that does not move with the kernel is a cost in the transport. **Not yet run on hardware**: unit-level and cross-compile only, and the 60-way comparison against tcmu is the number that would justify the change. Two limits are structural rather than unfinished: lazy pull cannot be served this way, because a lazily read layer is a URL the daemon range-reads while this reader needs a file it can seek (rejected at startup, not on the create that needed it), and a chain naming a layer only by digest is refused with the layer and image named. `commit` on this route is untested, as it is on the tcmu one. The LZ4 decoder is checked against blocks from the `lz4` CLI rather than only against blocks the tests build, since hand-built vectors cannot catch the encoder and decoder sharing a misreading |
| Build logs and cancellation | ⚠️ | A build reports no progress and cannot be stopped |
| ublk rootfs | ⚠️ | `UblkProvider` is implemented against a hand-written io_uring and ublk ABI -- there is no usable Go binding and `CGO_ENABLED=0` rules out wrapping liburing. **Verified single-sandbox on a 6.8 kernel**: a guest boots from a ublk device (`x86_64`, `3.20.10`, exit 0), copy-on-write holds with the base file byte-identical afterwards, and teardown leaks nothing. It serves the same converted ext4 from the same `ImageDir` as dm-snapshot, so an image prewarmed for either backend serves both; what differs is that creating the device writes io_uring commands instead of forking `losetup` twice and `dmsetup` once per sandbox, measured at ~26 ms a call. **Concurrency is now measured against dm-snapshot** on the same host, same image, same run, stepped 4 -> 16 -> 60 with a leak check at each step (60 is the ceiling `ublks_max=64` allows): `fc_rootfs` 2.461 s -> 0.034 s (72x), `runtime_create` 2.589 s -> 0.064 s, total 2.898 s -> 0.350 s, p50 3170 ms -> 440 ms, throughput 18.3 -> 101.7 creates/s, CPU per create 1.198 s -> 0.258 s, zero failures and zero devices left on either side. **That 72x on `fc_rootfs` settles the attribution**: the cost was never copying -- the layers are sparse and occupy tens of KB -- it was three `fork+exec` per sandbox, and ublk does none. The first attempt at this measurement went straight to 256 and did real damage; see "ublk: what 256 concurrent creates cost" below. Four defects were fixed as a result, including the one that mattered: nothing enforced `ublks_max`, so devices accumulated past the kernel's ceiling and could not be removed. The provider now reads that limit and refuses past it. **Operational constraint**: `ublks_max` defaults to 64, so a node cannot hold more than 64 ublk sandboxes without `modprobe ublk_drv ublks_max=N`. Opt-in with `--fc-ublk`; needs kernel 6.0 or later, and a node started with it on an older kernel refuses to start rather than falling back |
| overlaybd | ⚠️ | `OverlaybdProvider` is implemented and **verified end to end on hardware**: a sandbox boots from an overlaybd device, the guest reads `PRETTY_NAME="Alpine Linux v3.20"` from its own rootfs, writes land in the writable layer, and teardown leaks nothing (`hack/overlaybd-e2e.sh`, with `overlaybd_hw_linux_test.go` at the provider level and `hack/overlaybd-probe.sh` covering the negative cases). Measured against dm-snapshot on the same host (`hack/overlaybd-bench.sh`): **392 MiB → 118 MiB** of allocated disk for three images sharing a base, and conversion CPU dropping from a flat 2.2 s per image to 1.37 s / 0.49 s / 0.44 s as the shared layer is reused. **Cold-start latency is unchanged** — this path still downloads and converts every layer before assembling the device, so it does strictly more work than flattening on a first use and wins only on the second image and on disk. Lazy pull, the part that would cut the cold path, requires blobs that are already sealed overlaybd layers; a standard OCI layer has no block index to range-read, so a create naming one is now refused rather than silently building an unopenable config. Producing such blobs is `Prewarm`'s job, not a central pipeline's: it converts an image and publishes each sealed layer under its OCI digest to bean's own object store (`--fc-overlaybd-s3-endpoint`, `obdblobstore.go` / `obdindex.go`), and any node reading that store resolves those layers remotely instead of converting them. A create never publishes — an S3 upload of tens of MiB does not belong on a sandbox's latency path. So a genuinely cold create is still a conversion, but it is one per **fleet** per image rather than one per node, provided something prewarms. The lazy-pull read path itself (`--fc-overlaybd-lazy-pull`) is **implemented and not yet exercised against a registry**: the 7 ms mount and 19.6%-of-layer-bytes figures come from the manual verification in [decisions.md](decisions.md) §3.1, against a blob that had been sealed first, not from this code. Opt-in with `--fc-overlaybd`; dm-snapshot is still the default. **Concurrent fan-out now measured** on a 128-core host at 256 simultaneous creates, and it is where this backend earns its keep: `fc_rootfs` 3.809 s -> 0.908 s, `runtime_create` 4.169 s -> 0.992 s, total 4.512 s -> 1.299 s, throughput 47.5 -> 88.0 creates/s, zero failures and no leaks on either backend. The cause is subprocess count: dm-snapshot forks `losetup` twice and `dmsetup` once per sandbox at ~26 ms a call, while `attachTCMU` is configfs writes with no fork at all. Two defects had to be fixed first -- the device was sized independently of the filesystem on it, so any request under 2 GiB built a device smaller than its own ext4 and the guest refused to boot; see "Attribution notes" below. See [image-pipeline.md](image-pipeline.md) §7 |

## Measured latency

| Operation | Measured | Breakdown |
|---|---|---|
| create (image cached) | **952 ms** | 234 ms runtime + 770 ms to a reachable agent |
| create (cold image) | 5–10 s busybox … 2 m 45 s alpine on poor network | Almost entirely network. This is why prewarm is required rather than an optimisation |
| destroy | **214 ms** | Was 5.25 s — [decisions.md](decisions.md) §1 |
| snapshot (full) | 1.5 s, 15.5 MB | |
| snapshot (filesystem only) | **6109 B** | `--no-memory` |
| snapshot (incremental) | **298 KB** | `--base SNAP`, 52× smaller than full |
| restore | **392 ms** on a node-local cache hit | `/snapshot/load` is 7 ms of it. A first restore pays ~950 ms to unpack the bundle; the node keeps the unpacked form, so every later restore of that snapshot skips it — which is what makes fan-out cheap |

### Snapshots are three kinds, not three sizes

| Kind | Flag | Size | What a restore produces | Portability |
|---|---|---|---|---|
| full | *(default)* | 15.5 MB | a new sandbox continuing the captured guest; process tree survives | pinned to CPU vendor + family |
| filesystem-only | `--no-memory` | 6109 B | a new sandbox that boots fresh, files intact | **any CPU** |
| incremental | `--base SNAP` | 298 KB | a new sandbox continuing the captured guest | pinned to CPU vendor + family |

Guest memory records what the CPU it booted on offered, and vendor/family cannot
be masked away — so a memory snapshot only restores on a compatible CPU, and the
scheduler enforces that as a hard filter (`409 INCOMPATIBLE_CPU`) rather than
placing it and letting the guest misbehave afterwards.

Chain depth is capped at 8; past that a checkpoint silently becomes full, which
bounds restore cost and lets ancestors be reclaimed.

## Scale testing (2026-08-03)

Tools: `hack/stress-fc.sh` and `hack/phase-delta.py`. The second exists because a
cumulative histogram's `_sum/_count` gives a lifetime average, which cannot
attribute a single run — 26 fast creates hide 16 slow ones. Differencing two
scrapes gives the average over just the interval.

| Concurrency | p50 | agent_ready | runtime_create |
|---|---|---|---|
| 1 | 938 ms | 627 ms | 241 ms |
| 2 | 1228 ms | | |
| 4 | 2010 ms | | |
| 8 | 3803 ms | 2920 ms | 272 ms |
| 12 | 5556 ms | | |
| 16 | 6805 ms | 5710 ms | 369 ms |

### The bottleneck is guest boot competing for cores, not our code

`agent_ready` is 94% of a create (5710 ms of 6079 ms), while `runtime_create` —
device-mapper assembly plus VMM spawn — goes from 241 ms to only 369 ms. So the
dmsetup/losetup/sparse-file chain is not the bottleneck, and neither is
`DevMapperProvider.mu`: it wraps map operations only, and the one `losetup` inside
it runs once per image rather than once per sandbox.

`vmstat` during the run is decisive:

```
 r  b   bi    bo    in     cs    us sy id wa
16  0   0     20   5030   2048   62 38  0  0     ← 16 runnable / 16 cores / id=0
```

16 runnable threads on 16 cores, no I/O wait, no blocked tasks. Per-process
confirmation: each firecracker burns **5 CPU-seconds** in 21 s of wall time, so
16 × 5 = 80 CPU-seconds compressed into 16 cores stretches every boot about 5×.
CPU time then stays at exactly 5 s between 21 s and 53 s elapsed, which shows the
5 s is all boot — an idle guest costs nothing.

**Throughput is therefore ≈ cores ÷ 5 CPU-seconds, about 2.3 creates/s.** Lowering
create latency means reducing the CPU each boot costs (a trimmed guest kernel) or
not booting at all. **Restoring from a snapshot is the way around those 5
CPU-seconds** — which is the real value of restore over create.

### Create queueing: 16/30 became 30/30

`--create-wait 60s` on the gateway, off by default. Same host, same test:

| | Succeeded | Failed | Wall | p50 | p95 |
|---|---|---|---|---|---|
| Refuse (before) | 16/30 | 14 | 8 s | 6805 ms | 7497 ms |
| Queue (now) | **30/30** | **0** | 13 s | 7550 ms | 13213 ms |

This confirms the throughput analysis rather than contradicting it: wall time went
8 s → 13 s while successes went 16 → 30, so throughput held at ≈2.3 creates/s.
**Queueing does not raise throughput — it converts rejections into latency.** An
evaluation batch is a burst by construction, and a rejected caller retries as
another burst, so what queueing buys is a predictable answer instead of a retry
storm.

**Only create concurrency is waited on.** The distinction is duration, not
severity: those 16 creates drained in about seven seconds, whereas CPU, memory and
disk commitments are held for a sandbox's entire life. Waiting on those would
return the same rejection later, having also held a client.

A timeout answers **504 `QUEUE_TIMEOUT`**, not 503: the request was admissible and
the node was merely busy for longer than the caller would wait. 503 would suggest
the cluster is too small.

### `max_creates=16` was never the real limit

Three configurations, same 30-concurrent burst. The limiter moves to whichever
resource is accounted most coarsely:

| Node configuration | Succeeded | Actual limiter |
|---|---|---|
| disk 100 GiB, cpu 8 | **5** | `102400 / 20480` = nominal disk accounting |
| same, requesting 2 GiB disk | **8** | `cpuAllocatable 8 / 1 vCPU` |
| disk 1 TiB, cpu 32 | **16** | this one really is `max_creates` |

**Under default configuration disk binds first.** Attributing the earlier "16
succeeded" to `max_creates` was a coincidence — that host happened to have 16
cores. This is why a rejection now names the resource that ran out, how many nodes
it blocked, and how far short the closest node was.

### Disk: actual occupancy, and a floor on admission

The gap between commitment and reality is now visible from both sides — measured
on one node, `diskCommittedMiB: 0` while `diskUsedMiB: 76200`. **That gap is the
blind spot**: the ledger says the node is empty while 76 GB is in use by base
images, the snapshot cache, and other services sharing the volume.

No overcommit factor. A factor asks an operator to guess a multiplier, and the
nominal size of a sparse file was never a sound basis for accounting. Instead
`statfs` measures real occupancy and reports it three ways:
`bean_node_disk_{free,used}_bytes`, heartbeat `disk_used_mib`, and `diskUsedMiB`
on `/v1/nodes`.

**Placement still uses the commitment ledger.** A ledger cannot be oversold by a
burst, whereas measured occupancy lags — placing against a lagging number puts a
batch into a wall when they all start writing. The real defence is on the node:
`--min-free-disk-mib` / `--min-free-disk-percent`, off by default.

That defence is not optional, because the failure mode was measured
([decisions.md](decisions.md) §3.7) and it is unrecoverable: when the host
filesystem fills, dm-snapshot marks the target `Invalid`, **the guest's `write()`
calls keep returning success while the data is lost**, and the filesystem cannot
be remounted. There is nothing to salvage afterwards. The shared base image
survives, so the blast radius is one sandbox — which is what makes "refuse the
create" the obviously cheaper trade.

Verified: an unmeetable floor returns **503 `NO_CAPACITY`** with the path, current
free space, the floor and the consequence, leaving no VM, mapping or directory
behind; a realistic floor (5 GiB / 5%) admits 6 concurrent creates with no leaks.

**On the per-sandbox figure: 44 KiB is the number to quote.** The docs previously
carried 8 KiB and 80 KiB as well, and the three are not disagreements about the
same measurement — they were taken at different points in a sandbox's life. 8 KiB
is a freshly assembled CoW layer that has not been written to yet; 44 KiB is a
sandbox that has booted and written; 80 KiB was one specific small-write case in a
code comment. The useful comparison is against `FileProvider` copying the whole
base image per sandbox, and at that scale all three say the same thing.

### No leaks

After every stress round, dm mappings, firecracker processes and loop devices
holding deleted files all return to their baseline. The loop-device leak fix
(GitHub #16) holds under concurrency.

## Verification coverage

- **microVM end to end** on real KVM hardware, through both the Manager and the
  CLI: create → exec → bidirectional cp → pause → transparent wake → snapshot →
  create from snapshot → point-in-time semantics
- **Snapshot correctness through the real persistence layer**: assertions run
  after `echo 3 > /proc/sys/vm/drop_caches`, because a read served from the page
  cache passes against a corrupted device
- **Scheduler durability**: two replicas placing concurrently yield exactly N
  successes; a restart does not lose commitments
- **S3 against a real MinIO** in CI, not a fake server — `ErrBlobNotFound`
  mapping, abort leaving no object, and range-read boundaries are server
  behaviours
- **The store's requirements against a real Postgres 16**, not a mock: nine
  requirements plus a per-method smoke test, via
  `hack/postgres-conformance.sh`. Confirmed the pass is earned by the database
  rather than by the engine's locking — replacing the conditional `UPDATE` with a
  `SELECT` fails the reference-count requirement on Postgres too. Both engines are
  also run under `-race`, which matters because the store now holds no mutex

### Two testing rules earned the hard way

**Verify through the real persistence layer.** When state exists in both memory
and on disk, a test that reads memory proves nothing. The silent
filesystem-corruption bug passed three layers of tests: unit tests checked the tar
round-trip (correct — the data *was* written), end-to-end tests read the file from
inside the guest (page-cache hit), and `dmsetup status` inspected the wrong
device. None read the restored block device.

**Then break the fix and confirm the test fails.** For that bug every file-level
assertion was green against the broken implementation, so this was the only way to
know the new test was worth anything. Applied since to the loop-device leak, the
merge ordering, snapshot cache pinning, and the queue's transient-vs-lifetime
distinction.

### A third: a statement no test calls is a statement no engine has parsed

Adding Postgres was scoped by reading the SQL: 103 placeholders, one
`AUTOINCREMENT`, one `INTEGER`-as-bool, every `ON CONFLICT` portable. That survey
was right about the shape and wrong about the inventory. Running `migrate()`
against a real Postgres 16 found four more differences, and the reading could
never have found the last two:

- `secret BLOB` — Postgres has no such type and rejected the whole schema.
- `ADD COLUMN` idempotency — only Postgres can say `IF NOT EXISTS`, so the
  duplicate case is per engine rather than one error-text match applied to both.
- `INTEGER` — 64 bits in SQLite, 32 in Postgres, and every timestamp column
  stores Unix milliseconds. Five of seven requirements failed on overflow. **This
  is unreadable by inspection**: the spelling is identical and the meaning is not.
- **`Reserve` had no GPU guard at all.** Eight placeholders, nine arguments;
  SQLite ignored the extra one, so `gpu_committed` was never compared against
  `gpu_count`. A one-GPU node would hand the same device to two guests, and the
  failure surfaces inside a guest as a device already in use. Only an engine that
  counts placeholders objected.

Then the suite passed 8/8 while `Release` and `FinishCreate` had never executed on
Postgres — both used SQLite's two-argument `MAX(x - ?, 0)`, which Postgres cannot
run. No requirement called either. Had that shipped, capacity would be committed
at `Reserve` and never returned: nodes fill permanently and later placements
report `NO_CAPACITY` for resources nothing is using, with an error naming capacity
rather than the statement.

Measured afterwards: **23 of 38 interface methods were never executed against
Postgres by any test.** So there is now a smoke test calling every method once,
plus a reflection-based guard that fails when a method is missing from it — the
hand-written call list would otherwise decay exactly as the interfaces did. The
guard caught three snapshot methods left out of its own first draft.

### ublk: what 256 concurrent creates cost

The single-sandbox ublk path was verified end to end — guest boots, copy-on-write
holds, nothing leaks — and **none of that predicted concurrency**. Going straight
from 1 to 256 exposed four defects and left the host needing a reboot, at load 68
with 141 undeletable kernel objects and 37 processes unkillable in D state.

The four, in the order they surfaced:

1. **The shared control ring had no lock.** Submitting means writing the SQE at the
   tail then advancing it; reading a result means taking the one at the head. Two
   callers doing that at once take each other's completions. 255 of 256 creates
   failed with `this kernel's ublk lacks UBLK_F_USER_COPY (features=0x0)` — on a
   kernel that had answered `0x1fe` seconds earlier. **The error accused the kernel
   of a missing feature when the fault was entirely local**, which is the most
   misleading shape a message can take.
2. **`submitAndWait` treated the first `io_uring_enter` return as proof of a
   completion.** It is not: a signal, or the kernel completing asynchronously, both
   return without one. 195 of 256 failed with `enter returned with no completion`.
3. **A failed create left its device allocated**, because teardown returned early
   when `STOP_DEV` failed — and `STOP_DEV` waits on a queue that a failed create has
   already lost.
4. **Nothing enforced `ublks_max`.** This is the one that did the damage. The
   kernel's ceiling defaults to 64; 141 devices accumulated past it and could not be
   removed at all — `STOP_DEV` waits on a dead queue, and `DEL_DEV` blocks behind the
   kernel retrying IO to a server that is gone (`Buffer I/O error on dev ublkb68`).

**The lesson is about scaling a test, not about ublk.** A ublk device is a kernel
object with no force-remove: unlike `dmsetup remove`, there is no way back from
userspace once its server dies. The right sequence was 1, 4, 16, 64 with a leak
check at each step — and `ublks_max=64` had already been read during setup without
being allowed to bound the test. A limit you have seen and not encoded is a limit
you will exceed.

Run in that order afterwards, every step passed clean: 4/4, 16/16, 60/60, zero
devices left at each. The numbers that came out of it are in the ublk row above.

The provider now reads `ublks_max` from the kernel and refuses before allocating
anything, because refusing a create is recoverable and leaking a kernel object on a
shared host is not.

**One test was deleted rather than kept.** It launched 4× the limit concurrently and
checked that exactly `limit` were admitted — and it passed against a deliberately
broken `admit` that released the lock between the check and the increment, because
that window is a few instructions wide and the scheduler rarely lands inside it. A
test that passes against the bug it names reads as coverage. It now asserts the
invariant instead: `inFlight` never exceeds the limit, which broken arithmetic
violates whether or not the interleaving is ever observed.

## Attribution notes: the create and destroy paths, measured

Every figure here is from one 128-core host (AMD EPYC, Ubuntu 22.04, 503 GB) at
256 concurrent creates unless stated. They are recorded because each one overturned
something that had been asserted without measurement.

### Where a create's time goes

`runtime_create` used to be one opaque number covering almost the whole create, so
attribution stopped at "somewhere in here". It is now six sub-phases:

| phase | dm-snapshot | overlaybd |
|---|---|---|
| `fc_rootfs` | 3.809 s | 0.908 s |
| `fc_boot` | 0.133 s | 0.050 s |
| `fc_vmm_spawn` | 0.066 s | 0.025 s |
| `fc_api_ready` | 0.000 s | 0.002 s |
| `fc_cgroup` | 0.000 s | 0.000 s |
| `agent_ready` | 0.316 s | 0.306 s |
| **total** | **4.512 s** | **1.299 s** |

`fc_rootfs` is the ceiling, and only it scales with load: 0.863 s at n=16 against
3.809 s at n=256, while boot and spawn stay flat.

The cost is subprocess work, established by ruling the alternatives out on the host
rather than by reasoning. Raw dm-snapshot creation parallelises (10/s serial, 65/s
concurrent). `losetup --find` does not degrade as devices accumulate (26 ms per call
at 0, 100 and 200 existing devices). So it is the per-sandbox `fork`/`exec` of
`losetup` twice plus `dmsetup` once, at ~26 ms each. `attachTCMU` writes configfs
and forks nothing, which is why overlaybd is 4.2x faster on this phase.

### The `cores / cpu-per-create` claim was wrong

README, this file and `--create-wait`'s help text quoted a ceiling of `cores / 5`,
from a 16-core host when every create booted. Measured cost is **0.31-0.44 CPU-seconds
per create**, an order of magnitude less, and observed throughput is 0.16-0.28 of
what that cost predicts -- so host CPU is not the binding constraint at any
concurrency tested.

Two things were wrongly blamed for the gap before it was attributed, and both are
recorded so nobody re-runs them. The manager's mutex: the create path holds it only
briefly, and a test built to catch the 2N+1 access pattern passed against the broken
shape, so the test was deleted rather than kept. SQLite write contention: `TouchNode`
goes from 119 us to 25 ms under 300 concurrent writers -- 214x, and three orders of
magnitude short of mattering. Postgres was then measured at **half** SQLite's
throughput on the same run (47.7 vs 89.4 creates/s), which retires the idea that the
single-writer connection was limiting anything. Postgres is for multiple replicas
sharing one store, not for a faster single node.

### Destroy had a two-second floor that could never do anything

`killVMM` sent `SIGTERM`, waited up to 2 s for a graceful exit, then sent `SIGKILL`.
Firecracker installs a `SIGTERM` handler and does not exit on it: measured
`SigCgt: 0000000441801449` on a live VMM, still alive 3 s later, dead in 59 ms from
`SIGKILL`. Single destroy went 2184 ms -> ~200 ms once the futile wait was removed.

This is the second wait of exactly this shape on this path -- an ACPI poweroff wait
cost 5001 ms of a 5335 ms destroy and could never succeed either, because the guest
kernel has no `CONFIG_ACPI_BUTTON`. Both were invisible for the same reason: destroy
was a single number, so a fixed floor looked like the cost of tearing down a device.
`destroy_flush` and `destroy_network` are now separate, and are 0.418 s and 0.000 s.

**Still open**: destroys serialise at high concurrency. Wall time is linear in count
(31 -> 1968 ms, 64 -> 3445 ms, 128 -> 7372 ms) at a flat ~57 ms each, and 57 ms x 128
is the 7372 ms wall. It is not the kernel -- 10 serial backstore `rmdir`s take 15 ms
total -- and `Destroy` releases the runtime lock before tearing anything down, so the
serialisation is somewhere above configfs and has not been located yet.

### A node was declared LOST while healthy

Reconciliation ran synchronously before the heartbeat stream opened, and its duration
is unbounded: each orphaned device-mapper mapping costs `dmsetup remove --retry`
4.806 s before giving up, strictly serially. 109 orphans is 8.7 minutes before the
first heartbeat, against a 45-second lease. The log showed it plainly once read in
order -- registered at 20:19:54, lease expired at 20:20:44, no heartbeat between.

## Open gaps worth naming

- **Guest-side ENOSPC behaviour is unverified.** The host side is measured, but
  nobody has watched what a guest sees when its layer cannot allocate. If the
  guest does not report an error, a caller gets a sandbox that looks healthy while
  losing data — worse than a refused create.
- **`--track-dirty-pages` overhead is unmeasured**, so it defaults off even though
  incremental snapshots are implemented.
- **AVX-512 masking is unverified**: the test host is Zen 2 and has none, so five
  mask bits are correct per the CPUID spec but never exercised.
- **Cross-model restore within a family is unverified** — there is only one fc
  host, which is precisely what the CPU template exists to make possible.

Logging and CLI output standardisation used to be on this list and is now done: `slog` throughout
(92 call sites; the one remaining `log.Printf` is in `hack/tracedump`, a dev tool), levels, a
text/json handler switch, request ids on the context, and CLI `--json`, `--quiet` plus five exit
codes instead of two.

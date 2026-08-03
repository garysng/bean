# What is actually built

> 中文版:[zh/status.md](zh/status.md)
> Section status convention: [architecture.md](architecture.md) §0.
>
> **Authority order: code > this file > [decisions.md](decisions.md) > design docs.**
> When they disagree, the one on the left is right and the others are stale.

Every number here was measured on hardware. Where something is projected rather
than measured, it says so.

Measurement host unless noted: AMD EPYC 7542 (Zen 2), 16 physical cores, 24 GB,
guest kernel 6.1.102, Alpine 3.20.

## Delivered

| Area | | Notes |
|---|---|---|
| Lifecycle | ✅ | create → exec → cp → pause → resume → snapshot → restore → destroy |
| Images | ✅ | OCI pull and conversion to ext4, private registries (AES-256-GCM at rest), prewarm with image-affinity scheduling |
| Rootfs | ✅ | Shared read-only base + per-sandbox copy-on-write through device-mapper. **44 KiB of actual disk per sandbox** (see the note below on why other figures were quoted) |
| Snapshots | ✅ | Three kinds with different semantics — see below |
| Scheduler | ✅ | Two-level placement, hard filters, scoring, **commitments persisted** so replicas cannot double-place and a restart does not lose the ledger |
| Create queueing | ✅ | A burst larger than a node's create concurrency waits instead of being refused |
| Snapshot blobs on S3 | ✅ | SigV4 against the standard library, no AWS SDK; multipart upload and range reads |
| Tracing | ✅ | OpenTelemetry with W3C `traceparent` across gateway → noded → in-sandbox agent |
| Builds | ✅ | Dockerfile through BuildKit, and `commit` to freeze a running sandbox into a base image |
| Snapshot cache eviction | ✅ | High/low watermarks with LRU, and the cache's size is reported |
| Disk pressure | ✅ | Actual occupancy reported; a node stops admitting sandboxes below a floor |

## Not delivered

| | | |
|---|---|---|
| **Networking** | 📐 | **Sandboxes have no network at all.** The `vsock` link to the agent is a control channel, not data. Design in [network.md](network.md); the address pool is built, the plumbing is not. **Largest gap** — SWE-bench-style tasks need `pip install` |
| jailer / host cgroups | 📐 | The VMM runs as root in the host mount namespace. Hardware virtualisation is the boundary; defence in depth is thinner than it should be |
| Container tiers (runc/gVisor) | 📐 | microVM, plus a no-isolation `local` tier for development, are the only options |
| Volumes, port exposure, `fork` | 📐 | |
| Host resource reconciliation | 📐 | A crashed noded leaves dm mappings and sandbox directories behind |
| Postgres | ⚠️ | SQLite in use. There is no `Store` interface — `*store.Store` is a concrete type at every call site. What is true is that `database/sql` and the driver import appear only inside `internal/control/store`, so the SQL boundary is contained in one package; swapping the engine means changing that package, not extracting it from callers |
| Build logs and cancellation | ⚠️ | A build reports no progress and cannot be stopped |
| overlaybd lazy-pull | ⚠️ | **Verified working** (7 ms mount, 19.6% of layer bytes transferred to read a file) but not wired into the image provider — dm-snapshot is the live path |

## Measured latency

| Operation | Measured | Breakdown |
|---|---|---|
| create (image cached) | **952 ms** | 234 ms runtime + 770 ms to a reachable agent |
| create (cold image) | 5–10 s busybox … 2 m 45 s alpine on poor network | Almost entirely network. This is why prewarm is required rather than an optimisation |
| destroy | **214 ms** | Was 5.25 s — [decisions.md](decisions.md) §1 |
| snapshot (full) | 1.5 s, 15.5 MB | |
| snapshot (filesystem only) | **6109 B** | `--no-memory` |
| snapshot (incremental) | **298 KB** | `--base SNAP`, 52× smaller than full |
| restore | ~950 ms | `/snapshot/load` is 7 ms of it; the rest is unpacking |

### Snapshots are three kinds, not three sizes

| Kind | Flag | Size | Restore | Portability |
|---|---|---|---|---|
| full | *(default)* | 15.5 MB | resumes; process tree survives | pinned to CPU vendor + family |
| filesystem-only | `--no-memory` | 6109 B | boots fresh, files intact | **any CPU** |
| incremental | `--base SNAP` | 298 KB | resumes | pinned to CPU vendor + family |

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

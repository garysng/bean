# Pause / Resume / Snapshot Design

> 中文版:[zh/snapshot-resume.md](zh/snapshot-resume.md)

> The status-marker convention is defined in [architecture.md](architecture.md) §0; the state machine is in architecture.md §4.3. fc is the default main tier and the snapshot main path is FC-native
> snapshot; the container tier's (runc/runsc) checkpoint path serves the degraded/GPU scenarios.

> **Read §0 first.** Resume, restore and fork name three different operations, and the
> rest of this document assumes the distinction.

## 0. Resume, restore and fork are three different things

**Resume brings back the same sandbox. Restore creates a different one.** Everything
below follows from that sentence, and getting it wrong has cost real explanations more
than once.

| | **resume** | **restore** | **fork** |
|---|---|---|---|
| Starts from | a live firecracker process whose vCPUs are frozen | a snapshot blob on disk / in S3 | a *running* sandbox |
| Produces | the **same** sandbox, same id | a **new** sandbox, new id | N new sandboxes, new ids |
| Guest memory | never left host RAM | served on fault from the unpacked image (UFFD) | same as restore |
| Persistent object | none | a `snap_...` that outlives every sandbox made from it | none produced (use snapshot if you want one kept) |
| Cost | milliseconds — one `PATCH /vm {Resumed}` | **392 ms** on a node-local cache hit | restore's cost, minus the packing and transfer |
| Survives noded restart | ❌ the process dies with it | ✅ the blob is the state | ❌ derives from a live process |
| Crosses machines | ❌ bound to the process's host | ✅ that is the point | ❌ same node; cross-node goes through snapshot+restore |
| Fan-out (1 → N) | ❌ there is only ever one | ✅ **N independent sandboxes from one snapshot** | ✅ by construction |
| Constraint | none beyond "the process is still there" | pinned to the CPU vendor+family the memory was captured on | as restore |
| Pairs with | `pause` | `snapshot` | — |

### Why the distinction matters

**Fan-out is only possible with restore.** One snapshot restored N times gives N
independent sandboxes, and that is the core evaluation workload: set up an environment
once, then run N experiments against it that must not see each other's writes. Resume
cannot do this at all — a paused sandbox is one sandbox, and resuming it gives you that
one sandbox back. Reference counting reflects exactly this: `ref_count` on a snapshot is
a **counter, not a flag**, because several restores of one snapshot are a normal
occurrence rather than a conflict (`AcquireSnapshot`, `store.go`).

**Pause/resume is a cost mechanism, not a scaling one.** It exists so an idle sandbox
stops burning CPU while staying instantly available. It frees no memory — the scheduler
still accounts for the full allocation (§2) — so it trades CPU for latency, and nothing
else.

**Conflating them makes every performance number incoherent.** The competitive
"~100 ms start" claims in [competitive-analysis.md](competitive-analysis.md) refer to
**restore**. They are not create (a real boot: 952 ms and 5 CPU-seconds,
[status.md](status.md)) and they are not resume (which is a vCPU unfreeze and therefore
faster than any of them, while doing far less). Quoting a resume latency against a
competitor's restore latency compares an unfreeze to a machine build.

## 1. Capability Tiers

| Capability | Semantics | Implementation | Status |
|---|---|---|---|
| **pause/resume (same node)** | freeze execution, keep memory and state | fc: pause vCPU | ✅ |
| **snapshot** | full state persisted to S3, the sandbox can be destroyed | fc: memory + CoW extent list | ✅ three variants |
| **restore (cross-node)** | rebuild a **new** sandbox from a snapshot on any node | UFFD page serving + CoW backfill | ⚠️ see below |
| **fork (millisecond clone)** | one parent many children, agent branch exploration | shares restore's mechanism; only the API is missing | ⚠️ see §4.5 |
| **container-tier checkpoint** | CRIU / gVisor save | — | 📐 unimplemented |

⚠️ **Cross-node restore is unmeasured**: there is only one fc machine. Logically it holds
(the snapshot records the CPU that produced it, and the scheduler hard-filters on
vendor+family), and a 409 has been verified to come back correctly by "rewriting the
snapshot record to pose as GenuineIntel" — but "same family, different model really does
restore" has no empirical backing, and that is exactly why
`--cpu-template portable` exists. See `docs/decisions.md` §3.6.

> Phases all refer to the fc main path; the container-tier implementations in the table (freezer/CRIU/gVisor save) arrive with the P5 container tier.

## 2. Pause / Resume (lightweight, nothing written to disk) ✅

```
POST /sandboxes/{id}/pause      # can also be triggered automatically by lifecycle.onIdle=pause
  fc tier:        FC API PauseVM (vCPUs stop, memory kept as is), within hundreds of ms
  container tier: cgroup.freeze = 1 (cgroup v2 freezer, atomically freezes the whole process tree)
  both:           network kept, the agent is frozen along with it
Waking (platform default behaviour): a PAUSED sandbox receives an exec/port/file request → automatic resume → forward
  (the explicit resume API still exists; callers usually never notice)
POST /sandboxes/{id}/resume
  fc tier:  ResumeVM; container tier: cgroup.freeze = 0 — both return to RUNNING sub-second
```

- Memory is not released while frozen — the scheduler still accounts for its memory.max
  (preventing an OOM on resume after overcommit); use snapshot if you want to release the
  memory allocation
- PAUSED is retained indefinitely by default (the global reclamation policy is off by
  default, see api-design §5.2); P4 introduces snapshot archiving to free RAM
- A request against a PAUSED sandbox triggers a transparent wake (blocks until resume, and
  only returns 502 on timeout — consistent with api-design §5.2)
- The proxy returns 502 + Retry-After for PAUSED

## 3. Snapshot

### 3.0 fc tier (main path) ✅

```
POST /sandboxes/{id}/snapshot   {includeMemory?, base?, keepRunning?}
1. PauseVM (both snapshot kinds need it — without memory, pause is still the precondition
   for a filesystem-consistent result: a guest writing to the device while we read it will
   put torn writes into the snapshot)
2. FC CreateSnapshot: snapshot_type = Full | Diff
   Diff requires the guest to have had track_dirty_pages enabled at boot (see below)
3. Build the bundle (gzip tar stream):
     vmstate       as is
     memory        as is for a full snapshot (dense, FC writes every byte)
     memory.diff   by extent list for an incremental one (sparse, dirty pages only)
     rootfs        the CoW layer by extent list — a large supply that is barely used;
                   emitting the full 20 GiB of zero bytes to the compressor measured a 15s pause
4. Stream it to S3
5. keepRunning=true (default) → ResumeVM
```

**Three snapshot variants, differing in semantics and not just in size:**

| Variant | Parameter | Measured size | restore behaviour | CPU constraint |
|---|---|---|---|---|
| full | default | 15.5 MB | resume, the process tree survives | bound to vendor+family |
| filesystem only | `--no-memory` | **6109 B** | boots afresh, files kept | none |
| incremental | `--base SNAP` | **298 KB** | resume | bound to vendor+family |

- Whole-machine consistency: the TCP stack, fds and process tree are all frozen together
  inside the guest, so none of CRIU's external-state problems apply
- `--no-memory` buys **portability** and not just size: guest memory records what it read
  from the CPU, and vendor/family cannot be masked away (`docs/decisions.md` §3.6), so a
  snapshot carrying memory can only land on a compatible CPU; the scheduler hard-filters,
  and an incompatible one returns 409 `INCOMPATIBLE_CPU`
- `--base` only means something for memory: the filesystem layer is already O(changed) — CoW
  stores only the changed blocks

**The ordering in restore is load-bearing** — there was once a bug here that silently
corrupted the filesystem:

```
1. The control plane takes every layer along the base chain (store.SnapshotChain, ordered
   base-first), declares snapshot_chain in the spec, streams layer by layer and delimits
   with layer_end
2. The node first lands the bundle in a staging directory — **without touching any block device**
3. Merge the memory images along the chain: the base is written out in full, each layer's
   diff is applied on top (snapmerge_linux.go)
4. Images.Prepare(SeedWritable: backfill the CoW)
   ↑ the backfill happens **before** dmsetup create
5. LoadSnapshot(Uffd backend) → resume the vCPUs
```

**Why step 4's position cannot be moved**: dm-snapshot reads the exception table into kernel
memory at the moment the device is activated and never reads it back afterwards. Writing
into the CoW backend of an already-activated device leaves the kernel unaware of those
chunks, and the device keeps serving the base image. And on a full snapshot this failure is
**completely silent** — reads hit the page cache the memory snapshot brought back with it,
and after `drop_caches` the same file reads back as all zeroes, while `ls` still shows the
correct size, with no EIO and nothing in dmesg. See `docs/decisions.md` §3.0.

**The merge is materialised at restore time, not layered on the page-fault path**: E2B takes
the latter (chasing K BuildIds on a fault), at the cost of fragmentation growing with depth;
we materialise and then hand it to the existing UFFD handler, so the **page-fault path is
unchanged** — that is the hottest code in the system and the place where mistakes hide best.
The merge result goes into snapCache keyed by leaf id, so a fan-out pays for it once per
node. Past a chain depth of 8 it turns into a full snapshot automatically.
The option comparison is in `docs/decisions.md` §3.0.1.

**`track_dirty_pages` must be enabled before boot and is not stored in the snapshot**, so it
is a node configuration (`--track-dirty-pages`, off by default) rather than a snapshot
parameter. A guest without it that requests a diff **errors out explicitly** rather than
downgrading to full — the caller would think they saved space when they did not, and size
alone does not explain why.

**Deleting a base that has descendants returns 409**: a diff depends on its ancestors, and
deleting one invalidates the whole chain, while the failure is far away in both time and
space (deletion succeeds now, and restore fails later on a different machine).

**Restore's memory never hits disk (shipped, measured)**: use Firecracker's `Uffd` memory
backend rather than `File`. `File` reads the entire memory image in before the guest starts
running, at a cost proportional to guest size and independent of "how much was actually
accessed" — 1303ms on a 512 MiB guest. Switching to userfaultfd makes the guest memory an
anonymous mapping served by the handler on fault, and `/snapshot/load` drops to **7ms**.
e2b / agentenv / tensorlake all do it this way.

**Unpacking a bundle happens once per snapshot (shipped)**: every restore of the same
snapshot unpacks byte-identical content, so vmstate + memory are cached by snapshot id.
Safety comes from Firecracker mapping the memory file `MAP_PRIVATE` (measured: the host
file's md5 is unchanged after the guest writes 64MB). That `MAP_PRIVATE` is Firecracker's own
copy-on-write semantics for the guest; it is not the same mapping as the UFFD handler's
read-only `MAP_SHARED` (§below), which is what shares one page-cache copy across VMs.
**The writable rootfs is not cached** — two sandboxes restored from the same snapshot
diverge the moment either writes.

Measured: the first restore is ~950ms (paying the unpack cost), and every cache hit after that
is 392ms. The remaining cost is transferring the bundle from the gateway and gunzipping it just
to get that one rootfs member — unoptimised.
- fork ⚠️ **the mechanism ships, the API does not** (§4.5). The intended surface is
  `POST /sandboxes/{id}/fork {count}`: an instantaneous CoW snapshot + N LoadSnapshot calls
  → one parent many children, producing no persistent snapshot object (use /snapshot if you
  want one kept). Until it exists, `snapshot create` + N × `run --snapshot` has the same
  semantics through a persistent object.
  Every host-side resource for a child instance is allocated fresh: tap device, vsock CID,
  writable-layer CoW clone, new sandbox-id/token; the MAC/IP inside the guest is
  reconfigured by the agent. The optimal implementation of "set up the environment once,
  fan out N experiments"; the first pass forks on the same node, and cross-node goes through
  snapshot+restore
- balloon interaction 📐 **the balloon device is not wired up**: shrink the balloon before a
  snapshot (reducing the memory file), and after restore the balloon state comes back with
  the vmstate
- Limitations: the host CPU generation has to be compatible (the scheduler groups by CPU
  feature set); not applicable to GPU (GPU goes to the container tier)

### 3.1 Container tier (degraded/GPU scenarios) 📐

#### Composition

One snapshot = three parts, committed atomically:

```
s3://bean/snapshots/{snapId}/
├── manifest.json        # metadata: source image digest, isolation, resources, env,
│                        #   agent version, creation time, checksum of each part
├── checkpoint/          # process state: CRIU images (runc) or gVisor save files (runsc)
│                        #   multipart upload, zstd compressed
└── rootfs-diff.tar.zst  # writable-layer diff (containerd snapshotter exports the upper layer)
```

The base image does **not** go into the snapshot — the restoring node fetches it through the
regular image path (overlaybd/S3), and the diff holds only the increment. In eval scenarios
the diff is usually small (tens of MiB).

#### runc tier: CRIU

```
Flow (executed by noded):
1. Set state to SNAPSHOTTING; freeze first (for consistency)
2. criu dump --tree <pid1> --leave-frozen: process tree + memory pages + fd table + unix sockets
3. containerd snapshotter exports the upper layer → tar.zst streamed to S3 (presigned)
4. Package the criu images and push to S3
5. Write the manifest, commit to the control plane; resume or destroy the original sandbox
   according to the request parameters
```

Known CRIU limitations (documented explicitly for users):

| Limitation | Handling |
|---|---|
| External TCP connections | `--tcp-established` can save them but the peer is long gone — the application reconnects after restore; explicitly not guaranteed |
| GPU state | Not checkpointable. A sandbox with a GPU refuses snapshot (400) |
| Mount-point consistency | The restoring node reproduces the same mount topology (agent mount, resolv.conf and so on are rebuilt from the spec) |
| /dev/shm, large memory | Memory pages are written out in full, 10 GiB of memory ≈ minutes; snapshot is a heavy operation, noted in the API docs |
| The agent process | The agent itself is checkpointed along with everything else; after restore its in-memory state comes back and noded reconnects the socket (the agent already supports transport rebuild) |

#### runsc tier: gVisor save/restore

gVisor ships whole-sandbox-level save/restore (`runsc checkpoint/restore`), more reliable
than CRIU (the userspace kernel state is self-contained):

- The checkpoint produces a single state file → the same three-part layout
- Similar limitations (external TCP, GPU); restore across gVisor versions is not guaranteed — the manifest records the runsc version, and the restoring node refuses with a message on a version mismatch
- Within the container tier: runsc takes the gVisor native path (more reliable), and CRIU serves only the runc tier, best-effort

#### Restore (cross-node, container tier)

```
POST /sandboxes { "snapshot": "snap_...", ... }
1. Scheduling: pick a node by base-image affinity (the digest is in the manifest)
2. In parallel: pull the base image (likely a cache hit) + pull rootfs-diff + pull checkpoint
3. The snapshotter assembles the rootfs: base + the diff unpacked as a new upper layer
4. Network rebuild: a new IP (IP stability is explicitly not guaranteed, documented), netns/nftables as usual
5. runsc restore / criu restore → RUNNING
6. noded reconnects the agent socket
```

Target restore latency: P50 < 15s for diff+checkpoint under 1 GiB (parallel multipart pull from S3).

### 3.5 Interaction between volumes and snapshots 📐

- **shared-fs volumes**: the guest kernel's NFS client holds a TCP connection to the host,
  and after a whole-machine snapshot that connection is certain to die on a cross-node
  restore. The flow: before the snapshot the agent receives a command to unmount (lazily)
  every NFS mount point → snapshot → after restore the agent mounts again (against the new
  host's gateway address). An unmount failure (fd in use) → the snapshot fails with an
  explicit error
- The snapshot manifest records the full volume mount table (once dataset volumes are enabled: re-attach the block devices per the manifest)

### 3.5.5 Reclaiming the node-local unpack cache ✅

`snapCache` lets multiple restores of the same leaf reuse one unpacked memory image, and
that is why fan-out is cheap. The cost is that it **takes roughly one more guest's worth of
memory for every new snapshot restored**, and that space counts against no committed
quantity at all — the scheduler cannot see it, so a node can fill its disk while the books
still look healthy. Measured accumulation on one dev machine: 4.6 GB / 9 entries.

High/low watermarks + LRU, off by default (`--snapshot-cache-high-mib` /
`--snapshot-cache-low-mib`):

| Mechanism | Value | Reasoning |
|---|---|---|
| Trigger/reclaim lines come in pairs | low defaults to 80% of high | Copied from kubelet image GC. A single threshold makes every restore after the trigger pay for a reclamation |
| Size measured by allocated blocks | `st_blocks * 512` | The merged image is sparse, and going by nominal size would evict entries in order to reclaim zero bytes |
| Eviction order | directory mtime, `Touch` on hit | Under relatime, atime updates only once a day, so hot entries look cold |
| Deletion method | rename into a temp directory first, then delete | An in-place delete passes through a state where "the vmstate is gone but the memory image is still there", and a `Lookup` hitting that would consider the entry usable |

**The pin only protects the `Lookup`→`open` window.** A restore gets the path first and
opens the memory image a while later; if it is evicted between those two points, the open is
an ENOENT, and by then the stream has been consumed and there is no way to rebuild it. Once
open it is safe — unlinking a file that is already mmapped does not affect reads (verified by
measurement, decisions §3.7), so `stage.Close()` releases the pin without waiting for the VM
to end.

Reported as `bean_node_snapshot_cache_bytes` through the optional `runtime.CacheReporter`
interface — space that counts against no allocation should at least be visible.

### 3.6 Lifecycle (common to both tiers) ⚠️

- A snapshot is an independent object with an independent quota (total bytes per key); TTL is optional, with S3 lifecycle as the backstop
- Reference counting: a snapshot with a RESTORING in flight cannot be deleted. The count is a **counter, not a flag**, precisely because concurrent restores of one snapshot are the expected case
- The same snapshot can be restored many times, each producing an independent sandbox → this is the fan-out that "set up the environment, snapshot once, run N experiments" needs, and the core value point in eval scenarios. Resume cannot substitute: it returns one sandbox, the one that was paused

## 4. Comparing the two tiers, and the unified interface ⚠️

| Dimension | Container tier (CRIU/gVisor save) | fc tier (main path) |
|---|---|---|
| Consistency | Process-level, external state best-effort | Whole-machine (memory + devices + vCPU) |
| Speed | Minutes (large memory) | pause in hundreds of ms; diff snapshot incremental |
| restore | Rebuild the process tree | load snapshot + resume vCPUs, hundreds of ms |
| fork clone | Not supported | CoW memory → one parent many children (AgentENV demonstrated 16 children on a single node) |

Unified interface (invisible to the user):

- The Runtime interface's Checkpoint/Restore signatures are common to both tiers (io.Reader/Writer streaming)
- The manifest's `runtime` field distinguishes the format, and restore scheduling matches node capability by format
- The snapshot API semantics are identical; the tier difference shows up only in speed and in whether fork-style fan-out is available

## 4.5 fork: the mechanism exists, the API surface does not ⚠️

> **What is missing is one API call, not a capability.** Restore already *is* fork:
> `snapshot create` followed by `run --snapshot` run N times gives N independent
> sandboxes sharing one memory image. Every mechanism a fork needs is shipped and
> measured. What does not exist is a single call against a running sandbox that does
> both steps and skips the round trip through a persistent object.

### Restore is already fork/clone semantics ✅

Three properties together are the definition of a fork, and all three are in the code:

| Property | Where | Evidence |
|---|---|---|
| The immutable part is **shared** | `uffd_linux.go` | The memory image is mapped `PROT_READ \| MAP_SHARED`, "so several VMs restored from one snapshot use one page cache copy rather than one per VM" |
| The mutable part is **copied per instance** | `fc_lifecycle_linux.go` | `snapshotState`: "The writable layer is always extracted... because it cannot be shared: two sandboxes restored from one checkpoint diverge as soon as either writes" |
| The result is **N independent instances** | `store.go` | `ref_count` is a counter, not a flag; `AcquireSnapshot` increments it and its comment speaks of restores in the plural |

Sharing what is immutable while privately copying what is mutable, yielding instances
that cannot observe one another, is what a fork is.

Verified empirically, not just by reading: `hack/restore-repeat-check.sh` restores one
snapshot several times and, **after `drop_caches`**, each restored sandbox reads back its
own marker (the drop matters — a page-cache read passes even against a device serving the
base image, [decisions.md](decisions.md) §3.0). The unpack cache's eviction check makes
the same assertion across six restores ([tech-stack.md](tech-stack.md) §3.2).

The node-local path is built for this shape rather than merely tolerating it. `snapCache`
merges a chain once per node and every later restore of that leaf skips it; the comment on
that branch calls the case by name — "this is what makes a fan-out cheap".

### What a fork API would actually add

Not sharing, and not independence — those exist. Only these:

- **One call instead of two.** `POST /sandboxes/{id}/fork {count: N}` in place of
  `snapshot create` then N × `run --snapshot`.
- **No persistent object.** A fork produces no `snap_...` to name, quota, reference-count
  or reclaim. Today the intermediate snapshot has to be created and then deleted.
- **No pack/transfer round trip.** The parent is on this node and its memory is already
  resident, so a same-node fork can skip bundling to S3 and reading it back.

Everything below in this section is about that surface, and about the two numbers worth
measuring before it ships.

### Why one memory image can be shared by N child instances ✅ (the mechanism is verified)

This is the entire reason fork is cheap, and it is already load-bearing inside snapCache:

```
UFFD handler:  mmap(PROT_READ | MAP_SHARED)     ← we only read
Firecracker:   guest memory is an anonymous mapping
Page serving:  UFFDIO_COPY *copies* pages into the guest's anonymous pages
```

A guest writing its own memory writes its own anonymous pages and **never touches our image
file** — measured as "the host file's checksum is unchanged after the guest writes 64MB"
(the comment in snapcache_linux.go).

So **one unpacked memory image can serve arbitrarily many instances** with no copying.
That is exactly what snapCache relies on today to let multiple restores of one snapshot reuse
one memory image; fork just turns "multiple restores" into "one derivation of N".

### What every child instance has to allocate fresh

| Resource | Shareable | Reasoning |
|---|---|---|
| Memory image | ✅ shared | See above, guaranteed by UFFDIO_COPY semantics |
| vmstate | ✅ shared | Loaded read-only |
| **CoW layer** | ❌ new for each | It diverges on the first write, and that is the definition of a sandbox |
| **vsock UDS path** | ❌ separate for each | The path is relative to the sandbox directory (vm-assembly §5) |
| sandbox id / token | ❌ separate for each | Identity |
| dm mapping name | ❌ separate for each | `bean-<id>`, a flat namespace |

`guestCID` and the vsock port can both be constants, because every VM has its own vsock
namespace (vm-assembly §7) — which spares fork one layer of allocation.

A restored guest also has a MAC/IP that must line up inside the guest; because the
tap keeps the same name across a restore and the address rides on the restore
override (network.md §4), this resolves without per-fork reconfiguration.

### Relationship to snapCache

fork is inherently "one leaf being restored many times" — **precisely the scenario snapCache
was designed for**. So fork's implementation should reuse it rather than build a second path:
the first child fills the cache and the rest hit it directly.

`Fill`'s semantics already support this: every caller is invoked (to handle its own CoW
layer), but only one actually builds the shared entry. That semantic was pinned down while
fixing a bug — the waiter originally returned the cached entry directly without running the
callback, and so neither drained its own stream nor staged its own writable layer.

### Risks that need load testing

**Will N child instances faulting at the same time swamp the UFFD handler?**

The handler's `serve()` today is **a loop in a single goroutine**: read one fault event,
`UFFDIO_COPY` one page in, continue. Every VM has its own handler instance, so N forks are N
handlers — they do not share that loop.

But they do share:
- The same mmapped image (which is good at the page-cache level: there is only one copy)
- The host's memory bandwidth and page-fault handling capacity

So the risk is not in the handler's serialisation but in **the host's fault throughput when N
guests cold-start at once**. That number has never been measured — it falls within the scope
of the GitHub #18 load testing, and it is something to know before fork ships.

**The second risk**: N forked instances take N times the memory committed quantity, while
their actual RSS is far lower because pages are served on demand. That headroom is exactly
what overcommit (noded-design §3.2) wants to exploit, but memory overcommit is off by
default today — so forking N will consume N times the committed quantity against the
allocation, possibly far more conservative than what is actually needed.

### Suggested implementation order

1. Measure those two numbers above first (fault throughput; the gap between RSS and committed quantity)
2. Then implement fork — because if fault throughput is the bottleneck, fork's API shape may
   need to carry a concurrency limit

## 5. API summary (restated) ⚠️

```
POST   /v1/sandboxes/{id}/pause              ✅
POST   /v1/sandboxes/{id}/resume             ✅
POST   /v1/sandboxes/{id}/snapshot           ✅
       { "name", "labels", "keepRunning": true,
         "includeMemory": true,   # false = filesystem only, lands on any CPU but boots afresh
         "base": "snap_..." }     # stores only memory changed since base; requires includeMemory
GET    /v1/snapshots?label=...&state=...     ✅
GET    /v1/snapshots/{id}                    ✅
DELETE /v1/snapshots/{id}                    ✅  409 SNAPSHOT_IN_USE when it has descendants
POST   /v1/sandboxes { "snapshot": "snap_..." }  ✅  restore: a NEW sandbox, new id.
                                                    409 INCOMPATIBLE_CPU on an incompatible CPU.
                                                    Call it N times for a fan-out of N
POST   /v1/sandboxes/{id}/fork               ⚠️  no API; the mechanism is the two calls
                                                 above (§4.5)
```

Note that `pause`/`resume` act on `{id}` and return the same sandbox, while restore is a
`POST /v1/sandboxes` — a creation — and returns a different one. The URL shapes say so.

CLI: `bean snapshot create SBX [--name N] [--no-memory] [--base SNAP] [--no-keep-running]`,
`bean snapshot ls|rm`, `bean run --snapshot SNAP` (restore: a new sandbox each time),
`bean pause SBX` / `bean resume SBX` (the same sandbox). There is no `bean fork`.

The SDK shape is in [sdk-cli-design.md](sdk-cli-design.md).

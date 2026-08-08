# Live Migration Feasibility — Technical Report

> Status: 📐 **research / design-only.** Nothing in this document is built. It
> assesses whether live migration of a running sandbox from node A to node B is
> feasible on bean's existing machinery, what it would take, and where the hard
> limits are. Authority order still holds: code > `status.md` > `decisions.md` >
> design docs > this report.

> 中文版:[zh/live-migration.md](zh/live-migration.md)

---

## 0. The question, stated precisely

**Live migration** here means: move a *running* sandbox from host A to host B so
that, from the guest's and the client's point of view, it keeps running — the
process tree survives, in-guest state is preserved, and downtime is short enough
that open connections and the workload do not notice a failure.

This is a stronger claim than what bean does today. Today the cross-machine path
is **snapshot + create-from-snapshot**: capture a durable blob on A, create a
*new* sandbox on B from it. That is cold — it produces a different sandbox with a
new id, and the source is expected to stop. Live migration is the same physics
(move memory + disk + device state across a network) with two added
requirements:

1. **Continuity** — same sandbox identity, surviving connections, no client-visible restart.
2. **Bounded downtime** — the stop-the-world window is milliseconds, not the full transfer time.

The rest of this report measures the gap between "snapshot + restore on another
machine" (which works) and those two requirements.

---

## 1. What bean already has (the primitives)

Live migration is not one feature; it is an orchestration over primitives most of
which bean already ships for snapshot/restore/fork. Audited against the code:

| primitive | where | state | migration relevance |
|---|---|---|---|
| Consistent capture | `Checkpoint` — `fc_lifecycle_linux.go:155` (pause → `/snapshot/create` → sparse tar bundle) | ✅ shipped | the stop-and-copy step of any migration |
| Three snapshot kinds | full / `--no-memory` / `--base` diff — `fc_lifecycle_linux.go:178-207` | ✅ shipped | diff is the raw material for iterative pre-copy |
| Dirty-page tracking | `--track-dirty-pages`, `EnableDiffSnaps` — `fc_lifecycle_linux.go:510` | ⚠️ off by default, cost unmeasured | the convergence signal pre-copy needs |
| Exact revival via UFFD | `uffd_linux.go:47-101`, `/snapshot/load ResumeVM` | ✅ shipped, `/snapshot/load` = 7 ms | the target-side page-in engine |
| Shared read-only memory image | `MAP_SHARED` — `uffd_linux.go:93-97` | ✅ shipped | one page-cache copy across VMs (fork) |
| Diff-chain merge | `snapmerge_linux.go:37-139` | ✅ shipped | flattening a base + diffs into one image |
| Bundle cache, per snapshot id | `snapcache_linux.go` | ✅ shipped | 950 ms first unpack, 392 ms after |
| CoW rootfs | dm-snapshot `devmapper_linux.go`; overlaybd/TCMU `obdtcmu_linux.go` | ✅ / ⚠️ | local shared base + per-sandbox CoW |
| Address preservation | restore leaves `NetworkOverrides` empty on purpose — `fc_lifecycle_linux.go:513-520` | ✅ confirmed | the guest comes back with its IP/MAC |
| Per-sandbox netns | `network.md` §1, `runtime/netns_linux.go` | ✅ confirmed | identical addresses coexist across nodes |
| CPU hard-filter | `INCOMPATIBLE_CPU` — `cpucompat.go:33`, 409 in `snapshots.go:366` | ✅ shipped | the landing constraint for a memory guest |
| CPU template pre-boot | `cpu_template.go` | ✅ shipped | the portability lever across machines |
| S3 blob transfer | multipart + range + SigV4 — `s3blobs.go`, `s3/` | ✅ shipped | moving the capture between machines |

The address story is worth calling out: because every sandbox has its own netns
and the tap is named `beantap0` in each, a restored guest's snapshot already
points at the right device in its new netns — restore deliberately does **not**
override the network config (`fc_lifecycle_linux.go:513-520`). "The restored
guest keeps its IP/MAC" is exactly what reviving the same guest on another
machine needs, and it is nearly free here.

## 2. The gap — what live migration adds that bean does not have

There is **no live-migration, pre-copy, or post-copy code anywhere** (a full-repo
search finds only DB schema migrations and CPU-template comments). Today's
cross-machine model is: full capture → **via S3** → target fetches the whole
chain → local UFFD revival. That is snapshot + create-from-snapshot, and it is
*cold*: a new sandbox, new id, source expected to stop. The specific missing
pieces:

1. **No iterative pre-copy loop.** Diffs exist as a *storage* concept (capture a
   delta against a stored base), but nothing reads the dirty bitmap repeatedly on
   a *running* guest to converge across rounds. The bitmap is reset at load
   (`fc_lifecycle_linux.go:510`) and each diff is deleted after apply
   (`snapmerge_linux.go:139`). Pre-copy needs "sample dirty pages, ship them,
   repeat until the working set is small."
2. **UFFD serves only from a local file.** `newUffdHandler(uds, memImagePath)`
   opens a local path and mmaps it (`uffd_linux.go:79-101`). There is **no
   network page source** — the image must fully land on the target first. Post-copy
   (target runs while pulling pages from the source) needs a remote page channel.
3. **Blobs relay through S3, not source→target.** First-byte latency is bounded
   by object storage, not a direct node-to-node link.
4. **Downtime is unbounded.** Checkpoint pauses for the whole write-out; there is
   no "final small diff + millisecond stop-the-world cutover" path.
5. **VMM-side dirty pages are not tracked** (see §4 — the primitive bean is
   missing that Cloud Hypervisor had to build).

## 3. What the industry actually does

**Firecracker upstream does not do live migration, by choice** — Discussion
[#3119](https://github.com/firecracker-microvm/firecracker/discussions/3119):
microVMs boot and snapshot in milliseconds, so upstream ships snapshot/restore as
"a simpler thing that covers most of live migration's uses" and declines classic
in-line memory migration. Everyone's "migration" is built on top of snapshot +
UFFD, and almost all of it carries an explicit pause — nobody in this space is
doing zero-downtime pre-copy on Firecracker.

- **E2B** — pause writes a full snapshot (Firecracker snapfile + memory diff +
  rootfs diff, stored as content-addressed blocks); resume revives on another
  machine via UFFD lazy loading. This is bean's model almost exactly.
- **fly.io** — suspend/resume dumps full memory state to persistent storage;
  cross-host machine migration reuses that snapshot mechanism plus dm-clone/iSCSI
  for the volume. Explicitly *not* classic live migration — the machine stops and
  is rebuilt.
- **Morph** — Infinibranch snapshots/branches a whole environment (<250 ms
  claimed, but that is snapshot/branch, not cross-host migration downtime).
- **gVisor (runsc)** — its own checkpoint/restore (not CRIU, because the Sentry
  already holds the state). But a snapshot restores only under the *same runsc
  binary*, and network connections and GPU state are not preserved — used for
  fast-start/pooling ([Tencent agentic-RL at millions](https://gvisor.dev/blog/2026/04/23/scaling-agentic-rl-sandboxes-to-the-millions-with-gvisor-at-tencent/),
  Modal sub-second), not cross-host live migration.
- **runc + CRIU** — real container live migration exists but is fragile:
  established TCP needs `--tcp-established`, and redirecting an *already-open*
  connection to a new host is the known unsolved edge ([CRIU #1598](https://github.com/checkpoint-restore/criu/issues/1598)).

**The one direct reference is Cloud Hypervisor** — same rust-vmm lineage as
Firecracker, and it *does* ship production pre-copy live migration
([docs](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/live_migration.md)),
with userfaultfd-based remote post-copy added in
[v53.0](https://www.cloudhypervisor.org/blog/cloud-hypervisor-v53.0-released/).
It proves the path is viable on this VMM architecture; Firecracker's abstention is
a product decision, not a technical wall.

## 4. The mechanics, and the one lesson from Cloud Hypervisor

**Pre-copy** keeps the source running: round 0 copies all memory, each later round
re-ships only the pages dirtied since the last, until the working set is small
enough to stop, copy the residue plus vCPU/device state, and cut over. Downtime is
just that final stop-and-copy — tens to hundreds of ms. It converges only when
transfer bandwidth `B` exceeds the dirty rate `R`; a write-heavy guest with
`R ≳ B` never converges and must be forced into cutover (downtime spikes) or
switched to post-copy.

**Post-copy** cuts over first: pause the source, ship the minimal CPU/device
state, start the target immediately, and pull pages on demand as the guest faults
on them — exactly what userfaultfd is for. Downtime is decoupled from memory size,
but a fault now crosses the network, and if either side or the link dies
mid-migration the guest is unrecoverable (its memory is split across two hosts).
Production systems (QEMU) do **pre-copy first, post-copy as the fallback**.

**The lesson bean's own audit could not surface, from Cloud Hypervisor
([#2458](https://github.com/cloud-hypervisor/cloud-hypervisor/issues/2458)):
dirty pages have two sources.** KVM's dirty log captures what the *guest/vCPUs*
write. But the *VMM itself* writes into guest RAM too — VIRTIO device emulation,
DMA — and those writes are invisible to KVM. A migration that tracks only the KVM
log ships a subtly incomplete memory image. bean's `--track-dirty-pages` is the
KVM-log half; the VMM-side tracking is a second thing that would have to be built.
This is the kind of trap that only shows up in a VMM that has actually shipped
migration, which is why Cloud Hypervisor is the reference to read.

## 5. A path that fits bean's primitives

The gap is not primitives — it is the orchestration layer that connects them: a
dirty-page iteration loop and a node-to-node page channel. Ordered by how well
each stage reuses what already ships:

**Stage 0 — cross-node cold move (mostly already possible).** snapshot →
transfer → create-from-snapshot on B, with the source stopped. This is today's
capability with a thin "then destroy the source" wrapper and the source's id
retired. Downtime = capture + transfer + revive (seconds). Honest framing: this
is *relocation*, not live migration — new id, dropped connections — but it is the
zero-new-mechanism baseline and matches fly.io/E2B's shipped behaviour.

**Stage 1 — direct node-to-node transfer.** Replace the S3 relay with a
source→target channel for the bundle (the gRPC `RestoreSandboxFrame` streaming
already exists for chains; point it node-to-node). Cuts first-byte latency. Still
cold, but the transport is now what a live path would use.

**Stage 2 — post-copy revival (the highest-leverage step).** Extend the UFFD
handler from "serve from local file" to "serve from a remote source node":
capture minimal state, start the guest on B, and let `uffd_linux.go`'s fault path
pull absent pages from A over the Stage-1 channel instead of a local mmap. bean
already owns the target-side fault machinery — the increment is a remote page
source plus the source-side server. This is Cloud Hypervisor v53.0's exact move
and the direction bean's primitives fit best. **Risk to design for up front:** an
unanswered fault hangs Firecracker forever (`decisions.md:143`), so a mid-migration
network failure must have an explicit abort-and-recover, not a hang.

**Stage 3 — iterative pre-copy + bounded cutover.** Build the loop bean lacks:
read the dirty bitmap on the running guest, ship deltas over the Stage-1 channel,
repeat until the residue is under a threshold or a round cap is hit, then a short
stop-and-copy. Requires keeping the bitmap live across rounds (not resetting at
load, not deleting each diff) and — per §4 — adding VMM-side dirty tracking.
Pair with Stage 2 as the non-convergence fallback for write-heavy guests.

**Also worth doing early: same-node live upgrade.** Cloud Hypervisor's `--local`
and fly.io's in-process handoff both hand the guest between VMM processes on one
host. For a sandbox platform this enables noded upgrades without dropping
sandboxes, reuses the snapshot primitives directly, and is far simpler than the
cross-host path — a good place to prove the machinery.

## 6. Hard limits and risks

- **CPU compatibility is a hard filter, not a tunable.** A memory-carrying
  snapshot only revives on the same vendor + family; vendor and family cannot be
  masked (`decisions.md:382`), so the scheduler rejects with 409
  (`cpucompat.go:33`). Migration targets are constrained to a CPU-compatible pool,
  and the CPU template must be chosen at boot — it cannot be applied retroactively
  to a running guest. Cross-*model* restore is also unverified today
  (`status.md:290`, one fc host), and model is deliberately not persisted.
- **An unanswered UFFD fault hangs Firecracker forever** (`decisions.md:143`).
  Post-copy makes the fault source a remote node, so the failure mode of a network
  partition is a hung guest unless there is explicit liveness monitoring and an
  abort path. This is the single largest reliability risk in Stage 2/3.
- **Convergence is not guaranteed.** Write-heavy guests can dirty faster than the
  link ships; pre-copy then needs a round cap or dirty-set threshold to force
  cutover, trading downtime for termination. Post-copy is the fallback but carries
  the split-memory failure domain above.
- **`--track-dirty-pages` cost is unmeasured** (`status.md:286`) and off by
  default; it must be on *before boot*, so migration-eligible sandboxes pay
  whatever that overhead is from the start. Quantifying it is a prerequisite.
- **VMM-side dirty tracking does not exist** (§4) — required for a *complete*
  memory image, not just the KVM-visible pages.
- **TCP continuity across L3 is unsolved in general.** The guest keeps its IP/MAC
  (bean's address preservation helps), and same-subnet cutover can send a
  gratuitous ARP to redirect traffic, but crossing subnets needs an overlay. The
  ARP cache inside the restored guest also needs clearing (`network.md:166`,
  itself flagged as needing on-hardware verification).
- **Container tier (gVisor/runc) is a separate track.** runsc restores only under
  the same binary and drops network/GPU state; realistic near-term goal there is
  snapshot-restore relocation (accepting dropped connections), not
  connection-surviving live migration.

## 7. Bottom line

bean is missing the orchestration, not the primitives. Consistent capture, UFFD
revival, diff chains, CoW, CPU hard-filtering, S3 transfer, and — nearly for free
— network address preservation are all shipped and were built for
snapshot/restore/fork. What live migration adds is a dirty-page iteration loop and
a node-to-node page channel, plus VMM-side dirty tracking, liveness/abort around
remote faults, and a measured `--track-dirty-pages`.

The feasible order is: **cold cross-node relocation (Stage 0, almost free) →
direct transfer (Stage 1) → post-copy via UFFD (Stage 2, the best fit for bean's
primitives) → iterative pre-copy with bounded cutover (Stage 3)**, with same-node
live upgrade as an early, low-risk proving ground. Cloud Hypervisor — same
rust-vmm lineage, production pre-copy, v53.0 remote post-copy — is the reference
implementation to study, and it shows the path is viable rather than blocked.
Zero-downtime live migration on Firecracker is not an upstream feature and should
not be expected as one; a bounded-downtime, post-copy-first path built on bean's
existing machinery is the realistic target.

---

## Sources

**Firecracker** — [snapshot-support](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md) ·
[Discussion #3119 (live migration declined)](https://github.com/firecracker-microvm/firecracker/discussions/3119) ·
[handling page faults / UFFD](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/handling-page-faults-on-snapshot-resume.md) ·
[CPU templates](https://github.com/firecracker-microvm/firecracker/blob/main/docs/cpu_templates/cpu-templates.md)

**Cloud Hypervisor** — [live migration docs](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/live_migration.md) ·
[#2458 VMM-side dirty tracking](https://github.com/cloud-hypervisor/cloud-hypervisor/issues/2458) ·
[v53.0 remote post-copy](https://www.cloudhypervisor.org/blog/cloud-hypervisor-v53.0-released/)

**Post-copy / userfaultfd** — [QEMU post-copy](https://www.qemu.org/docs/master/devel/migration/postcopy.html) ·
[userfaultfd(2)](https://man7.org/linux/man-pages/man2/userfaultfd.2.html)

**Containers** — [gVisor checkpoint/restore](https://gvisor.dev/docs/user_guide/checkpoint_restore/) ·
[gVisor at Tencent (scale)](https://gvisor.dev/blog/2026/04/23/scaling-agentic-rl-sandboxes-to-the-millions-with-gvisor-at-tencent/) ·
[runc + CRIU](https://github.com/opencontainers/runc/blob/main/docs/checkpoint-restore.md) ·
[CRIU #1598 (established-TCP redirect)](https://github.com/checkpoint-restore/criu/issues/1598)

**Competitors** — [E2B persistence](https://e2b.dev/docs/sandbox/persistence) ·
[fly.io Making Machines Move](https://fly.io/blog/machine-migrations/) ·
[fly.io suspend/resume](https://fly.io/docs/reference/suspend-resume/) ·
[Morph Infinibranch](https://morph.so/blog/infinibranch)

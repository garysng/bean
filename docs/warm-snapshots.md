# Warm snapshots: boot once per image, not once per sandbox

> 中文版:[zh/warm-snapshots.md](zh/warm-snapshots.md)
> Section status convention: [architecture.md](architecture.md) §0.
> Tracked as GitHub #26.

A cold create costs 952ms and, measured per process, **5 CPU-seconds** of host CPU.
A restore from a cached snapshot costs **392ms** and almost no CPU, because it does
not boot a kernel. Throughput is bounded by the first number:
`cores / 5 CPU-seconds`, about 2.3 creates/s on 16 cores.

So the lever is not a faster boot. It is booting **once per image** instead of once
per sandbox.

## 1. What the competition does 📐

Read from source rather than marketing — `e2b-dev/infra` @ `17ffd81`:

`Factory.CreateSandbox` (real boot, sets the boot source) and
`Factory.ResumeSandbox` (`PUT /snapshot/load`) are two separate paths, and **every
caller of `CreateSandbox` lives under `packages/orchestrator/pkg/template/build/`**.
The user-facing gRPC handler calls `ResumeSandbox`. Real boot happens only when a
template is built.

Their `ResumeSandbox` is what bean calls **restore**: it does `PUT /snapshot/load`
and produces a new sandbox. It is not resume in bean's sense (unfreezing the vCPUs
of a live process), and the borrowed name is worth watching for when reading their
code. This document uses bean's vocabulary throughout —
[snapshot-resume.md](snapshot-resume.md) §0.

Three details worth taking:

- They **boot twice**. Provision runs a BusyBox init executing only the provision
  script, and readiness is detected by scraping the guest's serial console for a
  sentinel. Later phases use systemd, with readiness as an HTTP `POST /init` to the
  in-guest agent.
- The pause point is **after the user's start command has run**, not merely after
  boot. Even the user process's memory state is captured.
- Before pausing: freeze the guest filesystem (`FIFREEZE`), drain the balloon's
  free-page hints, then pause, then snapshot. The freeze matters because a
  filesystem captured mid-write restores dirty.

## 2. Shape 📐

```
prewarm(ref):
    pull and convert to ext4        (already implemented)
    boot one sandbox
    wait for the agent              (already the create path's readiness gate)
    checkpoint with memory          (already implemented)
    record ref + CPU identity -> snapshot id

create(ref):
    look up a warm snapshot for ref on a CPU this node can run
    hit  -> restore it              392ms, no boot
    miss -> boot as today           952ms, and warm for next time
```

Every primitive here exists. This is orchestration plus one data-model decision,
which is why the design note is short and the risks section is long.

## 2a. Which side does what 📐

Prewarm already exists and is already split across the two sides, so the question
is not where to put the work but which half grows.

| | today | with warm snapshots |
|---|---|---|
| **Control plane** (`runPrewarmJob`, `internal/control/api/images.go`) | picks READY nodes in the region, calls `PrewarmImage` per image with a 30-minute deadline, logs failures | unchanged |
| **Node** (`PrewarmImage` → `Images.Prewarm`) | prepares the image file: pull and convert to ext4 | **also boots once, waits for the agent, checkpoints with memory** |
| **Reporting** | node reports what it holds; the control plane does not record success | node also reports which `(digest, vendor, family, template)` it has warmed |

The existing code states the reporting rule and the reason for it
(`images.go:196`): success is not recorded by the control plane because *the node
reports what it holds, and that is the authority. Writing it from this side would
let the two disagree after a node loses its disk.* That rule carries over
unchanged — a warm snapshot is a file on a node's disk, and a node that lost the
disk must be able to say so by simply not reporting it.

So the execution belongs to the node, and it already does. What the node cannot
decide alone is *which* image to warm, because that follows from placement and
demand, which only the control plane sees. The division is therefore:

- **the control plane decides what to warm**, as it already does for images
- **the node does the warming and owns the artifact**, as it already does for images
- **the node reports the result**, and the control plane treats that as the truth

The one genuinely new thing is that the reported unit is no longer a bare image
reference. A warm snapshot is only usable on a compatible CPU (§3), so what the
node reports is a tuple, and the scheduler's existing `CPUConstraint` filter is
what consumes it. The digest half of that tuple is why images now report their
digest at all — see `UpdateNodeStatus` in `proto/bean/node/v1/node.proto`.

**What prewarm does *not* buy today, and this is the whole point of the feature.**
Preparing the image file removes the pull. It does not remove the boot: a create
against a fully prewarmed image still runs `configureAndBoot` and still costs the
~5 CPU-seconds that set the throughput ceiling at `cores / 5`. Prewarm as it
stands attacks latency on a cold node; it does nothing for throughput on a warm
one.

## 3. The data model: a warm snapshot is per image *and per CPU* 📐

This is the part that will be got wrong if it is not stated plainly.

Guest memory records what the CPU it booted on offered, and vendor and family
**cannot be masked away** (see `cpu_template.go`, and the measurements in
[decisions.md](decisions.md) §3.6). A memory snapshot therefore restores only on a
compatible CPU. The mapping is not `image -> snapshot`; it is:

```
(image ref, cpu vendor, cpu family, cpu template) -> snapshot id
```

A heterogeneous fleet needs one warm snapshot per CPU generation. That is not a
defect of this design — it is the same constraint that already makes the scheduler
refuse an incompatible restore with `409 INCOMPATIBLE_CPU`, and the fields to
express it are already on the `Snapshot` record.

e2b's answer to the same problem is a four-line hardcoded compatibility table plus
a scheduler filter, which silently forecloses migration between AMD and Intel. We
already have vendor and family filtering, so the same approach applies without new
machinery.

**A miss must be ordinary, not exceptional.** A node whose CPU has no warm
snapshot boots as it does today. If a miss were an error, adding a machine of a new
generation to a cluster would break creates on it.

## 4. Why `--no-memory` is not the answer 📐

A filesystem-only checkpoint is 6109 B against 15.5 MB, restores on **any** CPU,
and looks like the obvious candidate for a per-image artifact.

It does not help here. Restore dispatches on the bundle's contents: no memory
member means `configureAndBoot`, so a `--no-memory` restore **still boots** and
still costs the 5 CPU-seconds. It saves storage, not CPU.

Its portability is genuine value for a different purpose, so **its semantics must
not be changed** to make warm snapshots work.

## 5. Where the snapshot is taken from 📐

Two options, and the choice has consequences:

| | pause point | captures | cost |
|---|---|---|---|
| after the agent is reachable | guest booted, agent up, nothing else run | the boot | one boot per image |
| after a user start command | boot plus the user's own warm-up | boot and application startup | needs a per-image build spec |

The first is the whole win against the 5 CPU-seconds and needs no new user-facing
concept. The second is what e2b does and is strictly better for something like
`import torch` — Modal measured `import torch` going from ~5s to 1.05s p50 by
snapshotting after it — but it requires a template definition, which is a larger
feature.

**The plan is to do the first and leave the second to a template feature**, because
the first captures the throughput ceiling on its own and the second is an
application-level optimisation on top of it.

## 6. Risks 📐

**The warm snapshot becomes stale.** It pins the base image at the moment it was
taken. Since images are immutable by digest — a moved tag is a different digest and
therefore a different file ([image-pipeline.md](image-pipeline.md) §2) — the key
must be the resolved digest, not the tag. Keying by tag would serve a stale
environment after a tag moved, silently.

**Storage grows with images times CPU generations.** Warm snapshots are full
memory images, so this is roughly guest-memory-size per entry per generation.
Reclaim needs an owner: unlike a user's snapshot, nothing refers to a warm one, so
it will not be deleted by anything that exists today. The node-local unpacked cache
already has watermark eviction; the S3-side blobs do not.

**A restore that fails must fall back.** If a warm snapshot is corrupt or its blob
is missing, a create must boot rather than fail. The failure mode to avoid is one
bad warm snapshot making an image unusable across the cluster.

**Chain interaction.** A warm snapshot is a plausible base for incremental
checkpoints, which would make it undeletable while descendants exist — that
protection already exists (`409` on deleting a base with children). Worth deciding
deliberately rather than discovering.

## 7. Verification 📐

The measurement that matters is not the single-create latency but the **throughput
ceiling**, since that is what the 5 CPU-seconds bounds:

1. Create N sandboxes concurrently from a cold image; record creates/s and the
   phase split.
2. Warm the image; repeat. The prediction is that `agent_ready` collapses and
   throughput stops tracking `cores / 5`.
3. Confirm a restored sandbox is genuinely usable — exec, and read a file written
   before the snapshot, **after `drop_caches`**. A read served from the page cache
   would pass against a device serving the base image
   ([decisions.md](decisions.md) §3.0).
4. Confirm a node whose CPU has no warm snapshot still creates, by falling back to
   boot.
5. Break it deliberately: corrupt a warm snapshot's blob and confirm creates still
   succeed by booting.

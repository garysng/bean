# Bean Technical Architecture

> 中文版:[zh/architecture.md](zh/architecture.md)

> Container-native sandbox platform for AI evaluation workloads.

## 0. Reading Convention: Delivery Status Markers

This batch of design documents carries two things at once — **what has already
been built** and **what is intended to be built**. When both are written the
same way the reader cannot tell them apart, and that has already caused real
misjudgements: the network stack and jailer isolation were both taken for
delivered capabilities.

So every section that describes a concrete mechanism carries a status marker
after its heading:

| Marker | Meaning | Criterion |
|---|---|---|
| ✅ | **Implemented** | The code is in the repo, and there are tests or measured data |
| ⚠️ | **Partially implemented** | The main path works but there is a specific gap; the section states what is missing |
| 📐 | **Design only** | **There is no code.** This is intent, not capability |
| ❌ | **Abandoned** | A design that once existed and is now explicitly not being built; kept so the reason is not lost |

Sections without a marker are background, motivation, terminology — content
that does not describe a mechanism.

**Order of authority**: code > `docs/status.md` (how far things actually got) >
`docs/decisions.md` (why it was chosen this way) > this batch of design
documents. On conflict the earlier one wins, and **the document gets fixed**.

One self-imposed rule: a 📐 section does not say "our approach is", it says
"the plan is". The former reads as established fact, and that is precisely
where this went wrong before.

## 1. Background and Goals

### 1.1 The Problem

Characteristics of AI evaluation / agent rollout workloads (SWE-bench-class
tasks, for example):

- **Environment is the image**: every evaluation task corresponds to its own Docker image (2000+ of them, each several GB)
- **Short lifetime**: a sandbox is destroyed as soon as it is done; stateless
- **High-concurrency batch launches**: one evaluation round may create hundreds or thousands of sandboxes at once
- **Runs untrusted code**: AI-generated code executes inside the sandbox and needs isolation

Problems with existing options:

- **e2b** (Firecracker microVM + template): the Docker image must first be converted into a VM rootfs (minutes), which is unusable for the "large number of distinct evaluation images" case
- **K8s + Pod**: the scheduling and network stack are too heavy, the cold-start path is long, and we need full control over the layers underneath

### 1.2 Goals

- Images as first-class citizens: any OCI image serves directly as the sandbox environment, with **no conversion step**
- Second-scale cold start: image lazy-pull + node cache + prewarm
- Fully in-house stack: control plane, node runtime, sandbox agent, SDK, CLI all implemented by us
- Substrate-agnostic: bare metal and cloud VM nodes both supported
- S3 as the unified storage backend (image blobs, artifacts, snapshots)

### 1.3 Non-goals (initial P0–P2)

- Cross-node sandbox networking
- Multi-tenant billing

**Delivered, no longer non-goals**: pause/resume and snapshot are both
implemented and measured on a real KVM machine (full / `--no-memory` /
`--base` incremental, three variants; see snapshot-resume.md).

**Networking was the gap and is now built** (network.md): each sandbox gets its
own namespace, tap and egress, the metadata range and RFC1918 are denied by
default, and a port inside a sandbox is reachable from outside the node through
bean-proxy. All of it verified on a real kernel.

Cross-node sandbox connectivity remains a non-goal, and per-port access control
is genuinely missing — any port on a sandbox is reachable by anything that can
reach the proxy (api-design.md §3.4).

## 2. Overall Architecture ⚠️

```
                        ┌──────────────────────────────────────┐
  SDK (py/ts) / CLI ───▶│  Control Plane                       │
                        │  ├── api-gateway   REST/gRPC, auth,  │
                        │  │                 quota, port proxy │
                        │  ├── scheduler     node pick: image  │
                        │  │                 affinity + packing│
                        │  ├── state store   SQLite: sandbox   │
                        │  │                 metadata, leases  │
                        │  └── image-service image metadata,   │
                        │                    prewarm, GC       │
                        └──────────┬───────────────────────────┘
                                   │ ↓commands pushed over direct gRPC / ↑heartbeat + state reports (stream)
              ┌────────────────────┼────────────────────┐
              ▼                    ▼                    ▼
        ┌──────────┐         ┌──────────┐         ┌──────────┐
        │ noded    │         │ noded    │         │ noded    │   ← one per node
        │ (bare)   │         │ (cloud)  │         │ (bare)   │      node daemon
        └────┬─────┘         └──────────┘         └──────────┘
             │ overlaybd(ublk) direct + noded owns FC; containerd optional, container tier only
        ┌────▼────────────────────────────────┐
        │  ├── image: overlaybd ublk daemon   │ ← block-level lazy-pull from S3
        │  └── runtime: fc(default)│runc│runsc│ ← internal tier selection (D3)
        └────┬────────────────────────────────┘
             │
        ┌────▼──────────────────────┐
        │ sandbox                   │  fc: microVM (vsock to agent)
        │  └── beand (init/PID1)    │  container: runc/runsc (unix socket)
        │      └── user process     │  agent: exec/PTY/files/port-forward
        └───────────────────────────┘

        S3 ◀── image blobs (source of truth) / eval artifacts / snapshot / volume backend
```

### 2.1 Component Responsibilities ⚠️

| Component | Language | Responsibility |
|---|---|---|
| `api-gateway` | Go | ✅ REST + gRPC API, auth, quota (port reverse-proxying belongs to bean-proxy, which may be co-deployed) |
| `scheduler` | Go | Node selection (image affinity + resource bin-packing), lease management — **a logical module of the control plane** (`internal/control/scheduler`, in the same process as bean-api: the scheduling decision, the transactional resource deduction and the command dispatch have to complete atomically; split it out once it becomes a bottleneck or needs leader election) |
| `image-service` | Go | Image metadata index, format conversion orchestration, prewarm, S3 blob GC (a logical module of the control plane; embedded in bean-api through P0–P2) |
| `bean-proxy` | Go | ✅ Reverse proxy into sandboxes. Reads `{port}-{sandbox}` from the Host, looks up which node holds it, forwards. Carries both a user's exposed port and the agent's own interface, so port exposure and the data plane are one mechanism. Performs no user authentication (an external layer does; see A7) and refuses a public bind. TLS and DNS are the hosting layer's, not bean's |
| `noded` | Go | Node daemon: sandbox lifecycle, networking, image cache, volume mounts, health reporting |
| `beand` | Go (statically linked) | PID1 inside the sandbox: exec, PTY, file read/write, port forwarding |
| `sdk-python` | Python | Primary SDK for the evaluation/rollout side |
| `sdk-ts` | TypeScript | SDK for the Web/Node side |
| `cli` | Go | The `bean` command line: sandbox management, image prewarm, debugging |

## 3. Core Design Decisions

### D1. Zero image conversion, dual container/microVM form ⚠️

Any OCI image serves directly as the sandbox environment, eliminating e2b-style
template conversion. The image is assembled by overlaybd into a block device
(see D4), which can back an overlayfs rootfs for the container tier and can
equally be attached to a microVM over virtio-blk (see D9) — both forms share
one image path, and the user never notices the difference.

### D2. overlaybd driven directly, no containerd on the hot path ⚠️

> **"No containerd" is achieved; "overlaybd driven directly" is not.** The
> current backend is dm-snapshot: pull the whole image, convert it, share a
> read-only base, one CoW per sandbox (measured at 44 KiB/sandbox). The
> overlaybd capability has been measured working on a tcmu backend but is not
> wired into `image.Provider`.

The fc main path **does not bring in containerd** (same as AgentENV, whose
source is available locally at /Users/mac/project/agentenv for reference):
noded drives overlaybd's ublk daemon directly to assemble the block device
(S3 backing + local cache) → virtio-blk attached to the microVM. All three of
containerd's responsibilities have a more direct replacement in this design:

| containerd responsibility | This design |
|---|---|
| Image pull / content store | Blobs live in S3 (image-service converts offline), metadata pushed down by the control plane; the registry is not on the hot path |
| snapshotter | overlaybd ublk daemon driven directly (demonstrated by AgentENV's uvm-ublk) |
| Task lifecycle | fc: noded owns the FC process; container tier: containerd+runc (retained only here, an optional dependency) |

The container tier (GPU / no-KVM fallback) keeps containerd — runc lifecycle
and overlayfs assembly are not worth reimplementing; a pure fc node can skip
containerd entirely. For the runtime abstraction interface see noded-design §3.

### D3. Isolation tiers + node capability probing ⚠️

noded probes node capabilities at startup and reports them:

```
├── /dev/kvm available (bare metal or nested-virt VM) → [runc, runsc, fc]
└── no KVM (ordinary cloud VM)                        → [runc, runsc(ptrace)]
```

**The runtime tier is an internal mechanism and is not exposed** — users do not
pick an isolation level (once overlaybd lets fc cover all ordinary cases, the
container tier is left with internal uses only). The scheduler assigns the tier
automatically:

```
Tier rules (internal to the scheduler):
  KVM node (the normal case)   → fc (Firecracker microVM, default main tier, see D9)
  no-KVM node                  → runsc (gVisor fallback tier; deployments should avoid such nodes)
  GPU task (internal, reserved)→ runc + nvidia (FC has no GPU passthrough)
  internal allow-listed task   → runc (explicit internal marker, not via the public API)
```

- **fc**: strongest isolation, native snapshot/fork, a real guest kernel so no syscall compatibility problems
- **runsc**: fallback tier for environments without KVM (active in P5; through P0–P4 a no-KVM node cannot join the pool)
- **runc**: GPU path + internal trusted tasks (introduced in P5 as needed)
- ~~kata~~: superseded by fc, will not be introduced

API requests carry no isolation field (the internal proto keeps the enum so
operators can force a tier); sandbox details return the actual tier
(`runtime: fc|runsc|runc`) for troubleshooting. The scheduler matches on node
capability.

### D9. Firecracker main tier: container rootfs attached straight to the microVM ✅

The FC tier is **not** nested containers (Kata-style, containerd running again
inside the guest); the rootfs is attached directly:

```
overlaybd assembles the image block device: base layer (lazy-pull from S3)
  + overlaybd writable layer, composed on the host into a [single block device]
  (the industry-consistent approach: e2b and AgentENV both assemble host-side)
  → attached to the microVM over virtio-blk (the guest sees one disk)
    + the agent disk (read-only, see D5)
  → beand runs as init inside the guest: mounts /proc /sys /dev and the rest
    (replicating the OCI default mounts), applies the image config
    (ENV/USER/WORKDIR/Entrypoint+Cmd) and starts the user process
```

What the single host-side device buys: disk quota is enforced on the host (the
writable-layer file size is the ceiling), the snapshot disk-diff is taken
straight from the host-side overlaybd writable layer, and there is zero union
complexity inside the guest.

- No container layer inside the guest; "container" is reduced to an image format, and the zero-conversion promise is unchanged
- Compatibility: ENV/ENTRYPOINT/CMD/WORKDIR are recorded beside the image when it is converted and merged with the create request when the process starts (rules in [image-pipeline.md](image-pipeline.md) §5); USER is recorded but not yet enforced. The guest is a complete, real Linux kernel, so compatibility beats a gVisor emulation layer. The one difference: the kernel is packaged and provided by the platform (not the host kernel), which a purely user-space eval workload cannot tell apart. See the fcRuntime section of noded-design.md
- Agent communication goes over vsock (a transport abstraction; same protocol as the container tier's unix socket)
- Networking: a tap device joins the node's bean0 bridge, with the same nftables rules as the container tier
- This route is validated in production by AgentENV (the Kimi K3 training infrastructure); the implementation takes its overlaybd+ublk integration and snapshot design as reference

### D4. S3 as the unified storage backend ⚠️

| Data | Approach |
|---|---|
| Image blobs | **overlaybd block-level images** (a layer = a block-device diff) stored directly in S3, range-read on demand by the node through ublk; the registry holds metadata only |
| Node cache | Local NVMe as a block-chunk LRU on top of S3; bare metal (big disks) and cloud VMs (small disks) differ only in hit rate, the architecture is the same |
| Eval artifacts | agent/noded push straight to S3 via presigned URL (issued by the control plane; nodes hold no long-lived credentials) |
| Large downloads | The API returns a presigned URL redirect rather than proxying through the gateway |
| Snapshots (P3–P4) | FC memory snapshot / rootfs diff land in S3, enabling cross-node **restore** (a new sandbox on any node; resume is same-process and same-node, see snapshot-resume.md §0) |
| Volumes | shared-fs volume backend (JuiceFS on S3) mounted on the host and exported over nfsd (see D10); dataset volumes reserved |

overlaybd (block-level, DADI/Alibaba, already validated by AgentENV in the FC
case) was chosen over Nydus (file-level) for one decisive reason: **the block
device path serves the container tier (overlaybd-snapshotter → overlayfs) and
the microVM tier (virtio-blk straight into the guest) at once, so a single image
path covers every runtime tier**; Nydus's filesystem semantics cannot get into a
microVM, and the FC tier would need virtiofs instead (weakly supported by FC).
Nydus is kept as a fallback option for the container tier.

Hot state (sandbox metadata, leases, scheduling state) lands in a relational
database, not in S3. ⚠️ **Today that is SQLite** (`modernc.org/sqlite`, pure Go
with no cgo, `SetMaxOpenConns(1)` for single-writer) or Postgres, chosen by
whether `bean-api --postgres` is set. SQLite suits a single machine; a
multi-replica control plane needs Postgres, because SQLite is one file and two
replicas cannot share it.

The second engine is a dialect rather than a second implementation: one body of
statements written with `?`, rewritten per engine. That was sized by measurement
(103 placeholders and a handful of DDL constructs, every `ON CONFLICT` portable)
and the alternative was rejected on evidence — two bodies of SQL that must agree,
checked by a suite that can only report afterwards which one drifted.

What made the swap safe was not the interfaces but where atomicity lives. Each
operation's conditions are in its statement, so the database arbitrates rather
than a process-local lock; the store holds no mutex at all. A lock inside one
process could never have ordered writes from a second replica, and while it was
there it hid a genuine lost-update bug.

### D5. Agent injection: init/PID1 override (nothing enters the user image) ✅

Eval images are arbitrary and cannot be assumed to contain any toolchain. The
injection method depends on the tier:

| Tier | Injection | Communication |
|---|---|---|
| fc (default) | **Agent disk**: a small read-only disk (ext4) containing beand, attached as an extra virtio-blk; the guest kernel's init is the agent on that disk | vsock + gRPC |
| Container tier | Read-only bind mount + entrypoint override, agent runs as PID1 | unix socket + gRPC |

What they share: zero modification to the user image; the original
entrypoint/cmd/env/user/workdir are serialized into the spec and the agent
starts them following Docker semantics (details in noded-design.md §3.1/§6).
CRI streaming exec is not used: poor performance, no file API, and it depends on
a long chain of components.

### D6. Networking: in-node NAT, the greatest common denominator of bare metal and cloud VM 📐

> **Unimplemented**. The sandbox has no network stack today; see noded-design §5.

```
sandbox netns ←veth→ node bridge → SNAT egress
```

- One netns per sandbox, on a node-local private range (e.g. 10.100.x.0/24 per node)
- Default policy: egress allowed (for pulling dependencies), access to the node's internal network and metadata services denied (169.254.169.254 and friends), sandboxes isolated from each other (nftables)
- Port exposure: `{sbxId}-{port}.{region}.sandbox.<domain>` → regional proxy → noded sbxproxy → direct connection to the sandbox IP (agent ForwardPort is only a fallback), which sidesteps cloud providers' MAC/IP allow-list restrictions
- No dependency on underlay/BGP; both node kinds behave identically

### D7. Scheduling: bin-packing with image affinity first ✅

Evaluation scheduling is simple enough that writing our own actually allows
fine-grained optimisations K8s cannot do.

**Node resource profile** (reported by heartbeat, kept in the scheduler's
memory):

```
cpu:   allocatable vCPU (physical cores × overcommit factor, config default 3.0,
       with a system share reserved)
       committed = Σ sandbox.cpu; actual load = node load (for alerting only)
mem:   allocatable = physical memory − system reservation
       committed = Σ sandbox.memoryMiB (on the fc tier balloon reclaim does not
       reduce the commitment — it protects resume/burst)
disk:  headroom in the sandboxes pool (writable layers); cache pool watermark
       (affects scoring only, never a gate)
gpu:   free card count (whole cards, no slicing)
cap:   [runc, runsc, fc] × per-node concurrent-create headroom (default 16)
```

**Scheduling flow** (two levels: region first, then node; batchCreate runs the
same flow sequentially inside one lock):

```
0. Region selection: explicit region parameter > volume/snapshot data affinity
   (mandatory) > regions where the image blob is already replicated > capacity
   headroom
1. Filter (within the region, hard constraints):
   nodeSelector label match (e.g. pool=gpu-a100)
   isolation resolution (auto→fc/runsc/runc) → node capability match
   cpu/mem/disk committed + request ≤ allocatable; whole-GPU headroom
   node state = READY (SUSPECT/LOST/DRAINING excluded)
2. Score (weighted sum, weights configurable):
   w1·image affinity: the byte fraction of this image's overlaybd blocks in the
      node cache (heartbeat carries a bloom filter + byte count)
   w2·resource balance: fragmentation after packing (fill up first, leave large
      gaps for large shapes)
   w3·cache disk type: cold image → bonus for nodes with a large NVMe cache
   w4·spreading: moderate anti-affinity for the same label (the same eval run),
      so one node failure does not swallow a whole batch

3. Commit: deduct the commitment inside the transaction + write the command
   record → push directly to noded.CreateSandbox (see api-design §5.1)
4. Failure fallback: node reports FAILED (an ENOSPC race, for example) →
   release the commitment, reschedule (≤3 times, excluding the failed node);
   still failing → NO_CAPACITY returned to the caller
```

**Accounting consistency**: the database is authoritative for commitments (the
scheduler can rebuild its in-memory state after a restart); the actual usage in
node heartbeats is used only for alerting and balloon decisions and never for
admission — this avoids "admission on actual watermark" exploding into
overcommit under bursty load.

**Preemption**: not done. Eval tasks are homogeneous and short-lived; queueing
(NO_CAPACITY + client retry / a queue pool) is simpler and sufficient.

### D8. Failure model: leases + stateless rebuild ✅

- noded renews its lease by periodic heartbeat; lease timeout → the node is marked lost → the sandboxes on it are marked `lost`
- Eval tasks are stateless; once the upper layer (SDK/caller) sees `lost` it simply rebuilds
- After a noded restart, reconcile: compare local actual state (live FC processes ∪ containerd tasks, if enabled) against the control plane's desired state (SyncState)
- GC: idle reclamation (driven by lifecycle.onIdle), image block LRU eviction, cleanup of orphaned tap/netns/mounts

### D10. Volumes: a first-class data resource independent of images 📐

Image = environment (immutable, lives and dies with the sandbox), volume = data
(independent lifetime, survives across sandboxes, can be mounted more than
once). Two types:

| Type | Backend | Data plane | Use case |
|---|---|---|---|
| `shared-fs` (first release) | Host-mounted JuiceFS (on S3) / CephFS / local disk | **Exported by the host kernel's nfsd** (same route as e2b): the guest mounts a host-internal address with the kernel NFS client, and the traffic never leaves the node | Persistent workspace, shared read/write across sandboxes |
| `dataset` (reserved, not scheduled) | overlaybd read-only blocks (reusing the image pipeline) | Container tier: bind mount; fc tier: an extra virtio-blk | Massive read-only consumption of datasets/weights |

Why shared-fs goes through host NFS instead of running a distributed-FS client
inside the guest: the guest needs zero credentials and zero extra binaries; the
`none` network policy remains compatible by construction (the NFS target is the
host gateway, which is orthogonal to public egress); the host client cache is
shared by every sandbox (high hit rate when a batch of evals reads the same
data); and the backend stays swappable. See noded-design.md §3.3.

### D11. Multi-region (Region/Cell) and BYOC 📐

**One global control plane, a data plane autonomous per region.** Region = a
failure domain + a data domain + a forwarding domain:

```
Global Control Plane (bean-api / scheduler / relational DB, global digest index of image metadata)
   │ hosted gRPC ingress (TLS) + node token, noded/proxy connect outbound
   ├── Region A: noded node pool + regional proxy ×N + region S3 backend
   └── Region B (BYOC): customer nodes + customer S3, data never leaves the customer environment
```

- **Data domain**: image blobs, artifacts, snapshots and shared-fs volume backends are all closed inside the region; a node only reads its own region's S3, and cross-region traffic happens only for image blob replication, never on the hot path
- **Image replication**: metadata is globally unique (digest), blobs are stored per region; conversion happens once and writes the source region, other regions replicate **on demand** (fetched the first time something is scheduled there) plus **explicit prewarm replication** (`POST /images/prewarm` with a `region` parameter, used ahead of an eval batch)
- **Data gravity of volumes**: a volume belongs to a region, and a sandbox mounting an existing volume is forced into that volume's region
- **Forwarding domain**: one proxy group per region, domain `{sbxId}-{port}.{region}.sandbox.<domain>` (DNS goes straight to the regional proxy, with no global hop)
- **BYOC**: the customer provides nodes + S3 (+ optionally their own domain) against a hosted control plane; the control plane sees metadata only and holds no long-lived credentials for the customer's S3 — presigned/STS are issued by a lightweight token service deployed on the customer side; noded/proxy connect outbound on 443 to the hosted ingress and register with a bootstrap token (registration-only, optionally with manual approval; see noded-design §7.0)
- **Node membership**: `region` is a first-class field (declared in config, validated at Register time against the regions the control plane already knows, immutable for the node's lifetime); `labels` are free-form (pool/disk/tenant and so on), and scheduling requests constrain them through `nodeSelector` — GPU pools, BYOC-dedicated nodes and the like use labels rather than new fields
- **Ingress and identity**: the control plane is exposed through a cloud-hosted gRPC ingress (the gateway terminates TLS, so nodes need zero certificate configuration); node identity is an application-layer node token (short-lived, held in memory, bound to a nodeId), with no mTLS — this follows the existing infrastructure, and outbound 443 is enough for BYOC
- **Failure domain**: a region going unreachable marks that region's sandboxes LOST while other regions notice nothing; the global control plane being a single point is accepted for the first release (a control-plane failure does not affect the data plane of existing sandboxes, it only stops new creations), with control-plane multi-active held in reserve for P5

## 4. API Design ⚠️

### 4.1 REST API (external) ⚠️

```
# Sandbox lifecycle
POST   /v1/sandboxes                 # image, resources, env, lifecycle
                                     # (idleTimeout/onIdle), labels → sandbox
GET    /v1/sandboxes/{id}
GET    /v1/sandboxes?label=k=v       # list + filter
DELETE /v1/sandboxes/{id}
PATCH  /v1/sandboxes/{id}/lifecycle  # adjust idleTimeout / onIdle at runtime

# Process execution
POST   /v1/sandboxes/{id}/exec       # synchronous: cmd/cwd/env/timeout
                                     # → stdout/stderr/exitCode
WS     /v1/sandboxes/{id}/exec/ws    # streaming + PTY

# Filesystem
PUT    /v1/sandboxes/{id}/files?path=    # upload (small files inline, large
GET    /v1/sandboxes/{id}/files?path=    #  files return a presigned URL)
GET    /v1/sandboxes/{id}/files/ls?path=

# Ports
POST   /v1/sandboxes/{id}/ports      # expose a port → public URL

# Images
POST   /v1/images/prewarm            # list of refs + target node count
GET    /v1/images/{ref}/status       # cache distribution, blob readiness

# Lifecycle extensions / batch / volumes / snapshots / logs (full definition in api-design.md)
POST   /v1/sandboxes:batchCreate     # batch create (frequent in eval)
POST   /v1/sandboxes/{id}/pause|resume|snapshot|fork|start
CRUD   /v1/volumes                   # shared-fs volumes (dataset reserved)
CRUD   /v1/snapshots
GET    /v1/sandboxes/{id}/events + WS /v1/events   # lifecycle events
GET    /v1/sandboxes/{id}/logs
```

### 4.2 Internal gRPC ✅

- `control ↔ noded`: `NodeService` (Register/Heartbeat/SyncState) + `SandboxService` (implemented by noded, called directly by control: Create/Destroy/Pause/Snapshot/Exec forwarding/…)
- `noded ↔ agent`: `AgentService` (Exec/StreamExec/ReadFile/WriteFile/ListDir/ForwardPort/…; unix socket on the container tier, vsock on the fc tier)

The proto definitions all live in `proto/`, and the generated code goes into each
language's SDK.

### 4.3 Sandbox State Machine ✅

```
PENDING → SCHEDULED → PULLING → STARTING → RUNNING → STOPPING → STOPPED
                                    │          │
                                    └── FAILED ┘        RUNNING ─(lease lost)→ LOST

RUNNING ─pause→ PAUSED ─resume→ RUNNING      ← the same sandbox, same id
RUNNING/PAUSED ─snapshot→ SNAPSHOTTING → (back to previous state)
(from snapshot) PENDING → SCHEDULED → RESTORING → RUNNING
                             ↑ restore is a *new* sandbox with its own id, and one
                               snapshot can drive N of these at once

A snapshot object has its own state machine: CREATING → READY → DELETING
(not deletable while any RESTORING holds a refcount — a count, not a flag, since
concurrent restores of one snapshot are the normal case)
```

Resume and restore are different operations on different subjects: resume moves one
existing sandbox back to RUNNING, restore builds another one. See
[snapshot-resume.md](snapshot-resume.md) §0.

`DELETE /sandboxes/{id}` returns 202 and then goes STOPPING → STOPPED
asynchronously (the terminal record is kept for 30 days and then archived, see
noded-design GC); `?force=true` skips the graceful path and kills outright.

See [snapshot-resume.md](snapshot-resume.md).

## 5. Cold-start Path Optimisation ⚠️

Target: P50 < 2s (image already cached) / P50 < 10s (lazy-pull of a cold image).

1. **lazy-pull**: overlaybd + ublk block-level on-demand loading; startup needs only the metadata plus the hot blocks, and the rest is range-read from S3 while running
2. **Node cache**: chunk-level LRU, S3 is the source of truth, so the node disk can be GC'd freely
3. **prewarm API**: warm images onto the target nodes before an evaluation batch begins
4. **Image-affinity scheduling**: raises the cache hit rate for free
5. **Agent resident on the hot path**: the agent is a static binary (bind mount on the container tier, agent disk on the fc tier), so there is no in-image install step

## 6. Security Model ⚠️

- Untrusted code runs on fc by default (Firecracker microVM, a hardware virtualization boundary); nodes without KVM fall back to runsc
- ⚠️ The fc tier currently has only FC's built-in seccomp; **the jailer and the host-side cgroup wrapper are unimplemented** (security §A3). The container tier is unimplemented as a whole
- 📐 Network policy is unimplemented — the sandbox has no network stack today
- ⚠️ Nodes currently take their S3 credentials from environment variables; presigned URL / STS rotation is unimplemented
- API auth: API key (caller identification + quota; no user/tenant system — this is an internal cluster service)

## 7. Repo Layout ⚠️

```
bean/
├── proto/                  ✅ gRPC definitions (single source of truth)
├── cmd/
│   ├── bean/               ✅ CLI entry point
│   ├── bean-api/           ✅ gateway (scheduler / image / snapshot modules embedded)
│   ├── noded/              ✅ node daemon
│   ├── beand/              ✅ in-sandbox agent
│   └── bean-proxy/         ✅ reverse proxy into sandboxes (Host-routed)
├── internal/
│   ├── control/            ✅ api / scheduler / store / snapshot / s3
│   ├── node/               ✅ manager / runtime / image / vsock (no network module)
│   ├── beand/              ✅ in-sandbox daemon implementation
│   ├── obs/                ✅ OTel tracing + gRPC interceptors
│   ├── logging/            ✅ slog structured logging
│   └── gen/                ✅ protoc output
├── cli/                    ✅ CLI implementation
├── sdk/
│   ├── python/             ⚠️ hand-written httpx, not codegen; coverage in sdk-cli-design §2
│   └── typescript/         📐 unimplemented, the directory does not exist
├── hack/                   ✅ build-assets / dev-fc-stack / cpu-template-probe / tracedump
├── tests/e2e/              ⚠️ 6 functional tests running the local tier; no scale/load testing
├── deploy/                 📐 does not exist
└── docs/
```

Note: `internal/store/` does not exist; the store is at `internal/control/store/`.

## 8. Implementation Roadmap

See [roadmap.md](roadmap.md) (the single place this is maintained). In outline:
**P0 is direct fc boot** (overlaybd driven directly + FC + agent, referencing the
local AgentENV source) → P1 multi-node usable → P2 productionisation
(lazy-pull/prewarm/scheduling affinity) → P3 interactive/proxy/pause/shared-fs
volumes → P4 the full snapshot form → P5+ reserve (the container-tier GPU path
as needed).

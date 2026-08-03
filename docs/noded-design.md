# noded (Node Daemon) and beand (In-Sandbox Daemon) Detailed Design

> 中文版:[zh/noded-design.md](zh/noded-design.md)

> **noded**: one daemon per node (binary `noded`), the thing that actually executes the sandbox lifecycle.
> **beand**: the init/PID1 inside the sandbox (binary `beand`), the far end that executes exec/file/port operations.
> Naming convention: **noded lives on the host, beand lives inside the sandbox**.

> For the status marker convention see [architecture.md](architecture.md) §0.

## 1. Overall Structure of noded ⚠️

**The actual package layout** (`internal/node/`):

```
internal/node/
├── manager.go       ✅ Manager: lifecycle orchestration (create/destroy/pause/resume/
│                       snapshot/restore, transparent wake, idle reclaim, in-flight guard)
├── grpc.go          ✅ SandboxService implementation + data-plane pass-through to the agent
├── register.go      ✅ Outbound registration, heartbeat, SyncState reconciliation
├── auth.go / dial.go ✅ node token auth, agent connection
├── runtime/         ✅ Runtime interface + fc (real microVM) and local (process-level, dev only)
│                       including UFFD page supply, snapshot bundles, CPU templates, diff merge
├── image/           ✅ image.Provider: DevMapperProvider (shared base + CoW),
│                       FileProvider, PullingProvider; OCI pull-and-convert, commit, build
├── vsock/           ✅ AF_VSOCK dialling
├── agentmgr/        📐 empty directory
└── lifecycle/       📐 empty directory
```

**The `network/`, `volume/`, `sbxproxy/`, `reconcile/`, `gc/` and `report/`
entries previously listed here do not exist**, and they were written exactly the
way the delivered modules were. This is the most misleading spot in this batch of
documents: a reader would conclude that network isolation is already running. Of
those, the `reconcile` and `report` responsibilities actually live in
`register.go`, part of `gc` lives in `manager.go` (idle reclaim), and the other
three have no code at all.

**Configuration is by flag, not YAML.** A repo-wide
`grep -rn yaml --include='*.go'` comes back empty; there is no
`/etc/bean/noded.yaml`. The actual parameters (`cmd/noded/main.go`):

```
--listen / --control-plane / --node-token / --bootstrap-token / --region
--runtime fc|local          # the tier, not auto-detected
--firecracker-bin / --kernel / --agent-disk
--base-dir / --image-dir    # sandbox state and base images
--cpu / --memory-mib        # allocatable amount, given as a number rather than probed
--labels / --metrics
--cpu-template none|portable    # mask CPU features so memory snapshots cross machine models
--track-dirty-pages             # enables incremental snapshots, must be set before boot
--buildkit-addr                 # empty means this node takes no builds
--otlp-endpoint                 # empty installs a no-op tracer
--log-format / --log-level
```

The YAML below is the **planned form** (📐), kept because it records the intent
behind the configuration items — the `overcommit` section is now implemented
(see §3.2), the rest is not yet:

```yaml
nodeId: auto            # 📐 no such concept today; the node id is assigned by the control plane at registration
region: ap-east-1       # ✅ exists, as --region
labels:                 # ✅ exists, as --labels
  pool: gpu-a100
bootstrapToken: <...>   # ✅ exists, as --bootstrap-token
controlPlane: grpcs://<hosted-gateway>:443   # ⚠️ --control-plane exists, but there is no TLS
s3:
  endpoint: https://...  # ⚠️ comes from environment variables rather than config (credentials stay off the command line)
containerd: null        # 📐 the container tier is unimplemented
cidr: 10.100.0.0/24     # 📐 there is no network stack
cache:
  dir: /var/lib/bean/cache
  maxBytes: 800Gi        # 📐 no cache LRU; base images are not reclaimed automatically today
runtimes: auto           # 📐 not probed, specified explicitly with --runtime
overcommit:              # ✅ implemented, see §3.2
  cpu: 3.0
  memory: 1.0
network:                 # 📐 there is no network stack
  egressRateMbps: 100
```

## 2. Capability Probing (at startup) 📐

**Nothing is probed today.** The tier is given explicitly with
`--runtime fc|local`, and the allocatable amount is given as a number with
`--cpu`/`--memory-mib`. The only runtime check is
`DevMapperProvider.Available()` (it checks that dmsetup/losetup exist and that
`dmsetup targets` lists a snapshot target); it is neither reported nor does it
influence tier selection — if it is missing, startup fails rather than degrades.

The table below is the plan:


| Probe | Method | Effect |
|---|---|---|
| KVM | `/dev/kvm` can be opened | fc tier (the default main tier) |
| runsc | binary exists + `runsc --version` | Fallback tier without KVM; automatically `--platform=ptrace` when there is no KVM |
| NVMe/disk | Disk type of the cache directory + free space | Weighting of the cache disk in scheduling |
| GPU | NVML enumeration | GPU resource profile + nvidia runtime injection |
| cgroup v2 | `/sys/fs/cgroup/cgroup.controllers` | v2 is mandatory; v1 refuses to start |
| ublk/tcmu | /dev/ublk-control, target_core_user | overlaybd backend choice; neither present → the fc capability is not reported (fc depends on a block device) |
| Kernel version / erofs | `uname` + /proc/filesystems | Agent disk (erofs); overlayfs is container-tier only, P5 |

Probe results → reported via `Register`, and re-reported afterwards only when
they change.

## 3. Runtime Abstraction ✅

The actual interface (`internal/node/runtime/runtime.go`):

```go
type Runtime interface {
    Name() string
    Create(ctx context.Context, spec *Spec) (*Handle, error)
    Destroy(ctx context.Context, id string, force bool) error
    Pause(ctx context.Context, id string) error
    Resume(ctx context.Context, id string) error
    Checkpoint(ctx context.Context, id string, w io.Writer, opts CheckpointOptions) error
    Restore(ctx context.Context, spec *Spec, layers []SnapshotLayer) (*Handle, error)
}
```

Three differences from the earlier design, all of them cases where the original
design turned out to be wrong during implementation:

- **`Create` does not take a rootfs.** The image provider is a field of the runtime rather than a parameter — because the moment at which the rootfs is assembled is coupled to the runtime's internal state: restore has to fill the CoW in **before the device is assembled** (otherwise dm-snapshot's exception table is already in the kernel and the filesystem is silently corrupted; see `docs/decisions.md` §3.0). A parameter-style interface cannot express that ordering.
- **`Restore` takes `[]SnapshotLayer` rather than a single reader.** Incremental snapshots have to replay the whole chain, and each layer is an independent gzip stream; a single reader stops at the end of the first layer.
- **There is no `Stats`.** It was never implemented and has no caller — resource watermarks are currently accounted from the committed amounts in the heartbeat.

There are also three **optional** interfaces that a runtime implements according
to its capabilities and callers type-assert on: `ImageWarmer` (prewarm),
`ImageLister` (cache inventory), `ImageBuilder` (build) and `SandboxCommitter`
(sealing a sandbox into an image). The reason they are separate rather than
stuffed into `Runtime`: the local tier runs host processes and has no concept of
a "cached image", so making it stub out four methods conveys less information
than making callers check for the capability.

| Implementation | Status | Underlying | Responsibility boundary |
|---|---|---|---|
| `FCRuntime` (main tier) | ✅ | noded execs the firecracker process directly, **no jailer** (see security §A3), no containerd | dm-snapshot block device attached directly over virtio-blk; vsock to the agent; full/diff/memoryless snapshots |
| `LocalRuntime` (dev/CI) | ✅ | Host process | **No isolation**, must not be used for untrusted code |
| `runcRuntime` | 📐 | containerd + runc shim | Unimplemented |
| `runscRuntime` | 📐 | containerd + runsc shim | Unimplemented |

Key points:

- **Zero containerd on the fc hot path** ✅: the image module manages block devices itself, and a pure fc node does not install containerd. But the current backend is **dm-snapshot rather than overlaybd ublk** — the overlaybd capability has been measured working (7ms to mount, only 19.6% of layer bytes transferred) but is not yet wired into `image.Provider` (`docs/decisions.md` §3.1).
- `image.Rootfs` is produced by the image module (see §4) and carries two fields: `Device` (the path the VM attaches) and `Writable` (the CoW layer a snapshot has to capture). **`Writable` is the crucial one**: the snapshot captures it, and restore fills it back in through `PrepareOptions.SeedWritable` before the device is assembled.

### 3.1 fcRuntime Details ⚠️

**Boot chain**

```
1. The image module produces the rootfs block device: **dm-snapshot** — a shared
   read-only base (loop-mounted) + a sparse CoW file per sandbox, composed into
   a single `/dev/mapper/bean-<id>`.
   Quota = the CoW file size; the CoW layer is exactly what a snapshot captures.
   (overlaybd lazy-pull is the target form; the capability is measured but not wired in)
2. noded execs firecracker directly (**no jailer**, see security §A3):
   virtio-blk: **the agent disk is the root device** (`agent.ext4`, containing beand)
               + the user image as the second disk (`/dev/vdb` inside the guest)
   vsock; **no NIC, no balloon** (the network stack is unimplemented, balloon is not wired up)
   kernel cmdline: `init=/bean/beand -- --listen vsock:1024 --pivot ...`
3. beand runs as init inside the guest:
   a. Mount matrix: /proc /sys /dev /dev/shm /dev/pts /dev/mqueue /tmp
      (replicating the OCI runtime spec default mounts)
   b. Mount the rootfs disk and switch root (the guest sees one rootfs disk, zero union logic)
   c. Read the startup parameters (pushed on the first vsock connection): image config + sandbox spec + volume mount table
   d. Apply ENV/hostname/resolv.conf/hosts; mount volumes (dataset disk / NFS); listen on vsock
   e. autoStartCmd → start the user process following USER/WORKDIR/Entrypoint+Cmd semantics
```

**Container compatibility matrix**

| Item | Compatibility | Notes |
|---|---|---|
| ENV/ENTRYPOINT/CMD/USER/WORKDIR | ✅ | The agent replicates the image config, sharing code with the container tier |
| Filesystem/permissions/uid-gid | ✅ | The block device is mounted as-is |
| /proc /sys /dev | ✅ | A real kernel, more complete than gVisor's emulation |
| Dynamic linking/glibc/musl | ✅ | User space is unchanged |
| Kernel version | ⚠️ | The guest kernel is supplied by the platform, so `uname -r` is not the host's; a purely user-space workload cannot tell |
| VOLUME/EXPOSE/HEALTHCHECK | ➖ | Same as the container tier: ignored / metadata only / not executed |
| Image architecture | ❗ | Must match the node arch, there is no emulation (same on the container tier) |
| GPU | ❌ | FC has no passthrough → auto resolution falls through to the container tier |

## 3.2 Resource Model (cpu / mem configuration) ⚠️

**API layer**: `resources: {cpu, memoryMiB, gpu}` is declared at creation time and
is **immutable** (FC does not support hot adjustment, and the container tier does
not expose it either, to keep the semantics consistent).

| Tier | cpu enforcement | mem enforcement |
|---|---|---|
| Container tier | cgroup v2 `cpu.max` (hard) + `cpu.weight` | `memory.max` + `memory.swap.max=0` |
| fc tier | vCPU count = ceil(cpu), with the FC process wrapped in a host-side cgroup as well (cpu.max as a second line of defence + weight for fairness) | Guest memory = memoryMiB; virtio-balloon reclaims what is idle |

**Overcommit policy ✅ implemented** (flags, not YAML):

```
--overcommit-cpu 3.0        # reported allocatable = --cpu × this factor; 1.0 = no overcommit
--overcommit-memory 1.0     # memory is not overcommitted by default
```

Measured: `--cpu 8 --overcommit-cpu 3.0` → `/v1/nodes` reports
`cpuAllocatable: 24`.

**Why this is computed on the node side rather than in the scheduler**: the right
factor depends on what this particular node is for (a CPU-intensive pool wants
1.0, a general pool can go higher), and the comment on
`NodeRecord.CPUAllocatable` already said "overcommit factor included" — keep the
decision in exactly one place.

**Why the CPU and memory defaults differ**: exceeding CPU only makes things
slower (the kernel time-slices), whereas exceeding memory gets processes killed.
In theory the fc tier has memory headroom (FC supplies pages on demand, so the
guest's actual RSS is well below the declared value), but that deviation **has
never been measured**, and there is no cgroup wrapping the VMM process on the
host side (security §A3), so under pressure there is no kernel-level fairness
guarantee. Those two are the prerequisites for raising the memory factor.

**Rejecting < 1.0 rather than clamping it**: somebody who wants to "leave a
quarter spare" will write 0.75; clamping to 1.0 ignores them, and accepting it
reports less capacity than actually exists with nothing in the logs to explain
why. Reporting less capacity should be done by passing a smaller number to
`--cpu`, and the error message says so. The upper bound of 20 exists so that a
misplaced decimal point (3.0 → 30) fails immediately; otherwise it shows up as
unexplainable timeouts rather than as a configuration error.

- CPU: bursty eval workloads default to 3.0; a CPU-intensive node pool can be set to 1.0; cgroup cpu.weight is allocated in proportion to the shape to keep things fair; `dedicated: true` (a reserved field) → vCPU pinning, excluded from overcommit
- Memory: the container tier naturally reuses memory according to actual RSS; the fc tier relies on the balloon — noded drives balloon reclaim of idle guest memory periodically, and the scheduler accounts on two watermarks, the "shape's committed amount" and the "actual usage after ballooning": new creations look at the commitment (so resume/burst have headroom), alerting looks at actual usage
- A change to the factor affects only subsequent scheduling decisions; existing sandboxes are unaffected; when lowering it puts the commitment above the new allocatable, the node is marked "no longer accepting work" and drains naturally
- PAUSED: neither tier releases the memory allocation (to prevent OOM on resume); to release it, go through snapshot
- Shape ceiling: a single sandbox ≤ 32 vCPU / 128 GiB (an FC constraint on the fc tier; the container tier keeps the same limit for consistency)

**Storage (disk)**

The API layer gains `resources.diskMiB` (the writable-layer ceiling, default
20 GiB):

| Tier | Writable-layer implementation | Quota enforcement |
|---|---|---|
| Container tier | overlayfs upper dir | XFS project quota (hard limit) |
| fc tier | A writable overlay disk (a sparse file with ext4 pre-created) | The disk size is the ceiling, a hard limit by construction; the sparse file occupies host space according to what is actually written |

- Node disk is split into pools: `cache/` (image chunks, reclaimable by LRU) and `sandboxes/` (writable layers, lifetime bound to the sandbox) are accounted separately — the cache can always be sacrificed, the writable layer cannot
- Writable-layer usage is reported by heartbeat (the basis for the scheduler's disk watermark); behaviour when full: ENOSPC on the container tier, ENOSPC inside the guest on the fc tier, and neither affects the host
- tmpfs: `/dev/shm` defaults to 64 MiB and counts against the memory quota; configurable

## 3.3 Volumes (an independent first-class resource) 📐

**Resource model**: images and volumes are two orthogonal resources — the image
defines the environment (rootfs, immutable, destroyed with the sandbox), the
volume carries data (independent lifetime, exists before the sandbox and remains
after it, and can be mounted by several sandboxes at once). Volumes have their
own CRUD API and quota:

```
POST   /volumes        { "name": "swebench-data", "type": "shared-fs"|"dataset",
                         "quotaMiB": ..., "readOnly": ... }
GET    /volumes / DELETE /volumes/{id}

POST /sandboxes { ..., "volumes": [
  { "volume": "vol_...", "subPath": "run-0731", "mountPath": "/workspace", "readOnly": false }
] }
```

**Types: only `shared-fs` in the first release**; `dataset` (overlaybd read-only
blocks, reusing the image pipeline) is a reserved type and is not scheduled —
large read-only dataset cases should use shared-fs or bake the data into the
image for now, and it will be enabled once the requirement is clear.

| Type | Backend | Semantics | Use case |
|---|---|---|---|
| `shared-fs` | JuiceFS (on S3+Redis, consistent with the S3 substrate) or CephFS, configured by the platform and invisible to the user | POSIX shared read/write | Persistent workspace, data shared across sandboxes |
| `dataset` (reserved) | overlaybd read-only blocks | Read-only, versioned publishing | Massive read-only consumption of datasets/weights |

**shared-fs data plane: exported over NFS from the host (the same route as e2b,
verified against its source)**

```
Backend (mounted on the host, managed by the noded volume module): JuiceFS (on S3+Redis) / CephFS / local disk
    ▼
The host kernel's nfsd exports a per-volume directory (decision: the kernel nfsd,
    for zero user-space overhead and the highest maturity; quota is enforced by
    the backend — JuiceFS directory quota / CephFS quota)
    ▼  NFS traffic only goes sandbox→host gateway (virtio-net/veth, never leaving the node)
The agent inside the guest/container runs: mount -t nfs -o fg,hard <host gateway IP>:/<volumeName> <mountPath>
```

Reasons for choosing host NFS over running a JuiceFS client inside the guest:

- **Zero credentials and zero extra binaries in the guest** — all it needs is the kernel NFS client (standard on Linux); the storage credentials stay on the host
- **The `none` policy stays compatible by construction** — the NFS target is the host gateway, which is orthogonal to "public egress", so the no-exfiltration promise is not broken
- **The host client's cache is shared by every sandbox** — when a batch of evals reads the same data, the host fetches it once and everyone hits (with independent clients inside each guest it would be N caches and N trips to the origin)
- The backend is swappable (JuiceFS/CephFS/local disk); noded only sees a host path
- The cost: one extra NFS protocol hop, and small-file / metadata-heavy workloads are slower — such workloads should be steered to the writable layer (once dataset volumes are enabled, high-volume read-only traffic can move there)

**Mount matrix:**

| Tier | shared-fs | dataset (reserved) |
|---|---|---|
| Container tier | Bind mount the subPath of the host mount point directly (skipping NFS) | Mount the block device → bind mount |
| fc tier | The guest kernel's NFS client mounts the host export | Attach an extra virtio-blk |

- Quota: enforced by the backend (JuiceFS directory quota / CephFS quota); the nfsd layer intercepts nothing
- A mount failure counts as a sandbox creation failure (FAILED, with an explicit reason)
- nftables: the accept rule for sandbox → host gateway NFS port is inserted only for sandboxes that actually mount a shared-fs volume (a per-sandbox chain)

**Not doing**: real-time collaborative lock semantics between sandboxes — shared
writes are made consistent by going through the backend filesystem.

**Scheduling interaction**: shared-fs has no node affinity (the backend is
reachable from every node).

## 3.4 Building and Publishing the Guest Kernel and Agent Disk ✅

The fc tier has two platform artifacts, both built by CI, distributed over S3 and
fetched to local disk by version when noded starts:

| Artifact | Contents | Build | Versioning |
|---|---|---|---|
| Guest kernel | 6.x LTS, a trimmed config with virtio/vsock/nfs/overlayfs and the other essentials built in, bzImage | Kernel source + config in the repo, reproducibly built by CI | Its own version number; recorded in the manifest, and snapshot restore verifies it matches |
| Agent disk | An erofs read-only image: the static beand binary + busybox-level tools | Packaged by CI, released with the same version as noded | Follows the noded version; older versions are kept until nothing running references them |

- Storage: `s3://bean/artifacts/{kernel,agent-disk}/<version>/` + sha256 verification
- noded's configuration declares the version (defaulting to the noded release), cached locally under `/var/lib/bean/artifacts/`
- The container tier's agent bind-mounts the same binary from inside the agent disk, so both tiers come from a single build artifact

## 4. Image Module ⚠️

### 4.1 The overlaybd-direct main route 📐

The image module manages the overlaybd ublk daemon directly (without going
through a containerd snapshotter): from the image metadata (the layer list pushed
down by the control plane + S3 blob references) it generates an overlaybd config
→ the ublk device becomes ready → it is handed to the runtime. For the
demonstrated details see the local AgentENV source (`src/overlaybd/`, uvm-ublk
under crates, and the registryfs_v2 remote direct-read mode).

| Format | How it is consumed | Use case |
|---|---|---|
| overlaybd (block-level, DADI) | fc tier: the ublk block device attached over virtio-blk; container tier (P5): the same device mounted | The main path; blobs live in S3 and ublk range-reads on demand |
| Standard OCI (gzip layers) | containerd overlayfs (container tier only, P5) | Fallback: unconverted images |

- image-service (a logical module of the control plane, see 4.4) is responsible for converting images to the overlaybd format **offline** (the `convertor` tool, with layer-by-layer incremental conversion); conversion is done once on the server side, so the node side has zero conversion cost
- Conversion is non-blocking: if an image has no overlaybd version the first time it is used, the fc tier (the main one) either waits for the conversion or fails with a clear message telling the caller to prewarm first, while triggering the conversion in the background (with the container tier's standard pull as a fallback, P5)
- Writable layer: overlayfs upper on the container tier (XFS quota); a separate sparse overlay disk on the fc tier (see §3.2)

### 4.2 Cache Management 📐

```
/var/lib/bean/
├── cache/               # the sacrificeable pool (LRU)
│   ├── content/         #   containerd content store (standard layer blobs, the fallback path)
│   ├── snapshots/       #   overlayfs/overlaybd snapshot directories
│   └── obd-cache/       #   overlaybd block chunk cache (results of S3 range-reads)
└── sandboxes/           # the non-sacrificeable pool: writable/overlay disks, lifetime bound to the sandbox
```

- The LRU accounts at "image" granularity (with reference counting when blocks are shared by several images), and the chunk cache has its own LRU
- Watermark control: the cache pool above 85% triggers background GC, above 95% refuses new PULLING and reports that to the scheduler; the headroom in the sandboxes pool is a hard scheduling constraint
- Cache inventory digest: the heartbeat carries the delta of the local set of image refs + a bloom filter + the byte fraction (used by the scheduler's image affinity)

### 4.3 Prewarm ✅

- Once a PrewarmImage command arrives it is queued by priority, subject to dedicated bandwidth/concurrency limits (it does not compete with online PULLING)
- overlaybd prewarm = fetch the metadata + prefetch the hot blocks (following an access trace where one exists, otherwise everything)
- overlaybd natively supports recording a boot IO trace (`record-trace`); collect it on the first run and prefetch precisely from the trace afterwards — markedly effective for a fixed set of eval images

### 4.4 image-service Deployment Form ⚠️

image-service is a **logical module of the control plane**
(`internal/control/image`), not a separately deployed service; through P0–P2 it is
embedded in the bean-api process. Its responsibilities need a global view, which
is why they cannot be pushed down to the nodes:

- Global deduplication of format conversion (an image is converted once, and multiple nodes do not fight over it)
- Prewarm orchestration needs the cache view across all nodes
- S3 blob GC needs global reference counting (image ↔ blob ↔ running sandbox/snapshot)

Conversion tasks are CPU-heavy and can be split into a horizontally scalable
dedicated worker pool once the volume grows (the interfaces are already isolated
along the module boundary).

## 5. Network Module 📐

> **This entire section has no code.** A repo-wide
> `grep -rn 'nftables\|netns\|veth\|bean0'` returns 0. The sandbox currently
> **has no networking** — on the fc tier there is only vsock to the agent, and
> that is a control channel. What follows is design intent, not current
> behaviour; for the security semantics see security-and-startup.md §A4.


### 5.1 Data Plane 📐

```
Create:
1. ip netns add bean-<id>
2. veth pair: veth-<id> (host) ↔ eth0 (netns), parent bridge bean0 (10.100.0.1/24)
3. Configure the IP inside the netns (node-local IPAM, bitmap allocation), default route → 10.100.0.1
4. resolv.conf points at the node's DNS forwarder (auditable; upstream configurable, default 1.1.1.1)
5. Inject the sandbox hostname into /etc/hosts
```

### 5.2 nftables Rules (one set per node + a per-sandbox chain) 📐

```
table inet bean {
  chain forward {
    # sandbox → public internet: SNAT egress (masquerade in the nat table)
    iifname "bean0" oifname != "bean0" ct state new accept
    # forbid sandbox ↔ sandbox
    iifname "bean0" oifname "bean0" drop
    # forbid access to the node's internal ranges and metadata services
    iifname "bean0" ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.169.254 } drop
      # note: 10.100.0.1 (the gateway itself) and the DNS exception are handled by preceding accept rules
  }
  chain input {
    # only allow the DNS/agent ports the sandbox needs towards the gateway
  }
}
```

- Prerequisite: `br_netfilter` enabled (so bridged traffic traverses the forward chain; otherwise sandbox↔sandbox on the same bridge passes at layer 2 and bypasses the rules); the node bootstrap script pins that sysctl
- Bandwidth/connection count: per-sandbox tc egress rate limiting (a config item, default 100 Mbps) + a conntrack connection cap (to prevent scanning / DDoS amplification)
- `networkPolicy: none` → the netns has no default route, purely local loopback (except for the host NFS/gateway address, see §3.3)
- `allow-list` (reserved) → insert accept rules for target CIDRs into the per-sandbox chain
- Port exposure opens no inbound DNAT — regional proxy → noded sbxproxy → direct connection to the sandbox IP (see api-design.md §6.2); the node firewall's inbound side is open only to the control plane/proxy

### 5.3 fcRuntime Compatibility 📐

FC is not configured with a tap NIC today; there is no network device in
`fcMachineConfig`.


The FC microVM would use a tap device in place of the netns end of the veth,
joined to the same bean0 bridge, with the nftables rules unchanged.

## 6. beand ✅

### 6.1 Injection and Startup ✅

> This section describes **container-tier injection** (bind mount + entrypoint
> override, arriving with P5); for the agent-disk injection on the fc main path
> see §3.1/§3.4.

1. noded publishes the directory `/var/lib/bean/agent/<version>/beand` (statically linked against musl, ≈8 MiB)
2. The OCI spec gains a read-only bind mount: `/var/lib/bean/agent/<ver>/beand → /.bean/agent`, plus the socket directory `/run/bean/<id>/ → /.bean/run/` (read-write)
3. The entrypoint is overridden to `/.bean/agent`; the original image's entrypoint/cmd/env/user/workdir are serialized into a spec annotation for the agent to read
4. On startup the agent listens on the unix socket `/.bean/run/agent.sock` (noded connects directly from the host side at `/run/bean/<id>/agent.sock`) and reports Ready
5. When `autoStartCmd=true` or a StartUserProcess arrives, the agent forks the user process following the original entrypoint semantics (setuid to the image's USER, applying env/workdir)

Version upgrades: the agent is released as part of the noded package, the
directory is versioned, and running sandboxes are unaffected (older version
directories are kept until nothing references them).

Path conflicts: if `/.bean` collides with the image contents (extremely rare),
creation fails with an explicit error, and an alternative mount point can be
configured.

### 6.2 PID1 Responsibilities ✅

- **Zombie reaping**: `SIGCHLD` reaps every orphan
- **Signal forwarding**: SIGTERM → the user process group, SIGKILL after the graceful timeout
- **Process management**: a table of exec sessions (id → process group), supporting signal/kill for an individual session

### 6.3 Exec / PTY ⚠️

- Plain exec: `os/exec` + pipes, stdout/stderr split, output truncated at a limit
- PTY: `creack/pty`, resize frames call `TIOCSWINSZ`; the session is bound to the WS connection and is kept for 60s after a disconnect so it can be reattached (reconnect token)
- Concurrent exec has no global lock, with a per-sandbox cap (default 32 sessions)

### 6.4 File Operations ✅

- Streaming gRPC chunks (1 MiB per frame), preserving mode/uid/gid; directory-tree operations offer a `tar` mode (an uploaded tar is unpacked automatically, a downloaded directory is tarred) — the main path for eval batches injecting repo snapshots
- Large artifacts are pushed straight to S3: once the agent receives a command containing a presigned URL it PUTs from inside the container (over the sandbox's egress path), consuming none of noded's bandwidth

### 6.5 Logs ✅

- The user process's stdout/stderr → a ring buffer (8 MiB) + an optional live stream
- Before destruction noded has the agent archive the full log to S3 through a presigned URL

### 6.6 Transport Abstraction ✅

The agent's gRPC listener is abstracted as a `Transport` (a unix socket
implementation and a vsock implementation), so on the fc tier the agent code is
unchanged and only the transport (vsock) and the injection vehicle (the agent
disk, decision in §3.1/§3.4) differ.

## 7. Registration, Heartbeat, Leases and reconcile ✅

### 7.0 Node Registration and Credential Layering ✅

```
Admin: register a region on the control plane (S3 endpoint, proxy group, BYOC token service address)
      → generate a region bootstrap token (short TTL 24h, optionally use-limited, revocable)
Node: noded starts with the token configured → Register(token, region, capabilities, labels)
    → the control plane verifies the region is registered and the token is valid (a BYOC region can require manual approval)
    → the control plane issues a node token (short-lived, bound to nodeId+region)
    → every subsequent RPC carries the node token in metadata, and the heartbeat renews it automatically
```

**Transport and identity layering (following the cloud-hosted ingress, no mTLS
introduced)**:

- Transport: one-way TLS — the control plane is exposed through a hosted gRPC ingress (the gateway terminates TLS), and the node verifies the server with the system CA, so there is **zero certificate configuration** (consistent with the existing nexus edge-node model)
- Node identity: an application-layer node token (held in memory, never written to disk; a restart re-registers); the control plane verifies the token↔nodeId binding — a node can only report on or operate the sandboxes scheduled to it
- Token exposure: short-lived + bound to a nodeId, so misuse requires forging the heartbeat stream as well; revocation is immediate

Three credential layers with non-overlapping responsibilities:

| Credential | Permissions | Lifetime |
|---|---|---|
| region bootstrap token | **Register only** (registration-only, no data read or write of any kind) | Short TTL + use-limited; the worst outcome of a leak is a fake node registering, while task presigned URLs authorize only their own paths, plus the approval loop |
| node token | Identity for heartbeat/commands/SyncState, bound to nodeId+region | Short-lived, renewed by heartbeat; held in memory; revoked on exit or anomaly |
| S3 access | Image blobs = STS read-only (limited to the region bucket prefix); writing artifacts/snapshots = presigned per operation | STS 1h; presigned 15min |

BYOC: a customer node only needs outbound access to the hosted ingress (443, zero
certificate configuration), and identity lives entirely in the application-layer
token.

### 7.1 Heartbeat ✅

- A bidirectional stream at 3s intervals; it carries resource watermarks, per-sandbox {id, state, resource-usage summary}, the image cache delta, and the command ids currently executing
- The control plane not receiving anything for 15s (5 intervals) → the node goes SUSPECT → after 30s → LOST: the RUNNING sandboxes on it are marked LOST and the scheduler stops dispatching to it
- Recovery from a network blip: once the stream is re-established the full state is reported once; direct commands issued by the control plane during that window fail and are retried, and exceeding the threshold triggers rescheduling

### 7.2 noded Restart reconcile ⚠️

What is implemented is the **control-plane-side** reconciliation: `SyncState`
fetches the desired list and destroys local sandboxes that should not be there
(`register.go`). **What is not implemented is host resource reconciliation** —
after a restart nothing scans `losetup -a` / `dmsetup ls`, so the loop devices
and dm mappings left behind by the previous generation of the process are never
reclaimed. This has already produced a measured leak: restart noded once and the
shared base image gains another loop device (see the TODO in `docs/status.md`).


```
1. Enumerate the actual local state: live firecracker processes (by the
   /run/bean/fc/<id>/ jailer directory + pidfile convention, the fc-tier main
   path) ∪ containerd tasks (container tier, if enabled)
2. Get the control plane's desired state via SyncState
3. Three-way reconciliation:
   - present on both sides & states agree → re-attach the agent socket, resume monitoring
   - present on the control plane, absent locally → report FAILED (the upper layer decides whether to rebuild)
   - present locally, absent on the control plane (an orphan) → destroy + clean up netns/mounts/IPAM
4. Report everything and resume the heartbeat
```

netns/veth/nftables chains all follow the `bean-<id>` naming convention, so the
orphan scan compares by prefix against the set of live sandboxes.

### 7.3 GC Triggers ⚠️

| Object | Policy |
|---|---|
| Idle sandbox | Local idle detection on noded (the lifecycle is pushed down with create): no exec/port/file activity for idleTimeout → execute onIdle (pause/kill) and emit an event — no dependency on the control plane being online |
| Lingering PAUSED | Not reclaimed by default; an administrator can optionally enable a global policy (superseded by snapshot archival after P4) |
| Image/chunk cache | §4.2 watermark LRU |
| exec session | 60s disconnected with no reattach |
| Temporary files (S3 staging downloads) | An S3 lifecycle rule at 1 day |
| Terminal-state sandbox records in Postgres | A control-plane archival job moves them to cold storage after 30 days |

## 8. noded's Own Observability ✅

- Prometheus endpoint `--metrics <addr>` → `GET /metrics` (unauthenticated, scraped locally); a later package will export the same registry over OTLP:
  - `bean_node_create_phase_seconds{phase,runtime}` a histogram of the duration of each creation phase
    (phase: runtime_create / agent_ready / total; image_pull / rootfs / network to be added)
  - `bean_node_creates_total{outcome,runtime}`, `bean_node_destroys_total{outcome,runtime}`
  - `bean_node_idle_actions_total{action,outcome}` idle reclamation actions
  - `bean_node_sandboxes{state}`, `bean_node_requests_in_flight` (recomputed at scrape time)
  - Still to add: cache hit rate, nftables rule count, IPAM utilisation
- Per-sandbox resource time series (cgroup/FC stats → OTLP, with sandbox_id/labels as attributes); the agent can optionally pass through OTLP from applications inside the sandbox (localhost:4317 → forwarded over vsock)
- Structured logging (zap), with request_id propagated
- pprof port (internal network)

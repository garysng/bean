# Security Model and Fast Startup Design

> 中文版:[zh/security-and-startup.md](zh/security-and-startup.md)

## Part A — Security Model

### A1. Threat model

What runs inside a sandbox is **AI-generated untrusted code** (eval tasks, agent rollouts),
and the assumption is that the attacker fully controls the processes inside the sandbox.
What has to be defended:

| Threat | Consequence | Line of defence |
|---|---|---|
| Kernel escape | Take over the node | FC microVM / gVisor isolation tier (A2) |
| Lateral movement | Reach other sandboxes / internal services | 📐 the network stack is unimplemented, sandboxes currently have no network (A4) |
| Credential theft | Obtain S3/control-plane credentials | Zero long-lived credentials (A5) |
| Resource abuse | Mining, fork bombs, filling the disk | ⚠️ the guest kernel limiting itself plus the CoW disk size; a host cgroup around the VMM exists but is off unless `--fc-cgroups` is set (A3) |
| Egress abuse | Use it as a jump box to attack outward, DDoS | 📐 same as above, unimplemented (A4) |
| Malicious images | Supply-chain poisoning | Image provenance control (A6) |
| The agent's attack surface | Attack the agent from inside the container → noded | Minimal API + socket permissions (A7) |

### A2. Isolation tiers ⚠️ (an internal mechanism, not exposed outward; the tier-selection rules are in architecture.md D3)

**Only the `fc` and `local` tiers exist today.** `local` is a process-level sandbox for
development and CI only, with **no isolation whatsoever**; it is not in the table below and
should not be used for untrusted code. The container tiers (runc/runsc) are implemented
(`--runtime runsc|runc`); runsc is the one to reach for with untrusted code, since runc
shares the host kernel.

| Actual tier | Runtime | Escape defence | When it is used |
|---|---|---|---|
| `fc` (the default main tier) | Firecracker microVM | A hardware virtualisation boundary, with the smallest host-facing surface (FC's device model is minimal, plus built-in seccomp). **jailer is not wired up yet**, see A3 | KVM nodes — regular eval/rollout |
| `runsc` 📐 | gVisor | A userspace kernel intercepts syscalls, and the host kernel surface is ≈70 syscalls | The degraded tier for nodes without KVM. **Unimplemented** |
| `runc` 📐 | runc | namespace/seccomp/caps only | Internal trusted tasks + GPU (reserved internally). **Unimplemented** |

- In the fc tier the guest is a real kernel, so gVisor's syscall-compatibility problems do not apply
- Having runc carry the GPU means **GPU eval is more weakly isolated than the default tier** —
  a separate node pool for GPU nodes plus a tightened image allowlist serve as compensating
  controls; gVisor GPU support (nvproxy) is a P5 evolution item
- The compatibility regression set for the runsc degraded tier arrives with the P5 container tier; incompatible images are exempted explicitly, never downgraded silently

### A3. Hardening baseline ⚠️

**The actual state today, stated up front:**

| Hardening item | Status | Notes |
|---|---|---|
| Hardware virtualisation boundary | ✅ | A real Firecracker microVM, this is the main line of defence |
| seccomp on the FC process itself | ✅ | Firecracker's built-in strict profile, in effect as long as `--no-seccomp` is not passed |
| Hard limit on writable-layer disk size | ✅ | The host assembles the CoW file, and its size is naturally the ceiling |
| Self-limiting resources inside the guest | ✅ | The guest kernel manages itself, and the only thing it can exhaust is its own VM's resources |
| **jailer (chroot + device allowlist)** | 📐 | **Unimplemented.** noded execs the firecracker binary directly. The chroot and the narrowed `/dev` are GitHub #20 phase 2, blocked on placing the per-sandbox dm device inside a jail — see [jailer.md](jailer.md) §3 |
| **Privilege drop (separate uid/gid)** | ⚠️ | Implemented, **off by default**, `--fc-vmm-uid` / `--fc-vmm-gid`. Drops the VMM to an unprivileged uid; does **not** confine what it can see (see below) |
| **Host-side cgroup wrapping the FC process** | ⚠️ | Implemented, **off by default**, `--fc-cgroups`. Memory ceiling (RAM *and* swap), CPU quota and pid cap per sandbox, from that sandbox's own spec. **Requires cgroup v2**; a v1 node refuses to start rather than run unlimited. A node without the flag is unchanged and has no kernel-enforced limit |
| **rlimits on the FC process** | ⚠️ | `RLIMIT_NOFILE` and `RLIMIT_NPROC`, applied only when the privilege drop is on (they travel with `--fc-vmm-uid`) |

This section previously wrote jailer and cgroups up as "delivered in P2", and that was wrong
— they were never implemented. That kind of error is most expensive in a security document:
readers use it to judge what code they can run. The three ⚠️ rows above are the reason this
paragraph stays: they are code that exists and is **not on unless a flag turns it on**, which
is a different claim from "delivered", and the distinction is the whole point.

**What missing jailer still means**: the FC process runs in the host's mount namespace with no
chroot and no device allowlist. With `--fc-vmm-uid` it is at least not root, so an FC or KVM
vulnerability yields "an unprivileged user with a full view of the host filesystem" rather than
"host root" — but the *view* is unnarrowed, and narrowing it is what the mount namespace does.
That is the substantive half still outstanding; [jailer.md](jailer.md) §7 itemises what each
half buys. Note also that `PR_SET_NO_NEW_PRIVS` is **not** set: Go's `syscall.SysProcAttr` has
no such field (checked against go1.26.1 — jailer.md §7 claims otherwise and is wrong), and
setting it needs a wrapper binary, which is ruled out because the pid noded records would be
the wrapper's rather than the VMM's.

**What the cgroup now does, and what it does not**: with `--fc-cgroups` the VMM sits in a group
with a memory ceiling derived from its guest's declared RAM plus a fixed headroom, swap refused
outright (`memory.swap.max=0`), a CPU quota from the same vCPU count the machine configuration
gets, and a pid cap. Capping swap is what makes the ceiling a stop rather than a slowdown, and
it is why v2 is required — see below. That is the kernel
enforcement `overcommit.go` and `cmd/noded/main.go` both name as the prerequisite for raising
memory overcommit above 1.0 — the other prerequisite, a measurement of how far a guest's real
footprint sits below its declaration, still does not exist, so the ceiling's headroom is
deliberately generous rather than tight. **Without the flag nothing changed**: the committed
quantity is only the scheduler's ledger, and under host memory pressure there is no
kernel-level fairness guarantee (see architecture.md D12).

**cgroup v2 is a node requirement, and the reason is swap.** `--fc-cgroups` refuses to start on
a v1 host. This is not a preference for the newer interface:

- v1's only swap-aware ceiling is `memory.memsw.limit_in_bytes`, which exists **only** when the
  kernel booted with `swapaccount=1` — off by default on every distro kernel checked. So a v1
  memory ceiling bounds RAM and not swap, and the kernel's cheapest way to satisfy a VMM at its
  ceiling is to push pages to swap. The group stays under its limit, every log line reports the
  limit as enforced, and the host thrashes. A guest cannot tell its pages being on host swap
  apart from a hang.
- That is precisely the failure the ceiling exists to prevent, in precisely the scenario the
  feature was built for: overcommitted memory for untrusted evaluation workloads. On v1
  "limits are in place" would be untrue in the dimension that matters most, which is worse
  than not supporting v1 at all. v2 spells it `memory.swap.max` and needs no boot parameter;
  bean sets it to 0.
- The requirement is not an exotic ask. systemd has defaulted to the unified hierarchy since
  v243, so **Ubuntu 22.04+, Debian 11+, RHEL 9+** and anything newer are already v2. (Ubuntu
  20.04 is v1 — it patched the default back until 21.10 — so a 20.04 host does not meet the
  requirement.)

Two further points worth stating rather than leaving as absences:

- **A v1 host is refused, not silently downgraded.** The distinction is the point: a node with
  cgroups switched off is an operator's informed choice and says so at startup, while a node
  that quietly dropped to no enforcement because it was v1 leaves somebody believing there is a
  boundary where there is none — and raising `--overcommit-memory` on the strength of it. That
  is the same class of error as this section's original wrong claim, and in code rather than
  prose.
- **A node whose kernel lacks one of the controllers starts anyway**, with the remaining limits,
  and says so. Refusing there would take a working node out of service over an absence the
  startup line names outright — unlike v1, which offers a ceiling that *looks* enforced. The
  cost is that "limits requested" and "limits in force" can differ, which is why the startup
  line names the controllers that are *missing* as well as the ones in force.

**The privilege drop's uid is per node, not per sandbox.** Every sandbox on a node shares it, so
one compromised VMM can reach another sandbox's directory. A per-sandbox uid needs a reserved
range and an allocator with the same reclaim problem as every other per-sandbox resource here,
for a boundary between processes that are each already behind their own VM; it is deferred, not
dismissed. Turning the drop on requires the node's shared assets (guest kernel, agent disk) to
be world-readable and the uid to be in the group owning `/dev/kvm`; both are checked at startup
and are fatal, because each otherwise fails every create on the node with a symptom that does
not name its cause.

**Container tier** (runc/runsc, ✅ implemented, `--runtime runsc|runc`):

- cgroup v2 hard limits: cpu.max, memory.max (+ memory.swap.max=0), pids.max (4096 by default, against fork bombs), io weights
- Disk-write ceiling: XFS project quota on the rootfs writable layer (20 GiB by default, configurable)
- `no_new_privileges=true`; every capability dropped and then added back as needed (by default only CHOWN/SETUID/SETGID/DAC_OVERRIDE/FOWNER/KILL — enough for package managers and ordinary builds)
- A default seccomp profile (the runc tier uses containerd's default plus blocking keyctl/bpf/userfaultfd and the like; in the runsc tier gVisor has already narrowed it itself)
- `/proc` and `/sys` are handled per the OCI default masked/readonly paths
- No docker.sock mount, no privileged mode, host network/pid/ipc refused (there is no such option at the API layer)

**fc tier** (inside the guest the agent is the root init, so container hardening items do not apply and the defences sit on the host side):

- ✅ seccomp on the FC process itself (Firecracker's built-in strict profile, on by default)
- ✅ Guest disk-write ceiling = the size of the writable-layer file (assembled by the host, a natural hard limit)
- ✅ pids/fork bombs: the guest kernel limits itself (the only thing it can exhaust is its own VM's resources)
- 📐 jailer: chroot + device allowlist — **unimplemented** (GitHub #20 phase 2)
- ⚠️ Separate uid/gid for the FC process — implemented, off unless `--fc-vmm-uid` is set
- ⚠️ Host-side cgroup wrapping the FC process (cpu/mem+swap/pids) — implemented, off unless `--fc-cgroups` is set; requires cgroup v2 and refuses to start on v1

### A4. Network security 📐

**The entire section is unimplemented.** `grep -rn 'nftables\|netns\|veth\|bean0'` across the
repo returns 0 — there is no network module, and a sandbox currently has no network capability
at all (in the fc tier there is only vsock to the agent, and that is a control channel, not a
data network).

That means every line below is a **plan**, not a current security promise. In particular
"egress-only by default" — right now there is neither egress nor any isolation rule, because
there is no network stack at all. Do **not** read this section as "the sandbox is already
confined by these rules".

See noded-design.md §5 (equally unimplemented). The planned security semantics:

- `egress-only` by default: the public internet is reachable (pulling dependencies is a hard requirement for eval), and what is **forbidden** is: sandbox-to-sandbox access, the node's internal segments (RFC1918), and cloud metadata (169.254.169.254 / fd00:ec2::254)
- Per-sandbox egress bandwidth limiting (tc, 100 Mbps by default) + a conntrack connection cap (against port scanning / DDoS amplification)
- The `none` policy serves purely offline eval: no default route, ruling out data exfiltration (useful for model-cheating detection); volumes do not break that promise — a dataset volume is a local block device and a shared-fs volume goes over host NFS (the traffic only reaches the host gateway and never leaves the node), and both are orthogonal to "reaching the public internet". If even host shared storage has to be forbidden, simply mount no volume at creation
- DNS goes through a node forwarder, which can record an audit log
- Zero inbound exposure: no DNAT, and the only ingress is the application-layer path proxy → noded → agent

### A5. Credentials and the trust chain ⚠️

Implemented: bootstrap-token registration + node-token authentication, registry credentials
AES-256-GCM encrypted at rest, long-lived S3 credentials held only by the control plane, fc
tier vsock (the host-side FC API socket is reachable only by noded).
Unimplemented: presigned URL injection into the sandbox, STS read-only role rotation, sandbox
JWT, the TLS termination layer.


```
Long-lived S3 credentials: held only by the control plane
   ├── node artifact upload/snapshot: presigned URL (TTL 15min, bound to a key prefix + content-length)
   ├── overlaybd block reads: noded holds an STS read-only role (scoped to the blob bucket prefix, rotated hourly)
   └── direct artifact upload from inside the sandbox: a presigned PUT URL is injected (even if leaked it can only write the specified key)
Control plane ↔ noded: one-way TLS (terminated at the cloud managed gRPC ingress layer, zero certificate configuration on the node)
   + an application-layer node token (short-lived, held in memory, bound to nodeId — the
   control plane validates that a node can only operate on its own sandboxes); registration
   uses a bootstrap token, and credential tiering is in noded-design §7.0
noded ↔ agent: container tier over a unix socket (0700, on the host reachable only by the
   noded user; inside the container the mount point is readable only by root); fc tier over
   vsock (on the host the FC API socket is reachable only by noded, and inside the guest
   /dev/vsock is by default openable only by root — a non-root user process cannot call the
   agent API)
sandbox token (JWT): the signing key is held by the control plane, bound to sandbox-id + an expiry
```

### A6. Image provenance ⚠️

- First pass: only registries / S3 blob sources on a configured allowlist are permitted
- Image digests are fixed: scheduling and caching all key off the digest (the tag is resolved once at the entrance), guaranteeing eval reproducibility
- Reserved: the integration point for image signature verification (cosign) sits in image-service's resolution layer

### A7. Controlling the agent's attack surface ✅

- **On a sandbox with no networking, the agent is unreachable from inside by construction.** The container tier's agent is on a unix socket outside the guest's mount namespace, and an fc sandbox without `--guest-subnet` is on vsock, which is a host-to-guest address family no guest process can dial. Neither depends on a credential.
- **On a networked fc sandbox, that is no longer true, and a per-sandbox token is what replaces it.** The agent listens on TCP (`10001`) so that one addressing scheme covers both it and any port a user exposes — which is what makes port exposure and the data plane one mechanism. Any process in the sandbox can now connect: **measured**, a probe inside the guest reaches `127.0.0.1:10001` successfully.

  What holds instead is a token minted per sandbox, whose **hash** is published to the guest through MMDS. Three properties matter, and all three were verified on hardware:

  | Attempt from inside the sandbox | Result |
  |---|---|
  | No credential | `PermissionDenied` |
  | A guessed credential | `PermissionDenied` |
  | **The hash the guest can read from MMDS** | `PermissionDenied` |

  The third row is the point: the guest can read `agentTokenHash` and still cannot construct a usable token. The token is per sandbox, so reading one out of a compromised sandbox yields nothing for any other sandbox or node.

  Verification fails closed in all three directions — absent credential, unreadable metadata service, unpublished hash. The middle one matters most: a sandbox holding root can attempt to break its own metadata service with iptables, and treating "could not determine what to require" as "require nothing" would make that an attack rather than an outage.
- The agent runs as root (it has to setuid to the image's USER), so even with the token, a caller can only invoke operations equivalent to the privileges the sandbox's occupant already holds — there is no privilege gain, only the loss of the structural guarantee above.
- The agent binary is mounted read-only and cannot be replaced from inside the container
- The noded side applies length/rate limits to agent responses, so a compromised agent cannot turn around and attack noded

### A8. Platform surface 📐

- Audit every write operation on the API (who/what/when, Postgres + S3 archive)
- Minimise the node: a dedicated OS image, no superfluous services, an assessment of running noded non-root (P3; containerd, if enabled, P5)
- Run a sandbox-escape regression suite every cycle (mainly the FC/KVM attack surface; once the container tier arrives, add a subset of the gVisor exploit suite)

---

## Part B — Fast Startup

### B1. Cold-start budget ⚠️

**Measured** (real KVM machine, alpine, cache hit): create end to end **952ms**,
made up of `runtime_create` 234ms + `agent_ready` 770ms (overlapping). The "P50 < 2s on a hit"
target is met.

The cold-image target is not met and does not go through lazy-pull: today it is "pull the
whole thing + convert + share CoW", measured at 5-10s for busybox and **2m45s** for alpine on
an unstable network — which makes prewarm a requirement rather than an optimisation. The
overlaybd path is now implemented (B2) and opt-in with `--fc-overlaybd`, but lazy pull
against a registry — the part that would remove this wait — is untested.

**All of these numbers were measured by hand on a single sandbox.** How they degrade under
concurrency has never been measured — see the load-testing to-do in `docs/status.md`.

The original targets: **P50 < 2s on a cache hit; P50 < 10s for a cold image (overlaybd
lazy-pull)**. Broken down (the fc tier as the example; the container tier is faster by the
absence of the VM boot item):

| Phase | Cache-hit target | Cold-path target | Means |
|---|---|---|---|
| API + scheduling | 50 ms | 50 ms | Scheduler state in memory, no synchronous outbound calls |
| Command reaches noded | 50 ms | 50 ms | Direct push over gRPC (control plane → noded) |
| Image ready | ~0 (already cached) | 2–6 s | overlaybd: pull only metadata + the hot startup blocks (see B2) |
| rootfs device ready | 100 ms | 200 ms | ublk device assembly, overlaybd metadata cache |
| netns/network | 50 ms | 50 ms | Batched atomic veth/nftables operations; IPAM as an in-memory bitmap |
| Sandbox start | 200–500 ms | 200–500 ms | FC microVM start ≈125ms + kernel boot; container tier runc ≈100ms / runsc ≈300ms |
| agent ready | 100 ms | 100 ms | A static binary, no dependency loading |
| **Total** | **≈1–1.2 s** | **≈4–8 s** | |

Every phase is instrumented into the creation-latency histogram (the noded exporter), for regression monitoring.

**Measured** (2026-08-02, real KVM machine, image already cached, no network item):
`runtime_create` 234ms + `agent_ready` 770ms = **952ms**, inside the budget.
But the budget table's attribution is wrong: it puts the cost on "VM start + kernel boot",
while the two largest measured costs are both in our own code — gRPC reconnect backoff
(800ms) and the guest's synchronous serial writes (493ms), with the kernel itself worth only
90ms. See `docs/decisions.md` §5.

The restore path: 950ms (1617ms the first time). Guest memory is served on demand through
userfaultfd, and FC's `/snapshot/load` accounts for only 7ms.

### B2. overlaybd lazy-pull from S3 ⚠️

**`OverlaybdProvider` exists and is verified on hardware; lazy pull specifically is
implemented but untested.** The default path remains dm-snapshot: pull the whole thing +
convert + share a read-only base + one CoW per sandbox (measured at 44 KiB per sandbox).
overlaybd is opt-in with `--fc-overlaybd`, and `--fc-overlaybd-lazy-pull` selects
range-reading layers from the registry instead of converting them locally.

What is verified in code (`overlaybd_hw_linux_test.go`): layers are built and sealed,
attached through TCMU, and the mounted device serves the layer's contents. What is not:
the lazy-pull path against a real registry. The numbers below — 7ms to mount, 19.6% of
layer bytes transferred before files can be read, 8 HTTP 206s, a writable upper layer
occupying 40 KiB — come from the manual verification in `docs/decisions.md` §3.1, **not**
from this implementation.

See [image-pipeline.md](image-pipeline.md) §7 for the layer pipeline, the configfs
sequence, and the four constraints that only real hardware revealed.

```
Image publishing path (image-service, once, offline):
OCI image → overlaybd convertor (per-layer incremental conversion) → block-device-layer blobs → S3
                                     │
Node consumption path:               ▼
CreateSandbox → overlaybd/ublk assembles the block device (a few MiB of metadata) → mountable immediately
             → container tier mounts overlayfs / fc tier attaches virtio-blk straight to the guest
             → IO access triggers on-demand block range-reads from S3 → local obd-cache
```

- "Starting" needs only the metadata plus the hot blocks along the entrypoint path, and the
  data a SWE-bench-class image needs to start is usually < 5% of the whole image; after
  overlaybd `record-trace` captures the startup IO sequence, prefetching can be precise
- Block-level dedup: when 2000+ evaluation images share base layers (ubuntu/python), both S3
  storage and the node cache shrink substantially
- This route has been validated in production by AgentENV in the FC + massive-OCI-image
  scenario (a local disk as a bounded cache, with the total image volume allowed to exceed
  disk capacity)
- Risks and countermeasures:
  - S3 first-byte latency jitter → trace-driven prefetch + obd-cache hits as the backstop
  - ublk requires a newer kernel (6.0+) → a uniform node OS baseline; **the tcmu backend is
    measured functionally complete on 5.15** (7ms to mount, only 19.6% of the layer bytes
    transferred, HTTP 206 range reads; see `docs/decisions.md` §3.1), which makes it a usable
    main path rather than a degraded one. It is not merely slower, though: its teardown is 4.0 s
    for 128 devices and identical on 5.15 and 6.8, so ublk is the intended transport and its
    lower layers are now built and verified on 6.8 ([status.md](status.md));
    a node where neither is available does not report the fc capability (fc depends on a block-device backend), leaving only the container tier's overlayfs as the fallback
  - tcmu needs a unique `vpd_unit_serial` per backstore, otherwise the host's `multipathd`
    will merge the devices of different images and return wrong data (silently, with no error)
  - S3 unreachable while running → retry failed block reads + report a sandbox-level IO error (distinct from the task failing on its own)

### B3. Caching and prewarm strategy ⚠️

1. **Node cache** (noded-design §4.2): image-granularity LRU + chunk LRU, with S3 as the source of truth
2. **prewarm API**: before an eval batch starts, the orchestration layer computes the node
   coverage count from "the batch's image manifest × target concurrency" and dispatches the
   warm-up; image-service picks target nodes by node cache level
3. **Image-affinity scheduling**: score = w1·(fraction of layer bytes already cached) + w2·(free-resource fit) + w3·(cache disk type)
   — so repeated eval runs on the same image naturally land on the same set of nodes
4. **Base layers pinned resident**: profile the top shared layers (ubuntu, conda, python) and mark them pinned, exempt from LRU
5. **IO trace recording**: the first run does `record-trace` to capture the block access
   sequence and stores it in S3 metadata, after which prewarm/startup prefetches per the trace
   (a native overlaybd capability)

### B4. Batch launches (the eval storm) ⚠️

Protecting the path when 2000 sandboxes are created at once:

- gateway `batchCreate` → the scheduler decides in batch (bin-packing completed inside one lock acquisition, avoiding 2000 lock contentions)
- A per-node concurrent-creation cap (16 by default). ⚠️ **Today this is a hard filter rather
  than a queue**: once in-flight is full, that node is judged unavailable outright, and a
  single-node cluster will return `NO_CAPACITY`. For batch scenarios that is arguably the
  wrong semantic (the caller sees a failure instead of slowness), and the value 16 has no
  measured basis — see the to-dos and load-testing tasks in `docs/status.md`
- S3 handles concurrent reads naturally; the registry is not on the hot path (all blobs are in S3)
- Connection reuse: noded's S3 client connection pool + HTTP/2

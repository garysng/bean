# Competitive Analysis (2026-07, snapshot section added 2026-08)

> 中文版:[zh/competitive-analysis.md](zh/competitive-analysis.md)

> Perspective: bean's target scenario = AI evaluation / agent rollout, characterised by
> **large numbers of heterogeneous Docker images** (SWE-bench-class, 2000+ images), batch
> launches, short lifetimes, and self-controlled deployment.

> **The option comparison for incremental snapshots** (researched 2026-08, now shipped) is in `docs/decisions.md` §3.0.1.
> Summary of the conclusion: E2B does a layered lookup across base + N diff layers on a UFFD
> fault, and public analysis notes that cross-build fragmentation grows with depth; Cognition's
> blockdiff treats the chain purely as lineage and flattens it into raw before running (relying
> on XFS reflink to make flattening nearly free);
> Firecracker upstream's `snapshot-editor rebase` is also a flatten.
> bean chooses flatten, with the additional reason that snapCache makes a fan-out scenario pay
> for the merge once per node, and the page-fault path is unchanged.

## 1. Vendor by vendor

### Tensorlake (tensorlakeai, pivoted in 2026) ⭐ the closest commercial implementation at bean's layer

The original document-processing/RAG project (indexify) has pivoted into a *sandbox-native cloud
for AI agents*, with three products: Sandboxes (Firecracker microVM), Cloud Volumes (a
content-addressed versioned filesystem), and Orchestrate (`@application`/`@function` serverless
orchestration, one sandbox per function).

- **Isolation**: Firecracker microVM (not containers); memory + filesystem snapshots, instant
  clone, auto suspend/resume, live migration, prewarm pools, egress allow/deny, and
  `https://<port>-<sandbox>.sandbox.tensorlake.ai` ingress — overlapping heavily with bean's
  D9/D11/lifecycle design
- **Stack**: Rust (CLI/SDK/FUSE client) + Python/TS SDKs; the Lattice scheduler and Orion, an
  in-house distributed SQL metadata database (Apache-2.0)
- **Open-source boundary**: the main repo is Apache-2.0 but **only the SDK/CLI/FUSE client**; the
  server and control plane are closed, and the cloud service cannot be self-hosted
- **Activity**: ~976 stars, updated daily, commercially launched
- **What it means for bean**: the closest functional benchmark (volumes, snapshots, ingress,
  orchestration included), but closed-source and not self-hostable — which is exactly bean's
  footing of "self-controlled + BYOC". Three things worth borrowing:
  1. **`oci2rootfs`** (Apache-2.0, Rust): OCI → ext4 rootfs, with complete whiteout/opaque/xattr
     handling, but fully pre-materialised with no lazy load — usable as bean's **fallback
     converter for unconverted images** (overlaybd direct attach remains the main path and
     performs better)
  2. **Image as snapshot**: any sandbox snapshot can be `register`ed as a named image, which is
     practical for the "set up the environment once, reuse in bulk" scenario (see roadmap P4)
  3. **`harbor`** (same org): an agent evaluation / RL environment framework, which is precisely
     bean's target scenario, and its API shape is worth comparing against

### AgentENV (kvcache-ai / Kimi, open-sourced 2026-07) ⭐ the most direct benchmark

Built for Kimi K3's agentic RL training, and its target scenario (batches of heterogeneous images
+ RL rollout) all but coincides with bean's:

- **Isolation**: one Firecracker microVM per sandbox
- **Environment**: ✅ any OCI image with zero conversion — **overlaybd + ublk block-level
  on-demand loading**, with the local disk as a bounded cache, so total image volume may exceed
  disk capacity; snapshots can go to S3
- **snapshot/fork**: resume <50ms, pause <100ms, incremental snapshot <100ms; 16 forked child
  instances on a single node; virtio-balloon memory overcommit
- **API**: an E2B-compatible HTTP API (an existing E2B SDK works by swapping the endpoint) + a reverse proxy
- **Maturity**: the single-machine path is production-validated at Kimi; **the multi-node control plane is officially labelled a prototype**
- **What it means for bean**: it validates the whole technical route of "overlaybd block device
  attached directly to FC + any OCI image" (bean's D4/D9 adopt the same route); its weak spots
  (multi-node scheduling, quota, prewarm orchestration, the operations surface) are exactly where
  bean's own work is concentrated

### CubeSandbox (Tencent Cloud, open-sourced 2026-04, Apache-2.0)

- **Isolation**: an in-house RustVMM/KVM hypervisor (reworked Cloud Hypervisor/Kata components), Rust throughout
- **Environment**: ❌ the image→template conversion route (same as e2b), not direct any-OCI boot — it does not solve the heterogeneous-image-batch pain point
- **Cold start**: 60ms (resource pooling + snapshot cloning; P95 90ms at 50 concurrency); memory overhead <5MB/sandbox
- **snapshot**: the CubeCoW engine — checkpoint/rollback/fork; AutoPause/AutoResume
- **Components**: CubeAPI (E2B compatible) / CubeMaster / Cubelet / CubeVS (eBPF network isolation) / CubeEgress (an L7 egress gateway: domain filtering, credential injection, auditing)
- **Maturity**: production-validated at Tencent Cloud, with complete multi-node cluster capability
- **What it means for bean**: the template route does not fit the eval scenario, but **CubeVS's
  eBPF network isolation and CubeEgress's L7 egress governance** (credentials never entering the
  sandbox) are reference designs for bean's P5 network evolution

### e2b (e2b.dev)

- **Isolation**: Firecracker microVM
- **Environment**: ❌ cannot run OCI images directly. A Dockerfile is only build input, and
  `e2b template build` converts it into a VM rootfs snapshot registered in the template library,
  taking **5–15 minutes** on first build; historically there were failures building large images (>4.3GB)
- **Cold start**: ~150–200ms to restore a template snapshot (given the template is ready)
- **pause/resume**: in public beta, FC snapshot (pause ~4s/GiB, resume ~1s, retained 30 days)
- **Open source**: Apache-2.0, self-hostable (1 orchestrator + 2 hosts to start)
- **Pricing**: billed per second, ~$0.05/vCPU·hr; Pro from $150/month
- **For the eval scenario**: 2000 images = 2000 template builds, entirely infeasible. **This is the direct reason bean exists**

### Daytona (daytona.io)

- **Isolation**: Docker containers by default (shared kernel, no microVM/gVisor tier)
- **Environment**: ⚠️ the closest — a "Snapshot" can be created directly from an OCI image in any
  public or private registry, but there is still a registration conversion step, and it is
  AMD64-only
- **Cold start**: claims sub-90ms (down to 27ms with a prewarm pool)
- **snapshot**: supports fork/hibernate
- **Open source**: AGPL-3.0, self-hostable (the licence raises contagion concerns for commercial integration)
- **For the eval scenario**: the image semantics fit best, but containers share a kernel and there
  is **no strong isolation tier**, so running untrusted AI code needs compensating controls
  around it; and there is no S3-lazy-pull-class mechanism for batch image distribution

### Modal Sandboxes

- **Isolation**: gVisor
- **Environment**: ❌ does not accept direct any-OCI boot; the image must be rebuilt through the
  SDK's `Image.from_registry` with their in-house builder, which requires Python inside the image
  (or injects it), does not support directives such as ONBUILD/VOLUME, and often needs the
  entrypoint cleared
- **Cold start**: sub-second (an in-house lazy-load filesystem, load-tested at 1000 sandboxes/s)
- **snapshot**: FS snapshots + memory snapshots (Alpha)
- **Open source**: ❌ closed, not self-hostable
- **For the eval scenario**: the strongest infrastructure capability, but closed-source plus the image-rebuild constraint does not satisfy self-control

### Morph Cloud (morph.so)

- **Isolation**: an in-house MorphVM (microVM)
- **Environment**: ❌ no direct OCI boot. Snapshots are built by chaining `.setup()` from a minimal base image; containers can only be second-class citizens inside the VM
- **snapshot**: ✨ its strongest suit — Infinibranch: snapshot a running VM at any instant and fork many branches instantly, with near-zero storage overhead
- **Open source**: ❌ closed; MCU-metered pricing
- **For the eval scenario**: the branch-exploration capability is the benchmark (bean's fc tier is measured against it), but the image model does not match batch eval at all

### microsandbox (open source, Super Rad Company)

- **Isolation**: libkrun microVM (KVM/HVF)
- **Environment**: ✅ runs standard images from any OCI registry directly, zero conversion — **proving "OCI direct boot into a microVM" is technically feasible**
- **Cold start**: claims <200ms (measured ~320ms on Linux)
- **snapshot**: ❌ no productised pause/resume (pre-1.0)
- **Open source**: Apache-2.0, fully self-hostable; the cloud service is in closed beta
- **For the eval scenario**: the right direction but shaped as a local single-machine development tool, with no cluster scheduling / batch distribution / multi-node story; usable as an implementation reference for fcRuntime (the libkrun route vs bare FC)

### CodeSandbox SDK (→ Together Code Sandbox)

- **Isolation**: Firecracker; acquired by Together AI
- **Environment**: ⚠️ a devcontainer + Dockerfile wrapper (Docker running inside the VM), Debian/Ubuntu bases only, template build required
- **snapshot**: mature (hibernate resume 1–2s, fork a running instance <2s)
- **Open source**: ❌ the platform is closed
- **For the eval scenario**: the template system has the same problem as e2b's

### Cloudflare Sandboxes / Vercel Sandbox (briefly)

- **Cloudflare** (GA 2026/4): container isolation; standard images are usable but **the CF runtime must be embedded** (inherit their base image or inject their binary as the entrypoint), and it has to be pushed to the CF registry with wrangler; tied to the CF ecosystem
- **Vercel**: Firecracker; ✅ supports any OCI but it must first be pushed to the Vercel Container Registry; single region (iad1); closed source
- Both are "general-purpose sandboxes tied to a platform ecosystem", not positioned for batch eval

## 2. Side-by-side comparison

| Platform | Isolation | Direct any-OCI boot | Cold start | pause/resume/fork | Open source / self-hosted | Fit for eval batches |
|---|---|---|---|---|---|---|
| **Tensorlake** | Firecracker | ✅ but oci2rootfs pre-materialises fully | prewarm pool | ✅ snapshot/clone/migration | ❌ client only | ⚠️ closed, not self-hostable |
| **AgentENV** | Firecracker | ✅ overlaybd zero conversion | resume <50ms | ✅✨ fork 16 children | Apache-2.0 ✅ | ✅ but multi-node is a prototype |
| **CubeSandbox** | RustVMM | ❌ template route | 60ms | ✅ CubeCoW fork | Apache-2.0 ✅ | ❌ template cost |
| e2b | Firecracker | ❌ template build (5–15min each) | ~200ms | ✅ Beta | Apache-2.0 ✅ | ❌ |
| Daytona | Docker containers | ⚠️ registry pull + registration, AMD64 | <90ms | ✅ | AGPL-3.0 ✅ | ⚠️ no strong isolation / no distribution optimisation |
| Modal | gVisor | ❌ SDK rebuild + depends on Python | sub-second | ⚠️ Alpha | ❌ | ⚠️ closed |
| Morph | MorphVM | ❌ setup chain | <250ms | ✅✨ strongest fork | ❌ | ❌ |
| microsandbox | libkrun | ✅ zero conversion | ~300ms | ❌ | Apache-2.0 ✅ | ⚠️ single-machine shape |
| CodeSandbox | Firecracker | ⚠️ devcontainer wrapper | resume 1–2s | ✅ mature | ❌ | ❌ |
| Cloudflare | containers | ⚠️ must embed their runtime | fast | ✅ | ❌ (SDK open) | ❌ ecosystem lock-in |
| Vercel | Firecracker | ✅ but must push to their registry | seconds | ✅ FS snapshot | ❌ | ❌ single region |
| **bean (target)** | **FC as the default tier (runc for GPU / gVisor as the degraded tier, reserved internally for P5)** | **✅ overlaybd zero conversion, S3 lazy-pull** | **<2s on a hit / <10s cold** | **FC-native snapshot/fork (P3–P4)** | **in-house, self-hosted** | **✅ a first-class scenario (multi-node scheduling / prewarm / quota at the core)** |

> **What "cold start" means in this column.** Nearly every figure quoted above,
> whatever each vendor calls it, is the cost of **restoring** a new sandbox from a
> prepared snapshot or template — not of booting a guest, and not of resuming a
> paused one. Bean's comparable measured numbers are **392 ms** for a restore on a
> node-local cache hit against **952 ms** for a real create
> ([status.md](status.md)); a resume is a vCPU unfreeze and is faster than both while
> doing far less, so it is not the number to compare. Three distinct operations,
> three different costs — [snapshot-resume.md](snapshot-resume.md) §0.

## 3. Conclusion: bean's differentiated position

1. **The technical route is already validated, and the competitive focus is engineering
   completeness**: AgentENV proved that "overlaybd attached directly to FC + any OCI with zero
   conversion" is feasible and production-usable — bean adopts the same route (D4/D9), and
   differentiates by turning to AgentENV's blank spots: **multi-node scheduling (image-affinity
   bin-packing), prewarm orchestration, quota/lease/failure recovery, the GPU path (container
   tier, reserved internally for P5), and a complete operations surface**
2. **No commercial platform has achieved "zero conversion + on-demand loading" yet**: Tensorlake
   goes furthest (oci2rootfs converts OCI to ext4 with zero user action), but it is still fully
   pre-materialised with no lazy load; e2b/Morph/CodeSandbox/Modal require a template or an SDK
   rebuild; the closest, Daytona, has no strong isolation tier.
   Heterogeneous-image-batch evaluation is still a blank spot on the commercial side
3. **Image distribution is the second half of the game**: everyone optimises "one template
   started repeatedly", while eval's pain point is "2000 different images each started a few
   times" — S3 lazy-pull + block-level dedup + image-affinity scheduling + record-trace
   prefetching aim directly at that
4. **Automatic isolation tiering**: fc by default (strong isolation + no compatibility problems),
   GPU falling to the container tier automatically, gVisor as the degraded tier without KVM — one
   API selecting the tier by node capability and task characteristics, whereas the competition is
   all single-form
5. **Self-control**: the whole stack is in-house, deployable on S3 + bare metal / VMs with no
   ecosystem lock-in; the open-source references (AgentENV and CubeSandbox are both Apache-2.0)
   can accelerate the implementation without introducing a dependency

## 4. Signals worth tracking

- **Whether Tensorlake opens its server side or ships a self-hosted edition** — if it does, the functional overlap is the highest
- **How fast AgentENV's multi-node control plane moves from prototype to mature** — if it fills in
  scheduling/quota/operations, "build on top of AgentENV" becomes an option again
- Whether CubeSandbox adds a direct any-OCI boot route
- If Daytona adds a gVisor/microVM tier, its overlap with bean rises markedly
- Whether the evolution of e2b's Build System eliminates the per-image template cost
- Upstream evolution of overlaybd/ublk (the kernel ublk userspace block device ecosystem)

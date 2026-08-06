# Implementation Roadmap

> 中文版:[zh/roadmap.md](zh/roadmap.md)

> Refined from architecture.md §8. Each Phase closes on a demonstrable end-to-end milestone.

## Actual progress today (2026-08-02)

**The Phase division has been scrambled by the path actually taken**, so reading by capability
is more accurate than reading by Phase — the authoritative record is `docs/status.md`. The key
points:

- ✅ **Beyond P0/P1 already**: fc direct boot (952ms), multi-node scheduling with committed
  quantities persisted, S3 snapshot blobs, OCI pull and conversion, commit, BuildKit builds,
  end-to-end OTel trace
- ✅ **The snapshot part of P3/P4 is already done, ahead of schedule**: pause/resume, three
  snapshot variants (full / `--no-memory` / `--base` incremental), UFFD on-demand page serving,
  CPU templates and scheduler CPU filtering
- ⚠️ **The overlaybd ublk direct drive P0 talks about is not done**: it runs on dm-snapshot
  (44 KiB per sandbox). The overlaybd capability is measured but not wired in — it has become an
  optimisation rather than a foundation
- ⚠️ **The jailer P0 talks about is not done**: noded execs firecracker directly
- ✅ **The networking P0/P1 talk about is done**, and it was the largest gap: per-sandbox
  namespace, tap and NAT egress, metadata and RFC1918 denied by default, and a port inside a
  sandbox reachable from outside the node through bean-proxy. Verified on a real kernel,
  denials included. Cross-node sandbox connectivity is still a non-goal
- 📐 **Not done**: the container tiers (runc/runsc), volumes, per-port access control,
  Postgres, the TypeScript SDK

In one sentence: **vertically (snapshots / startup optimisation) it has gone deeper than the
roadmap; horizontally, networking has closed and what remains is the container tiers and
volumes.**

## P0 — Single-node end-to-end skeleton (fc direct boot, no containerd)

Reference implementation: local /Users/mac/project/agentenv (the AgentENV source — a complete
demonstration of uvm-ublk driving overlaybd directly, envd, and jailer/FC management, compared
module by module).

**Scope**

- `proto/`: NodeService / SandboxService / AgentService v1 definitions + buf tooling
- Guest kernel + agent disk build (built by hand at first, the pipeline is P2)
- `noded`: overlaybd ublk direct drive (pre-converted images fully local at first, lazy-pull is
  P2), jailer+FC process management, agent disk injection, basic tap/bridge/NAT networking
- `beand`: the init mount matrix, vsock gRPC, replicating the image config to start the process,
  synchronous exec, file read/write, zombie reaping
- `bean-api` minimal implementation: POST/GET/DELETE sandboxes, exec, files (single node, direct connection, no scheduler)
- state: SQLite/in-memory to begin with (with the Postgres interface abstracted properly)

**Acceptance**

```
curl POST /sandboxes {image} → fc microVM RUNNING
curl POST /exec {pytest} → exit code + output
curl DELETE → resources back to zero (no leftover FC process / tap / ublk device / mount)
```

## P1 — Usable across nodes (first eval integration)

**Scope**

- scheduler: Register/Heartbeat/leases, direct-push command dispatch + SyncState reconciliation,
  bin-packing + image affinity v1 (exact match by ref); the region field enters the model (running
  in a single region)
- Postgres state persistence, terminal-state/orphan GC (P1 covers only explicit kill + LOST
  cleanup; idle reclamation comes with the P3 lifecycle), noded restart reconcile
- The complete network isolation version (the nftables rule set, DNS injection, egress-only/none policies)
- Python SDK (sync + async + run_batch), the core CLI commands (run/ls/exec/cp/logs/kill)
- batchCreate, batch destroy by label
- Basic observability: a creation-phase latency histogram, platform metrics
- isolation behaviour: fc only through P0–P4 (the tier rule is fixed to fc; the container tier arrives in P5 with GPU demand)

**Acceptance**: a 3-node cluster running a small-scale eval at 100 concurrency (fc tier,
pre-converted images pulled in full), with the LOST retry path verified (kill one node).

## P2 — Productionisation (performance + security targets met)

**Scope**

- overlaybd + S3 lazy-pull (on-demand range-reads against S3 backing), image-service offline
  conversion, block cache, record-trace prefetching
- fc host-side hardening (tightened jailer parameters, cgroup wrapping, tc/conntrack limits)
- prewarm API + orchestration, image affinity v2 (block-level bloom + byte fraction)
- The guest kernel + agent disk build and release pipeline (noded-design §3.4)
- Artifacts pushed straight to S3 (the presigned path), sandbox log archiving
- Events (emitted from the state machine → Postgres + WS subscriptions); unified OTel export + per-sandbox resource metrics
- The credential system: node token (managed ingress TLS + application-layer identity), full STS/presigned coverage
- Quota / rate limiting

**Acceptance**: a 2000-image batch evaluation exercise (fc tier); cold start P50 < 10s, cache hit
P50 < 2s; the escape regression suite passes.

## P3 — Interactive and extended scenarios

**Scope**

- WS streaming exec + PTY (session reconnect), CLI interactive mode (run -it / attach)
- bean-proxy (regional): wildcard-domain TLS, port exposure (reverse proxy connecting straight to
  the sandbox IP), sandbox token authentication, transparent wake from PAUSED
- pause/resume (fc PauseVM) + transparent wake from PAUSED
- Lifecycle automation: idle detection (local to noded), onIdle pause/kill, transparent wake on a request to a PAUSED sandbox
- The fc tier's same-node snapshot path (memory+disk → S3)
- **shared-fs volumes** (the host mounts JuiceFS and exports it through the kernel nfsd, the agent mounts NFS, backend quota)
- The TS SDK, an e2b migration mapping document

**Acceptance**: the agent rollout scenario integrated (interactive terminal + port preview); the
resource billing basis after a pause is correct.

## P4 — Snapshot in its complete form

**Scope**

- fc-tier cross-node restore, diff snapshot increments, fork as a separate API (CoW one parent many children, same node)
- PAUSED archiving: past a threshold, snapshot to S3 automatically to free RAM, and restore transparently on the next access
- Snapshot lifecycle: quota, reference counting, TTL / S3 lifecycle
- **Image as snapshot**: `POST /images:register {snapshot}` registers any sandbox snapshot as a
  named image (the same as Tensorlake) — the shortest path for "set up the environment once, reuse
  in bulk"

**Acceptance**: a "set up the environment → snapshot → fan out 50 instances" demo (50 restores of one snapshot, 50 independent sandboxes); fc-tier **restore** P50 < 500ms — restore, not resume: the number being promised is the cost of building a new sandbox.

## P5+ — Reserve items

- Producing metering data (cpu·s / mem·s / storage / traffic, for internal reconciliation; not a tenant billing system)
- Image signing (cosign)
- allow-list network policies, TCP port exposure (SNI)
- dataset volumes (overlaybd read-only blocks, dataset/weight distribution — reserved, enabled once the demand is clear)
- Webhook event delivery (signing + retries; WS/polling first)
- OTLP pass-through for applications inside the sandbox
- Introducing the container tier (GPU: containerd+runc+nvidia; the no-KVM degraded tier: runsc) +
  the container hardening baseline + container-tier checkpoint (gVisor save / CRIU) + full GPU
  sandbox support
- Multi-region in its complete form: the region field enters the model in P1 (starting from a
  single region), and P5 extends to multi-region blob replication orchestration, the BYOC region
  onboarding flow (a customer-side token service, outbound registration) and an active-active
  control plane

## Risk register

| Risk | Impact | Mitigation |
|---|---|---|
| Complexity of building fcRuntime ourselves (VM lifecycle / guest kernel / vsock) | P0 slips | The AgentENV source is available locally for module-by-module reference (uvm-ublk/envd/warm-pool); P0's scope is narrowed (pre-converted images + a hand-built kernel) |
| ublk's kernel requirement (6.0+) | Older nodes have no lazy-pull | A uniform node OS baseline; the tcmu backend or a full overlayfs pull as the floor |
| overlaybd conversion coverage | Unconverted images are unusable on the fc tier | The conversion pipeline triggers automatically as images enter the catalogue; the container tier's standard pull as the backstop; tensorlake/oci2rootfs (Apache-2.0, OCI→ext4) can be reused as a fully pre-materialising fallback |
| FC snapshot host CPU generation compatibility | Cross-node restore is constrained | Scheduling groups by CPU feature set; the manifest records the generation |
| GPU on runc is weakly isolated | A security shortfall for GPU eval | A separate GPU node pool + an image allowlist; nvproxy (gVisor GPU) assessed in P5 |
| Nodes must have KVM (fc tier only through P0–P4) | Nodes without KVM are unusable | Procure/enable nested virtualisation; the container tier as the P5 floor |
| S3 latency jitter | A cold-start long tail | record-trace prefetching + pooling the node cache |
| The shared-fs volume path (JuiceFS operations + host nfsd stability) | Volumes unavailable or slow | The backend is replaceable (CephFS / local disk); in the extreme it can switch to the go-nfs userspace implementation |
| The guest kernel / agent disk version matrix | Cross-version snapshot restore fails | The manifest records versions; nodes keep multiple versions of the artifacts |
| Maturity of an in-house scheduler | Resource fragmentation / starvation | The eval workload is highly homogeneous, so start with a simple policy and iterate driven by metrics |

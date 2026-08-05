# bean

> 中文版:[README.zh.md](README.zh.md)

Firecracker microVM sandboxes for AI evaluation workloads — any OCI image, no
template build step.

`bean` runs untrusted code in hardware-isolated microVMs. It exists for one
shape of problem: evaluation and agent-rollout batches over **thousands of
heterogeneous Docker images** (SWE-bench and similar), where each task has its
own multi-GB image, sandboxes live for minutes, and a run creates hundreds at
once.

The whole stack is self-contained — control plane, node daemon, in-sandbox
agent, CLI, SDK — with no Kubernetes and no containerd on the hot path.

> **Status: working system, incomplete platform.** The microVM tier boots real
> Firecracker VMs on real hardware, and every number below is measured rather
> than projected. The container tiers (runc/gVisor) are unimplemented and the VMM
> is not yet confined by a jailer. Read [What works](#what-works) before planning
> around it.

---

## Why not e2b / Modal / plain containers

| | approach | cost for this workload |
|---|---|---|
| e2b | Firecracker + per-image template build | one template build per image, minutes each — unusable at 2000 images |
| Modal | own container runtime + lazy-loading FS | not self-hostable |
| K8s + Pod | container per task | no VM boundary for untrusted code; scheduling and network stack are heavy |
| **bean** | Firecracker + shared base image with per-sandbox CoW | **44 KiB of disk per sandbox**, 952 ms to a reachable agent |

The pivot is that a sandbox does not get its own copy of the image. One
read-only base is loop-mounted per node and shared; each sandbox gets a sparse
copy-on-write layer over it through device-mapper. Fanning out a hundred clones
of one image costs a hundred sparse files.

---

## What works

Measured on an AMD EPYC 7542 (Zen 2) host, guest kernel 6.1.102, Alpine 3.20.

### Lifecycle

```
create → exec → cp → pause → resume → snapshot → restore → destroy
```

| operation | measured | notes |
|---|---|---|
| create (image cached) | **952 ms** | 234 ms runtime + 770 ms to a reachable agent |
| create (cold image) | 5–10 s busybox … 2 m 45 s alpine on poor network | why prewarm is required, not an optimisation |
| destroy | **214 ms** | was 5.25 s — [decisions §1](docs/decisions.md) |
| snapshot (full) | 1.5 s, 15.5 MB | |
| restore | **392 ms** on a node-local cache hit | `/snapshot/load` is 7 ms of it; a first restore pays ~950 ms to unpack, and every later one on that node does not |

### Snapshots — three kinds, different semantics

Not three sizes of one thing:

| kind | flag | measured | restore | portability |
|---|---|---|---|---|
| full | *(default)* | 15.5 MB | resumes; process tree survives | pinned to CPU vendor + family |
| filesystem-only | `--no-memory` | **6109 B** | boots fresh, files intact | **any CPU** |
| incremental | `--base SNAP` | **298 KB** | resumes | pinned to CPU vendor + family |

Guest memory records what the CPU it booted on offered, and vendor/family cannot
be masked away — so a memory snapshot only restores on a compatible CPU, and the
scheduler enforces that as a hard filter (`409 INCOMPATIBLE_CPU`) rather than
placing it and letting the guest misbehave afterwards. `--no-memory` trades
resume for portability; `--base` stores only the pages written since its parent.

```bash
bean snapshot create $SBX --name base
bean snapshot create $SBX --name step1 --base snap_...   # 298 KB, not 15.5 MB
bean run --snapshot snap_...
```

### Networking

Each sandbox gets its own network namespace, a tap device and a `/30`, with two
layers of NAT to the uplink. Egress works; the ranges a sandbox has no business
reaching are denied by default.

| from inside a booted guest | result |
|---|---|
| public address | reachable, 7.9 ms |
| DNS | resolves |
| cloud metadata `169.254.169.254` | denied |
| the node's own address | denied |
| the node's gateway | denied |

The denials are only meaningful because the reachability checks pass in the same
guest at the same instant — an unconfigured interface would make every denial
"pass" while nothing worked at all, which is exactly the bug the end-to-end probe
was written to catch. `hack/guest-egress-probe.sh` asserts all seven from inside a
real microVM over `exec`; [docs/network.md](docs/network.md) §5a explains which
of the two rule scopes actually denies what.

Guest addresses are identical in every sandbox on purpose: a restored snapshot
comes back with the address it was captured with, so a constant is what lets one
checkpoint fan out to many sandboxes without collisions.

### Also working

- **Images** — OCI pull and conversion, private registries (credentials
  AES-256-GCM at rest), prewarm with image-affinity scheduling
- **Builds** — Dockerfile through BuildKit with streaming logs and cancellation,
  and `commit` to freeze a running sandbox's filesystem into a reusable base image
- **`fork`** — N independent sandboxes from one source, one checkpoint per batch,
  the source left running
- **Scheduling** — two-level placement; commitments persisted so replicas cannot
  double-place and a restart does not lose the ledger; configurable overcommit
- **Snapshot blobs on S3** — SigV4 implemented against the standard library, no
  AWS SDK; multipart upload and range reads
- **Tracing** — OpenTelemetry with W3C `traceparent` across gateway → noded →
  in-sandbox agent, arriving as one span tree per request

### Not built yet

| | |
|---|---|
| jailer / mount namespace | 📐 The VMM drops to an unprivileged uid and runs in a per-sandbox cgroup, but still sees the host filesystem. Hardware virtualisation is the boundary; `chroot` confinement is [#20](https://github.com/garysng/bean/issues/20) phase 2 |
| Container tiers (runc/gVisor) | 📐 microVM, plus a no-isolation `local` tier for development, are the only options |
| Volumes, port exposure | 📐 |
| Warm snapshots | 📐 The largest remaining throughput lever. A create still boots, costing ~5 CPU-seconds against a restore's near-zero — so the ceiling is `cores / 5` regardless of how fast one create is. [docs/warm-snapshots.md](docs/warm-snapshots.md) |
| Data plane | 📐 `exec` and file transfer are relayed through the control plane rather than going node-direct |
| Postgres | ⚠️ SQLite in use. No `Store` interface — all SQL is contained in one package, but callers hold the concrete type |
| overlaybd | ⚠️ `OverlaybdProvider` is implemented and **verified end to end on hardware**: a sandbox boots from an overlaybd device and the guest reads its own rootfs (`hack/overlaybd-e2e.sh`). Opt-in via `--fc-overlaybd`; dm-snapshot remains the default. Lazy pull against a registry is implemented but **untested**, and `commit` on this backend is unexercised. [docs/image-pipeline.md](docs/image-pipeline.md) §7 |

---

## Quick start

Needs a Linux host with `/dev/kvm`, root, and `dmsetup` / `losetup`.

```bash
make build
sudo hack/build-assets.sh          # guest kernel + agent disk
sudo hack/build-assets.sh kernel   # Firecracker CI vmlinux-6.1.102

sudo hack/dev-fc-stack.sh start    # gateway on :18080, one node

export BEAN_BASE_URL=http://127.0.0.1:18080 BEAN_API_KEY=devkey
SBX=$(bean run --image alpine:3.20 --quiet)
bean exec $SBX -- sh -c 'echo hello'
bean kill $SBX
```

For incremental snapshots, dirty-page tracking has to be on before a guest boots
— it cannot be enabled for a sandbox that is already running:

```bash
NODED_FLAGS="--track-dirty-pages" sudo hack/dev-fc-stack.sh start
```

---

## Architecture

```
  SDK / CLI ──REST──▶ bean-api ──gRPC──▶ noded ──vsock──▶ beand
                      │  scheduler        │  runtime         (PID 1 in guest)
                      │  image service    │  image provider
                      └─ SQLite           └─ Firecracker
                           │
                         S3 (snapshot blobs)
```

Four binaries: `bean` (CLI), `bean-api` (gateway, with the scheduler in-process
so placement and commitment happen in one transaction), `noded` (one per host),
`beand` (PID 1 inside each sandbox, shipped on its own read-only disk so user
images need no modification).

### How a sandbox boots

```
1. image provider assembles a rootfs block device
     shared read-only base (loop) + per-sandbox sparse CoW
     → dm-snapshot → /dev/mapper/bean-<id>
2. network: a netns, a tap, a veth pair to the host, NAT and filter rules
3. noded execs firecracker *inside that netns*
     virtio-blk: agent disk as root device, user image as /dev/vdb
     vsock for the agent, tap registered before InstanceStart
     init=/bean/beand, with ip= so the kernel configures eth0
4. beand as PID 1: mount matrix, then pivot into the user image
```

Four ordering constraints in there are load-bearing, and every one was found the
hard way:

- A CPU template must be applied **before** `InstanceStart`. A guest reads CPUID
  once during early boot and caches it — glibc picks its string routines from it
  — so masking later masks features the guest already committed to using.
- A NIC must be registered **before** `InstanceStart` too; the endpoint is
  pre-boot only, and a guest that misses it runs its whole life without an
  interface.
- The VMM must be `exec`'d **inside** the sandbox's netns. `setns` is per-thread
  and the Go runtime migrates goroutines at every blocking point, so this needs
  `LockOSThread` around setns/Start/setns-back in one goroutine.
- On restore, the CoW layer must be seeded **before** the dm-snapshot device is
  assembled. A dm-snapshot reads its exception table into kernel memory at
  activation and never re-reads it, so bytes written afterwards are invisible.
  The failure is *silent*: `ls` reports the right size, `cat` returns zeroes,
  `dmesg` says nothing. [decisions §3.0](docs/decisions.md).

The last one is the shape to internalise: each of these is a step that *looks*
done from every vantage point except the one that matters. The network stack had
five correct layers and no address in the guest, and every assertion passed.

---

## Documentation

Design docs carry per-section delivery status (✅ implemented / ⚠️ partial /
📐 design only), because writing intent and reality the same way is exactly what
made networking and jailer look shipped. Convention in
[architecture.md §0](docs/architecture.md).

**Authority order: code > `status.md` > `decisions.md` > design docs.**

| | |
|---|---|
| [status.md](docs/status.md) | **what is actually built**, with measurements |
| [decisions.md](docs/decisions.md) | **why** each choice was made — measured data, competitor comparisons, and the traps that only appeared on hardware |
| [architecture.md](docs/architecture.md) | components, design decisions, state machine |
| [vm-assembly.md](docs/vm-assembly.md) | how a microVM is assembled, and the two orderings that must not change |
| [image-pipeline.md](docs/image-pipeline.md) | OCI ref → mountable block device |
| [s3-storage.md](docs/s3-storage.md) | hand-rolled SigV4, multipart, the `Blobs` contract |
| [noded-design.md](docs/noded-design.md) | node daemon and in-sandbox agent |
| [api-design.md](docs/api-design.md) | REST and gRPC surface, auth, error codes |
| [snapshot-resume.md](docs/snapshot-resume.md) | pause/resume/snapshot/restore |
| [image-build.md](docs/image-build.md) | build and commit |
| [security-and-startup.md](docs/security-and-startup.md) | threat model, hardening, cold-start budget |
| [sdk-cli-design.md](docs/sdk-cli-design.md) | SDK and CLI |
| [network.md](docs/network.md) | ✅ netns per sandbox, the two filter scopes, and why a restored snapshot keeps its address |
| [warm-snapshots.md](docs/warm-snapshots.md) | 📐 booting once per image instead of once per sandbox |
| [competitive-analysis.md](docs/competitive-analysis.md) | e2b / Modal / Daytona / Morph / AgentENV, including how each one does networking |
| [roadmap.md](docs/roadmap.md) | phases, with actual progress noted |

`decisions.md` is the one to read if you are evaluating the approach: it records
what was measured, where competitors chose differently, and which conclusions
remain unverified.

---

## Development

```bash
make build          # all binaries
make test           # unit tests, race detector
make test-e2e       # end-to-end, local tier
make lint vet       # gofmt, go vet, and the ASCII check below
make proto          # regenerate from proto/
```

### Language

Prose documentation may be written in any language: `docs/` holds the English
versions and `docs/zh/` the Chinese ones. **Everything else is ASCII** — code,
comments, scripts, configuration, commit messages and branch names.

The reason is not preference. Someone who cannot read Chinese should be able to
work on every file that is not documentation, and `git log` should stay readable
to everyone, which it stops being as soon as half the history needs translating.

`hack/check-ascii.sh` enforces it and runs as part of `make lint`. It rejects only
CJK, not everything non-ASCII: em-dashes, arrows and box-drawing characters are
used deliberately in comments and diagrams. Adding `--commits` also checks the
messages of commits not yet pushed — the line is drawn there because rewriting
published history to fix a message costs more than the message is worth.

Most of the interesting behaviour needs a KVM host, root, and device-mapper, so
those tests **skip** rather than fail on a developer machine — `go test ./...`
stays green without proving much. Cross-compile and run on a real host for
anything touching the microVM tier:

```bash
GOOS=linux GOARCH=amd64 go test -c -o /tmp/img.test ./internal/node/image/
scp /tmp/img.test root@host:/tmp/ && ssh root@host /tmp/img.test
```

### Two testing rules worth stating

**Verify through the real persistence layer.** When state exists in both memory
and on disk, a test that reads memory proves nothing. The silent
filesystem-corruption bug above passed three layers of tests: unit tests checked
the tar round-trip (correct — the data *was* written), end-to-end tests read the
file from inside the guest (page-cache hit), and `dmsetup status` inspected the
wrong device. None read the restored block device. Snapshot assertions must
`drop_caches` first.

**Then break the fix and confirm the test fails.** For that bug, every
file-level assertion was green against the broken implementation, so this was the
only way to know the new test was worth anything. Same for the loop-device leak
and the merge-ordering test.

---

## License

Not yet chosen.

# Glossary

The terms bean uses, defined once. Where a term has a subtlety that has bitten
us, the definition says so rather than leaving it to be rediscovered.

**Authority order still holds: code > `status.md` > `decisions.md` > design docs > this page.**
If a definition here disagrees with those, they win — tell us and we'll fix it.

---

## Core objects

**sandbox** — one isolated execution environment: a rootfs, a network namespace,
and a process tree, running under one of the runtime tiers. It is the unit
everything else is about. A sandbox has one lifecycle (see the verbs below) and
one id (`sbx_...`).

**image** — an OCI image: *layers* (the filesystem, as a stack of tarballs) plus
a *configuration blob* describing how to start them. bean pulls and converts
images; it never requires a per-image template build step. An image is
read-only input to a sandbox, not a running thing.

**base image** — the shared, read-only image a node loop-mounts once and reuses
across every sandbox on it. A sandbox does **not** get its own copy; it gets a
copy-on-write layer over the shared base (see *CoW layer*). Promoting a filesystem
snapshot can freeze a running sandbox's filesystem into a new reusable base image.

**rootfs** — the root filesystem a sandbox boots with, assembled from the shared
base image plus the sandbox's own writable CoW layer.

**CoW layer** — the sparse copy-on-write layer each sandbox gets over the shared
base, through device-mapper. This is why fanning out a hundred sandboxes costs a
hundred sparse files rather than a hundred image copies — 44 KiB of disk per
sandbox at create.

**snapshot** — a durable, persisted capture of a sandbox that outlives it and can
be created from repeatedly. A `snap_...` object, stored as a blob (node-local
and/or S3). Three kinds, with **different semantics, not just different sizes**:

| kind | flag | what it captures | on create-from-snapshot | portability |
|---|---|---|---|---|
| full | *(default)* | memory + filesystem | resumes running, process tree intact | pinned to CPU vendor + family |
| filesystem-only | `--no-memory` | filesystem only | boots fresh, files intact | any CPU |
| incremental | `--base SNAP` | pages written since the parent snapshot | resumes running | pinned to CPU vendor + family |

A snapshot that captured memory only restores on a CPU of the same vendor and
family — guest memory records what that CPU offered and it cannot be masked
afterward, so the scheduler enforces it as a hard filter (`409 INCOMPATIBLE_CPU`).

**bundle** — the on-disk/on-wire packaging of a snapshot (vmstate + memory +
rootfs member). Unpacked once per node and cached by snapshot id; the first
create-from-snapshot on a node pays the unpack (~950 ms), later ones hit the
cache (392 ms).

---

## Lifecycle verbs

These are the **external operations** on a sandbox. Each maps to a per-runtime
implementation (see *runtime tier*) — the verb is the same, what happens under it
differs by tier, and some tiers do not implement all of them.

**create** — make a new sandbox. One endpoint (`POST /v1/sandboxes`) that branches
on its input: from an **image** it cold-boots; from a **snapshot** it does
create-from-snapshot. There is no separate `/restore` endpoint.

**create-from-snapshot** — create a **new** sandbox (new id) from a snapshot blob.
The snapshot is durable and can be used this way any number of times, each call
producing one independent sandbox. This is the operation earlier drafts called
"restore" as a user-facing verb; that word is now reserved for the runtime
mechanism, not the API.

**resume** — wake a **PAUSED** sandbox. Same sandbox, same id, same process tree —
its memory never left host RAM. Milliseconds. It is *not* create-from-snapshot:
nothing is unpacked and no new sandbox is made. A request against a PAUSED
sandbox triggers a resume transparently.

**pause** — freeze a RUNNING sandbox's vCPUs without destroying it, so a later
resume can wake it. The `on_idle=pause` policy does this automatically after an
idle timeout.

**fork** — produce N **new**, independent sandboxes from **one running source**,
with one checkpoint per batch, leaving the source running. The mechanism ships;
there is no dedicated API verb yet — the surface is one snapshot plus N
create-from-snapshot calls.

**snapshot** (verb) — capture a running or paused sandbox into a durable
`snap_...` object. A heavy operation for large-memory guests (all memory pages
written out).

**destroy** — tear the sandbox down and release its resources. Either explicit
(`DELETE`) or via the `on_idle=delete` policy after an idle timeout.

The states a sandbox moves through are fewer than the verbs: `create` →
**RUNNING** ↔ **PAUSED**, and the only exits are an idle sweep or an explicit
`DELETE`. See the state diagram in the README.

---

## Runtimes and components

**runtime tier** — the isolation backend a sandbox runs under, chosen with
`--runtime`. Three exist, and they implement the same interface with different
mechanisms and different levels of support:

| tier | flag | isolation | snapshot / fork |
|---|---|---|---|
| Firecracker microVM | `fc` | hardware (KVM microVM) | full support |
| OCI + gVisor / runc | `runsc` / `runc` | gVisor sentry, or runc namespaces | **not supported** (returns unsupported) |
| local | `local` | none — dev only | limited |

`fc` is the more heavily tested path and the one all the measured numbers come
from. The OCI tier serves the benchmark workload (any image, no per-image
template build) but has no checkpoint to fork from.

**bean-api** — the control plane (one process): API gateway, scheduler
(placement in-process so placement and commitment are one transaction), and
image service. Backed by SQLite or Postgres.

**noded** — the node daemon, one per host. Runs the runtime tiers and the image
subsystem (base image, CoW, overlaybd/TCMU).

**beand** — PID 1 inside each sandbox, shipped on its own read-only disk so user
images need no modification. Builds the mount matrix, then pivots into the user
image.

**bean-proxy** — the data-plane path for port traffic into a sandbox:
`{port}-{sandbox}` reaches that port in that guest, user server or agent alike.
No registration call, no host-port pool.

**bean** — the CLI.

---

## A layering reminder

The README and the lifecycle table stay at the **external interface** level:
create / resume / pause / snapshot / fork / destroy. Names like Firecracker's
`/snapshot/load` are **implementation detail of the `fc` tier**, not external
verbs — they live in the design docs, not the surface. When a term reads like an
operation but names a mechanism (restore, `/snapshot/load`), it belongs to a
runtime, not to the API.


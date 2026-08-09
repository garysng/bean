# Image Build Design

> 中文版:[zh/image-build.md](zh/image-build.md)

> Users can build images on the platform side, not only reference existing OCI images.
> The status-marker convention is defined in [architecture.md](architecture.md) §0.
> Terminology follows [api-design.md](api-design.md) §3.5: `ref` is the only identifier the user touches.

## 1. Why it is needed

bean's footing is "any OCI image boots directly with zero conversion", so it needs no
e2b-style per-image template build. But having no build capability at all leaves two real gaps:

- **Nowhere to add dependencies**: a user who wants to install `requirements.txt` on top of
  `python:3.12` has to either maintain an external registry themselves or repeat the install
  every time a sandbox comes up
- **"Set up the environment then reuse it" can only lean on snapshot**: a snapshot is bound to
  a runtime tier and cannot cross tiers, whereas an image is universal (see the distinction in §4)

## 2. The two origins of an image ⚠️

`store.Image.Source` distinguishes them, because their conversion costs are entirely different:

| Source | Origin | Conversion | State progression |
|---|---|---|---|
| `imported` | an OCI ref given by the user | tar.gz layer → ext4 image/block device, needs the convertor | `PENDING → CONVERTING → READY` |
| `built` | built by the platform | see below | `BUILDING → CONVERTING → READY` |

**On whether "built" means zero conversion — it does not; the BuildKit path always needs one
conversion**, and this has to be stated clearly or the cost gets misjudged:

| Build path | Output format | Conversion |
|---|---|---|
| **BuildKit** (Dockerfile / steps) | standard OCI layer | **still needs one conversion** |

Even though the BuildKit path needs a conversion, it is still an improvement over e2b: the
conversion happens at **build time** (once, cacheable, off the user's waiting path), whereas
e2b spends another 5–15 minutes converting to a VM rootfs after the build.

## 3. Three build forms

Common underneath: **execute steps in order inside a container, then commit the result as
layers**. The only difference is how the steps are described.

### 3.1 Dockerfile (full semantics) ✅

Uses **BuildKit** rather than an in-house parser. COPY/ADD semantics, multi-stage, ARG
interpolation, build cache, `.dockerignore`, heredocs — together those are months of work and
guaranteed to be incomplete; e2b and Daytona use BuildKit as well.

```
bean build -f Dockerfile -t myteam/eval-base:v1 .
```

The CLI packs the build context (subject to `.dockerignore`) and uploads it, and **the build
executes on the platform side** — the user needs no local Docker install, and the build cache
is shared on the platform side.

### 3.2 Declarative steps (Modal style) 📐

The eval orchestration side is writing Python anyway, so a chained declaration is easier to
work with than maintaining a Dockerfile, and every step is naturally a cache key:

```python
img = (client.images.build("myteam/eval-base:v1")
       .from_("python:3.12")
       .pip_install("-r", "requirements.txt")
       .run("apt-get update && apt-get install -y git")
       .env(PYTHONUNBUFFERED="1")
       .workdir("/app")
       .submit())
```

The SDK compiles the chained calls into the build plan of §5; the server does not distinguish
whether it came from a Dockerfile or from steps.

> "Install the environment interactively first, then freeze it" — that exploratory workflow is
> served by a **filesystem snapshot** (§4), which can be promoted into the image namespace, not
> by a build.

## 4. The difference between a built image and a snapshot ✅

A snapshot also captures a sandbox's filesystem, but it is **not the same thing** as a built
image, and conflating them makes a mess of both the data model and the user's mental model:

| | snapshot | built image |
|---|---|---|
| Contents | filesystem **+ memory/device state** (fc tier) | filesystem only |
| Across runtime tiers | ❌ the format is bound to the tier that produced it | ✅ it is just an image layer |
| Purpose | clone **one** sandbox's exact state, process tree included, into any number of new sandboxes | serve as **someone else's** base image |
| Identifier | `snap_...`, reclaimed by reference count + TTL (a count, since one snapshot can be restored many times at once) | `ref` + digest, referenced like any image |
| Typical scenario | "set up the environment → fan out N experiments" | "the eval base image a team shares" |

## 5. Build Plan: the unified intermediate representation ⚠️

> The `store.BuildPlan` / `BuildStep` types are defined ✅, but only the `dockerfile`
> kind works end to end; the compiler for the `steps` kind is unimplemented. The
> per-step cacheKey field exists but is unused.


All three forms compile into the same plan, and the server only knows about the plan. That way
adding a new front end (Bazel, Nix) does not touch the executor, and replacing the executor
(BuildKit → in-house) does not touch the API.

```go
type BuildPlan struct {
    From      string       // base image ref (imported or built, either is fine)
    Steps     []BuildStep  // ordered
    Tag       string       // output ref
    Env       map[string]string
    Workdir   string
    // Dockerfile path + context digest; empty for the steps form
    Dockerfile      string
    ContextDigest   string
}

type BuildStep struct {
    Kind string  // run | copy | env | workdir | user
    // CacheKey is a hash of (the chain of preceding steps + this step's content): the basis
    // for content-addressed caching, and how Modal-style "every step cached automatically"
    // is implemented
    CacheKey string
    Run  string
    Copy *CopyStep
    ...
}
```

## 6. API

> **Log streaming and cancellation** for builds are implemented: the log endpoint
> (`build.go:289`) and cancel (`build.go:358`) are served over noded's long-lived
> `BuildImage` stream (`grpc.go:143`). One caveat remains — the log buffer is
> per-replica in-memory (`buildlog.go`), so under multiple bean-api replicas a
> logs/cancel request must reach the replica that started the build; see
> [build-service.md §3.5](build-service.md).


```
POST /v1/images/build      Dockerfile or steps → 202 { buildId }
     { "tag": "...", "from": "...", "steps": [...],
       "dockerfile": "...", "contextRef": "..." }
POST /v1/images/build/{id}/context   upload the build context (tar)
GET  /v1/images/build/{id}           status, log location, output digest
GET  /v1/images/build?label=          list
POST /v1/images/build/{id}/cancel
```

Build state machine: `PENDING → RUNNING → CONVERTING → READY | FAILED | CANCELLED`.
Logs land in storage per build and can be viewed as a stream.

## 7. Where it executes ⚠️

Builds run **on nodes** (in the same pool as sandboxes, or on dedicated nodes labelled
`pool=builder`), because:

- BuildKit needs a containerd/OCI environment, which the node has anyway
- The build cache shares the local disk with the image block cache, so repeated builds within
  one eval batch have a high hit rate
- The scheduler's existing labels/nodeSelector mechanism is reused directly, with no new
  orchestration layer needed

noded's `BuildImage` RPC ✅ is shipped (BuildKit; a node with an empty `--buildkit-addr` accepts
no builds). ⚠️ Build outputs currently land in the **node-local ImageDir** as a base ext4 and
are not pushed to S3 blobs — so a built image is at present only usable on the node that built it.
The original plan: push to the platform's S3 (overlaybd blobs), with metadata written back to
the control plane.

## 7.5 Implementation details of Dockerfile builds ✅

`internal/node/image/build_linux.go`. It invokes buildctl rather than linking BuildKit's Go client:

```
buildctl --addr <buildkitd> build
  --frontend dockerfile.v0
  --local context=<dir> --local dockerfile=<dir>
  --output type=tar,dest=<out.tar>
  [--opt build-arg:K=V ...]
```

**Why `type=tar` and not `type=image`**: here a base image is just a flat filesystem (it has to
be mkfs'd into ext4). Exporting an image would mean BuildKit assembling a layered structure
that we then flatten — one extra step with no benefit. `type=tar` gives the flattened content
directly.

**stdout and stderr are collected together**: BuildKit writes progress to stderr, and on
failure that is where "which step failed" lives. So both are collected into the same buffer,
and on failure the last 40 lines are taken — the full output of a build can be thousands of
lines, and the useful information is at the end.

**The size estimate is more accurate than the one on the conversion side**: the tar a build
produces is uncompressed, so its size *is* the content size (compare image conversion, which
only has compressed layer sizes available and has to estimate with × 3, see image-pipeline §2).
Headroom is still left for filesystem overhead and for the sandbox's later writes.

**`--frontend dockerfile.v0` means full Dockerfile semantics** — multi-stage builds,
`COPY --from` and cache mounts are all BuildKit's own capabilities, and we do not parse the
Dockerfile. That is the entire reason for choosing BuildKit over implementing builds ourselves.

### The cacheKey field exists but is unused ⚠️

`store.BuildStep.CacheKey` is defined, and the design intent is "hash the chain of preceding
steps + this step's content" so that an unchanged prefix reuses the cache. **No code computes
or uses it today** — Dockerfile build caching is entirely BuildKit's own business (it has its
own content-addressed cache), and the `steps` form is unimplemented, so there is not yet a
scenario that requires us to compute a cacheKey ourselves.

## 8. Not doing (explicit boundaries)

- **Pushing back to an external OCI registry**: a built image is only usable inside bean.
  Reverse conversion or dual-format storage is markedly more expensive, and the current
  scenario (internal eval) does not need it. If it is ever wanted, what it affects is the blob
  layout, which would need redesigning
- **Any separate network access policy during a build**: it follows the sandbox's
  `egress-only`, with no special opening
- **Cross-region build orchestration**: blob replication for built images takes the same path
  as imported images (D11)

## 9. Comparison with the competition

| Platform | Build definition | Execution | Output |
|---|---|---|---|
| e2b | Dockerfile | BuildKit → convert to a VM rootfs | template (5–15 minutes each) |
| Daytona | Dockerfile / Declarative Builder | BuildKit | snapshot |
| Modal | chained Python calls | in-house builder (requires Python inside the image) | content-addressed layers |
| **bean** | Dockerfile ✅ / declarative steps 📐 | BuildKit (platform side) ✅ | ⚠️ currently a node-local ext4; an overlaybd layer on S3 is the target |

bean's difference: the build forms unify into one plan, and the output is already in the
block-device format the fc tier can use, with no further conversion needed.

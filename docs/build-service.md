# Should image build be a separate service?

> Status: 📐 **design / discussion.** No code changes proposed here yet. This
> assesses whether the image-build path should be split out of noded into its own
> service, what that buys, and what actually blocks it today. Authority order
> holds: code > `status.md` > `decisions.md` > design docs > this page. See
> [image-build.md](image-build.md) for what build *does*; this is about *where it
> should run*.

> 中文版:[zh/build-service.md](zh/build-service.md)

---

## 0. The question

The four binaries are `bean` (CLI), `bean-api`, `noded`, `beand`. Build has no
node of its own — so where does it run, and should it? Today build executes
**inside noded**, in the same process that serves `create`/`exec`, driving a
`buildkitd` alongside the sandboxes. The question is whether that coupling should
be broken into a dedicated build service.

The honest answer up front: **build is already decoupled in the ways that are
cheap, and still coupled in the one way that matters.** What blocks a clean split
is not the call graph — it is that a built image never leaves the node that built
it.

## 1. How build runs today

```
CLI/SDK ──► bean-api (build.go) ──gRPC stream──► noded ──► buildctl ──► buildkitd
                │ pickBuilder                      │ ImageBuilder            │
                │ (label-first, else any)          │ (optional per node)     │
                └─ image marked BUILDING           └─ output: flat ext4 in node-local ImageDir
```

- **Entry**: `handleBuild` (`internal/control/api/build.go:57`) decodes the
  context tar (64 MiB cap), records the image as `ImageBuilding`, and picks a
  node. Build then runs in the background under a `context.Background()` with a
  60-minute cap and returns `202` immediately; logs and cancel are two endpoints
  keyed on the image ref (`build.go:289`, `:358`).
- **Node selection is already independent of the scheduler.** `pickBuilder`
  (`build.go:148`) prefers a Ready node labelled `pool=builder`, else takes any
  Ready node — it does not consult the create-path affinity scoring in
  `scheduler.go` at all.
- **Execution**: noded's `BuildImage` RPC (`grpc.go:143`) is a long-lived
  server-stream; `Manager.BuildImage` (`manager.go:833`) type-asserts
  `runtime.ImageBuilder` and delegates. The `image.Builder` (`build_linux.go:29`)
  shells out to `buildctl` against a `buildkitd` socket.
- **buildkitd is an optional per-node dependency.** `--buildkit-addr` defaults to
  empty (`cmd/noded/main.go:81`); only the fc tier wires up a `Builder`, and only
  when the address is set (`fc_tier_linux.go:215`). A node with no address accepts
  no builds. The `pool=builder` label already lets a cluster run dedicated build
  nodes.

So the *execution* seam is clean: an optional interface, node selection already
split from scheduling, and a label mechanism for dedicated builders.

## 2. What is actually coupled

Three real couplings, in descending order of how much they block a split:

1. **The output is node-local and never uploaded.** A build lands a flat
   `.ext4` in that node's `ImageDir` (`build_linux.go:89`), read directly by the
   same node's rootfs provider (`rootfs.go:194`). `MarkReady` is called with an
   empty overlaybd ref and size 0 (`build.go:241`) — the code comment is blunt:
   READY *overstates the reach*, the image "exists only in the building node's
   ImageDir and is never uploaded, so no other node can start from it"
   (`build.go:236-240`). **This is the hard blocker.** A centralized build
   service that produced images no sandbox node could consume would be useless.
   `commit` has the identical property (`commit_linux.go:68`).
2. **Build shares the node with sandboxes and has zero resource isolation.**
   Build and `create`/`exec` run in the same noded process and `Manager`
   (`manager.go`), and `buildkitd` competes for the same host CPU, disk and IO.
   There is no concurrency limit, no rate limiter, no cgroup — the only guard is
   the 60-minute timeout (`build.go:143`). A heavy build can starve co-located
   sandbox starts.
3. **Only the fc tier can build.** `OCIRuntime` wires up no `Builder`
   (`oci_tier_linux.go` passes `BuildkitAddr` through but never assembles one), so
   the assertion `m.rt.(runtime.ImageBuilder)` fails on an OCI-tier node.

## 3. The distribution machinery already exists — for other images

bean already distributes *imported* images across nodes, and build simply does
not use that path yet:

- **S3 blob store**: `obdblobstore.go` puts sealed overlaybd layers keyed by OCI
  digest into an S3-compatible bucket (`:141`), which the overlaybd daemon
  range-reads anonymously (`:166`). The seal capability exists too
  (`obdbuild_linux.go`).
- **Prewarm publishes; a create never does** (`status.md:55`): prewarm converts
  an image and pushes each sealed layer to object storage, and any node reading
  that store resolves the layers remotely instead of converting locally.
- **Image-affinity scheduling**: the scheduler scores nodes that already have an
  image cached (`scheduler.go:324`, `ImageAffinity:10`), fed by `CachedImages` on
  the heartbeat.

The point: the "seal → push to S3 → other nodes range-read" hop is **built and
shipped for imported/overlaybd images**. Build outputs just don't go through it —
they stop at a local ext4. Closing that gap is the same mechanism, and
`build.go:238-240` already reserves for it ("the upload can land later").

## 4. The prerequisite, and why it comes first

**Whether or not build is split, the output must become distributable.** Publish
the build result the way prewarm publishes an imported image: instead of
`writeBaseImage` landing a local ext4, seal the rootfs into an overlaybd layer,
`BlobStore.Put` it to S3, and `MarkReady` with the real overlaybd ref. Then any
node range-reads it, exactly like an imported image.

This single change is worth more than the split itself:

- It makes a built image usable cluster-wide — the thing `build.go:236` says is
  missing today.
- It makes the *location* of the build irrelevant. Once the output is in the blob
  store, whether it was produced inside a noded, on a `pool=builder` node, or in a
  separate service is an operational choice, not an architectural one.

So the ordering is deliberate: **fix distribution first, then decide on the
split** — because after distribution, splitting is a deployment decision rather
than a correctness one.

## 5. Two shapes for a split, once distribution is fixed

**Shape A — dedicated builder nodes (small step).** Keep build inside noded, but
run it only on nodes labelled `pool=builder`, which `pickBuilder` already
supports (`build.go:148`). Output goes to the blob store (§4). This gets build
off the sandbox hot path with essentially no new component — it is a scheduling
and deployment policy, not a new binary.

- **Pros**: reuses everything; no new service to operate; `pickBuilder` and the
  label mechanism already exist.
- **Cons**: still the noded binary and its assumptions; resource isolation on a
  builder node is coarse (whole-node), not per-build.

**Shape B — a standalone build service (larger step).** A `bean-build` service
(or a mode of an existing binary) that owns buildkitd, exposes the build RPC, and
writes only to the blob store — never to a local `ImageDir`. bean-api's
`pickBuilder` becomes "route to the build service".

- **Pros**: build capacity scales independently of sandbox capacity; clean
  resource and failure isolation; buildkitd stops being a per-noded dependency.
- **Cons**: a new binary and deploy surface; the real work is migrating
  `image.Builder`'s local-`ImageDir` assumption (`build_linux.go`) to a
  blob-store writer — which §4 requires anyway.

The interface seam for Shape B is already narrow: `runtime.ImageBuilder` +
`runtime.BuildRequest` (with a `Logs io.Writer` stream, `runtime.go:294-320`),
with the real logic in `image.Builder` depending only on a buildctl address and
two local dirs. What moves is the output side.

## 6. Recommendation

**Do not split yet. Do the distribution fix first (§4), then prefer Shape A.**

Reasoning:

- The **distribution gap is the actual problem**, and it blocks build usefulness
  regardless of topology. It is worth doing on its own merits and unblocks
  everything else. It reuses shipped machinery (seal + blob store + prewarm
  publish), so it is the lowest-risk, highest-value move.
- After that, **Shape A** removes build from the sandbox hot path with no new
  binary, using the `pool=builder` label that already exists. For a project whose
  centre of gravity is the microVM hot path, that captures most of the benefit
  (isolation of heavy build load) at a fraction of the cost.
- **Shape B** (a true separate service) is justified only when build volume grows
  enough that independent scaling and clean failure isolation pay for a new binary
  and deploy surface. Until then it is speculative — and because the interface
  seam is already narrow and §4 does the hard output-side migration, deferring it
  costs little.

A secondary cleanup worth folding in: **the OCI tier cannot build at all**
(`oci_tier_linux.go` never assembles a `Builder`). If builds should be
tier-agnostic, that is a gap to close; if build is deliberately fc-only, the docs
should say so rather than leaving `BuildkitAddr` plumbed but unused.

And a doc-accuracy note surfaced by this audit: build **log streaming and cancel
are implemented** (`build.go:289`, `:358`, `grpc.go:143`) but
[image-build.md](image-build.md) §6 still marks them ⚠️ unimplemented. That should
be corrected independently.

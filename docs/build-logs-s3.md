# Build logs on S3 — a stateless, restart-proof build log and cancel path

> Status: ✅ **Steps A and B implemented.** The node uploads a
> build's log to a dedicated S3 logs bucket (`internal/control/s3/buildlog.go`,
> `BuildLogWriter`), the gateway reads it back statelessly over byte offsets
> (`handleBuildLogs` in `internal/control/api/build.go`, `BuildLogReader`), and
> cancellation resolves the build's node from the record and calls the node's
> `CancelBuild` RPC (`internal/node/grpc.go`, `internal/node/buildreg.go`). The
> in-memory `buildTracker` was removed. **Step B severed the result-carrying
> stream:** the node's `BuildImage` server stream is gone, replaced by a
> fire-and-forget `StartBuild` plus a polled `GetBuildStatus` (the node runs the
> build under its own context and caches the outcome), and bean-api's
> `ReconcileBuilds` re-attaches to in-flight builds on restart. A build now
> survives a bean-api restart. **KVM-host e2e (§14) passed — all 4 tests green
> on real hardware (2026-08-15)**, satisfying the binding rule; unit tests pass
> too. The status-marker convention is defined in
> [architecture.md](architecture.md) §0.
> **Authority order: code > [status.md](status.md) > [decisions.md](decisions.md) > design docs.**
> 中文版:[zh/build-logs-s3.md](zh/build-logs-s3.md)

## 1. Why change anything

A build takes minutes, so `POST /v1/templates/build` returns `202` and the
build runs detached on a node while the caller follows two endpoints:

- `GET  /v1/templates/build/logs?ref=` — streams the output
- `POST /v1/templates/build/cancel?ref=` — stops the build

Both are backed today by `buildTracker`: an in-memory `map[ref]*buildLog` on
**one** bean-api process, holding a 4 MiB ring buffer per build and a
`context.CancelFunc` that kills the build. The node streams log frames up the
`BuildImage` gRPC stream; `drainBuildStream` copies each frame into the ring
buffer; `handleBuildLogs` reads back out of it. It works, single-replica. It has
three defects the moment there is more than one bean-api, or a restart:

1. **Multi-replica 404.** The `/logs` and `/cancel` requests can land on any
   replica, but the buffer and the `CancelFunc` live only on the replica that
   handled `/build`. Every other replica answers `BUILD_NOT_FOUND` for a build
   that is running fine. (`docs/build-service.md` §3.5 already names this.)
2. **Restart loss.** The buffer is memory. Restart bean-api mid-build and the
   log is gone; `/cancel` can no longer reach the build (the `CancelFunc` died
   with the process) even though the build itself keeps running on the node.
3. **Double relay.** Every log byte travels node → bean-api (gRPC) → client,
   and is buffered whole in bean-api's RAM in between, bounded only by the 4 MiB
   window that then *drops* earlier output ("log truncated" in `handleBuildLogs`).

The fix is to stop making bean-api the log's home. bean-api should hold **no**
build state; it should be a stateless reader over a durable store, so any
replica serves any build's logs and a restart loses nothing.

## 2. Reference: how E2B does it

E2B (`infra/packages/`) is the closest working reference, and its shape is the
target:

- **Logs** are written to **Loki**, tagged `{service="template-manager",
  buildID=…, envID=…}`. The API serves logs by querying Loki by `buildID` and a
  `LogsOffset` (`template_build_status.go` → `lokiClient.QueryRange(...)`). The
  API holds nothing; any API replica answers.
- **Status** (Building/Ready/Failed + reason) lives in **Postgres**
  (`envbuild.Status`), not in the log store. Terminal-ness is a DB read, not a
  property of the log stream.
- **Cancellation** is owned by the **orchestrator node**: the build runs in a
  node-side goroutine registered in a per-node cache (`create_template.go`:
  `go func(){…}()`, `defer buildInfo.Cancel()`), and the API cancels by sending
  a `DeleteBuild` gRPC to the node that runs it (`template_start_build.go` →
  `delete_template.go`: `c.Cancel()`). The control plane holds no cancel handle.

The lesson is the **three-way split**: logs in a log store, status in the
database, cancel owned by the node. The control plane keeps nothing per-build.

bean adopts the split. It does **not** adopt Loki: bean already has a
first-class object-store contract (`s3.ObjectStore`) backing snapshot blobs and
overlaybd layers, with `GetRange`/`Head` and a dev `DirStore`. A build log is an
append-only byte stream addressed by offset — exactly what that contract serves,
and range reads are what `/logs?follow` needs. Adding Loki would be a second
storage system to run, secure and reason about for a payload the object store
already fits. So bean's log store **is S3**, in a dedicated bucket.

## 3. Decisions (user, 2026-08-15)

1. **noded uploads directly.** The node that runs the build writes the log
   chunks to S3 itself — not bean-api relaying them. This removes the double
   relay (§1.3) and, combined with decision 2, decouples the build's lifetime
   from any bean-api connection. Cost: noded needs write credentials for the
   logs bucket (§9).
2. **Cancel track lands with it.** Cancellation moves to the node in the same
   change, not a later one: persist the builder's `nodeId`, hold the cancel
   handle on the node, add a node `CancelBuild` RPC (§8).
3. **Dedicated bucket.** Build logs go in their own S3 bucket (e.g.
   `bean-build-logs`), separate from the blobs/overlaybd bucket — different
   lifetime (logs expire; layers are content-addressed and kept), different
   retention policy, and a smaller credential blast radius for the node
   (§9).
4. **Design doc first.** This document settles the boundaries before code:
   bucket key layout, who writes, the stateless read path, build state in the
   store, the node-owned cancel track, and removal of the in-memory buffer.

## 4. Architecture

```
  bean build ──POST /build──▶ bean-api ──StartBuild──▶ noded  (owns the build)
                                 │                        │
                                 │                        ├─▶ buildctl / buildkitd
                                 │                        │
                                 │                        └─▶ S3 logs bucket
                                 │                              buildlogs/<key>/NNNNNN
                                 ▼                              buildlogs/<key>/manifest
                          store.Template                        ▲
                          {State, NodeID, BuildID}              │
                                 ▲                              │
  bean build --follow ──GET /logs──▶ any bean-api replica ──────┘ (GetRange/Head)
  bean build cancel ────POST /cancel─▶ any replica ──CancelBuild──▶ owning noded
```

Three stores, no per-build state in bean-api:

- **Logs → S3** (dedicated bucket), written by noded, read by any bean-api
  replica over `GetRange`/`Head`.
- **Status → `store.Template`** (`State`, `Reason`, plus `NodeID`, `BuildID`
  which already exist on the record). The store is the single source of truth
  for "is this build done, and how did it end".
- **Cancel → the owning node.** bean-api resolves `Template.NodeID` and sends
  `CancelBuild(ref)`; the node cancels the build it is running.

Because the build runs under a node-owned context (registered in a node cancel
registry, not tied to a bean-api stream), it **survives a bean-api restart** and
any replica can follow or cancel it. That is the property the current design
lacks.

## 5. S3 layout — the dedicated logs bucket

One bucket, one flat prefix per build:

```
buildlogs/<key>/000000        first chunk  (immutable once written)
buildlogs/<key>/000001        next chunk
buildlogs/<key>/...
buildlogs/<key>/manifest      small JSON: {seq, done, failed, reason, updatedAt}
```

- **`<key>` is the build ref, sanitized.** The ref is the template tag (builds
  are keyed by tag — see `build.go`'s note on why there is no separate build
  id). It contains `/`, `:`, `@`, so it is mapped through the **same scheme as
  `refToFilename`** (`internal/node/image/file.go:15`): alnum/`-`/`_` pass
  through, everything else becomes `_<hex>`. The result is slash-free,
  collision-free, and stable — a clean single path segment. `refToFilename` is
  today unexported in `internal/node/image`; this design lifts the sanitizer
  into a small shared helper (e.g. `internal/control/s3.BuildLogKey(ref)` or a
  `buildkey` package) so **the writer (noded) and the reader (bean-api) derive
  the identical key** without either importing the other's package.

- **Chunks are immutable and append-only by sequence.** A chunk is written once,
  in full, via `s3.Put` (or `Writer`+`Close`), then never rewritten — S3's
  "nothing readable until Close" guarantee means a reader never sees a partial
  chunk. New output becomes the next `NNNNNN`. This sidesteps S3's lack of
  object append: instead of appending to one object we add objects. Six digits,
  zero-padded, sort lexically = chronologically.

- **The `manifest` object is the log store's own status side-channel**, small
  and overwritten (last-writer-wins) as the build progresses: how many chunks
  exist (so a reader knows where to stop without a `LIST`), and whether the build
  finished. The **authoritative** terminal status is still `store.Template`
  (§6); the manifest exists so the *log reader* can decide "is there more coming"
  from the same store it reads bytes from, without a round trip to the control
  DB on every poll. If the two ever disagree, `store.Template` wins.

Not using `LIST`: a reader walks `Head(buildlogs/<key>/NNNNNN)` from its current
sequence upward until `ErrNotFound`, or reads `manifest.seq`. `LIST` is an extra
permission and a slower, eventually-consistent call; sequential `Head` is the
same primitive lazy-pull already relies on.

## 6. Read path (bean-api, stateless)

`handleBuildLogs` no longer touches `buildTracker`. It becomes:

1. Look up `store.Template` by ref. Absent → `404`. Present but not a build
   (`Source != TemplateBuilt`) → `400`.
2. Read chunks from the logs `ObjectStore` in sequence order, streaming each to
   the client as chunked `text/plain` (unchanged content type and framing — a
   `curl` and `bean build --follow` still consume it without a parser).
3. **Offset.** The client's byte offset maps to (chunk index, intra-chunk
   offset); the reader `Head`s to learn chunk sizes and `GetRange`s the tail of
   the first partial chunk, then whole chunks after. This is offset-addressed
   like the current API, so `--follow` reconnection and range resumption keep
   working.
4. **Follow.** `?follow=true` (default): after draining known chunks, re-`Head`
   the next sequence / re-read `manifest` on a short poll interval until the
   manifest (or `store.Template`) says terminal, then drain the final chunk and
   stop. `?follow=false`: drain what exists and stop.
5. **Terminal outcome in the body**, as today: the response committed `200`
   before the outcome was known, so success/failure is written into the body
   (`build succeeded` / `build failed: <reason>`), read from `store.Template`.

No ring buffer, so **no "log truncated" gap** — every chunk is durable until the
bucket lifecycle expires it (§10). The only truncation a reader can see is a
whole build aged out of the bucket, which is a clean `404`, not a mid-stream gap.

Any replica serves this: it holds nothing, it only needs the logs `ObjectStore`
handle and the store. This is the multi-replica fix (§1.1) and the restart fix
(§1.2) for reads.

## 7. Write path (noded)

The node owns the build and its log upload. In `Builder.Build` /
`runBuildctl`, the `Logs io.Writer` that today is the gRPC stream sender becomes
an **S3 chunk writer**:

- A `logUploader` wraps the logs `ObjectStore` and buffers BuildKit's output.
  It flushes a chunk when the buffer reaches a size threshold (e.g. 256 KiB–1
  MiB) **or** a short time elapses (e.g. 1–2 s), whichever comes first — so a
  quiet build still shows progress and a chatty one does not make an object per
  line. Each flush `Put`s `buildlogs/<key>/<seq>` and bumps `manifest.seq`.
- On completion it writes the terminal `manifest` (`done`, `failed`, `reason`)
  and flushes any tail.
- The 40-line tail buffer (`buildLogTailLines`) that names the failing step in
  the build **error** stays — it reaches bean-api by a different route (the RPC
  result/error) and is what makes a failure legible without fetching the log.

The gRPC log frames (`BuildImageEvent.log`) are **no longer needed for
durability**. Options, decided in §8: either drop them (node uploads, bean-api
never sees log bytes) or keep them as a best-effort live tail. The design drops
them — one writer of the log, one reader path, no double relay.

## 8. Cancel track and the RPC reshape

Two decisions collide here productively: **noded uploads** (so bean-api need not
hold the log stream) and **cancel is node-owned** (so bean-api need not hold the
`CancelFunc`). Together they mean bean-api holds **nothing** per build, which in
turn means the `BuildImage` server-stream — whose entire job was to carry log
frames to bean-api and whose `ctx` was the cancel mechanism — has no job left.
So the node build RPC is reshaped:

- **`StartBuild(BuildImageRequest) → StartBuildResponse{buildId}`** — returns
  as soon as the build is registered and running on the node, not when it
  finishes. The node runs the build in its own goroutine under a node-owned
  `context` (derived from `context.Background()`, **not** the RPC's ctx), stored
  in a **per-node build registry** keyed by ref (mirrors E2B's
  `buildInfo`/`buildCache`). This is what makes the build outlive any bean-api
  connection.
- **`CancelBuild(ref) → CancelBuildResponse`** — looks up the ref in the
  registry and cancels its context, which kills `buildctl` exactly as the
  detached-ctx cancel does today. Cancelling an unknown/finished ref is not an
  error (idempotent), matching the object-store `Delete` convention.
- **Terminal result — bean-api polls the node.** Someone must still flip
  `store.Template` to READY/FAILED with the artifact coordinates
  (`overlaybd_ref`, `size_bytes`, `layer_digests`, `config`). The node caches its
  build's outcome in the registry and exposes **`GetBuildStatus(ref) →
  {phase, result, reason}`**; bean-api's per-build goroutine polls it once a
  second until a terminal phase, then does `MarkReady` / `MarkFailed`. The write
  of authoritative status stays control-side (the store is bean-api's); the node
  only reports. This is **exactly what E2B does** — its API calls a
  fire-and-forget `TemplateCreate`, then a background `PollBuildStatus` tickers
  the node's `GetStatus` every second and writes Postgres on the control side
  (`packages/api/internal/template-manager/{create_template,template_status}.go`).
  It is also the smaller change: it needs **zero** changes to `nodesvc` — no
  `image.Service` injection, no node→control result RPC, no ack — because bean-api
  already holds `s.images` and the control→node `SandboxServiceClient`.
  - (rejected) node pushes a `ReportBuildResult` up the heartbeat. The node does
    have an authenticated heartbeat, so this is feasible, but it is not what E2B
    does, needs a reconciler for missed pushes anyway, and couples `nodesvc` to
    the template store. Deferred as an optional latency fast-path (§9-adjacent).
- **Restart reconcile.** A restarted bean-api must re-attach to in-flight
  builds: on startup `ReconcileBuilds` lists `store.Template` in `BUILDING` and,
  for each, resumes `pollBuild(NodeID, ref)` under a fresh `maxBuildDuration`
  bound (a template with no `NodeID` is failed — no node owns it). Same poll loop
  as a live build, so one mechanism serves both. Builds themselves never stopped;
  only the status write was owed.

**Persisting `NodeID`.** `store.Template` already has `NodeID` and `BuildID`
(`store/types.go:209`), but `handleBuild` does not set `NodeID` on the record
today. This design sets it right after `pickBuilder` returns, before/at
`StartBuild`, so `/cancel` on any replica resolves the owning node from the
store. Near-zero cost — the field exists.

### Landing in two verifiable steps

The reshape is real surface area, so it lands in two e2e-able steps rather than
one big cut:

- **Step A — logs to S3, cancel to node, keep the stream.** noded uploads log
  chunks to S3 (§7); `/logs` reads from S3 (§6); add the node cancel registry +
  `CancelBuild`; persist `NodeID`; `/cancel` calls the node. Keep the
  `BuildImage` server-stream as the *result carrier* only (bean-api still waits
  on it for the result frame and does `MarkReady`). This already fixes the
  multi-replica 404, the restart loss for **logs**, and the double relay — and
  is fully testable.
- **Step B — sever the stream (done).** Replaced the held `BuildImage` stream
  with a fire-and-forget `StartBuild` + a polled `GetBuildStatus` + the
  `ReconcileBuilds` restart reconciler, so a bean-api restart no longer owes a
  status write it could miss — the build runs under the node's own context and
  the replacement re-attaches by polling. This is full E2B-shaped decoupling.

`buildlog.go`'s in-memory `buildTracker`/`buildLog` is deleted in Step A; the
`changed`-channel follow it provided is replaced by the S3 `Head`/manifest poll.

## 9. Credentials and the STS debt

noded uploading means noded needs **write** credentials for the logs bucket.
Rules from [s3-storage.md](s3-storage.md) §6 hold:

- **Secrets come from env vars, never flags** — `BEAN_S3_ACCESS_KEY` /
  `BEAN_S3_SECRET_KEY` (flags leak via `/proc/<pid>/cmdline` and `ps`). The
  endpoint/region/bucket may be flags. noded already loads S3 creds this way for
  the layer store (`cmd/noded/main.go:717`), so the logs bucket reuses the
  pattern — one `s3.Client`, a second `NewBucketStore` over the logs bucket
  (one client serves multiple buckets, as `objectstore.go` notes).
- **The dedicated bucket shrinks the blast radius.** Because logs are their own
  bucket, the node's logs credential can be scoped to *just* that bucket and
  need not be the same key as the layer-store credential. Logs are lower-value
  than layers, so leaking the logs key is a smaller problem than leaking the
  blob key.
- **Known live debt, unchanged by this design but relevant:** noded holds
  long-lived S3 credentials; STS rotation and presigned-URL upload are not yet
  implemented. This design *adds a second bucket the node writes*, so it widens
  that debt slightly and should be called out when STS lands — the eventual
  shape is bean-api minting a short-lived presigned PUT (or STS session) scoped
  to `buildlogs/<key>/*` and handing it to the node, so the node never holds a
  standing logs credential. Out of scope here; noted so it is not forgotten.

## 10. Retention — a bucket lifecycle rule, not a process

The in-memory design expired logs after 30 minutes (`buildLogRetention`) because
it had to — they were in RAM. On S3 there is no reason to hold logs in a process
at all, and no reason to hand-roll expiry. Retention is an **S3 lifecycle rule**
on the logs bucket, operator-configured (e.g. expire `buildlogs/` objects after
N days). The dedicated bucket (§3.3) is what makes this clean: the rule applies
to the whole bucket without touching the content-addressed layer blobs, which
must **never** expire. Dev/CI use `DirStore` and simply keep everything (or a
cron prunes the dir); there is no lifecycle daemon in bean.

## 11. Config

- **bean-api**: `--s3-logs-bucket` (or `BEAN_S3_LOGS_BUCKET`), default e.g.
  `bean-build-logs`. When set with the existing `--s3-endpoint`, bean-api builds
  a second `NewBucketStore` for reads. Unset → `DirStore` under a logs dir for
  dev, mirroring how blobs fall back (`snapshot.NewDirBlobs`).
- **noded**: the same logs bucket name (flag/env), so the node builds a
  `BucketStore` to write into. Endpoint/region reuse the node's existing
  `--s3-*` flags; only the bucket differs.
- Both derive the key with the shared `BuildLogKey(ref)` helper (§5) so writer
  and reader agree byte-for-byte.

## 12. Migration / what each piece becomes

| Today (`buildlog.go` / `build.go`) | Becomes |
|---|---|
| `buildTracker map[ref]*buildLog` (in-mem) | **deleted** — no per-build state in bean-api |
| 4 MiB ring buffer + `maxBuildLogBytes` | S3 chunk objects, no window, no drop |
| `buildLogRetention` (30 min, in-proc) | S3 bucket lifecycle rule (§10) |
| `changed` channel (no-poll follow) | `Head`/`manifest` short-poll in `/logs` |
| `log.cancel()` = local `CancelFunc` | `CancelBuild(ref)` RPC to `Template.NodeID` |
| `drainBuildStream` waits on result frame | **deleted** — node uploads chunks; `pollBuild` polls `GetBuildStatus` (Step B) |
| `handleBuildLogs` reads ring buffer | reads S3 chunks (stateless) |
| `handleBuildCancel` calls `log.cancel()` | resolves `NodeID`, calls node RPC |
| `Template.NodeID` unset on build | set at `pickBuilder`/`StartBuild` time |

Proto: added `CancelBuild` (Step A); for Step B, replaced the `BuildImage`
server stream with `StartBuild` + `GetBuildStatus` and retired the
`BuildImageEvent` message. `BuildImageResponse` is kept (reused inside
`GetBuildStatusResponse`). Regenerate `internal/gen`.

## 13. Doc-marker fixes (do first)

`status.md:54` still reads `| Build logs and cancellation | ⚠️ | A build reports
no progress and cannot be stopped |`. That is stale twice over: logs/cancel are
already implemented (single-replica), and this design changes how. Update it to
reflect the real state — implemented but single-replica/in-memory, with this
doc as the multi-replica plan — rather than "no progress and cannot be stopped".
Reconcile `image-build.md` and `build-service.md` §3.5 (which already predicts
the multi-replica crack) to point at this doc.

## 14. e2e (needs a real KVM host — binding rule)

Unit tests (S3 chunking against `DirStore`, key-derivation parity between writer
and reader, offset/follow reader logic) are necessary but **not sufficient** —
every phase is verified on the `.75` KVM host (see the storage-convergence
binding rule; `docs/bean-75` host quirks: buildctl on PATH,
`docker.m.daocloud.io` mirror, `vhost_vsock`). The e2e lives in
`tests/e2e/buildlogs_test.go` (build tag `e2e`, skips unless `BEAN_S3_ENDPOINT`
is set) and is run via `hack/buildlogs-e2e.sh <creds-env-file>`. The harness runs
its node with `--runtime fc` (plus the firecracker/kernel/agent-disk assets), not
`--runtime local`: only the fc tier implements `runtime.ImageBuilder`
(`internal/node/runtime/fc_linux.go`), so a local-runtime node cannot build at all
(`runtime local cannot build images`). Building never boots a microVM — it shells
out to buildkit — but `NewFCTier` still needs `/dev/kvm` and those assets to
construct. Passed on real hardware 2026-08-15 (all four green, `ok tests/e2e`).
The e2e proof:

1. Build a template on the KVM host with a real `--s3-logs-bucket` (MinIO).
   Confirm chunk objects land at `buildlogs/<key>/NNNNNN` and `manifest`
   advances. (`TestBuildLogsLandInS3`)
2. Follow the log from a **second** bean-api replica: output is continuous, no
   `BUILD_NOT_FOUND`, no `[log truncated]` gap. This is the multi-replica read
   fix, proven. (`TestBuildLogsServedFromOtherReplica`)
3. `bean build cancel` from a replica that did not start the build: the build
   stops (buildctl dies on the node), `store.Template` goes `FAILED`, the ref
   frees for a retry. (`TestBuildCancelFromOtherReplica`)
4. **Kill the bean-api that started a build, mid-build, and bring up a
   replacement on the same ports: the build keeps running on the node and the
   replacement's `ReconcileBuilds` drives the template to `READY` with the real
   `overlaybd_ref`/`layer_digests`.** This is the Step B restart-survival
   property. (`TestBuildSurvivesReplicaRestart`)
5. Confirm the logs-bucket lifecycle rule expires an old build's logs to a clean
   `404`, while the layer blobs in the other bucket are untouched. (manual /
   ops-configured, not in the automated suite)

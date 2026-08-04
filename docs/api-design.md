# API and Proxy Service Design

> 中文版:[zh/api-design.md](zh/api-design.md)

> Corresponding components: `bean-api` (api-gateway, ✅), `bean-proxy` (port reverse proxy, 📐 unimplemented).
> The status-marker convention is defined in [architecture.md](architecture.md) §0.
> Terminology and the state machine live in [architecture.md](architecture.md).

## 1. Design Principles

- **REST outward, gRPC inward**: SDK/CLI speak REST (+ WebSocket for streaming); control ↔ noded ↔ agent speak gRPC
- **Idempotency**: every creation endpoint accepts an `Idempotency-Key` header, deduplicated by a unique constraint in the state store
- **Large objects never pass through the gateway**: file upload/download above a threshold (4 MiB by default) always goes to S3 by presigned URL or straight to noded
- **proto is the single source of truth**: REST DTOs are derived from proto, and the OpenAPI spec is generated

## 2. Authentication

### 2.1 API Key ✅

- `Authorization: Bearer bk_<keyid>_<secret>`
- The key hash is stored in Postgres, together with its quota (concurrent sandbox count, total CPU/mem, volume capacity, prewarm permission)
- **No user/tenant system** — bean is an in-cluster internal service, and the key exists only to identify the caller, apply quota and attribute audit records; the security weight sits on in-cluster reliability (managed TLS + node token, credential tiering, isolation tiers) rather than on multi-tenancy

### 2.2 Sandbox-scoped short-lived credential 📐

- On sandbox creation the gateway issues a **sandbox token** (JWT, bound to the sandbox-id, fixed 24h TTL, renewable through the API; invalidated the moment the sandbox is destroyed)
- Purpose: proxy access to protected ports, WebSocket exec reconnects — so a long-lived API key never has to be handed to a browser or other weak environment

### 2.3 S3 Presigned URL 📐

- Issued centrally by the control plane (the only place holding long-lived S3 credentials)
- Scenarios: file upload/download, eval artifact reporting, snapshot read/write
- TTL 15 min by default; upload URLs are bound to a content-length-range

## 3. REST API in Detail

Base: `https://api.<domain>/v1`. Error responses are uniform:

```json
{ "error": { "code": "SANDBOX_NOT_FOUND", "message": "...", "details": {} } }
```

| HTTP | example code |
|---|---|
| 400 | INVALID_ARGUMENT, IMAGE_REF_INVALID |
| 401/403 | UNAUTHENTICATED, PERMISSION_DENIED, QUOTA_EXCEEDED |
| 404 | SANDBOX_NOT_FOUND, SNAPSHOT_NOT_FOUND, SNAPSHOT_DATA_MISSING, SNAPSHOT_BASE_MISSING |
| 409 | SANDBOX_NOT_RUNNING, IDEMPOTENCY_CONFLICT, SNAPSHOT_IN_USE, SNAPSHOT_NOT_READY, INCOMPATIBLE_CPU |
| 429 | RATE_LIMITED |
| 500/503 | INTERNAL, NO_CAPACITY, NODE_LOST |

### 3.1 Sandboxes ✅

```
POST /sandboxes
{
  "image": "registry.example.com/swebench/django__django-12345:latest",
  "resources": { "cpu": 2, "memoryMiB": 4096, "diskMiB": 20480 },
                                        // gpu/isolation are internal fields, not exposed;
                                        // the runtime tier is assigned by the scheduler
                                        // (architecture D3)
  "env": { "FOO": "bar" },
  "cmd": null,                          // overrides the image CMD; null = keep the original
                                        // entrypoint (started under the agent)
  "autoStartCmd": false,                // true starts the original entrypoint right after create
  "region": "ap-east-1",                // optional; defaults to the key's default region;
                                        // forced to the region holding the data when mounting an
                                        // existing volume or creating from a snapshot
  "nodeSelector": { "pool": "nvme" },   // optional; filters by node labels
  "lifecycle": {                        // optional; default = run forever
    "idleTimeout": "300s",              //   null/absent = never; "0s" = fires as soon as
                                        //   activity ends
    "onIdle": "pause"                   //   pause (default) | kill
  },
  "labels": { "eval-run": "swebench-0731", "task": "django-12345" },
  "networkPolicy": "egress-only",       // egress-only|none|allow-list (reserved)
  "volumes": [                          // optional, see §3.6
    { "volume": "vol_...", "subPath": "run-0731", "mountPath": "/workspace", "readOnly": false }
  ]
}
→ 201 { "sandbox": { "id": "sbx_...", "state": "PENDING", ... }, "token": "<sandbox JWT>" }
```

```
GET    /sandboxes/{id}                       → sandbox detail (state, runtime, nodeId, createdAt, lifecycle, lastActivityAt, endpoints)
GET    /sandboxes?label=eval-run%3Dswebench-0731&state=RUNNING&pageToken=&pageSize=100
DELETE /sandboxes/{id}                       → 202, destroyed asynchronously; ?force=true skips graceful
PATCH  /sandboxes/{id}/lifecycle { "idleTimeout": "600s", "onIdle": "kill" }   → adjust at runtime
POST   /sandboxes/{id}/pause                 → 202 → PAUSED
POST   /sandboxes/{id}/resume                → 202 → RUNNING
POST   /sandboxes/{id}/snapshot  { "name": "after-setup", "keepRunning": true }
                                             → 202 { "snapshotId": "snap_..." }
POST   /sandboxes/{id}/start                 → start the original entrypoint (manual start after autoStartCmd=false)
POST   /sandboxes/{id}/fork     { "count": 3, "labels": {...} }    // separate API (fc tier, P4)
       → 202 { "sandboxes": [ ...N new sandboxes... ] }
       // Semantics: take an instantaneous CoW snapshot of a running sandbox and clone N
       // independent instances (no persistent snapshot object is produced; use /snapshot
       // if you want one kept). The container tier returns 501.
       // NOTE: this is a convenience over the snapshot+restore pair, not a new capability.
       // POST /snapshot then N x POST /sandboxes{snapshot} already yields N independent
       // sandboxes today; fork saves the persistent object and the round trip.
       // See snapshot-resume.md 4.5
```

The sandbox detail response carries `runtime: fc|runsc|runc` (the actual tier, for troubleshooting).

Batch (frequent in eval scenarios):

```
POST /sandboxes:batchCreate   { "requests": [ ... ≤100 ... ] }
→ 207 per item { index, sandbox | error }     // partial-success semantics
DELETE /sandboxes?label=eval-run%3Dswebench-0731    → batch destroy, 202 + task count
```

### 3.2 Exec ⚠️

> Synchronous exec and streaming exec are both shipped; **PTY is unimplemented**.


```
POST /sandboxes/{id}/exec          // synchronous, suits a single eval command
{
  "cmd": ["python", "-m", "pytest", "tests/"],
  "cwd": "/workspace", "env": {}, "timeoutSeconds": 600,
  "stdin": "<base64, optional>",
  "maxOutputBytes": 1048576          // truncates past this and sets truncated=true
}
→ 200 { "exitCode": 1, "stdout": "...", "stderr": "...", "truncated": false, "durationMs": 42150 }
```

```
WS /sandboxes/{id}/exec/ws?pty=true&cols=120&rows=40
```

WebSocket subprotocol (JSON frames):

```
C→S: {"type":"start","cmd":["bash"],"pty":true,"env":{}}
C→S: {"type":"stdin","data":"<base64>"}
C→S: {"type":"resize","cols":120,"rows":40}
C→S: {"type":"signal","signal":"SIGINT"}
S→C: {"type":"stdout"|"stderr","data":"<base64>"}
S→C: {"type":"exit","exitCode":0}
```

Path: client → gateway (upgrade) → noded gRPC stream → agent. The gateway only forwards frames and authenticates.

### 3.3 Files ✅

```
PUT  /sandboxes/{id}/files?path=/workspace/patch.diff     // body ≤4MiB sent directly
     ?mode=0644&mkdirs=true
GET  /sandboxes/{id}/files?path=/workspace/report.json    // ≤4MiB returned directly
GET  /sandboxes/{id}/files/ls?path=/workspace             → [{name,size,mode,mtime,isDir}]
POST /sandboxes/{id}/files:uploadUrl   {"path": "...", "sizeBytes": 123456789}
     → { "url": "<presigned PUT>", "commit": "/files:commitUpload?token=..." }
     // Two-stage: client PUTs to S3 → calls commit → gateway instructs the agent to
     // FetchToSandbox (the agent pulls it to the target path inside the sandbox over a
     // presigned GET)
POST /sandboxes/{id}/files:downloadUrl {"path": "..."}
     → { "url": "<presigned GET>" }    // noded stages the file to S3, then signs a URL
DELETE /sandboxes/{id}/files?path=...
```

### 3.4 Ports 📐

> Unimplemented; depends on the network stack and on bean-proxy.


```
POST /sandboxes/{id}/ports    { "port": 8888, "auth": "token" }   // token|public
→ { "url": "https://sbx-abc123-8888.<region>.sandbox.<domain>" }
GET    /sandboxes/{id}/ports
DELETE /sandboxes/{id}/ports/{port}
```

### 3.5 Images ✅

**Terminology (these have to be kept apart)**:

| Concept | Owner | Notes |
|---|---|---|
| `ref` | **user input** | The native OCI reference (`python:3.12`). This is the only thing the user supplies and the only thing they see |
| `digest` | resolved by the platform | Resolved once from the tag and then fixed; scheduling, caching and reproducibility all key off the digest, so a moving tag cannot change the contents of a batch |
| overlaybd artifact | **platform-internal** | The converted block-device form; invisible to the user and not selectable |
| `state` | platform-internal | `PENDING → CONVERTING → READY \| FAILED` |

The `format` field tells the caller which tier can currently run the image: `oci`
(unconverted, standard pull path) or `overlaybd` (converted, usable by the fc tier).

```
GET  /images                      list
GET  /images/status?ref=<ref>     single-image status (ref goes in the query: it contains / and :)
     → { ref, digest, state, format, cachedNodes, sizeBytes }
POST /images/prewarm   { "refs": ["img:a"], "region": "ap-east-1",
                         "targetNodes": 10, "priority": "high" }
     → { jobId, refs, ready: {ref: nodeCount}, done }
GET  /images/prewarm/{jobId}      per-image × per-node readiness matrix
```

`cachedNodes` / `targetNodes` are **operator semantics, deliberately kept out of the
CLI**: how many machines a replica landed on is a scheduling detail the user cannot act
on, and exposing it only makes people depend on the scheduling result, after which the
scheduler can no longer migrate anything. The CLI side reports only ready / warming, and
the corresponding parameter is called `--replicas`. See `docs/sdk-cli-design.md` §4.1.

**Registry authentication** (private images): register the credential once per registry
host, after which a private image is used exactly like a public one — you supply only a ref.

```
PUT    /registries   { "host": "registry.example.com", "username": "robot",
                       "secret": "..." }     // secret is write-only: absent from responses, logs and sandboxes
GET    /registries                            → host/username/timestamps (no secret)
DELETE /registries/{host}
```

- Credentials are AES-256-GCM encrypted before hitting the database (`--secret-key` /
  `BEAN_SECRET_KEY`), so a database copy on its own is not enough to pull a private image;
  **with no master key the endpoint refuses rather than storing plaintext**
- A registry with no credential is pulled anonymously, so public images keep working
- Host normalisation: `https://r.io/` and `r.io` are treated as the same one; a ref with no
  host defaults to Docker Hub (the same rule container runtimes use)

### 3.6 Volumes 📐

Images and volumes are two orthogonal resources (image = environment, volume = data,
independent lifecycles). The data plane is in noded-design.md §3.3.

The first pass carries only the `shared-fs` type (`dataset` is reserved and not yet scheduled):

```
POST   /volumes    { "name": "alice-ws", "type": "shared-fs",
                     "quotaMiB": 102400, "labels": {} }
GET    /volumes?label=...          → includes usage (space/inode consumption)
GET    /volumes/{id}
DELETE /volumes/{id}               // 409 VOLUME_IN_USE while a mount is active

POST /sandboxes { ..., "volumes": [
  { "volume": "vol_...", "subPath": "run-0731", "mountPath": "/workspace",
    "readOnly": false }
] }
```

- Mount-level `readOnly` can tighten things further (default false)
- Volume state machine: `CREATING → READY → DELETING`
- Quota: space/inodes are enforced by the backend (JuiceFS directory quota); the per-key
  total volume capacity quota is in §7

### 3.7 Snapshots ✅

```
POST   /sandboxes/{id}/snapshot  { "name": "after-setup", "labels": {},
                                   "keepRunning": true,
                                   "includeMemory": true,
                                   "base": "snap_..." }
       → 202 { snapshotId, snapshot: {state, sizeBytes, includeMemory,
                                      baseId, chainDepth, cpuVendor, ...} }
GET    /snapshots?label=k%3Dv&state=READY   → list
GET    /snapshots/{id}
DELETE /snapshots/{id}      // RefCount>0 or has descendants → 409 SNAPSHOT_IN_USE
POST   /sandboxes    { "snapshot": "snap_..." }   // restore: a NEW sandbox with a new id,
                                                  // not a revival of the one snapshotted.
                                                  // Call it N times for N independent
                                                  // sandboxes from one snapshot
                                                  // image and snapshot are mutually exclusive
                     // incompatible CPU → 409 INCOMPATIBLE_CPU
```

Restore is a `POST /sandboxes` — a creation — while `resume` is a `POST` on an existing
`/sandboxes/{id}`. The two are different operations on different subjects; see
[snapshot-resume.md](snapshot-resume.md) §0.

- `includeMemory` defaults to **true**, which is what a snapshot has always meant (the
  restored sandbox continues the captured guest rather than booting). Setting it false
  captures only the filesystem: restore boots afresh,
  but **it can land on any CPU** — guest memory pins a snapshot to a compatible
  vendor+family. Measured at 6109 B against 15.5 MB for the full variant.
  **Use a pointer type** (`*bool`) to tell "absent" from "explicitly false": old snapshots
  do not carry the field, and a plain bool would decode it as false and thereby bypass the
  CPU constraint — precisely on the snapshots that need it most.
- `base` stores only the guest memory changed since that snapshot. Measured at 298 KB
  against 15.5 MB. It requires `includeMemory` (the filesystem layer is already
  O(changed)).
  Past a chain depth of 8 it **automatically produces a full snapshot and leaves baseId
  empty** — the baseId in the response is how the caller tells what it actually got, and
  it does not need to know what the limit is.
  The node must be started with `--track-dirty-pages`, otherwise this errors out explicitly
  rather than silently downgrading.

- `keepRunning` defaults to **true**: a snapshot should not disturb a sandbox that is
  working; it freezes briefly for consistency and then returns to its prior state (RUNNING
  or PAUSED, whichever it was)
- Snapshot state machine `CREATING → READY | FAILED`; a failure records a reason and does
  not get stuck in CREATING
- **Reference counting**: a reference is held for the duration of a restore, deletion during
  that window returns 409, and the reference is released automatically when the restore ends
- A restore inherits the snapshot's image (the rootfs base has to match the checkpoint)
- The checkpoint format **differs per runtime tier** and is not interchangeable, so a
  snapshot records the runtime that produced it
- Blob storage goes through the `snapshot.Blobs` interface: **S3 is shipped** (SigV4
  implemented in-house, multipart upload, range reads), and a local directory is the dev
  default. Both are atomic writes (temp file + rename / multipart commit), so a failure
  leaves no readable half-product
- **INCOMPATIBLE_CPU is a 409, not a 503**: waiting will not make it feasible, and a client
  retrying on a 503 will loop until it times out on its own

### 3.8 Events ✅

```
Event types: sandbox.lifecycle.{created,running,paused,resumed,stopped,failed,lost,oom}
             + sandbox.snapshot.{ready,failed}
             // stopped corresponds to the STOPPED state (covers explicit DELETE and
             // onIdle=kill); lost corresponds to losing the node lease
Event body:  { "id", "type", "timestamp", "sandboxId", "data": {...}, "version": "v1" }
             // Naming follows e2b (dotted sandbox.lifecycle.* hierarchy) for ecosystem
             // compatibility

GET /sandboxes/{id}/events?pageToken=      // history (Postgres events table, paginated)
GET /events?sandbox=<id>&label=k%3Dv       // live subscription (SSE: text/event-stream;
                                           //  filtered by sandbox/label; batch eval
                                           //  replaces polling with event-driven flow)
```

Implementation: every state-machine transition emits from one place → Postgres (history) +
in-memory pub/sub (subscriptions). The subscription transport is **SSE** rather than
WebSocket: no extra dependency, it holds up through proxies, and it is simple to consume
from a browser or SDK; slow subscribers are buffered to 64 events and then dropped with a
counter bump (one stuck client must not drag down the API). Webhook delivery is a P5
reserve item.

### 3.9 Logs / Observability ⚠️

```
GET /sandboxes/{id}/logs?follow=false&tailLines=1000    // agent ring buffer + S3 archive
GET /nodes                                              // operator surface: node list, capacity, capabilities
GET /metrics                                            // Prometheus format (unauthenticated: local scrape, contains no sandbox content)
    // bean_sandbox_creates_total{outcome}         creation outcome counter
    // bean_sandbox_create_duration_seconds{outcome}  end-to-end creation latency histogram
    // bean_exec_duration_seconds{outcome}         exec round-trip latency
    // bean_sandboxes{state}                       sandbox count per state (recomputed from the DB at scrape time)
    // bean_events_total{type}  bean_event_subscribers
```

**OTel collection**:

> Current status: metrics is a Prometheus endpoint (shipped, a separate thing from trace);
> logs are structured (`internal/logging`); **trace is shipped and measured** —
> enabled with `--otlp-endpoint`, one create/exec is a single cross-process span tree, and
> the request id is the trace id. The response header carries `X-Bean-Trace-Id`, so when a
> caller reports slowness you can hand them the exact trace to look at.
> Per-sandbox resource metrics and OTLP pass-through for applications inside the sandbox
> are both still unimplemented.

- Platform components (gateway/scheduler/noded/agent) export trace/metrics/logs uniformly
  over OTLP (the Prometheus-compatible endpoint is retained); request_id runs through the
  whole path and is the trace id
- **Per-sandbox resource metrics**: noded collects cpu/mem/io/net time series per sandbox
  (cgroup / FC stats) and tags resource attributes with sandbox_id/labels — so consumption
  can be aggregated per eval-run
- **OTLP pass-through for applications inside the sandbox (opt-in)**: the agent listens on
  localhost:4317 inside the sandbox, and application traces are forwarded out over
  vsock/socket and labelled with the sandbox

## 4. Internal gRPC proto draft ✅

```protobuf
// proto/bean/node/v1/node.proto —— control plane ↔ noded
service NodeService {                                              // noded → control (outbound)
  rpc Register(RegisterRequest) returns (RegisterResponse);        // capability/resource profile report
  rpc Heartbeat(stream HeartbeatRequest) returns (stream HeartbeatResponse);
      // Bidirectional stream: ↑ heartbeat + resource level + sandbox state summary +
      //   image cache manifest summary (bloom/hash)
      // ↓ lease confirmation (command dispatch goes over a direct push, see 5.1)
  rpc SyncState(SyncStateRequest) returns (SyncStateResponse);     // noded restart reconciliation: pull the full desired state
}

service SandboxService {                       // implemented by noded; control/gateway connect directly as clients
  rpc CreateSandbox(CreateSandboxRequest) returns (CreateSandboxResponse);
      // spec carries volumes: repeated VolumeMount (dataset disk ref / shared-fs export name)
  rpc RestoreSandbox(RestoreSandboxRequest) returns (RestoreSandboxResponse);  // from a snapshot
  rpc DestroySandbox(DestroySandboxRequest) returns (DestroySandboxResponse);
  rpc PauseSandbox(PauseSandboxRequest) returns (PauseSandboxResponse);
  rpc ResumeSandbox(ResumeSandboxRequest) returns (ResumeSandboxResponse);
  rpc SnapshotSandbox(SnapshotSandboxRequest) returns (SnapshotSandboxResponse);
  rpc ForkSandbox(ForkSandboxRequest) returns (ForkSandboxResponse);      // fc-tier CoW clone
  rpc StartUserProcess(StartUserProcessRequest) returns (StartUserProcessResponse);
  rpc PrewarmImage(PrewarmImageRequest) returns (PrewarmImageResponse);
  rpc PrepareVolume(PrepareVolumeRequest) returns (PrepareVolumeResponse);
      // shared-fs backend mount confirmation (dataset reserved)
  // Data plane: gateway/proxy connect directly to noded and forward (carrying a sandbox-id
  // routing header); a pure pass-through of AgentService:
  rpc Exec(ExecRequest) returns (ExecResponse);
  rpc StreamExec(stream StreamExecFrame) returns (stream StreamExecFrame);
  rpc ReadFile(ReadFileRequest) returns (stream FileChunk);
  rpc WriteFile(stream WriteFileFrame) returns (WriteFileResponse);
  rpc DeleteFile(DeleteFileRequest) returns (DeleteFileResponse);
  rpc ListDir(ListDirRequest) returns (ListDirResponse);
  rpc GetLogs(GetLogsRequest) returns (stream LogChunk);
  rpc ForwardPort(stream PortFrame) returns (stream PortFrame);   // proxy data plane
}

// proto/bean/agent/v1/agent.proto —— noded ↔ beand (fc tier over vsock as the main path / container tier over a unix socket, P5)
service AgentService {
  rpc Exec(ExecRequest) returns (ExecResponse);
  rpc StreamExec(stream StreamExecFrame) returns (stream StreamExecFrame);
  rpc ReadFile(ReadFileRequest) returns (stream FileChunk);
  rpc WriteFile(stream WriteFileFrame) returns (WriteFileResponse);
  rpc ListDir(ListDirRequest) returns (ListDirResponse);
  rpc DeleteFile(DeleteFileRequest) returns (DeleteFileResponse);
  rpc FetchToSandbox(FetchToSandboxRequest) returns (FetchToSandboxResponse);
      // pull a file into the sandbox from a presigned GET URL (stage two of the
      // two-stage large-file upload)
  rpc MountVolume(MountVolumeRequest) returns (MountVolumeResponse);     // NFS/dataset disk mount
  rpc UnmountVolume(UnmountVolumeRequest) returns (UnmountVolumeResponse);
  rpc StartUserProcess(StartUserProcessRequest) returns (StartUserProcessResponse);
  rpc ForwardPort(stream PortFrame) returns (stream PortFrame);    // proxy data plane
  rpc GetLogs(GetLogsRequest) returns (stream LogChunk);
  rpc Health(HealthRequest) returns (HealthResponse);
}
```

Key message field conventions:

- Every request carries a `request_id` (runs through the whole path, correlates logs)
- `CreateSandboxRequest` carries the complete `SandboxSpec` (image ref, resources,
  isolation, network, agent injection parameters, the bundle of S3 artifact presigned URLs)
- `ExecRequest/StreamExecFrame` share one message definition between SandboxService and
  AgentService (`proto/bean/common/v1/exec.proto`); noded is a pure pass-through

## 5. Control Flow Details

### 5.1 Command dispatch model: direct push ✅

The control plane calls noded's `SandboxService` over gRPC directly (noded is the gRPC
server, reached over the intra-region private network, validated by node token) — the same
approach the industry converged on in e2b/AgentENV/CubeSandbox, and the shortest scheduling
path:

```
scheduler decides (in-memory state + a Postgres transaction deducting the committed
                   quantity and writing the command record)
  → direct call to noded.CreateSandbox (returns acceptance synchronously)
  → noded executes asynchronously, state changes are reported over Heartbeat
```

- **Connection directions close up**: the data plane (gateway/proxy → noded for
  exec/files/ports) requires intra-region private reachability (the regional proxy is
  deployed in the same domain as the nodes). Control plane → noded commands close up like
  this under the node's "outbound-only, zero inbound exposure" premise: on start noded
  establishes a **long-lived bidirectional gRPC stream (CommandChannel)** to the managed
  ingress layer, and the control plane multiplexes SandboxService calls onto that stream
  (request/response frames correlate by command_id) — the semantics are still a direct push
  (the control plane initiates and waits synchronously for a response), it is only the
  transport that rides on the node's outbound connection; the node token is validated when
  the stream is established, and the lifetime of the stream is the lifetime of that identity
- **Reliability does not come from pull, it comes from writing to the DB + reconciliation**:
  - The command is written to Postgres first (audit + the state machine's source of truth);
    the RPC is only the delivery mechanism
  - RPC timeout/failure → bounded retries; still failing → release the committed quantity
    and reschedule (see architecture D7)
  - noded deduplicates idempotently by command_id (retry-safe)
  - noded restarts → `SyncState` (pull the full desired state) to reconcile and redeliver
    lost commands
- The Heartbeat bidirectional stream's responsibilities narrow to: ↑ heartbeat / resource
  level / sandbox state / cache summary, ↓ lease confirmation (it no longer carries command
  notification)

The proto is in §4 (NodeService.SyncState carries restart reconciliation; SandboxService is
implemented by noded and called directly by the control plane as a client).

### 5.2 Lifecycle automation semantics ⚠️

**Runs forever by default** — no hard timeout. Reclamation is driven by the idle mechanism:

| idleTimeout | Behaviour |
|---|---|
| absent / null | idle detection off (default), the sandbox keeps running |
| `"0s"` | fires onIdle the moment activity ends (batch eval: `onIdle: kill`, gone as soon as it is done) |
| `"300s"` | fires onIdle after 5 minutes idle |

- **Idle determination** (local to noded, does not depend on the control plane): no exec
  session + no active port connection + no file API operation, sustained for idleTimeout;
  any activity resets the timer
- **Waking is platform default behaviour, not a configuration**: when the gateway/proxy
  receives an exec/port/file request for a PAUSED sandbox → it triggers a resume (sub-second
  for fc) → blocks until recovery and then forwards; concurrent wakes are deduplicated by
  the control plane per sandbox-id
- Lingering in PAUSED: **retained indefinitely by default** (we do not take it upon
  ourselves to reclaim a sandbox the user paused); note that PAUSED still occupies host RAM
  and scheduler committed quantity, so the capacity cost is borne by capacity planning. An
  administrator can opt into global reclamation (off by default); the real long-term answer
  is the P4 snapshot archive: PAUSED past a threshold → state goes to S3, freeing RAM → the
  next access restores it automatically
- Industry alignment: CubeSandbox v0.5 (on_timeout: pause/kill + transparent wake on the
  data plane) and e2b auto-pause/auto-resume are the same shape; we express "never" as null,
  avoiding the -1/0 magic-number overload

### 5.3 Exec routing ✅

```
client → gateway: Bearer key / sandbox token
gateway: look up the sandbox in the state store → nodeId → noded address (cached + invalidation subscription)
gateway → noded: gRPC (intra-region private network, node token validated)
noded → agent: vsock (fc main path; container tier over a unix socket, P5)
```

State semantics: PAUSED → triggers a transparent wake, and the request blocks until resume
(only past the wake deadline, 10s by default, does it return 502 + Retry-After);
unwakeable states such as PULLING/STOPPING → 409 SANDBOX_NOT_RUNNING.

## 6. bean-proxy (port reverse-proxy service) 📐

> **The entire section is unimplemented**; `cmd/bean-proxy` does not exist. The network
> stack it depends on is now built (network.md), so the remaining blocker is the proxy
> itself rather than the addressing beneath it.

### 6.0 Two different things want the same binary ⚠️

This section designs **port exposure**: a browser reaching a port inside a sandbox.
[GitHub #27](https://github.com/garysng/bean/issues/27) asks for something else that
would live in the same process -- moving **exec and file traffic** off the control
plane -- and conflating them has already produced one wrong plan, so the difference is
recorded here.

| | port exposure (this section) | data plane (#27) |
|---|---|---|
| Who calls | a browser, arbitrary HTTP | the SDK and CLI |
| Addressed by | `{sbxId}-{port}.{region}...` hostname | the existing REST paths |
| Terminates at | the sandbox's own IP:port | noded, which relays to the agent |
| Exists to | make a port reachable at all | keep bulk bytes out of the scheduler's process |

**The stronger reason for the second one is not load.** `SandboxService` puts
`DestroySandbox`, `SnapshotSandbox` and `CommitSandbox` in the same service as
`Exec`, `ReadFile` and `WriteFile`, behind one shared `--node-token`. So "let clients
reach noded directly" is not a routing change with a performance benefit -- it would
hand every caller the ability to destroy any sandbox on the node. A proxy holding that
token and forwarding **only** the data-plane methods is how the interface gets narrowed;
the byte path is the lesser gain.

e2b arrives at the same shape from the other direction: `packages/client-proxy` resolves
a sandbox to its node and forwards, and it carries `trafficAccessToken` and
`envdAccessToken` as *separate* credentials rather than one cluster secret
(`internal/proxy/proxy.go`). Their orchestrator also listens on its own proxy port
(5007) rather than exposing the control RPCs to clients.

**Whether noded needs authentication at all** depends on where it listens, and the
answer today is the same one dockerd gives: `cmd/noded/main.go` refuses to start on a
non-loopback address without a token, and requires nothing on loopback. Network position
substitutes for authentication, exactly as a unix socket does for the Docker daemon. That
is coherent as long as noded is not client-reachable -- which is the invariant a data
plane must not break.


A standalone stateless service, co-deployable with the gateway or scaled horizontally.

### 6.1 Domains and TLS 📐

- One wildcard certificate per region, `*.{region}.sandbox.<domain>` (ACME DNS-01 auto-renewal)
- Host rule: `{sandboxId}-{port}.{region}.sandbox.<domain>`, where sandboxId is the short ID (e.g. base32 of the value with the `sbx-` prefix stripped)
- HTTP + WebSocket pass-through; non-HTTP protocols are not supported yet (a TCP over TLS SNI scheme is held in reserve)

### 6.2 Routing and data plane (aligned with e2b: the reverse proxy connects straight to the sandbox IP) 📐

```
browser → {sbxId}-{port}.{region}.sandbox.<domain> (DNS lands directly on that region's proxy)
        → regional proxy: parse Host → authenticate (6.3)
        → route lookup: state store → sandbox → nodeId → noded address
          (local LRU cache 30s + invalidation pushed over heartbeat; PAUSED → trigger a
           transparent wake, then re-route)
        → HTTP reverse proxy → sandbox-proxy embedded in noded (node-side reverse proxy)
        → direct to sandbox IP:port (fc tier tap IP / container tier veth IP, routed inside the node)
```

- **Two HTTP reverse-proxy hops, with the last one connecting straight to the sandbox IP** —
  the same as e2b/CubeSandbox; it does not go through an agent tunnel (saving one userspace
  copy and the vsock serialisation, which is what matters for performance on a high-traffic
  port)
- The agent's `ForwardPort` is retained as a fallback path (for future scenarios such as
  localhost-only services)
- WebSocket upgrades pass through naturally; connection-level timeout (>620s, to duck under
  the upstream LB), per-sandbox concurrency/bandwidth limits; connection liveness on the
  proxy side feeds the idle determination (lifecycle)
- The sandbox-proxy on the noded side also performs a second layer of validation beyond
  nftables (only exposed ports are let through)

### 6.3 Port authentication 📐

- `auth=public`: anyone holding the URL can access it (for internal demos)
- `auth=token` (default): requires `?bean_token=<sandbox JWT>` or a Cookie; the proxy
  validates the JWT signature and the sandbox-id match, then sets a Cookie (1h), so
  subsequent requests need no query parameter
- The proxy injects an `X-Bean-Sandbox-Id` header and strips any inbound header of the same name

### 6.4 Lifecycle interlock 📐

- Sandbox destroyed (including onIdle=kill) → gateway revokes the port record → proxy cache
  invalidation is pushed → subsequent requests 404
- PAUSED → triggers a transparent wake and blocks the forwarding; only on wake timeout
  (10s by default) does it return 502 + Retry-After

## 7. Quota and Rate Limiting ⚠️

| Layer | Mechanism |
|---|---|
| API key | concurrent sandbox count, total CPU/mem, total volume capacity/count, prewarms per day — Postgres counters + transactional validation at creation |
| Request rate limiting | gateway token bucket: global QPS + per-key QPS (exec and create in separate pools) |
| Exec output | maxOutputBytes truncation; per-connection bandwidth limiting on the WS stream |
| proxy | per-sandbox concurrent connections, bandwidth limiting (to stop abuse as a tunnelling proxy) |

## 8. Observability ⚠️

- **Platform metrics** (Prometheus): creation latency quantiles, sandbox count per state,
  node capacity level, image cache hit rate, exec QPS, proxy connection count
- **Audit log**: every write operation (create/destroy/exec summary) lands in Postgres and
  is archived to S3 periodically
- **Sandbox logs**: the agent's ring buffer (8 MiB by default) is queryable live; on
  destruction the terminal-state log plus the full stdout/stderr is archived to S3 through a
  presigned URL (path: `s3://<bucket>/logs/{sandboxId}/`)
- **trace**: request_id is propagated end to end, with OTel instrumentation (gateway, noded, agent)

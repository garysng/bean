# SDK and CLI Design

> 中文版:[zh/sdk-cli-design.md](zh/sdk-cli-design.md)

> The status-marker convention is defined in [architecture.md](architecture.md) §0.

## 1. Overall Strategy ⚠️

- **REST is the only protocol for clients** ✅ (gRPC is not exposed outward). WebSocket streaming 📐 unimplemented
- **Code generation** 📐 **unimplemented**: the `proto → OpenAPI spec → per-language client`
  pipeline does not exist, and no OpenAPI spec has been published. The Python SDK is
  **hand-written httpx**, and the CLI is hand-written Go.
  This does not necessarily have to change — a hand-written façade API currently fits usage
  better than a generated layer — but it should not be written up as an accomplished fact
- Versioning policy 📐: the SDK is unreleased, and the server does not return `X-Bean-Api-Version` either

## 2. Python SDK (the primary SDK, eval/rollout side) ⚠️

**What is actually covered today** (`sdk/python/bean/__init__.py`, a single hand-written file):

```
✅ BeanClient / sandboxes.create|get|list / snapshots.list|get|delete
✅ Sandbox.exec / write_file / read_file / ls / pause / resume / kill
✅ Sandbox.snapshot(name, labels, keep_running, include_memory, base)
✅ Sandbox.commit(tag) / events() / refresh() / context manager
✅ Snapshot.resumes_guest / base_id / chain_depth
✅ images.list|status|prewarm|prewarm_status
✅ Error tiering: BeanAPIError / BeanConnectionError

📐 Unimplemented: pty, exec_stream, ports.expose, volumes, fork, start,
   set_lifecycle, files.upload_dir/download_dir, bean.aio (the async stack),
   bean.batch.run_batch
```

If an API from that "unimplemented" list appears below, it is **design intent** and not a
current capability — in particular §2.3 calls `run_batch` "the first-class entry point for
SWE-bench scenarios", and it does not exist.

Package name: `bean-sdk` (import `bean`). Dependency: `httpx`.
(The originally planned sync+async dual stack and `websockets` have not been brought in yet.)

### 2.1 Core interface ⚠️

```python
from bean import Sandbox, BeanClient

client = BeanClient(api_key="bk_...", base_url="https://api.example.com")
# Environment-variable fallback: BEAN_API_KEY / BEAN_BASE_URL

# —— lifecycle ——
sbx = client.sandboxes.create(
    image="registry.example.com/swebench/django-12345:latest",
    cpu=2, memory_mib=4096, disk_mib=20480,
    env={"PYTHONUNBUFFERED": "1"},
    cmd=None, auto_start_cmd=False,   # the original entrypoint is managed; sbx.start() starts it manually
    network_policy="egress-only",
    idle_timeout="300s", on_idle="pause",   # default = run forever; eval uses ("0s","kill")
    labels={"eval-run": "r0731"},
    volumes=[{"volume": vol.id, "mount_path": "/workspace"}],   # optional
)                                     # blocks until RUNNING (polling/long-polling internally), wait=False available
# Note: no isolation parameter — the runtime tier is assigned by the platform (fc by default)

sbx = client.sandboxes.get("sbx_...")
for s in client.sandboxes.list(labels={"eval-run": "r0731"}): ...
sbx.set_lifecycle(idle_timeout="600s", on_idle="kill")
sbx.kill()

# —— execution ——
# str → ["/bin/sh","-c",...] (errors if the image has no sh); a list is executed as is
r = sbx.exec("python -m pytest tests/ -x", cwd="/workspace", timeout=600)
r.exit_code, r.stdout, r.stderr, r.truncated, r.duration_ms

# streaming
for ev in sbx.exec_stream(["bash", "-lc", "make test"]):
    if ev.type == "stdout": print(ev.data, end="")

# PTY (interactive rollout)
with sbx.pty(cols=120, rows=40) as term:
    term.send("ls -la\n")
    data = term.read(timeout=2)
    term.resize(80, 24)

# —— files ——
sbx.files.write("/workspace/patch.diff", patch_text)      # str|bytes|file-like
content = sbx.files.read("/workspace/report.json")
sbx.files.upload_dir("./repo_snapshot", "/workspace")      # local directory → tar stream
sbx.files.download_dir("/workspace/logs", "./out")
sbx.files.ls("/workspace")

# —— ports ——
url = sbx.ports.expose(8888, auth="token")                 # → https://...

# —— processes / logs / events ——
sbx.start()                                                # start the original entrypoint
for line in sbx.logs(follow=True): ...
for ev in client.events.subscribe(labels={"eval-run": "r0731"}): ...   # live over SSE
sbx.events()                                               # history

# —— volume (an independent resource: image = environment, volume = data, persists across
#    sandboxes; first pass is shared-fs only) ——
vol = client.volumes.create(name="alice-ws", type="shared-fs", quota_mib=51200)
client.volumes.list(labels={...}); vol.delete()

# —— snapshot ——
sbx.pause(); sbx.resume()
snap = sbx.snapshot(name="after-setup", keep_running=True)
client.snapshots.list(); snap.delete()
sbx2 = client.sandboxes.create(snapshot=snap.id)           # rebuild from a persistent snapshot
children = sbx.fork(count=8)                               # separate API: instantaneous CoW clone fan-out
```

### 2.2 The async twin 📐

`bean.aio` mirrors the same interface shape (`AsyncBeanClient` / `AsyncSandbox`), sharing the
generated layer and model definitions. The primary form for high-concurrency rollout scenarios.

### 2.3 eval batch helper 📐

> `bean.batch.run_batch` **does not exist**. What follows is design intent.


```python
from bean.batch import run_batch

results = run_batch(
    client,
    tasks=[{"image": f"swebench/{t}", "cmd": ..., "files": {...}} for t in tasks],
    concurrency=200,                # client-side concurrency window
    create_batch_size=100,          # uses batchCreate underneath
    on_result=lambda t, r: ...,     # streaming collection
    retry_lost=2,                   # automatic retry on LOST/NO_CAPACITY
)
```

What it wraps: batching for batchCreate (injecting lifecycle=("0s","kill") by default so
things go away as soon as they are done), a concurrency semaphore, event-driven collection
(a WS subscription replacing polling), LOST rebuild, and collecting artifacts straight from
S3 URLs. The first-class entry point for SWE-bench scenarios.

### 2.4 Behavioural conventions ⚠️

- Connection reuse: an httpx connection pool per client; one WS per session
- Retries: idempotent GET/DELETE retry automatically (exponential backoff + jitter); create retries safely via Idempotency-Key
- Timeout tiering: connect 5s / read 30s by default / exec follows the business timeout + 10s
- Error mapping: `BeanAPIError` as the base class, deriving `SandboxNotFound`, `QuotaExceeded`, `NoCapacity`… by code
- `Sandbox` supports the context manager protocol: `with client.sandboxes.create(...) as sbx:` destroys on exit

## 3. TypeScript SDK 📐

> **Does not exist.** `sdk/` contains only `python/`.

The original design:

Package name: `@bean/sdk` (an npm org scope, dodging the taken bare name). Runtime: Node 18+ /
browser (in the browser only the sandbox-token mode, no API key handed out).

```typescript
import { BeanClient } from "@bean/sdk";

const client = new BeanClient({ apiKey: process.env.BEAN_API_KEY });
const sbx = await client.sandboxes.create({ image: "...", cpu: 2, memoryMiB: 4096 });

const r = await sbx.exec("npm test", { cwd: "/app", timeoutSeconds: 300 });

const stream = sbx.execStream(["bash", "-lc", "npm run dev"]);
for await (const ev of stream) { ... }

await sbx.files.write("/app/input.json", JSON.stringify(data));
await sbx.kill();
```

- One-to-one semantic correspondence with Python (method names camelCased), sharing the example matrix in the docs
- WS uses the native WebSocket (browser) / `ws` (Node), and the PTY front end can be wired straight to xterm.js

## 4. CLI (`bean`, Go) ✅

Same repo and same release cadence as noded; cobra framework; configuration at
`~/.config/bean/config.yaml` (multi-profile: endpoint + key).

### 4.1 Command surface ⚠️

What is actually implemented (`bean --help`):

```
run --image IMG | --snapshot SNAP     ls    exec SBX -- CMD...
kill SBX [--force]    pause SBX | resume SBX    logs SBX [--tail N]
cp LOCAL sbx:SBX:/path | and the reverse        events SBX | events -f
build --tag REF [--file Dockerfile] [CONTEXT]
commit SBX --tag REF
snapshot create SBX [--name N] [--no-keep-running] [--no-memory] [--base SNAP]
snapshot ls [--label k=v] | snapshot rm SNAP
image ls | image status REF | image prewarm REF... [--replicas N]
output: --json / --quiet    exit codes: 0 / 64 / 69 / 70 / 125
```


**Shipped** (`cli/cli.go`, matching the code):

```
bean run --image IMG | --snapshot SNAP
         [--label k=v] [--idle-timeout 300s] [--on-idle pause|kill]
bean ls   [--label k=v]
bean exec SBX -- CMD...
bean cp   ./local sbx:SBX:/path  |  sbx:SBX:/path ./local
bean logs SBX [--tail N]
bean events SBX             # history; `-f [SBX] [--label k=v]` follows the live stream (SSE)
bean kill SBX [--force]
bean pause SBX / bean resume SBX
bean build  --tag REF [--file Dockerfile] [CONTEXT]   # build an image on the platform
bean commit SBX --tag REF                             # freeze the filesystem into an image
bean snapshot create SBX [--name N] [--no-keep-running]
bean snapshot ls [--label k=v] / bean snapshot rm SNAP
bean image ls | image status REF | image prewarm REF... [--replicas N]
bean version
```

**Not shipped** (kept as design intent): `attach`, `start`, `volume *`, `fork`,
`port expose`, `config`, batch `kill --label`, interactive PTY (`-i/-t`).

**Why there is no `bean node ls`**: a node is a scheduling object of the platform, not a
user-facing concept. Neither e2b, Modal nor Daytona exposes "which machine my sandbox landed
on" — once exposed, users depend on it, and the scheduler can no longer migrate freely.
`/v1/nodes` and drain are kept as **operator APIs and stay out of the CLI**.
By the same logic `prewarm`'s parameter is `--replicas` (replica count) rather than `--nodes`
(machine count).

### 4.2 Output conventions ✅

- Human-readable table by default (aligned with `tabwriter`); `--json` emits structured JSON;
  `--quiet` emits only identifiers (for scripts grabbing an id)
- The header is generated from field names, so it is impossible for a new field to appear in
  the data but not in the header
- Under `--json` progress notices (such as "uploading N KiB") are suppressed — mixed into the
  stream they would break parsing
- Exit codes distinguish "should the script retry":
  `0` success, `64` does not exist, `69` network unreachable / no capacity (**retrying may
  help**), `70` the platform explicitly refused, `125` usage error. `bean exec` passes the
  remote exit code through
- Environment variables: `BEAN_BASE_URL`, `BEAN_API_KEY`, `BEAN_TIMEOUT` (a Go duration, 15m by default)

**Not shipped**: automatic TTY colouring, spinner and phase display for long operations, `--no-wait`.

### 4.3 Interactive mode 📐

`bean run -it IMAGE -- bash` / `bean exec -it SBX -- bash`:

- Local terminal in raw mode wired to WS PTY frames, SIGWINCH → resize frame, Ctrl-P Ctrl-Q
  to detach (the session is kept for 60s so `bean attach SBX` can reconnect)

## 5. Documentation and examples 📐

- Publish the OpenAPI spec + a hosted API reference
- A quickstart trio: five minutes with the CLI, a Python eval batch example (a mini SWE-bench reproduction), a TS web demo (an xterm.js terminal)
- SDK examples plus an e2b migration mapping table (`e2b.Sandbox.create` → the `bean` equivalent), lowering the switching cost for existing users

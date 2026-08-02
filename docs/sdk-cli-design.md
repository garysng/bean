# SDK 与 CLI 设计

## 1. 总体策略

- **REST 为客户端唯一协议**（含 WebSocket 流式），不向外暴露 gRPC——降低鉴权/网络要求，浏览器可用
- **代码生成**：`proto → OpenAPI spec →` 各语言底层 client 自动生成；顶层手写符合语言习惯的门面 API（生成层不对用户暴露）
- 版本策略：SDK 版本与 API `v1` 解耦，语义化版本;server 返回 `X-Bean-Api-Version`

## 2. Python SDK（主 SDK，eval/rollout 侧）

包名：`bean-sdk`（import `bean`）。依赖最小化：`httpx`（sync+async 双栈）、`websockets`。

### 2.1 核心接口

```python
from bean import Sandbox, BeanClient

client = BeanClient(api_key="bk_...", base_url="https://api.example.com")
# 环境变量兜底：BEAN_API_KEY / BEAN_BASE_URL

# —— 生命周期 ——
sbx = client.sandboxes.create(
    image="registry.example.com/swebench/django-12345:latest",
    cpu=2, memory_mib=4096, disk_mib=20480,
    env={"PYTHONUNBUFFERED": "1"},
    cmd=None, auto_start_cmd=False,   # 原 entrypoint 托管;sbx.start() 手动拉起
    network_policy="egress-only",
    idle_timeout="300s", on_idle="pause",   # 缺省=一直运行;eval 用 ("0s","kill")
    labels={"eval-run": "r0731"},
    volumes=[{"volume": vol.id, "mount_path": "/workspace"}],   # 可选
)                                     # 阻塞至 RUNNING（内部轮询/长轮询），可 wait=False
# 注：无 isolation 参数——runtime 档位由平台自动分配（fc 默认）

sbx = client.sandboxes.get("sbx_...")
for s in client.sandboxes.list(labels={"eval-run": "r0731"}): ...
sbx.set_lifecycle(idle_timeout="600s", on_idle="kill")
sbx.kill()

# —— 执行 ——
# str → ["/bin/sh","-c",...]（镜像无 sh 时报错）;list 原样执行
r = sbx.exec("python -m pytest tests/ -x", cwd="/workspace", timeout=600)
r.exit_code, r.stdout, r.stderr, r.truncated, r.duration_ms

# 流式
for ev in sbx.exec_stream(["bash", "-lc", "make test"]):
    if ev.type == "stdout": print(ev.data, end="")

# PTY（交互式 rollout）
with sbx.pty(cols=120, rows=40) as term:
    term.send("ls -la\n")
    data = term.read(timeout=2)
    term.resize(80, 24)

# —— 文件 ——
sbx.files.write("/workspace/patch.diff", patch_text)      # str|bytes|file-like
content = sbx.files.read("/workspace/report.json")
sbx.files.upload_dir("./repo_snapshot", "/workspace")      # 本地目录 → tar 流
sbx.files.download_dir("/workspace/logs", "./out")
sbx.files.ls("/workspace")

# —— 端口 ——
url = sbx.ports.expose(8888, auth="token")                 # → https://...

# —— 进程 / 日志 / 事件 ——
sbx.start()                                                # 拉起原 entrypoint
for line in sbx.logs(follow=True): ...
for ev in client.events.subscribe(labels={"eval-run": "r0731"}): ...   # SSE 实时
sbx.events()                                               # 历史

# —— volume（独立资源：镜像=环境,卷=数据,跨 sandbox 留存;首期仅 shared-fs）——
vol = client.volumes.create(name="alice-ws", type="shared-fs", quota_mib=51200)
client.volumes.list(labels={...}); vol.delete()

# —— snapshot ——
sbx.pause(); sbx.resume()
snap = sbx.snapshot(name="after-setup", keep_running=True)
client.snapshots.list(); snap.delete()
sbx2 = client.sandboxes.create(snapshot=snap.id)           # 从持久快照重建
children = sbx.fork(count=8)                               # 独立 API：瞬时 CoW 克隆 fan-out
```

### 2.2 async 双形态

`bean.aio` 镜像同构接口（`AsyncBeanClient` / `AsyncSandbox`），共享生成层与模型定义。rollout 高并发场景的主形态。

### 2.3 eval 批量 helper

```python
from bean.batch import run_batch

results = run_batch(
    client,
    tasks=[{"image": f"swebench/{t}", "cmd": ..., "files": {...}} for t in tasks],
    concurrency=200,                # 客户端并发窗口
    create_batch_size=100,          # 底层用 batchCreate
    on_result=lambda t, r: ...,     # 流式回收
    retry_lost=2,                   # LOST/NO_CAPACITY 自动重试
)
```

封装内容：batchCreate 分批（默认注入 lifecycle=("0s","kill") 用完即走）、并发信号量、
事件驱动回收（WS 订阅替代轮询）、LOST 重建、产物直收 S3 URL。SWE-bench 场景的一等入口。

### 2.4 行为约定

- 连接复用：单 client 内 httpx 连接池;WS 每会话一条
- 重试：幂等 GET/DELETE 自动重试（指数退避 + jitter）;create 用 Idempotency-Key 安全重试
- 超时分层:connect 5s / read 默认 30s / exec 跟随业务 timeout+10s
- 错误映射:`BeanAPIError` 基类,按 code 派生 `SandboxNotFound`、`QuotaExceeded`、`NoCapacity`…
- `Sandbox` 支持 context manager:`with client.sandboxes.create(...) as sbx:` 退出即销毁

## 3. TypeScript SDK

包名:`@bean/sdk`(npm org scope,避开被占的裸名)。运行时:Node 18+ / 浏览器（浏览器仅 sandbox token 模式，不放 API key）。

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

- 与 Python 语义一一对应（方法名 camelCase 化），文档共享示例矩阵
- WS 用原生 WebSocket（浏览器）/ `ws`（Node），PTY 前端可直接对接 xterm.js

## 4. CLI（`bean`，Go）

与 noded 同 repo 同发版；cobra 框架;配置 `~/.config/bean/config.yaml`（多 profile：endpoint + key）。

### 4.1 命令面

**已实装**（`cli/cli.go`,与代码一致）：

```
bean run --image IMG | --snapshot SNAP
         [--label k=v] [--idle-timeout 300s] [--on-idle pause|kill]
bean ls   [--label k=v]
bean exec SBX -- CMD...
bean cp   ./local sbx:SBX:/path  |  sbx:SBX:/path ./local
bean logs SBX [--tail N]
bean events SBX             # 历史;`-f [SBX] [--label k=v]` 跟随实时流(SSE)
bean kill SBX [--force]
bean pause SBX / bean resume SBX
bean build  --tag REF [--file Dockerfile] [CONTEXT]   # 平台上构建镜像
bean commit SBX --tag REF                             # 把文件系统固化成镜像
bean snapshot create SBX [--name N] [--no-keep-running]
bean snapshot ls [--label k=v] / bean snapshot rm SNAP
bean image ls | image status REF | image prewarm REF... [--replicas N]
bean version
```

**未实装**（保留为设计意图）：`attach`、`start`、`volume *`、`fork`、
`port expose`、`config`、批量 `kill --label`、交互 PTY（`-i/-t`）。

**为什么没有 `bean node ls`**：节点是平台的调度对象,不是用户的概念。
e2b / Modal / Daytona 都不向用户暴露「我的 sandbox 落在哪台机器上」——
一旦暴露,用户就会依赖它,调度器也就不能再自由迁移了。
`/v1/nodes` 与 drain 保留为**运维 API,不进 CLI**。
同理 `prewarm` 的参数是 `--replicas`(副本数)而不是 `--nodes`(机器数)。

### 4.2 输出约定

- 默认人类可读 table(`tabwriter` 对齐);`--json` 输出结构化 JSON;
  `--quiet` 只输出标识符(脚本取 id 用)
- 表头由字段名生成,所以新增字段不可能只出现在数据里而不出现在表头
- `--json` 下进度提示(如「uploading N KiB」)不输出 —— 混进流里会破坏解析
- 退出码按「脚本该不该重试」区分:
  `0` 成功、`64` 不存在、`69` 网络不可达/无容量(**重试可能有用**)、
  `70` 平台明确拒绝、`125` 用法错误。`bean exec` 透传远端 exit code
- 环境变量：`BEAN_BASE_URL`、`BEAN_API_KEY`、`BEAN_TIMEOUT`（Go duration,默认 15m）

**未实装**：TTY 自动着色、长操作 spinner 与阶段展示、`--no-wait`。

### 4.3 交互模式

`bean run -it IMAGE -- bash` / `bean exec -it SBX -- bash`：

- 本地终端 raw mode + WS PTY 帧对接，SIGWINCH → resize 帧，Ctrl-P Ctrl-Q detach（会话保留 60s 可 `bean attach SBX` 重连）

## 5. 文档与示例

- OpenAPI spec 发布 + 托管 API reference
- Quickstart 三件套：CLI 五分钟、Python eval 批量示例（SWE-bench 迷你复现）、TS Web demo（xterm.js 终端）
- SDK 示例与 e2b 迁移对照表（`e2b.Sandbox.create` → `bean` 等价写法），降低已有用户切换成本

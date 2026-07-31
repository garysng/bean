# API 与 Proxy 服务设计

> 对应组件：`bean-api`（api-gateway）、`bean-proxy`（端口反代）。
> 术语与状态机见 [architecture.md](architecture.md)。

## 1. 设计原则

- **REST 对外，gRPC 对内**：SDK/CLI 走 REST（+WebSocket 流式），control ↔ noded ↔ agent 走 gRPC
- **幂等**：所有创建类接口支持 `Idempotency-Key` 头，state store 唯一约束去重
- **大对象不进 gateway**：文件上传/下载超过阈值（默认 4 MiB）一律 presigned URL 直连 S3 或 noded 直连
- **proto 是 single source of truth**：REST DTO 由 proto 派生，OpenAPI spec 生成

## 2. 鉴权

### 2.1 API Key

- `Authorization: Bearer bk_<keyid>_<secret>`
- key 哈希存 Postgres；附带配额（并发 sandbox 数、CPU/mem 总量、镜像 prewarm 权限）
- 首期单租户多 key；租户/RBAC 字段预留（key 表带 `tenant_id`）

### 2.2 Sandbox 级短时凭证

- 创建 sandbox 时 gateway 签发 **sandbox token**（JWT，绑定 sandbox-id，TTL = sandbox timeout）
- 用途：proxy 访问受保护端口、WebSocket exec 重连，避免长期 API key 下发到浏览器/弱环境

### 2.3 S3 Presigned URL

- 由 control plane 统一签发（唯一持有 S3 长期凭证的位置）
- 场景：文件上传/下载、eval 产物上报、snapshot 读写
- TTL 默认 15 min；上传 URL 绑定 content-length-range

## 3. REST API 详细定义

Base: `https://api.<domain>/v1`。错误响应统一：

```json
{ "error": { "code": "SANDBOX_NOT_FOUND", "message": "...", "details": {} } }
```

| HTTP | code 示例 |
|---|---|
| 400 | INVALID_ARGUMENT, IMAGE_REF_INVALID |
| 401/403 | UNAUTHENTICATED, PERMISSION_DENIED, QUOTA_EXCEEDED |
| 404 | SANDBOX_NOT_FOUND, SNAPSHOT_NOT_FOUND |
| 409 | SANDBOX_NOT_RUNNING, IDEMPOTENCY_CONFLICT |
| 429 | RATE_LIMITED |
| 500/503 | INTERNAL, NO_CAPACITY, NODE_LOST |

### 3.1 Sandboxes

```
POST /sandboxes
{
  "image": "registry.example.com/swebench/django__django-12345:latest",
  "isolation": "standard",              // none|standard|strong，默认 standard
  "resources": { "cpu": 2, "memoryMiB": 4096, "gpu": 0 },
  "env": { "FOO": "bar" },
  "cmd": null,                          // 覆盖镜像 CMD；null=保留原 entrypoint（由 agent 托管拉起）
  "autoStartCmd": false,                // true 则创建后立即拉起原 entrypoint
  "timeoutSeconds": 1800,               // 到期自动销毁；可续期
  "labels": { "eval-run": "swebench-0731", "task": "django-12345" },
  "networkPolicy": "egress-only"        // egress-only|none|allow-list（预留）
}
→ 201 { "sandbox": { "id": "sbx_...", "state": "PENDING", ... }, "token": "<sandbox JWT>" }
```

```
GET    /sandboxes/{id}                       → sandbox 详情（state、nodeId、createdAt、expiresAt、endpoints）
GET    /sandboxes?label=eval-run%3Dswebench-0731&state=RUNNING&pageToken=&pageSize=100
DELETE /sandboxes/{id}                       → 202，异步销毁；?force=true 跳过 graceful
POST   /sandboxes/{id}/timeout   { "timeoutSeconds": 3600 }   → 续期（从 now 起算）
POST   /sandboxes/{id}/pause                 → 202 → PAUSED
POST   /sandboxes/{id}/resume                → 202 → RUNNING
POST   /sandboxes/{id}/snapshot  { "name": "after-setup" }    → 202 { "snapshotId": "snap_..." }
```

批量（eval 场景高频）：

```
POST /sandboxes:batchCreate   { "requests": [ ... ≤100 ... ] }
→ 207 逐项 { index, sandbox | error }     // 部分成功语义
DELETE /sandboxes?label=eval-run%3Dswebench-0731    → 批量销毁，202 + 任务计数
```

### 3.2 Exec

```
POST /sandboxes/{id}/exec          // 同步，适合 eval 单条命令
{
  "cmd": ["python", "-m", "pytest", "tests/"],
  "cwd": "/workspace", "env": {}, "timeoutSeconds": 600,
  "stdin": "<base64, 可选>",
  "maxOutputBytes": 1048576          // 超出截断并置 truncated=true
}
→ 200 { "exitCode": 1, "stdout": "...", "stderr": "...", "truncated": false, "durationMs": 42150 }
```

```
WS /sandboxes/{id}/exec/ws?pty=true&cols=120&rows=40
```

WebSocket 子协议（JSON 帧）：

```
C→S: {"type":"start","cmd":["bash"],"pty":true,"env":{}}
C→S: {"type":"stdin","data":"<base64>"}
C→S: {"type":"resize","cols":120,"rows":40}
C→S: {"type":"signal","signal":"SIGINT"}
S→C: {"type":"stdout"|"stderr","data":"<base64>"}
S→C: {"type":"exit","exitCode":0}
```

链路：client → gateway（升级）→ noded gRPC stream → agent。gateway 只做帧透传与鉴权。

### 3.3 Files

```
PUT  /sandboxes/{id}/files?path=/workspace/patch.diff     // body ≤4MiB 直传
     ?mode=0644&mkdirs=true
GET  /sandboxes/{id}/files?path=/workspace/report.json    // ≤4MiB 直回
GET  /sandboxes/{id}/files/ls?path=/workspace             → [{name,size,mode,mtime,isDir}]
POST /sandboxes/{id}/files:uploadUrl   {"path": "...", "sizeBytes": 123456789}
     → { "url": "<presigned PUT>", "commit": "/files:commitUpload?token=..." }   // 大文件两段式
POST /sandboxes/{id}/files:downloadUrl {"path": "..."}
     → { "url": "<presigned GET>" }    // noded 把文件推 S3 暂存后签 URL
DELETE /sandboxes/{id}/files?path=...
```

### 3.4 Ports

```
POST /sandboxes/{id}/ports    { "port": 8888, "auth": "token" }   // token|public
→ { "url": "https://sbx-abc123-8888.sandbox.<domain>", "expiresAt": "..." }
GET    /sandboxes/{id}/ports
DELETE /sandboxes/{id}/ports/{port}
```

### 3.5 Images

```
POST /images/prewarm   { "refs": ["img:a", "img:b"], "targetNodes": 10, "priority": "high" }
→ { "jobId": "pw_..." }
GET  /images/prewarm/{jobId}      → 各镜像 × 节点就绪矩阵摘要
GET  /images/{ref}/status         → { "blobReady": true, "cachedNodes": 7, "sizeBytes": ..., "format": "nydus" }
```

### 3.6 Snapshots

```
GET    /snapshots?label=...            → 列表（id、srcSandboxId、sizeBytes、state）
GET    /snapshots/{id}
DELETE /snapshots/{id}
POST   /sandboxes    { "snapshot": "snap_...", ... }     // 从 snapshot 创建（代替 image 字段）
```

### 3.7 Logs / 可观测

```
GET /sandboxes/{id}/logs?follow=false&tailLines=1000    // agent 环形缓冲 + S3 归档
GET /nodes                                              // 运维面：节点列表、容量、能力
GET /metrics                                            // Prometheus 格式（平台自身指标）
```

## 4. 内部 gRPC proto 草案

```protobuf
// proto/bean/node/v1/node.proto —— control plane ↔ noded
service NodeService {
  rpc Register(RegisterRequest) returns (RegisterResponse);        // 能力/资源画像上报
  rpc Heartbeat(stream HeartbeatRequest) returns (stream HeartbeatResponse);
      // 双向流：↑ 心跳+资源水位+sandbox 状态摘要+镜像缓存清单摘要（bloom/hash）
      // ↓ 租约确认 + 待执行指令通知（推拉结合，详见 5.1）
}

service SandboxService {                                           // control → noded
  rpc CreateSandbox(CreateSandboxRequest) returns (CreateSandboxResponse);
  rpc DestroySandbox(DestroySandboxRequest) returns (DestroySandboxResponse);
  rpc PauseSandbox(PauseSandboxRequest) returns (PauseSandboxResponse);
  rpc ResumeSandbox(ResumeSandboxRequest) returns (ResumeSandboxResponse);
  rpc SnapshotSandbox(SnapshotSandboxRequest) returns (SnapshotSandboxResponse);
  rpc PrewarmImage(PrewarmImageRequest) returns (PrewarmImageResponse);
  // Exec/File/Port 由 gateway 直连 noded 转发（携带 sandbox-id 路由头）：
  rpc Exec(ExecRequest) returns (ExecResponse);
  rpc StreamExec(stream StreamExecFrame) returns (stream StreamExecFrame);
  rpc ReadFile(ReadFileRequest) returns (stream FileChunk);
  rpc WriteFile(stream WriteFileFrame) returns (WriteFileResponse);
  rpc ListDir(ListDirRequest) returns (ListDirResponse);
}

// proto/bean/agent/v1/agent.proto —— noded ↔ bean-agent（unix socket / 未来 vsock）
service AgentService {
  rpc Exec(ExecRequest) returns (ExecResponse);
  rpc StreamExec(stream StreamExecFrame) returns (stream StreamExecFrame);
  rpc ReadFile(ReadFileRequest) returns (stream FileChunk);
  rpc WriteFile(stream WriteFileFrame) returns (WriteFileResponse);
  rpc ListDir(ListDirRequest) returns (ListDirResponse);
  rpc StartUserProcess(StartUserProcessRequest) returns (StartUserProcessResponse);
  rpc ForwardPort(stream PortFrame) returns (stream PortFrame);    // proxy 数据面
  rpc GetLogs(GetLogsRequest) returns (stream LogChunk);
  rpc Health(HealthRequest) returns (HealthResponse);
}
```

关键消息字段约定：

- 所有请求带 `request_id`（贯穿链路，日志关联）
- `CreateSandboxRequest` 含完整 `SandboxSpec`（image ref、resources、isolation、
  network、agent 注入参数、S3 产物 presigned URL 束）
- `ExecRequest/StreamExecFrame` 在 SandboxService 与 AgentService 中共享 message
  定义（`proto/bean/common/v1/exec.proto`），noded 纯透传

## 5. 控制流细节

### 5.1 指令下发模型

Heartbeat 双向流做**通知**，SandboxService 做**执行**：

1. scheduler 决策后把指令写 state store（`commands` 表，状态 PENDING）
2. 经 Heartbeat 下行帧通知 noded「有新指令」
3. noded 回调 `SandboxService`（由 control plane 反向暴露？——否）

修正：为避免 noded 需要可入方向连通性（云 VM 常无固定入口），**SandboxService 由 noded 侧实现、control plane 作为 client 调用**仅在 control 可直连 noded 时用；默认路径是 noded 从 Heartbeat 下行帧拿到指令 ID 后，主动 `PullCommands` + 上报结果：

```protobuf
service NodeService {
  rpc PullCommands(PullCommandsRequest) returns (PullCommandsResponse);
  rpc ReportCommandResult(ReportCommandResultRequest) returns (ReportCommandResultResponse);
}
```

- **数据面**（exec/文件/端口）要求 gateway/proxy → noded 直连（内网可达是部署前提）；
  控制面指令走拉模式，兼容 NAT 后的节点
- 指令幂等：noded 按 command_id 去重；重启后 PullCommands 全量对账

### 5.2 Exec 路由

```
client → gateway：Bearer key / sandbox token
gateway：state store 查 sandbox → nodeId → noded 地址（缓存 + 失效订阅）
gateway → noded：gRPC（mTLS，内网）
noded → agent：unix socket
```

sandbox 处于 PAUSED/PULLING 等非 RUNNING 态 → 409 SANDBOX_NOT_RUNNING。

## 6. bean-proxy（端口反代服务）

独立无状态服务，可与 gateway 合部或水平扩展。

### 6.1 域名与 TLS

- 通配证书 `*.sandbox.<domain>`（ACME DNS-01 自动续期）
- Host 规则：`{sandboxId}-{port}.sandbox.<domain>`，sandboxId 用短 ID（如 `sbx-` 去前缀后的 base32）
- HTTP + WebSocket 透传；非 HTTP 协议暂不支持（预留 TCP over TLS SNI 方案）

### 6.2 路由与数据面

```
浏览器 → proxy（解析 Host → sandboxId+port）
       → 鉴权（见 6.3）
       → 路由查询：state store 查 sandbox → nodeId → noded 地址（本地 LRU 缓存 30s + 心跳失效推送）
       → noded HTTP/1.1|h2c 反代 → noded 内部 dial agent ForwardPort 流 → sandbox 内 localhost:port
```

- noded 为每个已暴露端口维护一个本地 listener 是**不必要的**：proxy → noded 用一条
  gRPC `ForwardPort` 双向流每连接一路，noded 转发到 agent，agent 在 netns 内 dial
- 连接级超时/最大并发 per sandbox 可配；断流自动清理

### 6.3 端口鉴权

- `auth=public`：任何持有 URL 者可访问（内部演示用）
- `auth=token`（默认）：要求 `?bean_token=<sandbox JWT>` 或 Cookie；proxy 校验 JWT
  签名与 sandbox-id 匹配后种 Cookie（1h），后续请求免 query
- proxy 注入 `X-Bean-Sandbox-Id` 头，剥离入站的同名头

### 6.4 生命周期联动

- sandbox 销毁/超时 → gateway 撤销端口记录 → proxy 缓存失效推送 → 后续请求 404
- PAUSED 状态 → 502 + Retry-After（resume 后自动恢复）

## 7. 配额与限流

| 层 | 机制 |
|---|---|
| API key | 并发 sandbox 数、总 CPU/mem、prewarm 次数/天 —— Postgres 计数 + 创建时事务校验 |
| 请求限流 | gateway 令牌桶：全局 QPS + per-key QPS（exec 与 create 分池） |
| Exec 输出 | maxOutputBytes 截断；WS 流带宽 per-connection 限速 |
| proxy | per-sandbox 并发连接数、带宽限速（防滥用做穿透代理） |

## 8. 可观测

- **平台指标**（Prometheus）：创建延迟分位、各状态 sandbox 数、节点容量水位、
  镜像缓存命中率、exec QPS、proxy 连接数
- **审计日志**：所有写操作（create/destroy/exec 摘要）落 Postgres + 定期归档 S3
- **sandbox 日志**：agent 环形缓冲（默认 8 MiB）实时查询；销毁时终态日志 +
  stdout/stderr 全量经 presigned URL 归档 S3（路径：`s3://<bucket>/logs/{sandboxId}/`）
- **trace**：request_id 全链路透传，OTel 埋点（gateway、noded、agent）

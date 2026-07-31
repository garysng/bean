# API 与 Proxy 服务设计

> 对应组件：`bean-api`（api-gateway）、`bean-proxy`（端口反代）。
> 术语与状态机见 [architecture.md](architecture.md)。

## 1. 设计原则

- **REST 对外，gRPC 对内**：SDK/CLI 走 REST（+WebSocket 流式），control ↔ beand ↔ agent 走 gRPC
- **幂等**：所有创建类接口支持 `Idempotency-Key` 头，state store 唯一约束去重
- **大对象不进 gateway**：文件上传/下载超过阈值（默认 4 MiB）一律 presigned URL 直连 S3 或 beand 直连
- **proto 是 single source of truth**：REST DTO 由 proto 派生，OpenAPI spec 生成

## 2. 鉴权

### 2.1 API Key

- `Authorization: Bearer bk_<keyid>_<secret>`
- key 哈希存 Postgres；附带配额（并发 sandbox 数、CPU/mem 总量、卷容量、prewarm 权限）
- **不做用户/租户体系**——bean 是集群内部服务,key 仅用于调用方识别、配额与
  审计归属;安全重心在集群内可靠性（托管 TLS + node token、凭证分层、隔离档）而非多租户

### 2.2 Sandbox 级短时凭证

- 创建 sandbox 时 gateway 签发 **sandbox token**（JWT，绑定 sandbox-id，TTL 固定 24h,
  可经 API 续签;sandbox 销毁即失效）
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
  "resources": { "cpu": 2, "memoryMiB": 4096, "diskMiB": 20480 },
                                        // gpu/isolation 为内部字段不对外;runtime
                                        // 档位由调度自动分配（architecture D3）
  "env": { "FOO": "bar" },
  "cmd": null,                          // 覆盖镜像 CMD；null=保留原 entrypoint（由 agent 托管拉起）
  "autoStartCmd": false,                // true 则创建后立即拉起原 entrypoint
  "region": "ap-east-1",                // 可选;缺省按 key 默认 region;
                                        // 挂已有卷/从 snapshot 创建时强制数据所在 region
  "nodeSelector": { "pool": "nvme" },   // 可选;按节点 labels 过滤
  "lifecycle": {                        // 可选;缺省 = 一直运行
    "idleTimeout": "300s",              //   null/缺省=永不;"0s"=活动一结束即触发
    "onIdle": "pause"                   //   pause（默认）| kill
  },
  "labels": { "eval-run": "swebench-0731", "task": "django-12345" },
  "networkPolicy": "egress-only",       // egress-only|none|allow-list（预留）
  "volumes": [                          // 可选，见 §3.6
    { "volume": "vol_...", "subPath": "run-0731", "mountPath": "/workspace", "readOnly": false }
  ]
}
→ 201 { "sandbox": { "id": "sbx_...", "state": "PENDING", ... }, "token": "<sandbox JWT>" }
```

```
GET    /sandboxes/{id}                       → sandbox 详情（state、runtime、nodeId、createdAt、lifecycle、lastActivityAt、endpoints）
GET    /sandboxes?label=eval-run%3Dswebench-0731&state=RUNNING&pageToken=&pageSize=100
DELETE /sandboxes/{id}                       → 202，异步销毁；?force=true 跳过 graceful
PATCH  /sandboxes/{id}/lifecycle { "idleTimeout": "600s", "onIdle": "kill" }   → 运行时调整
POST   /sandboxes/{id}/pause                 → 202 → PAUSED
POST   /sandboxes/{id}/resume                → 202 → RUNNING
POST   /sandboxes/{id}/snapshot  { "name": "after-setup", "keepRunning": true }
                                             → 202 { "snapshotId": "snap_..." }
POST   /sandboxes/{id}/start                 → 拉起原 entrypoint（autoStartCmd=false 后手动启动）
POST   /sandboxes/{id}/fork     { "count": 3, "labels": {...} }    // 独立 API（fc 档,P4）
       → 202 { "sandboxes": [ ...N 个新 sandbox... ] }
       // 语义：对运行中 sandbox 做瞬时 CoW 快照并克隆 N 个独立实例（不产生
       // 持久 snapshot 对象;要留存用 /snapshot）。容器档返回 501
```

sandbox 详情返回 `runtime: fc|runsc|runc`（实际档位，排障用）。

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

链路：client → gateway（升级）→ beand gRPC stream → agent。gateway 只做帧透传与鉴权。

### 3.3 Files

```
PUT  /sandboxes/{id}/files?path=/workspace/patch.diff     // body ≤4MiB 直传
     ?mode=0644&mkdirs=true
GET  /sandboxes/{id}/files?path=/workspace/report.json    // ≤4MiB 直回
GET  /sandboxes/{id}/files/ls?path=/workspace             → [{name,size,mode,mtime,isDir}]
POST /sandboxes/{id}/files:uploadUrl   {"path": "...", "sizeBytes": 123456789}
     → { "url": "<presigned PUT>", "commit": "/files:commitUpload?token=..." }
     // 两段式：client PUT S3 → 调 commit → gateway 指令 agent FetchToSandbox
     //（agent 经 presigned GET 拉入 sandbox 内目标路径）
POST /sandboxes/{id}/files:downloadUrl {"path": "..."}
     → { "url": "<presigned GET>" }    // beand 把文件推 S3 暂存后签 URL
DELETE /sandboxes/{id}/files?path=...
```

### 3.4 Ports

```
POST /sandboxes/{id}/ports    { "port": 8888, "auth": "token" }   // token|public
→ { "url": "https://sbx-abc123-8888.<region>.sandbox.<domain>" }
GET    /sandboxes/{id}/ports
DELETE /sandboxes/{id}/ports/{port}
```

### 3.5 Images

```
POST /images/prewarm   { "refs": ["img:a", "img:b"], "region": "ap-east-1",
                         "targetNodes": 10, "priority": "high" }
                       // region 缺省 = 镜像源 region;跨 region 首次 prewarm
                       // 触发 blob 复制到该 region S3
→ { "jobId": "pw_..." }
GET  /images/prewarm/{jobId}      → 各镜像 × 节点就绪矩阵摘要
GET  /images/{ref}/status         → { "blobReady": true, "cachedNodes": 7, "sizeBytes": ..., "format": "overlaybd" }
```

### 3.6 Volumes

镜像与卷为两种正交资源（镜像=环境，卷=数据，独立生命周期）。数据面见 beand-design.md §3.3。

首期仅 `shared-fs` 类型（`dataset` 预留，暂不排期）：

```
POST   /volumes    { "name": "alice-ws", "type": "shared-fs",
                     "quotaMiB": 102400, "labels": {} }
GET    /volumes?label=...          → 含 usage（空间/inode 用量）
GET    /volumes/{id}
DELETE /volumes/{id}               // 有活跃挂载时 409 VOLUME_IN_USE

POST /sandboxes { ..., "volumes": [
  { "volume": "vol_...", "subPath": "run-0731", "mountPath": "/workspace",
    "readOnly": false }
] }
```

- 挂载级 `readOnly` 可收紧（默认 false）
- volume 状态机：`CREATING → READY → DELETING`
- 配额：空间/inode 由后端执行（JuiceFS 目录配额）;per-key 卷总容量配额见 §7

### 3.7 Snapshots

```
GET    /snapshots?label=...            → 列表（id、srcSandboxId、sizeBytes、state）
GET    /snapshots/{id}
DELETE /snapshots/{id}
POST   /sandboxes    { "snapshot": "snap_...", ... }     // 从 snapshot 创建（代替 image 字段）
```

### 3.8 Events

```
事件类型：sandbox.lifecycle.{created,running,paused,resumed,stopped,failed,lost,oom}
          + sandbox.snapshot.{ready,failed}
          // stopped 对应状态机 STOPPED（含显式 DELETE 与 onIdle=kill）,
          // lost 对应节点租约丢失
事件体：  { "id", "type", "timestamp", "sandboxId", "data": {...}, "version": "v1" }
          // 命名对齐 e2b（sandbox.lifecycle.* 点分层级）,便于生态兼容

GET /sandboxes/{id}/events?pageToken=      // 历史（Postgres events 表,分页）
WS  /events?label=eval-run%3Dr0731         // 实时订阅（按 label/id 过滤;
                                           //  批量 eval 用事件驱动替代轮询）
```

实现：状态机变更处统一发件 → Postgres（历史）+ 内存 pub/sub（WS）。
webhook 推送为 P5 储备项。

### 3.9 Logs / 可观测

```
GET /sandboxes/{id}/logs?follow=false&tailLines=1000    // agent 环形缓冲 + S3 归档
GET /nodes                                              // 运维面：节点列表、容量、能力
GET /metrics                                            // Prometheus 格式（平台自身指标）
```

**OTel 采集**：

- 平台组件（gateway/scheduler/beand/agent）trace/metrics/logs 统一 OTLP 导出
  （Prometheus 兼容端点保留）;request_id 贯穿即 trace id
- **per-sandbox 资源指标**：beand 按 sandbox 采 cpu/mem/io/net 时序（cgroup/FC
  stats）,resource attributes 带 sandbox_id/labels——可按 eval-run 聚合消耗
- **sandbox 内应用 OTLP 透传（可选开启）**：agent 在 sandbox 内 listen
  localhost:4317,应用 trace 经 vsock/socket 转发出去并打 sandbox 标签

## 4. 内部 gRPC proto 草案

```protobuf
// proto/bean/node/v1/node.proto —— control plane ↔ beand
service NodeService {                                              // beand → control（出向）
  rpc Register(RegisterRequest) returns (RegisterResponse);        // 能力/资源画像上报
  rpc Heartbeat(stream HeartbeatRequest) returns (stream HeartbeatResponse);
      // 双向流：↑ 心跳+资源水位+sandbox 状态摘要+镜像缓存清单摘要（bloom/hash）
      // ↓ 租约确认（指令下发走 push 直连，见 5.1）
  rpc SyncState(SyncStateRequest) returns (SyncStateResponse);     // beand 重启对账：拉全量期望状态
}

service SandboxService {                       // beand 实现,control/gateway 作为 client 直连
  rpc CreateSandbox(CreateSandboxRequest) returns (CreateSandboxResponse);
      // spec 含 volumes: repeated VolumeMount（dataset 盘 ref / shared-fs 导出名）
  rpc RestoreSandbox(RestoreSandboxRequest) returns (RestoreSandboxResponse);  // 从 snapshot
  rpc DestroySandbox(DestroySandboxRequest) returns (DestroySandboxResponse);
  rpc PauseSandbox(PauseSandboxRequest) returns (PauseSandboxResponse);
  rpc ResumeSandbox(ResumeSandboxRequest) returns (ResumeSandboxResponse);
  rpc SnapshotSandbox(SnapshotSandboxRequest) returns (SnapshotSandboxResponse);
  rpc ForkSandbox(ForkSandboxRequest) returns (ForkSandboxResponse);      // fc 档 CoW 克隆
  rpc StartUserProcess(StartUserProcessRequest) returns (StartUserProcessResponse);
  rpc PrewarmImage(PrewarmImageRequest) returns (PrewarmImageResponse);
  rpc PrepareVolume(PrepareVolumeRequest) returns (PrepareVolumeResponse);
      // shared-fs 后端挂载确认（dataset 预留）
  // 数据面：gateway/proxy 直连 beand 转发（携带 sandbox-id 路由头）,纯透传 AgentService：
  rpc Exec(ExecRequest) returns (ExecResponse);
  rpc StreamExec(stream StreamExecFrame) returns (stream StreamExecFrame);
  rpc ReadFile(ReadFileRequest) returns (stream FileChunk);
  rpc WriteFile(stream WriteFileFrame) returns (WriteFileResponse);
  rpc DeleteFile(DeleteFileRequest) returns (DeleteFileResponse);
  rpc ListDir(ListDirRequest) returns (ListDirResponse);
  rpc GetLogs(GetLogsRequest) returns (stream LogChunk);
  rpc ForwardPort(stream PortFrame) returns (stream PortFrame);   // proxy 数据面
}

// proto/bean/agent/v1/agent.proto —— beand ↔ bean-agent（fc 档 vsock 主路径 / 容器档 unix socket,P5）
service AgentService {
  rpc Exec(ExecRequest) returns (ExecResponse);
  rpc StreamExec(stream StreamExecFrame) returns (stream StreamExecFrame);
  rpc ReadFile(ReadFileRequest) returns (stream FileChunk);
  rpc WriteFile(stream WriteFileFrame) returns (WriteFileResponse);
  rpc ListDir(ListDirRequest) returns (ListDirResponse);
  rpc DeleteFile(DeleteFileRequest) returns (DeleteFileResponse);
  rpc FetchToSandbox(FetchToSandboxRequest) returns (FetchToSandboxResponse);
      // 从 presigned GET URL 拉文件入 sandbox（大文件上传两段式的第二段）
  rpc MountVolume(MountVolumeRequest) returns (MountVolumeResponse);     // NFS/dataset 盘挂载
  rpc UnmountVolume(UnmountVolumeRequest) returns (UnmountVolumeResponse);
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
  定义（`proto/bean/common/v1/exec.proto`），beand 纯透传

## 5. 控制流细节

### 5.1 指令下发模型：push 直连

控制面直接 gRPC 调用 beand 的 `SandboxService`（beand 是 gRPC server,同 region 内网直连,node token 校验）——与 e2b/AgentENV/CubeSandbox 的业界一致做法相同，调度路径最短：

```
scheduler 决策（内存态 + Postgres 事务扣承诺量、写指令记录）
  → 直连 beand.CreateSandbox（同步返回受理结果）
  → beand 异步执行,状态变更经 Heartbeat 上报
```

- **连接方向闭合**：数据面（gateway/proxy → beand 的 exec/文件/端口）要求同
  region 内网可达（regional proxy 与节点同域部署）。控制面 → beand 的指令在
  节点「出向-only、入站零暴露」前提下这样闭合：beand 启动即向托管接入层建立
  **长连 gRPC 双向流（CommandChannel）**,控制面把 SandboxService 调用多路复用
  到该流上下发（请求/响应帧带 command_id 关联）——语义仍是 push 直连（控制面
  发起、同步等响应）,只是传输承载在节点出向连接上;node token 在流建立时校验,
  流存续期即身份有效期
- **可靠性不靠 pull,靠写库 + 对账**：
  - 指令先写 Postgres（audit + 状态机 source of truth）,RPC 只是投递方式
  - RPC 超时/失败 → 有限重试;仍失败 → 释放承诺量重调度（见 architecture D7）
  - beand 按 command_id 幂等去重（重试安全）
  - beand 重启 → `SyncState`（拉全量期望状态）对账,补投丢失指令
- Heartbeat 双向流职责收敛为：↑ 心跳/资源水位/sandbox 状态/缓存摘要,
  ↓ 租约确认（不再承担指令通知）

proto 见 §4（NodeService.SyncState 承担重启对账;SandboxService 由 beand 实现、
control plane 作为 client 直连调用）。

### 5.2 Lifecycle 自动化语义

**默认一直运行**——无硬 timeout。回收由 idle 机制驱动：

| idleTimeout | 行为 |
|---|---|
| 缺省 / null | 不启用 idle 检测（默认）,sandbox 持续运行 |
| `"0s"` | 活动一结束立即触发 onIdle（eval 批量：`onIdle: kill` 用完即走） |
| `"300s"` | 闲置 5 分钟触发 onIdle |

- **idle 判定**（beand 本地,不依赖控制面）：无 exec 会话 + 无端口活跃连接 +
  无文件 API 操作,持续 idleTimeout;任一活动重置计时
- **唤醒是平台默认行为（非配置）**：gateway/proxy 对 PAUSED sandbox 收到
  exec/端口/文件请求 → 触发 resume（fc 亚秒）→ 阻塞至恢复后透传;并发唤醒
  由控制面按 sandbox-id 去重
- PAUSED 滞留：**默认无限期保留**（不擅自回收用户暂停的 sandbox）;注意 PAUSED
  仍占宿主 RAM 与调度承诺量,容量代价由容量规划承担。管理员可选开启全局回收
  （默认关）;长期正解是 P4 的 snapshot 归档:PAUSED 超阈值 → 状态落 S3 释放
  RAM → 再访问自动 restore
- 业界对齐:CubeSandbox v0.5(on_timeout: pause/kill + 数据面透明唤醒)、
  e2b auto-pause/auto-resume 同构;我们以 null 表达「永不」,避开 -1/0 魔数重载

### 5.3 Exec 路由

```
client → gateway：Bearer key / sandbox token
gateway：state store 查 sandbox → nodeId → beand 地址（缓存 + 失效订阅）
gateway → beand：gRPC（同 region 内网,node token 校验）
beand → agent：vsock（fc 主路径;容器档 unix socket,P5）
```

状态语义：PAUSED → 触发透明唤醒,请求阻塞至 resume（超过唤醒时限,默认 10s,
才回 502 + Retry-After）;PULLING/STOPPING 等不可唤醒态 → 409 SANDBOX_NOT_RUNNING。

## 6. bean-proxy（端口反代服务）

独立无状态服务，可与 gateway 合部或水平扩展。

### 6.1 域名与 TLS

- 每 region 通配证书 `*.{region}.sandbox.<domain>`（ACME DNS-01 自动续期）
- Host 规则：`{sandboxId}-{port}.{region}.sandbox.<domain>`，sandboxId 用短 ID（如 `sbx-` 去前缀后的 base32）
- HTTP + WebSocket 透传；非 HTTP 协议暂不支持（预留 TCP over TLS SNI 方案）

### 6.2 路由与数据面（对齐 e2b：反代直连 sandbox IP）

```
浏览器 → {sbxId}-{port}.{region}.sandbox.<domain>（DNS 直达该 region 的 proxy）
       → regional proxy：解析 Host → 鉴权（6.3）
       → 路由查询：state store 查 sandbox → nodeId → beand 地址
         （本地 LRU 缓存 30s + 心跳失效推送;PAUSED → 触发透明唤醒后重路由）
       → HTTP 反代 → beand 内嵌 sandbox-proxy（节点侧反代）
       → 直连 sandbox IP:port（fc 档 tap IP / 容器档 veth IP,节点内路由）
```

- **两跳 HTTP 反代、末端直连 sandbox IP**——e2b/CubeSandbox 同款;不经 agent
  隧道（省一层用户态拷贝与 vsock 序列化,高流量端口性能关键）
- agent 的 `ForwardPort` 保留为兜底路径（未来 localhost-only 服务等场景）
- WebSocket 天然升级透传;连接级超时（>620s,躲上游 LB）、per-sandbox
  并发/带宽限制;proxy 侧连接活跃度喂 idle 判定（lifecycle）
- beand 侧 sandbox-proxy 亦做 nftables 之外的第二层校验（仅放行已暴露端口）

### 6.3 端口鉴权

- `auth=public`：任何持有 URL 者可访问（内部演示用）
- `auth=token`（默认）：要求 `?bean_token=<sandbox JWT>` 或 Cookie；proxy 校验 JWT
  签名与 sandbox-id 匹配后种 Cookie（1h），后续请求免 query
- proxy 注入 `X-Bean-Sandbox-Id` 头，剥离入站的同名头

### 6.4 生命周期联动

- sandbox 销毁（含 onIdle=kill）→ gateway 撤销端口记录 → proxy 缓存失效推送 → 后续请求 404
- PAUSED → 触发透明唤醒并阻塞透传;唤醒超时（默认 10s）才回 502 + Retry-After

## 7. 配额与限流

| 层 | 机制 |
|---|---|
| API key | 并发 sandbox 数、总 CPU/mem、卷总容量/个数、prewarm 次数/天 —— Postgres 计数 + 创建时事务校验 |
| 请求限流 | gateway 令牌桶：全局 QPS + per-key QPS（exec 与 create 分池） |
| Exec 输出 | maxOutputBytes 截断；WS 流带宽 per-connection 限速 |
| proxy | per-sandbox 并发连接数、带宽限速（防滥用做穿透代理） |

## 8. 可观测

- **平台指标**（Prometheus）：创建延迟分位、各状态 sandbox 数、节点容量水位、
  镜像缓存命中率、exec QPS、proxy 连接数
- **审计日志**：所有写操作（create/destroy/exec 摘要）落 Postgres + 定期归档 S3
- **sandbox 日志**：agent 环形缓冲（默认 8 MiB）实时查询；销毁时终态日志 +
  stdout/stderr 全量经 presigned URL 归档 S3（路径：`s3://<bucket>/logs/{sandboxId}/`）
- **trace**：request_id 全链路透传，OTel 埋点（gateway、beand、agent）

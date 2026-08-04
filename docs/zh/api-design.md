# API 与 Proxy 服务设计

> 对应组件:`bean-api`（api-gateway,✅）、`bean-proxy`（进入 sandbox 的反向代理,✅）。
> 状态标注约定见 [architecture.md](architecture.md) §0。
> 术语与状态机见 [architecture.md](architecture.md)。

## 1. 设计原则

- **REST 对外，gRPC 对内**：SDK/CLI 走 REST（+WebSocket 流式），control ↔ noded ↔ agent 走 gRPC
- **幂等**：所有创建类接口支持 `Idempotency-Key` 头，state store 唯一约束去重
- **大对象不进 gateway**：文件上传/下载超过阈值（默认 4 MiB）一律 presigned URL 直连 S3 或 noded 直连
- **proto 是 single source of truth**：REST DTO 由 proto 派生，OpenAPI spec 生成

## 2. 鉴权

### 2.1 API Key ✅

- `Authorization: Bearer bk_<keyid>_<secret>`
- key 哈希存 Postgres；附带配额（并发 sandbox 数、CPU/mem 总量、卷容量、prewarm 权限）
- **不做用户/租户体系**——bean 是集群内部服务,key 仅用于调用方识别、配额与
  审计归属;安全重心在集群内可靠性（托管 TLS + node token、凭证分层、隔离档）而非多租户

### 2.2 Sandbox 级短时凭证 📐

- 创建 sandbox 时 gateway 签发 **sandbox token**（JWT，绑定 sandbox-id，TTL 固定 24h,
  可经 API 续签;sandbox 销毁即失效）
- 用途：proxy 访问受保护端口、WebSocket exec 重连，避免长期 API key 下发到浏览器/弱环境

### 2.3 S3 Presigned URL 📐

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
       // 持久 snapshot 对象;要留存用 /snapshot）。容器档返回 501。
       // 注意:这是 snapshot+restore 这对操作的便利封装,不是一项新能力。
       // 今天 POST /snapshot 再 N 次 POST /sandboxes{snapshot} 已经能得到 N 个
       // 互相独立的 sandbox;fork 省掉的是那个持久对象和那一圈往返。
       // 见 snapshot-resume.md 4.5
```

sandbox 详情返回 `runtime: fc|runsc|runc`（实际档位，排障用）。

批量（eval 场景高频）：

```
POST /sandboxes:batchCreate   { "requests": [ ... ≤100 ... ] }
→ 207 逐项 { index, sandbox | error }     // 部分成功语义
DELETE /sandboxes?label=eval-run%3Dswebench-0731    → 批量销毁，202 + 任务计数
```

### 3.2 Exec ⚠️

> 同步 exec 与流式 exec 已实装;**PTY 未实现**。


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

### 3.3 Files ✅

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
     → { "url": "<presigned GET>" }    // noded 把文件推 S3 暂存后签 URL
DELETE /sandboxes/{id}/files?path=...
```

### 3.4 Ports —— 没有注册步骤 ✅

访问沙箱内的端口是通的,而且**不需要任何 API 调用**。端口写在 Host 头里
(`{port}-{sandbox}`,见 §6),bean-proxy 转发过去。沙箱内有进程在听就能访问,
没有就返回 502。

下面这套设计是先画的,**没有实现,而且是刻意不实现**:

```
POST /sandboxes/{id}/ports    { "port": 8888, "auth": "token" }
GET    /sandboxes/{id}/ports
DELETE /sandboxes/{id}/ports/{port}
```

它会给一件 guest 已经决定的事再造一个真相来源。端口开没开是沙箱内进程的事实,
而一份「打算开的端口」清单只能与它一致或不一致 —— 不一致的表现就是一个解析得到
却什么都没有的 URL,或者一个能用但平台说它关着的端口。什么都不分配也意味着没有池
需要在重启后重建,而这正是当初不用宿主端口的另一个理由。

真正缺的是**按端口的访问控制**(上面那个 `"auth": "token"`)。现在沙箱上任何端口,
只要能连到 proxy 就能访问,所以不要给沙箱一个它不希望调用方看到的端口。外部认证层
(A7)管的是能否访问这个沙箱,不是沙箱内的哪个端口。

`10001` 是保留端口 —— 那是 agent —— 任何代用户映射端口的东西都必须拒绝它。

### 3.5 Images ✅

**术语（必须区分清楚）**：

| 概念 | 归属 | 说明 |
|---|---|---|
| `ref` | **用户输入** | 原生 OCI 引用（`python:3.12`）。用户只提供也只看到这个 |
| `digest` | 平台解析 | tag 解析一次后固定;调度/缓存/复现全部按 digest,避免移动 tag 改变批次内容 |
| overlaybd 产物 | **平台内部** | 转换后的块设备形态,用户不可见、不可指定 |
| `state` | 平台内部 | `PENDING → CONVERTING → READY \| FAILED` |

`format` 字段告诉调用方当前哪个档能跑该镜像：`oci`（未转换,走标准拉取）
或 `overlaybd`（已转换,fc 档可用）。

```
GET  /images                      列表
GET  /images/status?ref=<ref>     单镜像状态（ref 走 query:含 / 与 :）
     → { ref, digest, state, format, cachedNodes, sizeBytes }
POST /images/prewarm   { "refs": ["img:a"], "region": "ap-east-1",
                         "targetNodes": 10, "priority": "high" }
     → { jobId, refs, ready: {ref: nodeCount}, done }
GET  /images/prewarm/{jobId}      各镜像 × 节点就绪矩阵
```

`cachedNodes` / `targetNodes` 是**运维语义,故意不进 CLI**：副本落在几台机器上
是调度细节,用户无法据此行动,暴露了只会让人依赖调度结果、调度器反而不能再迁移。
CLI 侧只报 ready / warming,对应的参数叫 `--replicas`。
详见 `docs/sdk-cli-design.md` §4.1。

**Registry 认证**（私有镜像）：按 registry host 登记一次凭证,之后私有镜像与
公开镜像用法完全相同——只给 ref。

```
PUT    /registries   { "host": "registry.example.com", "username": "robot",
                       "secret": "..." }     // secret 只写:响应/日志/sandbox 均不含
GET    /registries                            → host/username/时间戳（无 secret）
DELETE /registries/{host}
```

- 凭证 AES-256-GCM 加密后落库（`--secret-key` / `BEAN_SECRET_KEY`）,
  数据库副本本身不足以拉取私有镜像;**无 master key 时端点拒绝而非明文存储**
- 无凭证的 registry 按匿名拉取,公开镜像照常工作
- host 归一化:`https://r.io/` 与 `r.io` 视为同一个;无 host 的 ref 默认
  Docker Hub（与容器运行时同规则）

### 3.6 Volumes 📐

镜像与卷为两种正交资源（镜像=环境，卷=数据，独立生命周期）。数据面见 noded-design.md §3.3。

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

### 3.7 Snapshots ✅

```
POST   /sandboxes/{id}/snapshot  { "name": "after-setup", "labels": {},
                                   "keepRunning": true,
                                   "includeMemory": true,
                                   "base": "snap_..." }
       → 202 { snapshotId, snapshot: {state, sizeBytes, includeMemory,
                                      baseId, chainDepth, cpuVendor, ...} }
GET    /snapshots?label=k%3Dv&state=READY   → 列表
GET    /snapshots/{id}
DELETE /snapshots/{id}      // RefCount>0 或有子代 → 409 SNAPSHOT_IN_USE
POST   /sandboxes    { "snapshot": "snap_..." }   // restore:一个**新的** sandbox、新 id,
                                                  // 不是把被快照的那个救回来。
                                                  // 调 N 次就是一份快照出 N 个独立 sandbox
                                                  // image 与 snapshot 互斥
                     // CPU 不兼容 → 409 INCOMPATIBLE_CPU
```

restore 是 `POST /sandboxes` —— 一次创建 —— 而 `resume` 是打在已存在的
`/sandboxes/{id}` 上的 POST。两者是作用在不同对象上的不同操作,见
[snapshot-resume.md](snapshot-resume.md) §0。

- `includeMemory` 默认 **true**,即快照一直以来的含义(restore 出的 sandbox 接着被采集
  的那个 guest 跑,而不是重新开机)。
  设成 false 只抓文件系统:restore 重新 boot,但**可以落在任意 CPU 上** ——
  guest 内存把快照钉死在兼容的 vendor+family 上。实测 6109 B 对全量 15.5 MB。
  **用指针类型**(`*bool`)区分「缺省」与「显式 false」:老快照没有这个字段,
  plain bool 会解成 false 从而绕过 CPU 约束 —— 恰好是最需要约束的那批。
- `base` 只存自该快照以来改动的 guest 内存。实测 298 KB 对 15.5 MB。
  要求 `includeMemory`(文件系统层本来就是 O(changed))。
  链深超 8 时**自动产出 full 并把 baseId 留空** —— 响应里的 baseId 是
  调用方判断实际拿到什么的依据,不必知道上限是多少。
  节点必须带 `--track-dirty-pages` 启动,否则明确报错而不静默降级。

- `keepRunning` 默认 **true**：snapshot 不应打扰正在工作的 sandbox;
  为一致性会短暂冻结,完成后恢复原状态（RUNNING 或 PAUSED 均保持）
- snapshot 状态机 `CREATING → READY | FAILED`;失败会记录 reason,
  不会卡在 CREATING
- **引用计数**：restore 期间持有引用,期间删除返回 409;restore 结束自动释放
- restore 继承 snapshot 的 image（rootfs 基底必须与 checkpoint 匹配）
- checkpoint 格式**按 runtime 档区分**,不可互换,故 snapshot 记录产出它的 runtime
- blob 存储走 `snapshot.Blobs` 接口:**S3 已实装**(SigV4 自实现、分片上传、
  range 读),本地目录是 dev 默认。两者都是原子写(临时文件 + rename / 分片提交),
  失败不留可读的半成品
- **INCOMPATIBLE_CPU 用 409 而不是 503**:等待不会让它变可行,而客户端在 503 上
  重试会一直循环到自己超时

### 3.8 Events ✅

```
事件类型：sandbox.lifecycle.{created,running,paused,resumed,stopped,failed,lost,oom}
          + sandbox.snapshot.{ready,failed}
          // stopped 对应状态机 STOPPED（含显式 DELETE 与 onIdle=kill）,
          // lost 对应节点租约丢失
事件体：  { "id", "type", "timestamp", "sandboxId", "data": {...}, "version": "v1" }
          // 命名对齐 e2b（sandbox.lifecycle.* 点分层级）,便于生态兼容

GET /sandboxes/{id}/events?pageToken=      // 历史（Postgres events 表,分页）
GET /events?sandbox=<id>&label=k%3Dv       // 实时订阅（SSE:text/event-stream;
                                           //  按 sandbox/label 过滤;批量 eval
                                           //  用事件驱动替代轮询）
```

实现：状态机变更处统一发件 → Postgres（历史）+ 内存 pub/sub（订阅）。
订阅传输选 **SSE** 而非 WebSocket：无额外依赖、穿代理稳、浏览器/SDK 接入简单;
慢订阅者按 64 事件缓冲后丢弃并计数（一个卡住的客户端不能拖住 API）。
webhook 推送为 P5 储备项。

### 3.9 Logs / 可观测 ⚠️

```
GET /sandboxes/{id}/logs?follow=false&tailLines=1000    // agent 环形缓冲 + S3 归档
GET /nodes                                              // 运维面：节点列表、容量、能力
GET /metrics                                            // Prometheus 格式（免鉴权:本地采集,不含 sandbox 内容）
    // bean_sandbox_creates_total{outcome}         创建结果计数
    // bean_sandbox_create_duration_seconds{outcome}  端到端创建延迟直方图
    // bean_exec_duration_seconds{outcome}         exec 往返延迟
    // bean_sandboxes{state}                       各状态 sandbox 数（scrape 时按库重算）
    // bean_events_total{type}  bean_event_subscribers
```

**OTel 采集**：

> 当前状态:metrics 是 Prometheus 端点(已实装,与 trace 是两套东西);
> logs 已字段化(`internal/logging`);**trace 已实装并实测** ——
> `--otlp-endpoint` 开启,一次 create/exec 是一棵跨进程 span 树,
> request id 即 trace id。响应头回 `X-Bean-Trace-Id`,
> 调用方报慢时可以直接给出要查的 trace。
> per-sandbox 资源指标与 sandbox 内应用 OTLP 透传仍未实装。

- 平台组件（gateway/scheduler/noded/agent）trace/metrics/logs 统一 OTLP 导出
  （Prometheus 兼容端点保留）;request_id 贯穿即 trace id
- **per-sandbox 资源指标**：noded 按 sandbox 采 cpu/mem/io/net 时序（cgroup/FC
  stats）,resource attributes 带 sandbox_id/labels——可按 eval-run 聚合消耗
- **sandbox 内应用 OTLP 透传（可选开启）**：agent 在 sandbox 内 listen
  localhost:4317,应用 trace 经 vsock/socket 转发出去并打 sandbox 标签

## 4. 内部 gRPC proto 草案 ✅

```protobuf
// proto/bean/node/v1/node.proto —— control plane ↔ noded
service NodeService {                                              // noded → control（出向）
  rpc Register(RegisterRequest) returns (RegisterResponse);        // 能力/资源画像上报
  rpc Heartbeat(stream HeartbeatRequest) returns (stream HeartbeatResponse);
      // 双向流：↑ 心跳+资源水位+sandbox 状态摘要+镜像缓存清单摘要（bloom/hash）
      // ↓ 租约确认（指令下发走 push 直连，见 5.1）
  rpc SyncState(SyncStateRequest) returns (SyncStateResponse);     // noded 重启对账：拉全量期望状态
}

service SandboxService {                       // noded 实现,control/gateway 作为 client 直连
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
  // 数据面：gateway/proxy 直连 noded 转发（携带 sandbox-id 路由头）,纯透传 AgentService：
  rpc Exec(ExecRequest) returns (ExecResponse);
  rpc StreamExec(stream StreamExecFrame) returns (stream StreamExecFrame);
  rpc ReadFile(ReadFileRequest) returns (stream FileChunk);
  rpc WriteFile(stream WriteFileFrame) returns (WriteFileResponse);
  rpc DeleteFile(DeleteFileRequest) returns (DeleteFileResponse);
  rpc ListDir(ListDirRequest) returns (ListDirResponse);
  rpc GetLogs(GetLogsRequest) returns (stream LogChunk);
  rpc ForwardPort(stream PortFrame) returns (stream PortFrame);   // proxy 数据面
}

// proto/bean/agent/v1/agent.proto —— noded ↔ beand（fc 档 vsock 主路径 / 容器档 unix socket,P5）
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
  定义（`proto/bean/common/v1/exec.proto`），noded 纯透传

## 5. 控制流细节

### 5.1 指令下发模型：push 直连 ✅

控制面直接 gRPC 调用 noded 的 `SandboxService`（noded 是 gRPC server,同 region 内网直连,node token 校验）——与 e2b/AgentENV/CubeSandbox 的业界一致做法相同，调度路径最短：

```
scheduler 决策（内存态 + Postgres 事务扣承诺量、写指令记录）
  → 直连 noded.CreateSandbox（同步返回受理结果）
  → noded 异步执行,状态变更经 Heartbeat 上报
```

- **连接方向闭合**：数据面（gateway/proxy → noded 的 exec/文件/端口）要求同
  region 内网可达（regional proxy 与节点同域部署）。控制面 → noded 的指令在
  节点「出向-only、入站零暴露」前提下这样闭合：noded 启动即向托管接入层建立
  **长连 gRPC 双向流（CommandChannel）**,控制面把 SandboxService 调用多路复用
  到该流上下发（请求/响应帧带 command_id 关联）——语义仍是 push 直连（控制面
  发起、同步等响应）,只是传输承载在节点出向连接上;node token 在流建立时校验,
  流存续期即身份有效期
- **可靠性不靠 pull,靠写库 + 对账**：
  - 指令先写 Postgres（audit + 状态机 source of truth）,RPC 只是投递方式
  - RPC 超时/失败 → 有限重试;仍失败 → 释放承诺量重调度（见 architecture D7）
  - noded 按 command_id 幂等去重（重试安全）
  - noded 重启 → `SyncState`（拉全量期望状态）对账,补投丢失指令
- Heartbeat 双向流职责收敛为：↑ 心跳/资源水位/sandbox 状态/缓存摘要,
  ↓ 租约确认（不再承担指令通知）

proto 见 §4（NodeService.SyncState 承担重启对账;SandboxService 由 noded 实现、
control plane 作为 client 直连调用）。

### 5.2 Lifecycle 自动化语义 ⚠️

**默认一直运行**——无硬 timeout。回收由 idle 机制驱动：

| idleTimeout | 行为 |
|---|---|
| 缺省 / null | 不启用 idle 检测（默认）,sandbox 持续运行 |
| `"0s"` | 活动一结束立即触发 onIdle（eval 批量：`onIdle: kill` 用完即走） |
| `"300s"` | 闲置 5 分钟触发 onIdle |

- **idle 判定**（noded 本地,不依赖控制面）：无 exec 会话 + 无端口活跃连接 +
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

### 5.3 Exec 路由 ✅

```
client → gateway：Bearer key / sandbox token
gateway：state store 查 sandbox → nodeId → noded 地址（缓存 + 失效订阅）
gateway → noded：gRPC（同 region 内网,node token 校验）
noded → agent：vsock（fc 主路径;容器档 unix socket,P5）
```

状态语义：PAUSED → 触发透明唤醒,请求阻塞至 resume（超过唤醒时限,默认 10s,
才回 502 + Retry-After）;PULLING/STOPPING 等不可唤醒态 → 409 SANDBOX_NOT_RUNNING。

## 6. bean-proxy（进入 sandbox 的反向代理）✅

> 已建成:`cmd/bean-proxy`。在真机上端到端验证过 —— 用户的服务器与 agent 都经它到达,
> 未知沙箱 404,畸形 Host 400。

### 6.0 那两件事其实是一件 ⚠️

本节原本把**端口暴露**(浏览器访问 sandbox 内的端口)和**数据面**
([GitHub #27](https://github.com/garysng/bean/issues/27),把 exec 与文件流量移出控制面)
设计成两件事,并警告混淆两者已经导致过一次错误的方案。

**那个警告对风险的判断是对的,对结论的判断是错的。** 它们是同一个机制,而让它们合并的
是把 agent 从 vsock 移到 guest 内的一个 TCP 端口:一旦 agent 就是 *guest 上的一个端口*,
「访问 agent」和「访问用户在 8000 上的服务」就是同一个请求,只是 Host 里的数字不同。

| | 原设计(两件事) | 实际实现(一件事) |
|---|---|---|
| 如何寻址 | 域名 vs REST 路径 | 两者都是 `{port}-{sandbox}` |
| 终点 | sandbox 的 IP:port vs noded 转给 agent | 始终是 sandbox 的 IP:port |
| 路由器需要区分吗 | 需要 | **不需要** —— 它转发的是一个端口 |

顺序是 `{port}-{sandbox}`,端口在前,与 e2b 的 `ParseHost` 一致:沙箱 id 长度可变且
可能含分隔符,端口两者都不是。

下面保留的是当初推理中站得住的部分 —— 动机是收窄接口,而不是负载。

**第二件事更强的理由不是负载。** `SandboxService` 把 `DestroySandbox`、
`SnapshotSandbox`、`CommitSandbox` 和 `Exec`、`ReadFile`、`WriteFile` 放在同一个服务里,
共用一个 `--node-token`。所以"让客户端直连 noded"不是一个带性能收益的路由改动 ——
它会把"销毁节点上任何沙箱"的能力交给每一个调用方。一个持有该 token、且**只**转发数据面方法的
proxy,才是收窄这个接口的办法;字节路径是次要收益。

e2b 从另一个方向到了同一个形状:`packages/client-proxy` 把 sandbox 解析到它所在的 node 再转发,
而且它携带 `trafficAccessToken` 与 `envdAccessToken` 两个**分开的**凭据,而不是一个集群密钥
(`internal/proxy/proxy.go`)。他们的 orchestrator 也监听自己的 proxy 端口(5007),
而不是把控制面 RPC 暴露给客户端。

**noded 到底需不需要认证**,取决于它监听在哪 —— 而今天的答案与 dockerd 相同:
`cmd/noded/main.go` 在非 loopback 地址上没有 token 就拒绝启动,在 loopback 上则什么都不要求。
网络位置替代了认证,正如 unix socket 对 Docker daemon 所做的那样。只要 noded 不被客户端直连,
这就是自洽的 —— 而那正是数据面不能破坏的不变量。


独立无状态服务，可与 gateway 合部或水平扩展。

### 6.1 域名与 TLS 📐

- 每 region 通配证书 `*.{region}.sandbox.<domain>`（ACME DNS-01 自动续期）
- Host 规则：`{sandboxId}-{port}.{region}.sandbox.<domain>`，sandboxId 用短 ID（如 `sbx-` 去前缀后的 base32）
- HTTP + WebSocket 透传；非 HTTP 协议暂不支持（预留 TCP over TLS SNI 方案）

### 6.2 路由与数据面（对齐 e2b：反代直连 sandbox IP）📐

```
浏览器 → {sbxId}-{port}.{region}.sandbox.<domain>（DNS 直达该 region 的 proxy）
       → regional proxy：解析 Host → 鉴权（6.3）
       → 路由查询：state store 查 sandbox → nodeId → noded 地址
         （本地 LRU 缓存 30s + 心跳失效推送;PAUSED → 触发透明唤醒后重路由）
       → HTTP 反代 → noded 内嵌 sandbox-proxy（节点侧反代）
       → 直连 sandbox IP:port（fc 档 tap IP / 容器档 veth IP,节点内路由）
```

- **两跳 HTTP 反代、末端直连 sandbox IP**——e2b/CubeSandbox 同款;不经 agent
  隧道（省一层用户态拷贝与 vsock 序列化,高流量端口性能关键）
- agent 的 `ForwardPort` 保留为兜底路径（未来 localhost-only 服务等场景）
- WebSocket 天然升级透传;连接级超时（>620s,躲上游 LB）、per-sandbox
  并发/带宽限制;proxy 侧连接活跃度喂 idle 判定（lifecycle）
- noded 侧 sandbox-proxy 亦做 nftables 之外的第二层校验（仅放行已暴露端口）

### 6.3 端口鉴权 📐

- `auth=public`：任何持有 URL 者可访问（内部演示用）
- `auth=token`（默认）：要求 `?bean_token=<sandbox JWT>` 或 Cookie；proxy 校验 JWT
  签名与 sandbox-id 匹配后种 Cookie（1h），后续请求免 query
- proxy 注入 `X-Bean-Sandbox-Id` 头，剥离入站的同名头

### 6.4 生命周期联动 📐

- sandbox 销毁（含 onIdle=kill）→ gateway 撤销端口记录 → proxy 缓存失效推送 → 后续请求 404
- PAUSED → 触发透明唤醒并阻塞透传;唤醒超时（默认 10s）才回 502 + Retry-After

## 7. 配额与限流 ⚠️

| 层 | 机制 |
|---|---|
| API key | 并发 sandbox 数、总 CPU/mem、卷总容量/个数、prewarm 次数/天 —— Postgres 计数 + 创建时事务校验 |
| 请求限流 | gateway 令牌桶：全局 QPS + per-key QPS（exec 与 create 分池） |
| Exec 输出 | maxOutputBytes 截断；WS 流带宽 per-connection 限速 |
| proxy | per-sandbox 并发连接数、带宽限速（防滥用做穿透代理） |

## 8. 可观测 ⚠️

- **平台指标**（Prometheus）：创建延迟分位、各状态 sandbox 数、节点容量水位、
  镜像缓存命中率、exec QPS、proxy 连接数
- **审计日志**：所有写操作（create/destroy/exec 摘要）落 Postgres + 定期归档 S3
- **sandbox 日志**：agent 环形缓冲（默认 8 MiB）实时查询；销毁时终态日志 +
  stdout/stderr 全量经 presigned URL 归档 S3（路径：`s3://<bucket>/logs/{sandboxId}/`）
- **trace**：request_id 全链路透传，OTel 埋点（gateway、noded、agent）

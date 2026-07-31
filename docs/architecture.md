# Bean 技术架构设计

> Container-native sandbox platform for AI evaluation workloads.

## 1. 背景与目标

### 1.1 问题

AI evaluation / agent rollout 场景（如 SWE-bench 类任务）的特点：

- **环境即镜像**：每个评测任务对应一个独立的 Docker 镜像（数量 2000+，单个数 GB）
- **短生命周期**：sandbox 用完即销毁，无状态
- **高并发批量拉起**：一轮评测可能同时创建成百上千个 sandbox
- **运行不可信代码**：AI 生成的代码在 sandbox 内执行，需要隔离

现有方案的问题：

- **e2b**（Firecracker microVM + template）：Docker 镜像必须先转换为 VM rootfs（分钟级），对"大量不同评测镜像"的场景不可用
- **K8s + Pod**：调度/网络栈太重，冷启动路径长，且我们需要完全自主可控的底层

### 1.2 目标

- 镜像为一等公民：任意 OCI 镜像直接作为 sandbox 环境，**无转换步骤**
- 秒级冷启动：镜像 lazy-pull + 节点缓存 + 预热
- 全自研栈：control plane、节点 runtime、sandbox agent、SDK、CLI 全部自主实现
- 底座无关：同时支持裸金属和云 VM 节点
- S3 作为统一存储 backend（镜像 blob、产物、快照）

### 1.3 非目标（首期）

- 跨节点 sandbox 网络互通
- pause/resume / 内存快照（预留设计位，Phase 2+）
- 多租户计费

## 2. 总体架构

```
                        ┌──────────────────────────────────────┐
  SDK (py/ts) / CLI ───▶│  Control Plane                       │
                        │  ├── api-gateway   REST/gRPC、鉴权、  │
                        │  │                 配额、端口反代     │
                        │  ├── scheduler     节点选择：镜像亲和  │
                        │  │                 + bin-packing     │
                        │  ├── state store   Postgres：sandbox │
                        │  │                 元数据、节点租约    │
                        │  └── image-service 镜像元数据、       │
                        │                    prewarm 编排、GC   │
                        └──────────┬───────────────────────────┘
                                   │ gRPC（双向：指令下发 + 心跳上报）
              ┌────────────────────┼────────────────────┐
              ▼                    ▼                    ▼
        ┌──────────┐         ┌──────────┐         ┌──────────┐
        │ noded    │         │ noded    │         │ noded    │   ← 每节点一个
        │ (裸金属) │         │ (云 VM)  │         │ (裸金属) │      daemon
        └────┬─────┘         └──────────┘         └──────────┘
             │ containerd（镜像面）+ beand 自管 FC（VM 面）
        ┌────▼─────────────────────────────┐
        │  ├── snapshotter: overlaybd       │ ← 块级 lazy-pull from S3
        │  └── runtime: fc │ runc │ runsc   │ ← isolation auto 解析
        └────┬─────────────────────────────┘
             │
        ┌────▼─────────────────┐
        │ sandbox (container)   │
        │  └── bean-agent (PID1)│ ← exec/PTY/文件/端口转发
        │      └── 用户进程      │
        └──────────────────────┘

        S3 ◀── 镜像 blob（source of truth）/ eval 产物 / 未来快照
```

### 2.1 组件职责

| 组件 | 语言 | 职责 |
|---|---|---|
| `api-gateway` | Go | REST + gRPC API、鉴权、配额、sandbox 端口反向代理 |
| `scheduler` | Go | 节点选择（镜像亲和 + 资源 bin-packing）、租约管理 |
| `image-service` | Go | 镜像元数据索引、prewarm 编排、S3 blob GC |
| `noded` | Go | 节点 daemon：sandbox 生命周期、网络、镜像缓存、健康上报 |
| `bean-agent` | Go（静态编译） | sandbox 内 PID1：exec、PTY、文件读写、端口转发 |
| `sdk-python` | Python | evaluation/rollout 侧主 SDK |
| `sdk-ts` | TypeScript | Web/Node 侧 SDK |
| `cli` | Go | `bean` 命令行：sandbox 管理、镜像预热、调试 |

## 3. 核心设计决策

### D1. 镜像零转换，容器与 microVM 双形态

任意 OCI 镜像直接作为 sandbox 环境，消除 e2b 式 template 转换。镜像经 overlaybd
组装为块设备（见 D4），既能给容器档做 overlayfs rootfs，也能 virtio-blk 直挂
microVM（见 D9）——两种形态共享同一条镜像链路，用户无感。

### D2. containerd 之上自研（而非直接驱动 runc）

containerd 解决 OCI 镜像拉取、snapshotter 插件、runc 生命周期管理这些最重的部分，且是库不是平台。自研价值集中在调度、网络、API、agent。noded 中 runtime 抽象为接口：

```go
type Runtime interface {
    Create(ctx context.Context, spec SandboxSpec) (SandboxHandle, error)
    Destroy(ctx context.Context, id string) error
    Status(ctx context.Context, id string) (SandboxStatus, error)
}
```

后续下沉到 libcontainer 或 microVM 只换实现。

### D3. 隔离分档 + 节点能力探测

noded 启动时探测节点能力并上报：

```
├── /dev/kvm 可用（裸金属 or 嵌套虚拟化 VM）→ [runc, runsc, fc]
└── 无 KVM（普通云 VM）                     → [runc, runsc(ptrace)]
```

sandbox 创建请求携带 `isolation: auto | container | vm`，默认 `auto`：

```
auto 解析规则：
  KVM 节点 → fc（Firecracker microVM，默认主档，见 D9）
  无 KVM 节点            → runsc（gVisor 降级档）
  GPU（内部预留，不对外）→ runc 容器档（FC 无 GPU passthrough）
显式 container(runc|runsc) / vm(fc) 仍允许
```

- **fc**：隔离最强、snapshot/fork 原生、guest 真内核无 syscall 兼容性问题——常规 eval/rollout 默认
- **runsc**：无 KVM 环境的降级档
- **runc**：GPU 路径 + 内部可信任务 + P0/P1 链路打通基线
- ~~kata~~：被 fc 取代，不再引入

scheduler 按能力匹配节点。

### D9. Firecracker 主档：容器 rootfs 直挂 microVM

FC 档**不是**嵌套容器（Kata 式 guest 内再跑 containerd），而是 rootfs 直挂：

```
overlaybd 组装镜像块设备（见 D4）
  → virtio-blk 挂给 microVM（只读 base + 可写 overlay 盘）
  → guest 内 bean-agent 作为 init：挂载 /proc /sys /dev 等（按 OCI 默认
    mounts 复刻）、应用 image config（ENV/USER/WORKDIR/Entrypoint+Cmd）
    拉起用户进程
```

- guest 内无容器层，"容器"只剩镜像格式；镜像零转换的承诺不变
- 兼容性：ENV/ENTRYPOINT 等 config 语义由 agent 复刻（与容器档同一份代码）；
  guest 是完整真实 Linux 内核，兼容性优于 gVisor 模拟层。唯一差异：内核
  由平台统一打包提供（非宿主内核），对纯用户态 eval 负载无感。
  详见 beand-design.md fcRuntime 节
- agent 通信走 vsock（transport 抽象，与容器档 unix socket 同协议）
- 网络：tap 设备接入节点 bean0 桥，nftables 规则与容器档一致
- 该路线已被 AgentENV（Kimi K3 训练基础设施）在生产验证；实现参考其
  overlaybd+ublk 集成与 snapshot 设计

### D4. S3 为统一存储 backend

| 数据 | 方案 |
|---|---|
| 镜像 blob | **overlaybd 块级镜像**（层 = 块设备 diff）直存 S3，节点经 ublk 按需 range-read；registry 仅存元数据 |
| 节点缓存 | 本地 NVMe 作为 S3 之上的块 chunk LRU 缓存；裸金属（大盘）与云 VM（小盘）仅命中率差异，架构统一 |
| eval 产物 | agent/noded 经 presigned URL 直推 S3（control plane 签发，节点不持长期凭证） |
| 大文件下载 | API 返回 presigned URL 重定向，不过 gateway 转发 |
| 快照（Phase 2+） | rootfs diff / 内存快照落 S3，支持跨节点 resume |

选 overlaybd（块级，DADI/阿里，AgentENV 已在 FC 场景验证）而非 Nydus（文件级）的关键原因：**块设备链路同时服务容器档（overlaybd-snapshotter → overlayfs）与 microVM 档（virtio-blk 直挂 guest），一条镜像链路通吃全部 runtime 档位**；Nydus 的文件系统语义进不了 microVM，FC 档需另走 virtiofs（FC 支持弱）。Nydus 保留为容器档备选。

热状态（sandbox 元数据、租约、调度状态）用 Postgres，不进 S3。

### D5. Agent 注入：PID1 override

eval 镜像任意、不可假设内含工具链。创建 sandbox 时：

1. noded 将静态编译的 `bean-agent` 通过 bind mount 挂入容器（只读）
2. override 容器 entrypoint，`bean-agent` 作为 PID1 启动
3. agent 与 noded 之间通过 vsock/unix socket + gRPC 通信
4. 用户原 entrypoint/cmd 信息保留在 spec 中，由 agent 按需拉起

不走 CRI streaming exec：性能差、无文件 API、依赖长链路。

### D6. 网络：节点内 NAT，取裸金属/云 VM 最大公约数

```
sandbox netns ←veth→ 节点 bridge → SNAT 出网
```

- 每 sandbox 独立 netns，节点本地私有网段（如 10.100.x.0/24 per node）
- 默认策略：允许出网（拉依赖），禁止访问节点内网/元数据服务（169.254.169.254 等），sandbox 间互相隔离（nftables）
- 端口暴露：`{sandbox-id}-{port}.sandbox.<domain>` → api-gateway 反代 → noded → agent 端口转发，绕开云厂商 MAC/IP 白名单限制
- 不依赖 underlay/BGP，两种节点行为完全一致

### D7. 调度：镜像亲和优先的 bin-packing

evaluation 调度足够简单，自研反而能做 K8s 做不了的精细优化。

**节点资源画像**（心跳上报，调度器内存态维护）：

```
cpu:   allocatable vCPU（物理核 × 超卖系数,配置项默认 3.0,预留系统份额）
       已承诺 = Σ sandbox.cpu;实际负载 = 节点 load（仅告警用）
mem:   allocatable = 物理内存 − 系统预留
       已承诺 = Σ sandbox.memoryMiB（fc 档气球回收不减承诺量——保 resume/突发）
disk:  sandboxes 池余量（可写层）;cache 池水位（只影响打分不做门槛）
gpu:   空闲卡数（按整卡分配，不切分）
cap:   [runc, runsc, fc] × 每节点并发创建余量（默认 16）
```

**调度流程**（单 sandbox;batchCreate 在一次锁内顺序执行同流程）：

```
1. 过滤（硬约束）：
   isolation 解析（auto→fc/runsc/runc）→ 节点能力匹配
   cpu/mem/disk 承诺量 + 请求 ≤ allocatable;GPU 整卡余量
   节点状态 = READY（SUSPECT/LOST/DRAINING 排除）
2. 打分（加权和，权重可配）：
   w1·镜像亲和：该镜像 overlaybd 块在节点缓存的字节占比（心跳带 bloom+字节数）
   w2·资源平衡：装箱后碎片度（优先填满，留大块空位给大规格）
   w3·缓存盘类型：冷镜像 → NVMe 大缓存节点加分
   w4·打散：同 label（同一 eval run）适度反亲和，避免单节点故障吞掉整批
3. 提交:Postgres 事务扣承诺量 + 写指令 → 心跳下行通知节点
4. 失败回退:节点报 FAILED（如 ENOSPC 竞态）→ 释放承诺量,重调度(≤3 次,
   排除失败节点),仍失败 → NO_CAPACITY 返回调用方
```

**记账一致性**：承诺量以 Postgres 为准（调度器重启可重建内存态）;节点心跳
实际用量仅用于告警与 balloon 决策，不参与准入——避免「实际水位准入」在
突发负载下超卖爆炸。

**抢占**：不做。eval 任务同质、短生命周期，排队（NO_CAPACITY + 客户端重试/
排队池）比抢占简单且足够。

### D8. 故障模型：租约 + 无状态重建

- noded 定期心跳续约；租约超时 → 节点标记失联 → 其上 sandbox 标记 `lost`
- eval 任务无状态，上层（SDK/调用方）收到 `lost` 后重建即可
- noded 重启后 reconcile：对账 containerd 实际状态 vs control plane 期望状态
- GC：sandbox 超时回收、镜像层 LRU 淘汰、孤儿 netns/挂载点清理

## 4. API 设计

### 4.1 REST API（对外）

```
# Sandbox 生命周期
POST   /v1/sandboxes                 # image, cpu/mem/gpu, env, isolation,
                                     # timeout, labels → sandbox
GET    /v1/sandboxes/{id}
GET    /v1/sandboxes?label=k=v       # list + filter
DELETE /v1/sandboxes/{id}
POST   /v1/sandboxes/{id}/timeout    # 续期

# 进程执行
POST   /v1/sandboxes/{id}/exec       # 同步：cmd/cwd/env/timeout
                                     # → stdout/stderr/exitCode
WS     /v1/sandboxes/{id}/exec/ws    # 流式 + PTY

# 文件系统
PUT    /v1/sandboxes/{id}/files?path=    # 上传（小文件直传，大文件返回
GET    /v1/sandboxes/{id}/files?path=    #  presigned URL）
GET    /v1/sandboxes/{id}/files/ls?path=

# 端口
POST   /v1/sandboxes/{id}/ports      # 暴露端口 → 公网 URL

# 镜像
POST   /v1/images/prewarm            # ref 列表 + 目标节点数
GET    /v1/images/{ref}/status       # 缓存分布、blob 就绪度
```

### 4.2 内部 gRPC

- `control ↔ noded`：`NodeService`（RegisterNode/Heartbeat/…）+ `SandboxService`（Create/Destroy/Exec 转发/…）
- `noded ↔ agent`：`AgentService`（Exec/StreamExec/ReadFile/WriteFile/ListDir/ForwardPort/…）

proto 定义统一放 `proto/`，生成代码进各语言 SDK。

### 4.3 Sandbox 状态机

```
PENDING → SCHEDULED → PULLING → STARTING → RUNNING → STOPPING → STOPPED
                                    │          │
                                    └── FAILED ┘        RUNNING ─(租约丢失)→ LOST

RUNNING ─pause→ PAUSED ─resume→ RUNNING
RUNNING/PAUSED ─snapshot→ SNAPSHOTTING → (回原状态)；snapshot 对象独立生命周期
```

详见 [snapshot-resume.md](snapshot-resume.md)。

## 5. 冷启动路径优化

目标：P50 < 2s（镜像已缓存）/ P50 < 10s（lazy-pull 冷镜像）。

1. **lazy-pull**：overlaybd + ublk 块级按需加载，启动只需元数据 + 热块，运行中按需 range-read S3
2. **节点缓存**：chunk 级 LRU，S3 为 source of truth，节点盘可随意 GC
3. **prewarm API**:评测批次开始前预热镜像到目标节点
4. **镜像亲和调度**：天然提升缓存命中
5. **agent 常驻热路径**：agent 静态二进制 bind mount，无镜像内安装步骤

## 6. 安全模型

- 默认 runsc（gVisor）运行不可信代码
- 容器降权：no-new-privileges、只读 rootfs 可选、seccomp/AppArmor 默认 profile、cgroup 硬限制
- 网络默认拒内网、拒元数据服务
- 节点不持长期 S3 凭证，全部走 control plane 签发的 presigned URL / STS
- API 鉴权：API key（首期）→ 租户/RBAC（后期）

## 7. Repo 结构

```
bean/
├── proto/                  # gRPC 定义（single source of truth）
├── cmd/
│   ├── bean-api/           # api-gateway 入口
│   ├── bean-scheduler/
│   ├── bean-imaged/        # image-service
│   ├── beand/              # node daemon (noded)
│   └── bean-agent/         # sandbox 内 agent（静态编译）
├── internal/
│   ├── control/            # gateway/scheduler/image-service 实现
│   ├── node/               # runtime 抽象、网络、镜像缓存、reconcile
│   ├── agent/
│   └── store/              # Postgres / S3 访问层
├── sdk/
│   ├── python/
│   └── typescript/
├── cli/                    # bean CLI
├── deploy/                 # 节点 bootstrap 脚本、systemd unit、S3/DB 初始化
└── docs/
```

## 8. 实施路线

| Phase | 内容 | 里程碑 |
|---|---|---|
| **P0 骨架** | proto、noded + containerd(runc) 创建/销毁 sandbox、agent exec、最小 REST API | 单节点端到端：`POST /sandboxes` → exec → destroy |
| **P1 可用** | scheduler + 多节点、Postgres 状态、Python SDK、CLI、文件 API、网络隔离(nftables) | 多节点跑通一轮小规模 eval |
| **P2 生产** | fcRuntime 主档（rootfs 直挂）、overlaybd + S3 lazy-pull、prewarm、镜像亲和调度、产物直推 S3 | 2000 镜像批量评测,冷启动达标 |
| **P3 扩展** | TS SDK、端口暴露反代、PTY/交互式 rollout、GPU 容器档完善 | agent rollout 场景接入 |
| **P4+** | 快照/resume、多租户 | — |

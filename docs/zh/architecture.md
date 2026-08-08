# Bean 技术架构设计

> Container-native sandbox platform for AI evaluation workloads.

## 0. 阅读约定:交付状态标注

这批设计文档同时承载两件事 —— **已经建成的**和**打算建成的**。两者写法一样时
读者无法分辨,而这已经造成过实际误判:网络栈和 jailer 隔离都曾被当成已交付能力。

所以每个描述具体机制的章节标题后带一个状态标记:

| 标记 | 含义 | 判据 |
|---|---|---|
| ✅ | **已实现** | 代码在仓库里,且有测试或实测数据 |
| ⚠️ | **部分实现** | 主路径通了但有明确缺口,章节内说明缺什么 |
| 📐 | **仅设计** | **没有代码**。这是意图,不是能力 |
| ❌ | **已放弃** | 曾经的设计,现在明确不做,保留是为了记住为什么 |

没有标记的章节是背景、动机、术语这类不描述机制的内容。

**权威性顺序**:代码 > `docs/status.md`(做到哪一步)> `docs/decisions.md`
(为什么这么选)> 本批设计文档。冲突时以前者为准,并且**改文档**。

一个自我约束:📐 章节里不写「我们的做法是」,写「计划是」。前者读起来像既成事实,
而这正是之前出问题的地方。

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

### 1.3 非目标（首期 P0–P2）

- 跨节点 sandbox 网络互通
- 多租户计费

**已交付、不再是非目标**:pause/resume 与 snapshot 都已实装并在真 KVM 机器实测
(full / `--no-memory` / `--base` 增量三种,见 snapshot-resume.md)。

**网络曾是最大空白,现已建成**(network.md):每个 sandbox 有独立 namespace、tap
与出网,元数据网段与 RFC1918 默认拒绝,沙箱内的端口可以从节点外经 bean-proxy 到达。
全部在真实内核上验证过,包括那些拒绝规则。

跨节点 sandbox 互通仍是非目标。真正缺的是**按端口的访问控制** —— 沙箱上的任何端口,
只要能连到 proxy 就能访问(api-design.md §3.4)。

## 2. 总体架构 ⚠️

```
                        ┌──────────────────────────────────────┐
  SDK (py/ts) / CLI ───▶│  Control Plane                       │
                        │  ├── api-gateway   REST/gRPC、鉴权、  │
                        │  │                 配额、端口反代     │
                        │  ├── scheduler     节点选择：镜像亲和  │
                        │  │                 + bin-packing     │
                        │  ├── state store   SQLite：sandbox   │
                        │  │                 元数据、节点租约    │
                        │  └── image-service 镜像元数据、       │
                        │                    prewarm 编排、GC   │
                        └──────────┬───────────────────────────┘
                                   │ ↓指令 push 直连 gRPC / ↑心跳·状态上报（流）
              ┌────────────────────┼────────────────────┐
              ▼                    ▼                    ▼
        ┌──────────┐         ┌──────────┐         ┌──────────┐
        │ noded    │         │ noded    │         │ noded    │   ← 每节点一个
        │ (裸金属) │         │ (云 VM)  │         │ (裸金属) │      node daemon
        └────┬─────┘         └──────────┘         └──────────┘
             │ overlaybd 直驱 + noded 自管 FC 与容器 runtime;不依赖 containerd
        ┌────▼─────────────────────────────┐
        │  ├── 镜像: overlaybd + TCMU       │ ← 块级 lazy-pull from S3
        │  └── runtime: fc(默认)│runc│runsc │ ← 内部自动分档（D3）
        └────┬─────────────────────────────┘
             │
        ┌────▼──────────────────────┐
        │ sandbox                    │  fc: microVM（vsock 通 agent）
        │  └── beand (init/PID1)│  container: runc/runsc（其 netns 内的 TCP）
        │      └── 用户进程           │  agent: exec/PTY/文件/端口转发
        └───────────────────────────┘

        S3 ◀── 镜像 blob（source of truth）/ eval 产物 / snapshot / 卷后端
```

### 2.1 组件职责 ⚠️

| 组件 | 语言 | 职责 |
|---|---|---|
| `api-gateway` | Go | ✅ REST + gRPC API、鉴权、配额（端口反代由 bean-proxy 承担,可合部） |
| `scheduler` | Go | 节点选择（镜像亲和 + 资源 bin-packing）、租约管理——**control plane 逻辑模块**（`internal/control/scheduler`,与 bean-api 同进程:调度决策与事务扣量、指令下发需原子完成;成为瓶颈或需选主时再拆） |
| `image-service` | Go | 镜像元数据索引、格式转换编排、prewarm、S3 blob GC（control plane 逻辑模块，P0–P2 内嵌 bean-api） |
| `bean-proxy` | Go | ✅ 进入 sandbox 的反向代理。从 Host 读 `{port}-{sandbox}`,查出沙箱所在节点后转发。用户暴露的端口和 agent 自己的接口走同一条路——端口暴露和数据面是一个机制而非两个。不做用户认证(外部层负责,见 A7),拒绝绑公网地址。TLS 与 DNS 属于托管层,不在 bean 内 |
| `noded` | Go | 节点 daemon：sandbox 生命周期、网络、镜像缓存、卷挂载、健康上报 |
| `beand` | Go（静态编译） | sandbox 内 PID1：exec、PTY、文件读写、端口转发 |
| `sdk-python` | Python | evaluation/rollout 侧主 SDK |
| `sdk-ts` | TypeScript | Web/Node 侧 SDK |
| `cli` | Go | `bean` 命令行：sandbox 管理、镜像预热、调试 |

## 3. 核心设计决策

### D1. 镜像零转换，容器与 microVM 双形态 ⚠️

任意 OCI 镜像直接作为 sandbox 环境，消除 e2b 式 template 转换。镜像经 overlaybd
组装为块设备（见 D4），既能给容器档做 overlayfs rootfs，也能 virtio-blk 直挂
microVM（见 D9）——两种形态共享同一条镜像链路，用户无感。

### D2. overlaybd 直驱,无 containerd 热路径 ⚠️

> **「无 containerd」已达成,「overlaybd 直驱」未达成。** 当前后端是 dm-snapshot:
> 拉全量 + 转换 + 共享只读 base + 每 sandbox CoW(实测 44 KiB/sandbox)。
> overlaybd 能力已在 tcmu 后端实测跑通但未接入 `image.Provider`。

fc 主路径**不引入 containerd**（AgentENV 同款,其源码已在本地 /Users/mac/project/agentenv
可参考）：noded 直接驱动 overlaybd（经 TCMU）组装块设备（S3 backing + 本地
缓存）→ virtio-blk 挂 microVM。containerd 的三项职责在本设计中均有更直接的替代：

| containerd 职责 | 本设计 |
|---|---|
| 镜像拉取/content store | blob 在 S3（image-service 离线转换）,元数据控制面下发;registry 不在热路径 |
| snapshotter | overlaybd 直驱（经 TCMU 暴露块设备；AgentENV 的 uvm-ublk 实证） |
| task 生命周期 | fc:noded 自管 FC 进程;容器档:noded 直驱 runsc/runc（无 containerd,见下） |

> **已修正。** 容器档**不使用** containerd。这一段写于 overlaybd 尚未接入
> `image.Provider` 之时;现在它接入了,再引入 containerd 会带来自己的 content store
> 和 snapshotter,让节点出现两套互不知晓的镜像体系。这一档直驱 runsc/runc,
> 并复用 fc 的 rootfs provider —— 见 D3。
>
> 具体的阻塞点是查证过的,不是推断:overlaybd 的 containerd snapshotter 要求镜像位于
> registry 且 manifest 带 `containerd.io/snapshot/overlaybd/version` 注解,
> 而 bean 发布到 S3 的是裸 blob 前缀,两者都没有。

原本的理由仍然成立,故保留:runc 生命周期与 overlayfs 组装不值得自研,
纯 fc 节点可完全不装 containerd。runtime 抽象接口见 noded-design §3。

### D3. 隔离分档 + 节点能力探测 ⚠️

noded 启动时探测节点能力并上报：

```
├── /dev/kvm 可用（裸金属 or 嵌套虚拟化 VM）→ [runc, runsc, fc]
└── 无 KVM（普通云 VM）                     → [runc, runsc(ptrace)]
```

**runtime 档位是内部机制，不对外暴露**——用户不选隔离级别（overlaybd 让 fc 覆盖
全部普通场景后，container 档只剩内部用途）。调度器自动分档：

```
分档规则（调度器内部）：
  KVM 节点（常规情况）    → fc（Firecracker microVM，默认主档，见 D9）
  无 KVM 节点             → runsc（gVisor 降级档;部署上应尽量避免此类节点）
  GPU 任务（内部预留）     → runc + nvidia（FC 无 GPU passthrough）
  内部白名单任务           → runc（显式内部标记，不经公开 API）
```

- **fc**：隔离最强、snapshot/fork 原生、guest 真内核无 syscall 兼容性问题
- **runsc**：已实现 ✅ —— `--runtime runsc`，真机端到端验证通过
- **runc**：同一份实现，`--runtime runc`。共享宿主内核，因此用于可信任务或 GPU，而非不可信代码
- ~~kata~~：被 fc 取代，不再引入

API 请求不含 isolation 字段（内部 proto 保留枚举，便于运维强制指定）;
sandbox 详情返回实际档位（`runtime: fc|runsc|runc`）供排障。
scheduler 按节点能力匹配。

#### 容器档的实际实现 ✅

一份实现、两个二进制：runsc 与 runc 都是 OCI runtime，接受相同的 bundle 和子命令，
所以节点用哪个是配置而非第二条代码路径。`internal/node/runtime/oci_linux.go`。

它复用**与 fc 相同的 rootfs provider**（`selectProvider`），因此同一节点上的容器与
microVM 共享镜像缓存、已转换的 overlaybd 层和对象存储。这种共享正是「直驱 OCI runtime
而不引入 containerd」的理由 —— containerd 会带来自己的 content store 和 snapshotter，
让节点出现两套互不知晓的镜像体系。这也修正了 D2 的结论，那段写于 overlaybd 尚未接入
`image.Provider` 之时。

实测,两个二进制、同一台机器、同一套 14 项端到端(`hack/oci-tier-e2e.sh`):

| | 冷启动 create | 稳态 |
|---|---|---|
| runsc | 22.2s | 0.9s |
| runc | 20.8s | **0.7s** |

冷启动那部分是 overlaybd 转换,fc 档同样要付;稳态两者都与 fc 已发布层的 0.8s
在噪声范围内。runc 略快是因为少了 Sentry 的启动开销 —— 那正是它换来的隔离的代价。

两个都真跑了,不是跑一个然后推断另一个:「一份实现、两个二进制」是关于行为的断言,
而 runc 与 runsc 的差异(宿主内核、无 Sentry、自己的 netns 处理)足以让其中任何一步失效。

**并发,实测。** `hack/oci-tier-concurrent.sh`:

| | |
|---|---|
| 5 个并发 create | 5/5,约 4s |
| 30 个并发 create | 25/30,墙钟 9.6s(最快 2.0s、中位 4.7s、最慢 9.6s) |

对照 0.9s 的稳态 create,25 个若完全串行需 22.5s,所以存在约 2.3 倍并行度而非完全串行。
分阶段指标指出剩下的时间去了哪:

```
network_setup   158.6s / 31 = 每次 5.1s   <- 占总时长 78%
runtime_create   43.2s / 31 = 每次 1.4s
agent_ready       1.8s / 28 = 每次 0.06s
```

`network_setup` 在单个 create 时是 0.165s,并发下变成 5.1s —— 涨了 30 倍,这就是并行度
受限的原因。根源是 xtables 锁:每个 create 要插 5 条 iptables 规则,而 iptables 通过
一把「每表一锁」串行化,那把锁还与宿主上其他写 iptables 的东西共享。`-w` 让这些 create
排队而不是失败(在它之前,5 个并发只成功 1 个),但排队仍然是串行化。

修法(本次未做)是不要每个 sandbox 取五次锁:`iptables-restore` 能在一次事务里应用整套规则。
那是网络层的改动、两个 tier 共用,应当单独度量,而不是塞进一次 runtime 变更里。

**四条约束，没有一条会给出点明原因的报错。** 每条都是驱动真实系统才发现的，
且都在代码里连同理由一起固化：

1. **unix socket 不穿透 bind mount。** 容器内 bind 成功，宿主永远看不到那个 socket ——
   gVisor 在 Sentry 内部实现 unix socket。而写到同一挂载点的**普通文件确实可见**，
   所以这是 socket 特有的。因此 agent 监听 TCP，node 经 sandbox 的网络命名空间拨入 ——
   用的是 `dial.go` 早已有、`portforward.go` 早已在用的 `netns:` 传输。
2. **地址要用 veth 的，不是 `GuestIP`。** `GuestIP` 是 guest 内核在 tap 上配的地址，
   而容器没有 guest 内核：tap 保持 DOWN，那个地址在任何地方都不存在。拨它得到
   「network is unreachable」，因为宿主把它按默认网关解析了。
3. **runsc 需要 `--network=host`。** 它默认的用户态网络栈（netstack）接管了 veth，
   于是 agent 打印 `beand listening` 而命名空间内 `ss` 看不到任何监听。安全代价见下。
4. **agent 在 TCP 上强制要求 token，而 node 必须为它提供。** `cmd/beand` 把这个要求
   绑定在传输上而非某个 flag，因为 TCP 地址**从 sandbox 内部可达**。期望的 hash 来自
   169.254.169.254 上的 metadata service —— Firecracker 提供它，容器没有 ——
   所以这一档在命名空间内跑一个**每 sandbox 独立**的实现（`oci_mmds_linux.go`）。
   不共享：那份文档里是某一个 sandbox 的凭据。

**`--network=host` 放弃了什么，明说。** netstack 是 gVisor 的隔离边界之一，
host 网络放弃了它：sandbox 使用宿主内核的网络栈，因此那里的漏洞从内部可达。
保留下来的是**整个 Sentry** —— 文件系统与进程类系统调用两种模式下都被拦截 ——
并且 sandbox 仍限制在自己的网络命名空间里，只看到一对 veth 而非宿主的接口。
替代方案是让 node **穿过** netstack 到达 agent，那意味着转发端口或代理 socket；
两者都是额外工作量，且都不明显优于「接受一个命名空间范围内的网络栈」。

**未实现**：`Checkpoint` 与 `Fork` 返回 `ErrCheckpointUnsupported` ——
用独立错误，使调度器能区分「这一档做不到」与「这次尝试失败了」。CRIU 是独立工作量，
而 warm snapshot 是 fc 的吞吐杠杆，容器档不是它的替代品。同样未验证的是 **GPU**：
它的记账在 proto、调度器和 API 里都齐全，而 `internal/node/` 从不设置 `GpuCount` ——
所以每个节点都上报 0，那条路径目前是死代码。gVisor 完全不支持透传，
那正是 runc 存在的用途。

### D9. Firecracker 主档：容器 rootfs 直挂 microVM ✅

FC 档**不是**嵌套容器（Kata 式 guest 内再跑 containerd），而是 rootfs 直挂：

```
overlaybd 组装镜像块设备：base 层（lazy-pull S3）+ overlaybd 可写层，
  在宿主侧合成【单一块设备】（业界一致做法：e2b/AgentENV 均 host 侧组装）
  → virtio-blk 挂给 microVM（guest 见一块盘）+ agent 盘（只读，见 D5）
  → guest 内 beand 作为 init：挂载 /proc /sys /dev 等（按 OCI 默认
    mounts 复刻）、应用 image config（ENV/USER/WORKDIR/Entrypoint+Cmd）
    拉起用户进程
```

宿主侧单设备的收益：磁盘配额在宿主执行（可写层文件大小即上限）、snapshot
disk-diff 直接取宿主 overlaybd 可写层、guest 内零 union 复杂度。

- guest 内无容器层，"容器"只剩镜像格式；镜像零转换的承诺不变
- 兼容性：ENV/ENTRYPOINT/CMD/WORKDIR 在镜像转换时记录在镜像旁，启动用户进程时
  与创建请求合并（规则见 [image-pipeline.md](image-pipeline.md) §5）；USER 已记录但尚未生效。
  guest 是完整真实 Linux 内核，兼容性优于 gVisor 模拟层。唯一差异：内核
  由平台统一打包提供（非宿主内核），对纯用户态 eval 负载无感。
  详见 noded-design.md fcRuntime 节
- agent 通信走 vsock（transport 抽象；容器档同协议但走 netns 内的 TCP，见 D3）
- 网络：tap 设备接入节点 bean0 桥，nftables 规则与容器档一致
- 该路线已被 AgentENV（Kimi K3 训练基础设施）在生产验证；实现参考其
  overlaybd+ublk 集成与 snapshot 设计

### D4. S3 为统一存储 backend ⚠️

| 数据 | 方案 |
|---|---|
| 镜像 blob | **overlaybd 块级镜像**（层 = 块设备 diff）直存 S3，节点经 TCMU 暴露、按需 range-read；registry 仅存元数据 |
| 节点缓存 | 本地 NVMe 作为 S3 之上的块 chunk LRU 缓存；裸金属（大盘）与云 VM（小盘）仅命中率差异，架构统一 |
| eval 产物 | agent/noded 经 presigned URL 直推 S3（control plane 签发，节点不持长期凭证） |
| 大文件下载 | API 返回 presigned URL 重定向，不过 gateway 转发 |
| 快照（P3–P4） | FC memory snapshot / rootfs diff 落 S3，支持跨节点 **restore**（在任意节点造出一个新 sandbox;resume 是同进程同节点的,见 snapshot-resume.md §0） |
| 卷 | shared-fs 卷后端（JuiceFS on S3）宿主挂载 + nfsd 导出（见 D10）;dataset 卷预留 |

选 overlaybd（块级，DADI/阿里，AgentENV 已在 FC 场景验证）而非 Nydus（文件级）的关键原因：**块设备链路同时服务容器档（overlaybd-snapshotter → overlayfs）与 microVM 档（virtio-blk 直挂 guest），一条镜像链路通吃全部 runtime 档位**；Nydus 的文件系统语义进不了 microVM，FC 档需另走 virtiofs（FC 支持弱）。Nydus 保留为容器档备选。

热状态（sandbox 元数据、租约、调度状态）落关系库,不进 S3。引擎由 `bean-api --postgres`
是否给出决定:SQLite(`modernc.org/sqlite`,纯 Go 无 cgo,`SetMaxOpenConns(1)` 单写)
适合单机;多副本控制面需要 Postgres —— SQLite 是一个文件,两个副本没法共享它。

第二个引擎是一层方言,不是第二套实现:一套用 `?` 写的语句,按引擎改写。这个选择基于实测
(103 处占位符加少数 DDL 构造,八条 `ON CONFLICT` 全部原样可移植),而不是基于口味 ——
两套必须保持一致的 SQL、再配一个只能事后告诉你哪一套漂了的套件,是更糟的处境。

真正让换引擎成立的不是接口,而是原子性放在哪里。每个操作的条件都在它自己的语句里,
由数据库裁决而不是进程内的锁;store 里已经没有任何 mutex。进程内的锁本来就无法为
第二个副本的写入定序,而它还在的时候掩盖了一个真实的丢更新 bug。

### D5. Agent 注入：init/PID1 override（不进用户镜像）✅

eval 镜像任意、不可假设内含工具链。注入方式按档位：

| 档 | 注入 | 通信 |
|---|---|---|
| fc（默认） | **agent 盘**：含 beand 的只读小盘（ext4）作为附加 virtio-blk，guest 内核 init=盘内 agent | vsock + gRPC |
| 容器档 | overlaybd 设备挂成目录 + agent 作 PID1 | netns 内 TCP + gRPC |

共同点：用户镜像零修改;原 entrypoint/cmd/env/user/workdir 序列化进 spec，
由 agent 按 Docker 语义托管拉起（详见 noded-design.md §3.1/§6）。
不走 CRI streaming exec：性能差、无文件 API、依赖长链路。

### D6. 网络：节点内 NAT，取裸金属/云 VM 最大公约数 📐

> **未实现**。sandbox 当前没有网络栈,见 noded-design §5。

```
sandbox netns ←veth→ 节点 bridge → SNAT 出网
```

- 每 sandbox 独立 netns，节点本地私有网段（如 10.100.x.0/24 per node）
- 默认策略：允许出网（拉依赖），禁止访问节点内网/元数据服务（169.254.169.254 等），sandbox 间互相隔离（nftables）
- 端口暴露：`{sbxId}-{port}.{region}.sandbox.<domain>` → regional proxy → noded sbxproxy → 直连 sandbox IP（agent ForwardPort 仅兜底）,绕开云厂商 MAC/IP 白名单限制
- 不依赖 underlay/BGP，两种节点行为完全一致

### D7. 调度：镜像亲和优先的 bin-packing ✅

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

**调度流程**（两级：先 region 后节点;batchCreate 在一次锁内顺序执行同流程）：

```
0. Region 选择：显式 region 参数 > 卷/snapshot 数据亲和（强制） >
   镜像 blob 已复制的 region > 容量余量
1. 过滤（region 内,硬约束）：
   nodeSelector 标签匹配（如 pool=gpu-a100）
   isolation 解析（auto→fc/runsc/runc）→ 节点能力匹配
   cpu/mem/disk 承诺量 + 请求 ≤ allocatable;GPU 整卡余量
   节点状态 = READY（SUSPECT/LOST/DRAINING 排除）
2. 打分（加权和，权重可配）：
   w1·镜像亲和：该镜像 overlaybd 块在节点缓存的字节占比（心跳带 bloom+字节数）
   w2·资源平衡：装箱后碎片度（优先填满，留大块空位给大规格）
   w3·缓存盘类型：冷镜像 → NVMe 大缓存节点加分
   w4·打散：同 label（同一 eval run）适度反亲和，避免单节点故障吞掉整批

3. 提交:事务内扣承诺量 + 写指令记录 → push 直连 noded.CreateSandbox（见 api-design §5.1）
4. 失败回退:节点报 FAILED（如 ENOSPC 竞态）→ 释放承诺量,重调度(≤3 次,
   排除失败节点),仍失败 → NO_CAPACITY 返回调用方
```

**记账一致性**：承诺量以数据库为准（调度器重启可重建内存态）;节点心跳
实际用量仅用于告警与 balloon 决策，不参与准入——避免「实际水位准入」在
突发负载下超卖爆炸。

**抢占**：不做。eval 任务同质、短生命周期，排队（NO_CAPACITY + 客户端重试/
排队池）比抢占简单且足够。

### D8. 故障模型：租约 + 无状态重建 ✅

- noded 定期心跳续约；租约超时 → 节点标记失联 → 其上 sandbox 标记 `lost`
- eval 任务无状态，上层（SDK/调用方）收到 `lost` 后重建即可
- noded 重启后 reconcile：对账本地实际状态（存活 FC 进程 ∪ containerd task,如启用）vs control plane 期望状态（SyncState）
- GC：idle 回收（lifecycle.onIdle 驱动）、镜像块 LRU 淘汰、孤儿 tap/netns/挂载清理

### D10. Volume：独立于镜像的一等数据资源 📐

镜像=环境（不可变，随 sandbox 生灭），卷=数据（独立生命周期，跨 sandbox 留存，
可多挂）。两种类型：

| 类型 | 后端 | 数据面 | 场景 |
|---|---|---|---|
| `shared-fs`（首期） | 宿主挂载 JuiceFS（on S3）/CephFS/本地盘 | **宿主内核 nfsd 导出**（e2b 同款路线）：guest 用内核 NFS client 挂宿主内部地址，流量不出节点 | 持久工作区、跨 sandbox 共享读写 |
| `dataset`（预留，暂不排期） | overlaybd 只读块（复用镜像管道） | 容器档 bind mount;fc 档附加 virtio-blk | 数据集/权重海量只读消费 |

shared-fs 走宿主 NFS 而非 guest 内跑分布式 FS 客户端的原因：guest 零凭证零
额外二进制、`none` 网络策略天然兼容（NFS 目标是宿主网关，与出公网正交）、
宿主客户端缓存全 sandbox 共享（eval 同批读同数据时命中率高）、后端可换。
详见 noded-design.md §3.3。

### D11. 多区域（Region/Cell）与 BYOC 📐

**控制面全局一份，数据面按 region 自治。** Region = 故障域 + 数据域 + 转发域：

```
Global Control Plane（bean-api / scheduler / 关系库,镜像元数据全局 digest 索引）
   │ 托管 gRPC 接入层(TLS)+node token,noded/proxy 出向连接
   ├── Region A：noded 节点池 + regional proxy ×N + region S3 backend
   └── Region B（BYOC）：客户节点 + 客户 S3,数据不出客户环境
```

- **数据域**：镜像 blob/产物/snapshot/shared-fs 卷后端全部 region 内闭环;
  节点只读本 region S3,跨 region 流量仅发生在镜像 blob 复制,不在热路径
- **镜像复制**：元数据全局唯一（digest）,blob 按 region 存;转换一次写源
  region,其他 region **按需复制**（首次调度到该 region 时拉取）+
  **prewarm 显式复制**（`POST /images/prewarm` 带 `region` 参数,eval 批次前用）
- **卷的数据引力**：卷有 region 归属,挂已有卷的 sandbox 强制落卷所在 region
- **转发域**：每 region 一组 proxy,域名 `{sbxId}-{port}.{region}.sandbox.<domain>`
  （DNS 直达 region proxy,无全局中转）
- **BYOC**：客户提供节点 + S3（+可选自有域名）,hosted control plane;
  控制面只见元数据,不持客户 S3 长期凭证——presigned/STS 由部署在客户侧的
  轻量 token 服务签发;noded/proxy 出向 443 连托管接入层 + bootstrap token
  注册（registration-only,可配人工 approve;详见 noded-design §7.0）
- **节点归属**：`region` 为一级字段（配置声明,Register 时控制面校验该 region
  已注册,生命周期内不可变）;`labels` 为自由标签（pool/disk/tenant 等）,
  调度请求经 `nodeSelector` 约束——GPU 池、BYOC 专属节点等用标签,不加字段
- **接入与身份**：控制面经云上托管 gRPC 接入层暴露（网关终结 TLS,节点
  零证书配置）;节点身份用应用层 node token（短期、内存持有、绑定 nodeId）,
  不引入 mTLS——顺应现有基建,BYOC 出向 443 即通
- **故障域**：region 失联 → 该 region sandbox 标 LOST,其他 region 无感;
  全局控制面单点首期接受（控制面故障不影响存量 sandbox 数据面,仅停新建）,
  控制面多活为 P5 储备

## 4. API 设计 ⚠️

### 4.1 REST API（对外）⚠️

```
# Sandbox 生命周期
POST   /v1/sandboxes                 # image, resources, env, lifecycle
                                     # (idleTimeout/onIdle), labels → sandbox
GET    /v1/sandboxes/{id}
GET    /v1/sandboxes?label=k=v       # list + filter
DELETE /v1/sandboxes/{id}
PATCH  /v1/sandboxes/{id}/lifecycle  # idleTimeout / onIdle 运行时调整

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

# 生命周期扩展 / 批量 / 卷 / 快照 / 日志（完整定义见 api-design.md）
POST   /v1/sandboxes:batchCreate     # 批量创建（eval 高频）
POST   /v1/sandboxes/{id}/pause|resume|snapshot|fork|start
CRUD   /v1/volumes                   # shared-fs 卷（dataset 预留）
CRUD   /v1/snapshots
GET    /v1/sandboxes/{id}/events + WS /v1/events   # 生命周期事件
GET    /v1/sandboxes/{id}/logs
```

### 4.2 内部 gRPC ✅

- `control ↔ noded`：`NodeService`（Register/Heartbeat/SyncState）+ `SandboxService`（noded 实现，control 直连调用：Create/Destroy/Pause/Snapshot/Exec 转发/…）
- `noded ↔ agent`：`AgentService`（Exec/StreamExec/ReadFile/WriteFile/ListDir/ForwardPort/…;容器档走其 netns 内的 TCP、fc 档 vsock）

proto 定义统一放 `proto/`，生成代码进各语言 SDK。

### 4.3 Sandbox 状态机 ✅

```
PENDING → SCHEDULED → PULLING → STARTING → RUNNING → STOPPING → STOPPED
                                    │          │
                                    └── FAILED ┘        RUNNING ─(租约丢失)→ LOST

RUNNING ─pause→ PAUSED ─resume→ RUNNING      ← 同一个 sandbox、同一个 id
RUNNING/PAUSED ─snapshot→ SNAPSHOTTING → (回原状态)
(from snapshot) PENDING → SCHEDULED → RESTORING → RUNNING
                             ↑ restore 产出的是一个**新** sandbox、自己的 id,
                               而一份快照可以同时驱动 N 个这样的流程

snapshot 对象独立状态机：CREATING → READY → DELETING（任一 RESTORING 持有引用计数
期间不可删 —— 是计数而非标志位,因为同一快照被并发 restore 是常态）
```

resume 与 restore 是作用在不同对象上的不同操作:resume 把一个已存在的 sandbox 送回
RUNNING,restore 造出另一个。见 [snapshot-resume.md](snapshot-resume.md) §0。

`DELETE /sandboxes/{id}` 返回 202 后异步走 STOPPING → STOPPED（终态记录保留
30 天后归档,见 noded-design GC）;`?force=true` 跳过 graceful 直接 kill。

详见 [snapshot-resume.md](snapshot-resume.md)。

## 5. 冷启动路径优化 ⚠️

目标：P50 < 2s（镜像已缓存）/ P50 < 10s（lazy-pull 冷镜像）。

1. **lazy-pull**：overlaybd + TCMU 块级按需加载，启动只需元数据 + 热块，运行中按需 range-read S3
2. **节点缓存**：chunk 级 LRU，S3 为 source of truth，节点盘可随意 GC
3. **prewarm API**:评测批次开始前预热镜像到目标节点
4. **镜像亲和调度**：天然提升缓存命中
5. **agent 常驻热路径**：agent 静态二进制（容器档 bind mount / fc 档 agent 盘），无镜像内安装步骤

## 6. 安全模型 ⚠️

- 默认 fc（Firecracker microVM，硬件虚拟化边界）运行不可信代码;无 KVM 节点降级 runsc
- ✅ fc 档的宿主侧收束已经做了:VMM 降到非特权 uid(`--fc-vmm-uid`)、跑在每 sandbox
  的 cgroup 里(内存上限、CPU 配额、pid 上限,`--fc-cgroups`),并默认拥有自己的
  pid、mount 与 network 命名空间。Firecracker 内置的 seccomp 叠在这些之上,而非替代它们
- ❌ ~~jailer~~ 不再计划引入。上面的命名空间、cgroup 与 uid 下放都已具备,
  jailer 额外带来的只是 `chroot` 和设备白名单 —— [#20](https://github.com/garysng/bean/issues/20)
  phase 2,且大概不是对的形态。记为「已放弃」而不是「待做」,这样它不再被读成缺口
- ✅ 容器档已实现(D3):`--runtime runsc|runc`
- ⚠️ sandbox 之间的网络策略未实现。每个 sandbox **确实**有自己的命名空间、tap 与出网,
  且元数据网段与 RFC1918 默认拒绝(network.md) —— 缺的是**按端口的访问控制**,
  所以能到 bean-proxy 的东西可以访问 sandbox 的任意端口([#50](https://github.com/garysng/bean/issues/50))
- ⚠️ 节点当前经环境变量拿 S3 凭证;presigned URL / STS 轮换未实现
- API 鉴权：API key（调用方识别+配额;不做用户/租户体系——集群内部服务）

## 7. Repo 结构 ⚠️

```
bean/
├── proto/                  ✅ gRPC 定义（single source of truth）
├── cmd/
│   ├── bean/               ✅ CLI 入口
│   ├── bean-api/           ✅ gateway（内嵌 scheduler / image / snapshot 模块）
│   ├── noded/              ✅ node daemon
│   ├── beand/              ✅ sandbox 内 agent
│   └── bean-proxy/         ✅ 进入 sandbox 的反向代理(按 Host 路由)
├── internal/
│   ├── control/            ✅ api / scheduler / store / snapshot / s3
│   ├── node/               ✅ manager / runtime / image / vsock（无网络模块）
│   ├── beand/              ✅ sandbox 内 daemon 实现
│   ├── obs/                ✅ OTel tracing + gRPC 拦截器
│   ├── logging/            ✅ slog 结构化日志
│   └── gen/                ✅ protoc 产物
├── cli/                    ✅ CLI 实现
├── sdk/
│   ├── python/             ⚠️ 手写 httpx,非 codegen;覆盖面见 sdk-cli-design §2
│   └── typescript/         📐 未实现,目录不存在
├── hack/                   ✅ build-assets / dev-fc-stack / cpu-template-probe / tracedump
├── tests/e2e/              ⚠️ 6 个功能测试,跑 local 档;无规模压测
├── deploy/                 📐 不存在
└── docs/
```

注:`internal/store/` 不存在,store 在 `internal/control/store/`。

## 8. 实施路线

详见 [roadmap.md](roadmap.md)（单一维护处）。概要：**P0 即 fc 直启**（overlaybd
直驱 + FC + agent,参考本地 AgentENV 源码）→ P1 多节点可用 → P2 生产化
（lazy-pull/prewarm/调度亲和）→ P3 交互/proxy/pause/shared-fs 卷 → P4 snapshot
完整形态 → P5+ 储备（容器档 GPU 路径按需）。

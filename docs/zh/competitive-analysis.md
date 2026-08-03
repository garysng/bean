# 竞品分析（2026-07,快照部分 2026-08 补充）

> 视角：bean 的目标场景 = AI evaluation / agent rollout，特点是**大量异构 Docker 镜像**
> （SWE-bench 类 2000+ 镜像）、批量拉起、短生命周期、自主可控部署。

> **增量快照的选型对照**(2026-08 调研,已落地)见 `docs/decisions.md` §3.0.1。
> 结论摘要:E2B 在 UFFD 缺页时分层查找 base + N 层 diff,公开分析指出
> cross-build fragmentation 随深度增长;Cognition blockdiff 把链只当血缘、
> 运行前 flatten 成 raw(靠 XFS reflink 使 flatten 近乎免费);
> Firecracker 上游 `snapshot-editor rebase` 也是 flatten。
> bean 选 flatten,额外理由是 snapCache 让 fan-out 场景每节点只付一次合并,
> 且缺页路径零改动。

## 1. 逐家分析

### Tensorlake（tensorlakeai，2026 转型）⭐ 与 bean 同层最接近的商业实现

原文档处理/RAG 项目（indexify）已转型为 *sandbox-native cloud for AI agents*，三块产品：
Sandboxes（Firecracker microVM）、Cloud Volumes（内容寻址版本化文件系统）、
Orchestrate（`@application`/`@function` serverless 编排,每 function 独占 sandbox）。

- **隔离**：Firecracker microVM（非容器）;内存+文件系统快照、instant clone、
  auto suspend/resume、live migration、预热池、egress allow/deny、
  `https://<port>-<sandbox>.sandbox.tensorlake.ai` ingress——与 bean 的 D9/D11/
  lifecycle 设计高度重合
- **技术栈**：Rust（CLI/SDK/FUSE 客户端）+ Python/TS SDK;调度器 Lattice、
  自研分布式 SQL 元数据库 Orion（Apache-2.0）
- **开源边界**：主仓 Apache-2.0 但**仅 SDK/CLI/FUSE 客户端**;服务端/控制面闭源,
  云服务不可自托管
- **活跃度**：~976 star,日更,商业化已上线
- **对 bean 的意义**：功能面最接近的对标（含卷、快照、ingress、编排）,
  但闭源+不可自托管——这正是 bean「自主可控 + BYOC」的立足点。三个可借鉴点：
  1. **`oci2rootfs`**（Apache-2.0,Rust）:OCI → ext4 rootfs,whiteout/opaque/xattr
     处理完整,但全量预物化无 lazy load——可作 bean **未转换镜像的 fallback 转换器**
     （overlaybd 直挂仍是主路径,性能更优）
  2. **镜像即快照**：任意 sandbox 快照可 `register` 成命名镜像,对「装环境一次、
     批量复用」场景实用（见 roadmap P4）
  3. **`harbor`**（同 org）:agent evaluation / RL environment 框架,正是 bean 的
     目标场景,API 形状值得对照

### AgentENV（kvcache-ai / Kimi，2026-07 开源）⭐ 最直接的对标

为 Kimi K3 的 agentic RL 训练而建，与 bean 的目标场景（批量异构镜像 + RL rollout）几乎重合：

- **隔离**：Firecracker microVM per sandbox
- **环境**：✅ 任意 OCI 镜像零转换——**overlaybd + ublk 块级按需加载**，本地盘做有界缓存，镜像总量可超磁盘容量;snapshot 可落 S3
- **snapshot/fork**：resume <50ms、pause <100ms、增量快照 <100ms;单节点 fork 16 子实例;virtio-balloon 内存超卖
- **API**：E2B 兼容 HTTP API（存量 E2B SDK 换 endpoint 即用）+ 反向代理
- **成熟度**：单机路径经 Kimi 生产验证;**多节点控制面官方标注 prototype**
- **对 bean 的意义**：验证了「overlaybd 块设备直挂 FC + 任意 OCI 镜像」整条技术路线（bean D4/D9 已采纳同路线）;其弱项（多节点调度、配额、prewarm 编排、运维面）恰是 bean 自研的重点

### CubeSandbox（腾讯云，2026-04 开源，Apache-2.0）

- **隔离**：RustVMM/KVM 自研 hypervisor（改造 Cloud Hypervisor/Kata 组件），Rust 全栈
- **环境**：❌ image→template 转换路线（e2b 同款），非任意 OCI 直启——不解决批量异构镜像痛点
- **冷启动**：60ms（资源池化 + 快照克隆;50 并发 P95 90ms）;内存开销 <5MB/sandbox
- **snapshot**：CubeCoW 引擎——checkpoint/回滚/fork;AutoPause/AutoResume
- **组件**：CubeAPI（E2B 兼容）/CubeMaster/Cubelet/CubeVS（eBPF 网络隔离）/CubeEgress（L7 出口网关：域名过滤、凭证注入、审计）
- **成熟度**：腾讯云生产验证,完整多节点集群能力
- **对 bean 的意义**：template 路线不适配 eval 场景,但 **CubeVS 的 eBPF 网络隔离与 CubeEgress 的 L7 出口治理**（凭证不进 sandbox）是 bean P5 网络演进的参考设计

### e2b（e2b.dev）

- **隔离**：Firecracker microVM
- **环境**：❌ 不能直接跑 OCI 镜像。Dockerfile 只是构建输入，`e2b template build` 转换为 VM rootfs 快照注册进模板库，首次构建 **5–15 分钟**；历史上有大镜像构建失败问题（>4.3GB）
- **冷启动**：模板快照恢复 ~150–200ms（模板已就绪的前提下）
- **pause/resume**：公测，FC snapshot（pause ~4s/GiB、resume ~1s、保留 30 天）
- **开源**：Apache-2.0，可自托管（1 orchestrator + 2 host 起步）
- **定价**：按秒计费 ~$0.05/vCPU·hr；Pro $150/月起
- **对 eval 场景**：2000 镜像 = 2000 次 template build，完全不可行。**这正是 bean 立项的直接原因**

### Daytona（daytona.io）

- **隔离**：默认 Docker 容器（共享内核，无 microVM/gVisor 档）
- **环境**：⚠️ 最接近——"Snapshot" 可直接从任意公私 registry 的 OCI 镜像创建，但仍有一步注册转换,且限 AMD64
- **冷启动**：宣称 sub-90ms（预热池可至 27ms）
- **snapshot**：支持 fork/hibernate
- **开源**：AGPL-3.0 可自托管（license 对商业集成有传染性顾虑）
- **对 eval 场景**：镜像语义最顺，但容器共享内核**无强隔离档**，跑不可信 AI 代码需要外围补偿；批量镜像分发无 S3 lazy-pull 类机制

### Modal Sandboxes

- **隔离**：gVisor
- **环境**：❌ 不接受任意 OCI 直启;须 SDK `Image.from_registry` 用自研构建器重建,要求镜像内有 Python(或注入),ONBUILD/VOLUME 等指令不支持,常需清 entrypoint
- **冷启动**：亚秒（自研 lazy-load 文件系统,压测 1000 sandbox/s）
- **snapshot**：FS 快照 + 内存快照(Alpha)
- **开源**：❌ 闭源,不可自托管
- **对 eval 场景**：基础设施能力最强,但闭源 + 镜像重建约束,不满足自主可控

### Morph Cloud（morph.so）

- **隔离**：自研 MorphVM（microVM）
- **环境**：❌ 无 OCI 直启。从最小基础镜像链式 `.setup()` 构建快照；容器只能作为 VM 内二等公民
- **snapshot**：✨ 最强项——Infinibranch：运行中 VM 任意时刻快照+即时 fork 多分支，近零存储开销
- **开源**：❌ 闭源；MCU 计量定价
- **对 eval 场景**：分支探索能力是标杆（bean FC 档的对标对象），但镜像模式与批量 eval 完全不匹配

### microsandbox（开源，Super Rad Company）

- **隔离**：libkrun microVM（KVM/HVF）
- **环境**：✅ 直接跑任意 OCI registry 标准镜像，零转换——**证明「OCI 直启 microVM」技术可行**
- **冷启动**：宣称 <200ms（实测 Linux ~320ms）
- **snapshot**：❌ 无产品化 pause/resume（pre-1.0）
- **开源**：Apache-2.0 全自托管；云服务 closed beta
- **对 eval 场景**：方向对但偏本地单机开发工具形态，无集群调度/批量分发/多节点故事；可作为 fcRuntime 的实现参考（libkrun 路线 vs 裸 FC）

### CodeSandbox SDK（→ Together Code Sandbox）

- **隔离**：Firecracker;已被 Together AI 收购
- **环境**：⚠️ devcontainer + Dockerfile 套壳（Docker 跑在 VM 内），仅 Debian/Ubuntu 基底,须 template build
- **snapshot**：成熟(hibernate resume 1–2s、fork 运行态 <2s)
- **开源**：❌ 平台闭源
- **对 eval 场景**：模板体系同 e2b 问题

### Cloudflare Sandboxes / Vercel Sandbox（简）

- **Cloudflare**（2026/4 GA）：容器隔离;可用标准镜像但**必须内嵌 CF 运行时**（继承基镜像或注入其 binary 作 entrypoint）,须 wrangler 推 CF registry;绑定 CF 生态
- **Vercel**：Firecracker;✅ 支持任意 OCI 但须先推 Vercel Container Registry;单区域(iad1);闭源
- 两者都是「绑平台生态的通用沙箱」，非批量 eval 定位

## 2. 横向对比

| 平台 | 隔离 | 任意 OCI 直启 | 冷启动 | pause/resume/fork | 开源/自托管 | eval 批量适配 |
|---|---|---|---|---|---|---|
| **Tensorlake** | Firecracker | ✅ 但 oci2rootfs 全量预物化 | 预热池 | ✅ 快照/clone/迁移 | ❌ 仅客户端开源 | ⚠️ 闭源不可自托管 |
| **AgentENV** | Firecracker | ✅ overlaybd 零转换 | resume <50ms | ✅✨ fork 16 子 | Apache-2.0 ✅ | ✅ 但多节点 prototype |
| **CubeSandbox** | RustVMM | ❌ template 路线 | 60ms | ✅ CubeCoW fork | Apache-2.0 ✅ | ❌ template 成本 |
| e2b | Firecracker | ❌ template build（5–15min/个） | ~200ms | ✅ Beta | Apache-2.0 ✅ | ❌ |
| Daytona | Docker 容器 | ⚠️ registry 拉取+注册,AMD64 | <90ms | ✅ | AGPL-3.0 ✅ | ⚠️ 无强隔离/无分发优化 |
| Modal | gVisor | ❌ SDK 重建+依赖 Python | 亚秒 | ⚠️ Alpha | ❌ | ⚠️ 闭源 |
| Morph | MorphVM | ❌ setup 链 | <250ms | ✅✨ fork 最强 | ❌ | ❌ |
| microsandbox | libkrun | ✅ 零转换 | ~300ms | ❌ | Apache-2.0 ✅ | ⚠️ 单机形态 |
| CodeSandbox | Firecracker | ⚠️ devcontainer 套壳 | resume 1–2s | ✅ 成熟 | ❌ | ❌ |
| Cloudflare | 容器 | ⚠️ 须嵌其运行时 | 快 | ✅ | ❌(SDK 开源) | ❌ 生态绑定 |
| Vercel | Firecracker | ✅ 但须推其 registry | 秒级 | ✅ FS 快照 | ❌ | ❌ 单区域 |
| **bean（目标）** | **FC 默认档（runc GPU/gVisor 降级为 P5 内部预留）** | **✅ overlaybd 零转换，S3 lazy-pull** | **命中<2s/冷<10s** | **FC 原生 snapshot/fork（P3–P4）** | **自研自托管** | **✅ 一等场景（多节点调度/prewarm/配额为核心）** |

> **这一列的「冷启动」指的是什么。** 上表引用的绝大多数数字,不管各家自己怎么叫,
> 量的都是从一份准备好的快照/模板 **restore** 出一个新 sandbox 的开销 —— 既不是开机,
> 也不是唤醒一个 paused 的 sandbox。bean 可比的实测数是节点本地缓存命中时 restore
> **392 ms**,对比真 create 的 **952 ms**(见 [status.md](status.md));resume 只是解冻
> vCPU,比两者都快但做的事也少得多,所以它不是用来对标的那个数。三种操作、三种开销 ——
> 见 [snapshot-resume.md](snapshot-resume.md) §0。

## 3. 结论：bean 的差异化定位

1. **技术路线已被验证，竞争焦点在工程完成度**：AgentENV 证明了「overlaybd 直挂
   FC + 任意 OCI 零转换」可行且生产可用——bean 采纳同一路线（D4/D9），
   差异化转向 AgentENV 的空白区：**多节点调度（镜像亲和 bin-packing）、prewarm
   编排、配额/租约/故障恢复、GPU 路径（容器档,P5 内部预留）、完整运维面**
2. **商业平台仍无人做到「零转换 + 按需加载」**：Tensorlake 走得最远（oci2rootfs
   把 OCI 转 ext4,对用户零操作）,但仍是全量预物化、无 lazy load;e2b/Morph/
   CodeSandbox/Modal 要求 template 或 SDK 重建;最接近的 Daytona 无强隔离档。
   批量异构镜像评测在商业侧仍是空白
3. **镜像分发是下半场**：各家优化「单模板反复启动」，eval 痛点是「2000 个不同
   镜像各启动几次」——S3 lazy-pull + 块级去重 + 镜像亲和调度 + record-trace
   预取直接打这个点
4. **隔离自动分档**：fc 默认（强隔离+零兼容性问题）、GPU 自动落容器档、无 KVM
   降级 gVisor——同一 API 按节点能力与任务特征选档，竞品均为单一形态
5. **自主可控**：全栈自研,S3+裸金属/VM 即可部署,无生态绑定;开源参考
   （AgentENV/CubeSandbox 均 Apache-2.0）可加速实现而不引入依赖

## 4. 需要持续跟踪的信号

- **Tensorlake 是否开放服务端或推出自托管版**——若开放,功能重合度最高
- **AgentENV 多节点控制面从 prototype 走向成熟的速度**——若其补齐调度/配额/
  运维面,「基于 AgentENV 二开」将重新成为选项
- CubeSandbox 是否增加任意 OCI 直启路线
- Daytona 若补上 gVisor/microVM 档,与 bean 重叠度会显著上升
- e2b Build System 演进是否消除 per-image template 成本
- overlaybd/ublk 上游演进（内核 ublk 用户态块设备生态）

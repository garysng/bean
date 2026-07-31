# 竞品分析（2026-07）

> 视角：bean 的目标场景 = AI evaluation / agent rollout，特点是**大量异构 Docker 镜像**
> （SWE-bench 类 2000+ 镜像）、批量拉起、短生命周期、自主可控部署。

## 1. 逐家分析

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
| e2b | Firecracker | ❌ template build（5–15min/个） | ~200ms | ✅ Beta | Apache-2.0 ✅ | ❌ |
| Daytona | Docker 容器 | ⚠️ registry 拉取+注册,AMD64 | <90ms | ✅ | AGPL-3.0 ✅ | ⚠️ 无强隔离/无分发优化 |
| Modal | gVisor | ❌ SDK 重建+依赖 Python | 亚秒 | ⚠️ Alpha | ❌ | ⚠️ 闭源 |
| Morph | MorphVM | ❌ setup 链 | <250ms | ✅✨ fork 最强 | ❌ | ❌ |
| microsandbox | libkrun | ✅ 零转换 | ~300ms | ❌ | Apache-2.0 ✅ | ⚠️ 单机形态 |
| CodeSandbox | Firecracker | ⚠️ devcontainer 套壳 | resume 1–2s | ✅ 成熟 | ❌ | ❌ |
| Cloudflare | 容器 | ⚠️ 须嵌其运行时 | 快 | ✅ | ❌(SDK 开源) | ❌ 生态绑定 |
| Vercel | Firecracker | ✅ 但须推其 registry | 秒级 | ✅ FS 快照 | ❌ | ❌ 单区域 |
| **bean（目标）** | **runc/gVisor/kata→FC 分档** | **✅ 零转换，S3 lazy-pull** | **命中<2s/冷<10s** | **P3 pause、P4 snapshot/fork** | **自研自托管** | **✅ 一等场景** |

## 3. 结论：bean 的差异化定位

1. **「任意 OCI 零转换直启」在商业平台中几乎无人做到**——microVM 阵营全部要求
   rootfs 转换或 SDK 重建；最接近的 Daytona 也有注册步骤且无强隔离档。
   批量异构镜像评测是真实空白
2. **镜像分发是没人做的下半场**：各家优化的是「单模板反复启动」（快照恢复快），
   而 eval 的痛点是「2000 个不同镜像各启动几次」——bean 的 S3 lazy-pull +
   chunk 去重 + 镜像亲和调度直接打这个点
3. **隔离分档**是对「容器派（Daytona，快但弱隔离）vs microVM 派（强但镜像不友好）」
   二选一困局的回答：同一 API 下按信任度选档
4. **自主可控**：核心竞品中只有 e2b/Daytona/microsandbox 可自托管，且各有
   license 或形态限制;bean 全栈自研,S3+裸金属/VM 即可部署,无生态绑定
5. **fork/分支**（Morph 的王牌）通过 FC 档预留位补齐路线,容器档先用
   「snapshot 一次 fan-out N」满足 eval 的主要复用需求

## 4. 需要持续跟踪的信号

- Daytona 若补上 gVisor/microVM 档,与 bean 重叠度会显著上升
- microsandbox 云服务 GA 后的集群化能力
- e2b Build System 演进是否消除 per-image template 成本
- Together（CodeSandbox）与 AI eval 生态的整合动作

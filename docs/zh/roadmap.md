# 实施路线图

> 从 architecture.md §8 细化。每个 Phase 以可演示的端到端里程碑收口。

## 当前实际进度(2026-08-02)

**Phase 划分已被实际路径打乱**,所以按能力读比按 Phase 读更准 ——
权威记录是 `docs/status.md`。要点:

- ✅ **已超出 P0/P1**:fc 直启(952ms)、多节点调度与承诺量落库、S3 快照 blob、
  OCI 拉取转换、BuildKit 构建、OTel trace 全链路
- ✅ **已提前做完 P3/P4 的快照部分**:pause/resume、full / `--no-memory` /
  `--base` 增量三种快照、UFFD 按需供页、CPU template 与调度器 CPU 过滤
- ⚠️ **P0 里说的 overlaybd ublk 直驱做了一半**:默认仍是 dm-snapshot(每 sandbox 44 KiB),
  overlaybd 已接在 `--fc-overlaybd` 后面 —— 但走 TCMU,不是 P0 描述的 ublk 直驱。
  ublk 不再是「优化项而非基础」:TCMU 拆 128 个设备的 4.0 s 在 5.15 和 6.8 上一模一样,
  那是传输层的限制而不是内核的(见 [status.md](status.md))
- ⚠️ **P0 里说的 jailer 没做**:noded 直接 exec firecracker
- ✅ **P0/P1 里说的网络已做完**,而它曾是最大的空白:每沙箱 namespace、tap、
  NAT 出网,元数据网段与 RFC1918 默认拒绝,沙箱内端口可从节点外经 bean-proxy 到达。
  在真实内核上验证过,包括拒绝规则。跨节点 sandbox 互通仍是非目标
- 📐 **未做**:volume、按端口的访问控制、TypeScript SDK

一句话:**纵向(快照/启动优化)走得比路线图深;横向上网络与容器档都已补齐,剩下的是卷。**

## P0 — 单节点端到端骨架（fc 直启,无 containerd）

参考实现：本地 /Users/mac/project/agentenv（AgentENV 源码——uvm-ublk 直驱
overlaybd、envd、jailer/FC 管理的完整实证,逐模块对照）。

**范围**

- `proto/`：NodeService / SandboxService / AgentService v1 定义 + buf 工程化
- guest 内核 + agent 盘构建（先手工构建,流水线 P2）
- `noded`：overlaybd ublk 直驱（先预转换镜像全量本地,lazy-pull P2）、
  jailer+FC 进程管理、agent 盘注入、tap/bridge/NAT 基础网络
- `beand`：init 挂载矩阵、vsock gRPC、image config 复刻拉起、同步 exec、
  文件读写、僵尸回收
- `bean-api` 最小实现：POST/GET/DELETE sandboxes、exec、files（单节点直连，无调度器）
- state：先 SQLite/内存（Postgres 接口抽象好）

**验收**

```
curl POST /sandboxes {image} → fc microVM RUNNING
curl POST /exec {pytest} → exit code + output
curl DELETE → 资源清零（FC 进程/tap/TCMU 设备/挂载无残留）
```

## P1 — 多节点可用（eval 首次接入）

**范围**

- scheduler：Register/Heartbeat/租约、push 直连指令下发 + SyncState 对账、bin-packing + 镜像亲和 v1（按 ref 精确匹配）;region 字段进模型（单 region 运行）
- Postgres 状态落地、终态/孤儿 GC（P1 仅显式 kill + LOST 清理;idle 回收随 P3 lifecycle）、noded 重启 reconcile
- 网络隔离完整版（nftables 规则集、DNS 注入、egress-only/none 策略）
- Python SDK（sync + async + run_batch）、CLI 核心命令（run/ls/exec/cp/logs/kill）
- batchCreate、标签批量销毁
- 基础可观测：创建阶段耗时直方图、平台 metrics
- isolation 行为：P0–P4 仅 fc（分档规则固定 fc;容器档 P5 按 GPU 需求引入）

**验收**：3 节点集群跑 100 并发小规模 eval（fc 档,预转换镜像全量拉取），LOST 重试路径验证（kill 一个节点）。

## P2 — 生产化（性能 + 安全达标）

**范围**

- overlaybd + S3 lazy-pull（S3 backing 按需 range-read）、image-service
  离线转换、块缓存、record-trace 预取
- fc 宿主侧加固（jailer 参数收紧、cgroup 包裹、tc/conntrack 限制）
- prewarm API + 编排、镜像亲和 v2（块级 bloom + 字节占比）
- guest 内核 + agent 盘构建发布流水线（noded-design §3.4）
- 产物直推 S3（presigned 链路）、sandbox 日志归档
- Events（状态机发件 → Postgres + WS 订阅）;OTel 统一导出 + per-sandbox 资源指标
- 凭证体系：node token（托管接入层 TLS + 应用层身份）、STS/presigned 全覆盖
- 配额/限流

**验收**：2000 镜像批量评测演练（fc 档）;冷启动 P50 < 10s、缓存命中 P50 < 2s;逃逸回归集通过。

## P3 — 交互与扩展场景

**范围**

- WS 流式 exec + PTY（会话重连）、CLI 交互模式（run -it / attach）
- bean-proxy（regional）：通配域名 TLS、端口暴露（反代直连 sandbox IP）、sandbox token 鉴权、PAUSED 透明唤醒
- pause/resume（fc PauseVM）+ PAUSED 透明唤醒
- lifecycle 自动化：idle 检测（noded 本地）、onIdle pause/delete、PAUSED 请求透明唤醒
- fc 档 snapshot 本节点路径（memory+disk → S3）
- **shared-fs 卷**（宿主挂 JuiceFS + 内核 nfsd 导出、agent NFS 挂载、后端配额）
- TS SDK、e2b 迁移对照文档

**验收**：agent rollout 场景接入（交互式终端 + 端口预览）;pause 后资源计费口径正确。

## P4 — Snapshot 完整形态

**范围**

- fc 档跨节点 restore、diff snapshot 增量、✅ fork 独立 API（CoW 一母多子,本节点 —— 已实装,`POST /v1/sandboxes/{id}/fork`）
- PAUSED 归档：超阈值自动 snapshot 落 S3 释放 RAM,再访问透明 restore
- snapshot 生命周期：配额、引用计数、TTL/S3 lifecycle
- **镜像即快照**：`POST /images:register {snapshot}` 把任意 sandbox 快照注册为
  命名镜像（Tensorlake 同款）——「装环境一次、批量复用」的最短路径

**验收**：「装环境 → snapshot → fan-out 50 实例」演示(一份快照 restore 50 次、50 个
互相独立的 sandbox);fc 档 **restore** P50 < 500ms —— 是 restore 不是 resume:
承诺的这个数是「造出一个新 sandbox」的开销。

## P5+ — 储备项

- 计量数据产出（cpu·s/mem·s/存储/流量,内部对账;不做租户计费体系）
- 镜像签名（cosign）
- allow-list 网络策略、TCP 端口暴露（SNI）
- dataset 卷（overlaybd 只读块,数据集/权重分发——预留,需求明确后启用）
- webhook 事件推送（签名 + 重试;先靠 WS/轮询）
- sandbox 内应用 OTLP 透传
- 容器档引入（GPU:containerd+runc+nvidia;无 KVM 降级:runsc）+ 容器加固基线
  + 容器档 checkpoint（gVisor save / CRIU）+ GPU sandbox 完整支持
- 多区域完整形态：region 字段 P1 进模型（单 region 起步）,P5 扩展
  多 region blob 复制编排、BYOC region 接入流程（客户侧 token 服务、
  出向注册）、控制面多活

## 风险登记簿

| 风险 | 影响 | 缓解 |
|---|---|---|
| fcRuntime 自研复杂度（VM 生命周期/guest 内核/vsock） | P0 延期 | AgentENV 源码在本地逐模块参考（uvm-ublk/envd/warm-pool）;P0 范围收敛（预转换镜像+手工内核） |
| ublk 内核要求（6.0+） | 旧节点无 lazy-pull | 节点 OS 统一基线;tcmu 后端或 overlayfs 全量拉取保底 |
| overlaybd 转换覆盖率 | 未转换镜像 fc 档不可用 | 转换流水线随镜像入库自动触发;容器档标准拉取兜底;可复用 tensorlake/oci2rootfs（Apache-2.0,OCI→ext4）做全量预物化 fallback |
| FC snapshot 宿主 CPU 代际兼容 | 跨节点 restore 受限 | 调度按 CPU feature set 分组;manifest 记录代际 |
| GPU 走 runc 隔离弱 | GPU eval 安全短板 | GPU 独立节点池 + 镜像白名单;nvproxy（gVisor GPU）P5 评估 |
| 节点必须有 KVM（P0–P4 仅 fc 档） | 无 KVM 节点不可用 | 采购/开通嵌套虚拟化;容器档 P5 兜底 |
| S3 延迟波动 | 冷启动长尾 | record-trace 预取 + 节点缓存池化 |
| shared-fs 卷链路（JuiceFS 运维 + 宿主 nfsd 稳定性） | 卷不可用/慢 | 后端可换(CephFS/本地盘);极端时可切 go-nfs 用户态实现 |
| guest 内核/agent 盘版本矩阵 | snapshot 跨版本 restore 失败 | manifest 记录版本;节点保留多版本工件 |
| 自研调度器成熟度 | 资源碎片/饥饿 | eval 负载同质化高,先简单策略 + 指标驱动迭代 |

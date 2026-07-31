# 实施路线图

> 从 architecture.md §8 细化。每个 Phase 以可演示的端到端里程碑收口。

## P0 — 单节点端到端骨架（打通主链路）

**范围**

- `proto/`：NodeService / SandboxService / AgentService v1 定义 + buf 工程化
- `beand`：containerd(runc) 创建/销毁 sandbox、agent 注入（bind mount + PID1 override）、基础 netns/veth/NAT（先不做 nftables 细则）
- `bean-agent`：unix socket gRPC、同步 exec、文件读写、僵尸回收
- `bean-api` 最小实现：POST/GET/DELETE sandboxes、exec、files（单节点直连，无调度器）
- state：先 SQLite/内存（Postgres 接口抽象好）

**验收**

```
curl POST /sandboxes {image} → RUNNING
curl POST /exec {pytest} → exit code + output
curl DELETE → 资源清零（netns/挂载/containerd task 无残留）
```

## P1 — 多节点可用（eval 首次接入）

**范围**

- scheduler：Register/Heartbeat/租约、push 直连指令下发 + SyncState 对账、bin-packing + 镜像亲和 v1（按 ref 精确匹配）
- Postgres 状态落地、sandbox 超时 GC、beand 重启 reconcile
- 网络隔离完整版（nftables 规则集、DNS 注入、egress-only/none 策略）
- Python SDK（sync + async + run_batch）、CLI 核心命令（run/ls/exec/cp/logs/kill）
- batchCreate、标签批量销毁
- 基础可观测：创建阶段耗时直方图、平台 metrics

**验收**：3 节点集群跑 100 并发小规模 eval（overlayfs 全量拉镜像），LOST 重试路径验证（kill 一个节点）。

## P2 — 生产化（性能 + 安全达标）

**范围**

- **fcRuntime 主档**：firecracker + jailer 进程管理、overlaybd 块设备 virtio-blk
  直挂、guest 内核打包、agent vsock transport + init 挂载矩阵（参考 AgentENV 实现）
- overlaybd + S3 lazy-pull（ublk 路径验证）、image-service 离线转换、块缓存、record-trace 预取
- isolation auto 解析（fc 默认/GPU→runc/无 KVM→runsc）;runsc 降级档兼容性回归集
- 容器加固基线全量（seccomp/caps/pids/quota）
- prewarm API + 编排、镜像亲和 v2（块级 bloom + 字节占比）
- 产物直推 S3（presigned 链路）、sandbox 日志归档
- 凭证体系：mTLS 内部 CA、STS/presigned 全覆盖
- 配额/限流

**验收**：2000 镜像批量评测演练（fc 档）;冷启动 P50 < 10s、缓存命中 P50 < 2s;逃逸回归集通过。

## P3 — 交互与扩展场景

**范围**

- WS 流式 exec + PTY（会话重连）、CLI 交互模式（run -it / attach）
- bean-proxy：通配域名 TLS、端口暴露、sandbox token 鉴权
- pause/resume（fc PauseVM / 容器档 cgroup freezer）
- fc 档 snapshot 本节点路径（memory+disk → S3）
- TS SDK、e2b 迁移对照文档

**验收**：agent rollout 场景接入（交互式终端 + 端口预览）;pause 后资源计费口径正确。

## P4 — Snapshot 完整形态

**范围**

- fc 档跨节点 restore、diff snapshot 增量、fork（CoW 一母多子）
- 容器档 checkpoint 兜底：gVisor save/restore + runc CRIU（GPU/无 KVM 场景）
- snapshot 生命周期：配额、引用计数、TTL/S3 lifecycle

**验收**：「装环境 → snapshot → fan-out 50 实例」演示;fc 档 resume P50 < 500ms。

## P5+ — 储备项

- 多租户 RBAC 与计费
- 镜像签名（cosign）
- allow-list 网络策略、TCP 端口暴露（SNI）
- GPU sandbox 完整支持（探测已就位，重点是驱动注入与 gVisor GPU 路径评估）
- 跨 region 部署与就近调度

## 风险登记簿

| 风险 | 影响 | 缓解 |
|---|---|---|
| fcRuntime 自研复杂度（VM 生命周期/guest 内核/vsock） | 主档延期 | AgentENV/CubeSandbox 开源实现可深度参考;P0/P1 用 runc 先打通全链路，fc 档并行开发 |
| ublk 内核要求（6.0+） | 旧节点无 lazy-pull | 节点 OS 统一基线;tcmu 后端或 overlayfs 全量拉取保底 |
| overlaybd 转换覆盖率 | 未转换镜像 fc 档不可用 | 转换流水线随镜像入库自动触发;容器档标准拉取兜底 |
| FC snapshot 宿主 CPU 代际兼容 | 跨节点 restore 受限 | 调度按 CPU feature set 分组;manifest 记录代际 |
| GPU 走 runc 隔离弱 | GPU eval 安全短板 | GPU 独立节点池 + 镜像白名单;nvproxy（gVisor GPU）P5 评估 |
| runsc 降级档兼容性 | 无 KVM 节点体验差 | 回归集扫描;采购/开通嵌套虚拟化优先 |
| S3 延迟波动 | 冷启动长尾 | record-trace 预取 + 节点缓存池化 |
| 自研调度器成熟度 | 资源碎片/饥饿 | eval 负载同质化高,先简单策略 + 指标驱动迭代 |

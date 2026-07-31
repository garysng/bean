# Pause / Resume / Snapshot 设计

> 状态机见 architecture.md §4.3。本文覆盖容器档（runc/runsc）实现、S3 布局、
> 跨节点 resume，以及 Firecracker 档的最终形态。

## 1. 能力分级

| 能力 | 语义 | 实现 | Phase |
|---|---|---|---|
| **pause/resume（本节点）** | 冻结进程，停止计费 CPU，保留内存与状态 | cgroup freezer | P3 |
| **snapshot** | 完整状态持久化到 S3，sandbox 可销毁 | checkpoint + rootfs diff | P4 |
| **restore（跨节点）** | 从 snapshot 在任意节点重建 | 反向过程 | P4 |
| **memory snapshot（毫秒级）** | fork 式克隆、agent 分支探索 | Firecracker 原生 | P4+（FC 档） |

## 2. Pause / Resume（轻量，不落盘）

```
POST /sandboxes/{id}/pause
  beand: cgroup.freeze = 1（cgroup v2 freezer，整棵进程树原子冻结）
       + 网络连接保留（conntrack 不清）、netns 不动
       + agent 自身也被冻结——控制面把状态置 PAUSED 后拒绝 exec（409）
POST /sandboxes/{id}/resume
  beand: cgroup.freeze = 0，秒回 RUNNING
```

- 冻结期间内存不释放——调度器仍按其 memory.max 记账（防止超卖后 resume OOM）；
  若要释放内存额度，用 snapshot
- 超时销毁计时器在 PAUSED 期间**继续走**（防泄漏），可通过 timeout 接口续期
- proxy 对 PAUSED 返回 502 + Retry-After

## 3. Snapshot（容器档）

### 3.1 组成

一个 snapshot = 三部分，原子提交：

```
s3://bean/snapshots/{snapId}/
├── manifest.json        # 元数据：源镜像 digest、isolation、resources、env、
│                        #   agent 版本、创建时间、各部分校验和
├── checkpoint/          # 进程态：CRIU images（runc）或 gVisor save 文件（runsc）
│                        #   分片上传，zstd 压缩
└── rootfs-diff.tar.zst  # 可写层 diff（containerd snapshotter 导出 upper layer）
```

base 镜像**不进** snapshot——restore 节点从常规镜像链路（Nydus/S3）取，diff 只含增量。eval 场景 diff 通常很小（几十 MiB 级）。

### 3.2 runc 档：CRIU

```
流程（beand 执行）：
1. 状态置 SNAPSHOTTING;先 freeze（保证一致性）
2. criu dump --tree <pid1> --leave-frozen：进程树+内存页+fd 表+unix socket
3. containerd snapshotter 导出 upper layer → tar.zst 流式推 S3（presigned）
4. criu images 打包推 S3
5. 写 manifest,提交控制面;按请求参数决定 resume 原 sandbox 或销毁
```

CRIU 已知限制（文档明确告知用户）：

| 限制 | 处理 |
|---|---|
| 外部 TCP 连接 | `--tcp-established` 可保存但对端早已断——restore 后由应用重连;明确不保证 |
| GPU 状态 | 不可 checkpoint。带 GPU 的 sandbox 拒绝 snapshot（400） |
| 挂载点一致性 | restore 节点复原相同挂载拓扑（agent mount、resolv.conf 等由 spec 重建） |
| /dev/shm、大内存 | 内存页全量落盘,10 GiB 内存 ≈ 分钟级;snapshot 是重操作,API 文档标注 |
| agent 进程 | agent 自身被一并 checkpoint;restore 后 agent 内存态恢复,socket 由 beand 重连（transport 重建逻辑 agent 已支持） |

### 3.3 runsc 档：gVisor save/restore

gVisor 自带整 sandbox 级 save/restore（`runsc checkpoint/restore`），比 CRIU 更可靠（用户态内核状态自包含）：

- checkpoint 产出单一状态文件 → 同样三件套布局
- 限制类似（外部 TCP、GPU）；跨 gVisor 版本 restore 不保证——manifest 记录 runsc 版本，restore 节点版本不匹配时拒绝并提示
- **结论：standard 档（默认档）的 snapshot 走 gVisor 原生路径，CRIU 仅服务 runc 档**——这条路径成熟度决定了容器档 snapshot 的整体可用性

### 3.4 Restore（跨节点）

```
POST /sandboxes { "snapshot": "snap_...", ... }
1. 调度：按 base 镜像亲和选节点（manifest 里有 digest）
2. 并行：拉 base 镜像（缓存大概率命中）+ 拉 rootfs-diff + 拉 checkpoint
3. snapshotter 组装 rootfs：base + 解包 diff 为新 upper layer
4. 网络重建：新 IP（不保证 IP 不变，文档明确）、netns/nftables 常规流程
5. runsc restore / criu restore → RUNNING
6. beand 重连 agent socket
```

目标恢复时延：diff+checkpoint 1 GiB 以内 P50 < 15s（S3 并行分片拉取）。

### 3.5 生命周期

- snapshot 独立对象、独立配额（总字节数 per key）；TTL 可选，S3 lifecycle 兜底
- 引用计数：有 RESTORING 进行中的 snapshot 不可删
- 同一 snapshot 可多次 restore → 天然支持「装好环境 snapshot 一次，fan-out N 个实验」——eval 场景的核心价值点

## 4. Firecracker 档（最终形态，Phase 4+）

FC 原生 snapshot 解决容器档所有痛点，是把 snapshot 做成**高频操作**的路线：

| 维度 | 容器档（CRIU/gVisor save） | FC 档 |
|---|---|---|
| 一致性 | 进程级，外部状态尽力而为 | 整机级（内存+设备+vCPU），TCP 栈都在 guest 内一起冻结 |
| 速度 | 分钟级（大内存） | 亚秒级 pause;snapshot 写盘受 IO 限制;diff snapshot 支持增量 |
| resume | 重建进程树 | load snapshot + resume vCPU，百 ms 级 |
| fork 克隆 | 不支持 | copy-on-write memory + diff snapshot → 一母多子，agent 分支探索/A-B rollout |

设计预留（现在就固化的接口）：

- Runtime 接口的 Checkpoint/Restore 签名已兼容（io.Reader/Writer 流式）
- manifest 的 `runtime` 字段区分 checkpoint 格式，restore 调度按格式匹配节点能力
- snapshot API 语义不变——用户无感切换底层
- guest 内 agent 走 vsock（transport 抽象已就位，见 beand-design §6.6）

## 5. API 汇总（重述）

```
POST   /sandboxes/{id}/pause
POST   /sandboxes/{id}/resume
POST   /sandboxes/{id}/snapshot     { "name": "...", "keepRunning": true }
GET    /snapshots?label=...
DELETE /snapshots/{id}
POST   /sandboxes                   { "snapshot": "snap_...", ... }
```

SDK 形态见 [sdk-cli-design.md](sdk-cli-design.md)。

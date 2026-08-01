# Pause / Resume / Snapshot 设计

> 状态机见 architecture.md §4.3。fc 为默认主档，snapshot 主路径是 FC 原生
> snapshot;容器档（runc/runsc）的 checkpoint 路径服务降级/GPU 场景。

## 1. 能力分级

| 能力 | 语义 | 实现 | Phase |
|---|---|---|---|
| **pause/resume（本节点）** | 冻结执行，保留内存与状态 | fc：pause vCPU;容器档：cgroup freezer | P3 |
| **snapshot** | 完整状态持久化到 S3，sandbox 可销毁 | fc：memory+disk snapshot（主路径）;容器档：checkpoint + rootfs diff | P3–P4 |
| **restore（跨节点）** | 从 snapshot 在任意节点重建 | 反向过程 | P4 |
| **fork（毫秒级克隆）** | 一母多子、agent 分支探索 | FC diff snapshot + CoW（仅 fc 档） | P4 |

> Phase 均指 fc 主路径;表中容器档实现（freezer/CRIU/gVisor save）随 P5 容器档引入。

## 2. Pause / Resume（轻量，不落盘）

```
POST /sandboxes/{id}/pause      # 亦可由 lifecycle.onIdle=pause 自动触发
  fc 档:   FC API PauseVM（vCPU 停止，内存原样保留），百 ms 内
  容器档:  cgroup.freeze = 1（cgroup v2 freezer，整棵进程树原子冻结）
  共同:    网络保留、agent 一并冻结
唤醒（平台默认行为）: PAUSED 收到 exec/端口/文件请求 → 自动 resume → 透传
  （显式 resume API 仍在;调用方通常无感）
POST /sandboxes/{id}/resume
  fc 档:   ResumeVM;容器档: cgroup.freeze = 0——均亚秒回 RUNNING
```

- 冻结期间内存不释放——调度器仍按其 memory.max 记账（防止超卖后 resume OOM）；
  若要释放内存额度，用 snapshot
- PAUSED 默认无限期保留（全局回收策略默认关,见 api-design §5.2）;P4 引入 snapshot 归档释放 RAM
- 对 PAUSED 的请求触发透明唤醒（阻塞至 resume,超时才 502——与 api-design §5.2 一致）
- proxy 对 PAUSED 返回 502 + Retry-After

## 3. Snapshot

### 3.0 fc 档（主路径）

```
POST /sandboxes/{id}/snapshot
1. PauseVM → FC CreateSnapshot：memory file + vmstate（支持 diff snapshot 增量）
2. 可写 overlay 盘打增量（reflink/块 diff）
3. memory/vmstate/disk-diff + manifest 流式推 S3（zstd 分片）
4. keepRunning=true → ResumeVM
```

- 整机级一致性：TCP 栈、fd、进程树全部在 guest 内一起冻结，无 CRIU 的外部状态问题
- restore：拉 base 镜像块设备（缓存命中）+ memory file + overlay diff → LoadSnapshot
  → resume vCPU，目标百 ms 级（本地）/ 秒级（跨节点冷拉）
- fork（独立 API：`POST /sandboxes/{id}/fork {count}`）：瞬时 CoW 快照 + N 次
  LoadSnapshot → 一母多子,不产生持久 snapshot 对象（要留存用 /snapshot）。
  子实例宿主侧资源全部新分配：tap 设备、vsock CID、可写层 CoW 克隆、新
  sandbox-id/token;guest 内 MAC/IP 由 agent 重配。「装环境一次 fan-out N
  实验」的最优实现;首期本节点 fork,跨节点走 snapshot+restore
- balloon 交互：snapshot 前先收缩气球（减小 memory file）,restore 后气球状态
  随 vmstate 恢复
- 限制：宿主 CPU 代际需兼容（调度按 CPU feature set 分组）;GPU 不适用（GPU 走容器档）

### 3.1 容器档（降级/GPU 场景）

#### 组成

一个 snapshot = 三部分，原子提交：

```
s3://bean/snapshots/{snapId}/
├── manifest.json        # 元数据：源镜像 digest、isolation、resources、env、
│                        #   agent 版本、创建时间、各部分校验和
├── checkpoint/          # 进程态：CRIU images（runc）或 gVisor save 文件（runsc）
│                        #   分片上传，zstd 压缩
└── rootfs-diff.tar.zst  # 可写层 diff（containerd snapshotter 导出 upper layer）
```

base 镜像**不进** snapshot——restore 节点从常规镜像链路（overlaybd/S3）取，diff 只含增量。eval 场景 diff 通常很小（几十 MiB 级）。

#### runc 档：CRIU

```
流程（noded 执行）：
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
| agent 进程 | agent 自身被一并 checkpoint;restore 后 agent 内存态恢复,socket 由 noded 重连（transport 重建逻辑 agent 已支持） |

#### runsc 档：gVisor save/restore

gVisor 自带整 sandbox 级 save/restore（`runsc checkpoint/restore`），比 CRIU 更可靠（用户态内核状态自包含）：

- checkpoint 产出单一状态文件 → 同样三件套布局
- 限制类似（外部 TCP、GPU）；跨 gVisor 版本 restore 不保证——manifest 记录 runsc 版本，restore 节点版本不匹配时拒绝并提示
- 容器档内：runsc 走 gVisor 原生路径（更可靠），CRIU 仅服务 runc 档、尽力而为

#### Restore（跨节点，容器档）

```
POST /sandboxes { "snapshot": "snap_...", ... }
1. 调度：按 base 镜像亲和选节点（manifest 里有 digest）
2. 并行：拉 base 镜像（缓存大概率命中）+ 拉 rootfs-diff + 拉 checkpoint
3. snapshotter 组装 rootfs：base + 解包 diff 为新 upper layer
4. 网络重建：新 IP（不保证 IP 不变，文档明确）、netns/nftables 常规流程
5. runsc restore / criu restore → RUNNING
6. noded 重连 agent socket
```

目标恢复时延：diff+checkpoint 1 GiB 以内 P50 < 15s（S3 并行分片拉取）。

### 3.5 Volume 与 snapshot 的交互

- **shared-fs 卷**：guest 内核 NFS client 持有到宿主的 TCP 连接,整机 snapshot
  后跨节点 restore 该连接必死。流程：snapshot 前 agent 收指令 unmount（lazy）
  所有 NFS 挂载点 → snapshot → restore 后 agent 重新 mount（新宿主的网关地址）。
  卸载失败（fd 占用）→ snapshot 失败并报明确错误
- snapshot manifest 记录完整卷挂载表（dataset 卷启用后:按 manifest 重 attach 块设备）

### 3.6 生命周期（两档共通）

- snapshot 独立对象、独立配额（总字节数 per key）；TTL 可选，S3 lifecycle 兜底
- 引用计数：有 RESTORING 进行中的 snapshot 不可删
- 同一 snapshot 可多次 restore → 天然支持「装好环境 snapshot 一次，fan-out N 个实验」——eval 场景的核心价值点

## 4. 两档对比与接口统一

| 维度 | 容器档（CRIU/gVisor save） | fc 档（主路径） |
|---|---|---|
| 一致性 | 进程级，外部状态尽力而为 | 整机级（内存+设备+vCPU） |
| 速度 | 分钟级（大内存） | pause 百 ms;diff snapshot 增量 |
| resume | 重建进程树 | load snapshot + resume vCPU，百 ms 级 |
| fork 克隆 | 不支持 | CoW memory → 一母多子（AgentENV 单节点 16 子实证） |

接口统一（用户无感）：

- Runtime 接口 Checkpoint/Restore 签名两档通用（io.Reader/Writer 流式）
- manifest 的 `runtime` 字段区分格式，restore 调度按格式匹配节点能力
- snapshot API 语义一致，档位差异只体现在速度与 fork 可用性

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

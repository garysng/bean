# Pause / Resume / Snapshot 设计

> 状态标注约定见 [architecture.md](architecture.md) §0;状态机见 architecture.md §4.3。fc 为默认主档，snapshot 主路径是 FC 原生
> snapshot;容器档（runc/runsc）的 checkpoint 路径服务降级/GPU 场景。

## 1. 能力分级

| 能力 | 语义 | 实现 | 状态 |
|---|---|---|---|
| **pause/resume（本节点）** | 冻结执行，保留内存与状态 | fc:pause vCPU | ✅ |
| **snapshot** | 完整状态持久化到 S3，sandbox 可销毁 | fc:memory + CoW extent 列表 | ✅ 三种形态 |
| **restore（跨节点）** | 从 snapshot 在任意节点重建 | UFFD 供页 + CoW 回填 | ⚠️ 见下 |
| **容器档 checkpoint** | CRIU / gVisor save | — | 📐 未实现 |

⚠️ **跨节点 restore 未实测**:只有一台 fc 机器。逻辑上是通的(快照记录产出它的
CPU,调度器按 vendor+family 硬过滤),且已用「改写快照记录冒充 GenuineIntel」验证过
409 会正确返回 —— 但「同 family 跨 model 真的能恢复」没有实证,
`--cpu-template portable` 存在的理由正是这个。见 `docs/decisions.md` §3.6。
| **fork（毫秒级克隆）** | 一母多子、agent 分支探索 | FC diff snapshot + CoW（仅 fc 档） | P4 |

> Phase 均指 fc 主路径;表中容器档实现（freezer/CRIU/gVisor save）随 P5 容器档引入。

## 2. Pause / Resume（轻量，不落盘）✅

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

### 3.0 fc 档（主路径）✅

```
POST /sandboxes/{id}/snapshot   {includeMemory?, base?, keepRunning?}
1. PauseVM(两种快照都要 —— 不含内存时,pause 仍是文件系统一致的前提:
   guest 边写边读设备会把撕裂的写入放进快照)
2. FC CreateSnapshot: snapshot_type = Full | Diff
   Diff 要求 guest 在 boot 时就开了 track_dirty_pages(见下)
3. 打 bundle(gzip tar 流):
     vmstate       原样
     memory        全量时原样(dense,FC 每字节都写)
     memory.diff   增量时按 extent 列表(sparse,只有脏页)
     rootfs        CoW 层按 extent 列表 —— 供给量大而用得少,
                   全量输出 20 GiB 零字节给压缩器测得 15s 的暂停时间
4. 流式推 S3
5. keepRunning=true(默认)→ ResumeVM
```

**三种快照,语义不同而非只是尺寸不同:**

| 形态 | 参数 | 实测尺寸 | restore 行为 | CPU 约束 |
|---|---|---|---|---|
| full | 默认 | 15.5 MB | resume,进程树存活 | 绑 vendor+family |
| 仅文件系统 | `--no-memory` | **6109 B** | 重新 boot,文件保留 | 无 |
| 增量 | `--base SNAP` | **298 KB** | resume | 绑 vendor+family |

- 整机级一致性:TCP 栈、fd、进程树全部在 guest 内一起冻结,无 CRIU 的外部状态问题
- `--no-memory` 换的是**可移植性**而不只是尺寸:guest 内存记录了它从 CPU 读到的
  东西,vendor/family 掩不掉(`docs/decisions.md` §3.6),所以带内存的快照只能落
  在兼容 CPU 上,调度器硬过滤,不兼容返回 409 `INCOMPATIBLE_CPU`
- `--base` 只对内存有意义:文件系统层本来就是 O(changed) —— CoW 只存改动块

**restore 的顺序是 load-bearing 的** —— 这里曾有一个静默损坏文件系统的 bug:

```
1. 控制面按 base 链取全部层(store.SnapshotChain,base 优先定序),
   在 spec 里声明 snapshot_chain,逐层 stream 并用 layer_end 分界
2. 节点先把 bundle 落到 staging 目录 —— **不碰任何块设备**
3. 按链合并内存镜像:base 全量写出,每层 diff 叠加(snapmerge_linux.go)
4. Images.Prepare(SeedWritable: 回填 CoW)
   ↑ 回填发生在 dmsetup create **之前**
5. LoadSnapshot(Uffd backend)→ resume vCPU
```

**为什么第 4 步的顺序不能换**:dm-snapshot 在设备激活那一刻把 exception table
读进内核内存,之后不再回读。往已激活设备的 CoW 后端补写,内核不认这些 chunk,
设备继续供 base image。而这个失败在 full snapshot 上**完全静默** —— 读命中的是
内存快照带回的 page cache,`drop_caches` 之后同一个文件读出全零,`ls` 仍显示
正确 size、无 EIO、无 dmesg。详见 `docs/decisions.md` §3.0。

**合并在 restore 时物化,不在缺页路径分层**:E2B 走后者(fault 时 chase K 个
BuildId),代价是 fragmentation 随深度增长;我们物化后交给现有 UFFD handler,
**缺页路径零改动** —— 那是全系统最热、出错最隐蔽的代码。合并结果按 leaf id 进
snapCache,所以 fan-out 每节点只付一次。链深超 8 自动转 full。
选型对照见 `docs/decisions.md` §3.0.1。

**`track_dirty_pages` 必须 boot 前开且不存进快照**,所以是节点配置
(`--track-dirty-pages`,默认关)而非快照参数。没开的 guest 请求 diff **明确报错**
不降级成 full —— 调用方以为省了空间实际没省,而尺寸本身不解释原因。

**删 base 有子代时返回 409**:diff 依赖祖先,删掉会让整条链失效,
而失败在时间和空间上都很远(现在删成功,以后在另一台机器上恢复失败)。

**restore 的内存不落盘（已实装,实测）**：用 Firecracker 的 `Uffd` memory backend
而不是 `File`。`File` 会在 guest 跑起来之前把整个内存镜像读进来,成本跟 guest
大小成正比而与「实际访问了多少」无关 —— 512 MiB guest 上是 1303ms。
改成 userfaultfd 后 guest 内存匿名映射,缺页时由 handler 供页,
`/snapshot/load` 降到 **7ms**。e2b / agentenv / tensorlake 都是这么做的。

**解 bundle 每快照只做一次(已实装)**:同一快照的每次 restore 解出的字节完全
相同,所以按 snapshot id 缓存 vmstate + memory。安全性来自 Firecracker 对
memory 文件是 `MAP_PRIVATE`(实测 guest 写 64MB 后宿主文件 md5 不变)。
**可写 rootfs 不缓存** —— 同一快照恢复出的两个 sandbox 一写就分叉。

实测 restore ~950ms(首次 1617ms,付 unpack 代价)。剩余成本是把 bundle
从 gateway 传过来并解 gzip,只为取 rootfs 那个 member —— 未优化。
- fork 📐 **未实现**(独立 API:`POST /sandboxes/{id}/fork {count}`):瞬时 CoW 快照 + N 次
  LoadSnapshot → 一母多子,不产生持久 snapshot 对象（要留存用 /snapshot）。
  子实例宿主侧资源全部新分配：tap 设备、vsock CID、可写层 CoW 克隆、新
  sandbox-id/token;guest 内 MAC/IP 由 agent 重配。「装环境一次 fan-out N
  实验」的最优实现;首期本节点 fork,跨节点走 snapshot+restore
- balloon 交互 📐 **未接 balloon 设备**:snapshot 前先收缩气球（减小 memory file）,restore 后气球状态
  随 vmstate 恢复
- 限制：宿主 CPU 代际需兼容（调度按 CPU feature set 分组）;GPU 不适用（GPU 走容器档）

### 3.1 容器档（降级/GPU 场景）📐

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

### 3.5 Volume 与 snapshot 的交互 📐

- **shared-fs 卷**：guest 内核 NFS client 持有到宿主的 TCP 连接,整机 snapshot
  后跨节点 restore 该连接必死。流程：snapshot 前 agent 收指令 unmount（lazy）
  所有 NFS 挂载点 → snapshot → restore 后 agent 重新 mount（新宿主的网关地址）。
  卸载失败（fd 占用）→ snapshot 失败并报明确错误
- snapshot manifest 记录完整卷挂载表（dataset 卷启用后:按 manifest 重 attach 块设备）

### 3.6 生命周期（两档共通）⚠️

- snapshot 独立对象、独立配额（总字节数 per key）；TTL 可选，S3 lifecycle 兜底
- 引用计数：有 RESTORING 进行中的 snapshot 不可删
- 同一 snapshot 可多次 restore → 天然支持「装好环境 snapshot 一次，fan-out N 个实验」——eval 场景的核心价值点

## 4. 两档对比与接口统一 ⚠️

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

## 5. API 汇总（重述）⚠️

```
POST   /v1/sandboxes/{id}/pause              ✅
POST   /v1/sandboxes/{id}/resume             ✅
POST   /v1/sandboxes/{id}/snapshot           ✅
       { "name", "labels", "keepRunning": true,
         "includeMemory": true,   # false = 仅文件系统,可落任意 CPU 但重新 boot
         "base": "snap_..." }     # 只存自 base 以来改动的内存;需 includeMemory
GET    /v1/snapshots?label=...&state=...     ✅
GET    /v1/snapshots/{id}                    ✅
DELETE /v1/snapshots/{id}                    ✅  有子代时 409 SNAPSHOT_IN_USE
POST   /v1/sandboxes { "snapshot": "snap_..." }  ✅  不兼容 CPU 时 409 INCOMPATIBLE_CPU
POST   /v1/sandboxes/{id}/fork               📐  未实现
```

CLI:`bean snapshot create SBX [--name N] [--no-memory] [--base SNAP] [--no-keep-running]`,
`bean snapshot ls|rm`,`bean run --snapshot SNAP`。

SDK 形态见 [sdk-cli-design.md](sdk-cli-design.md)。

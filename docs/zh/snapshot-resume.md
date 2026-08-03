# Pause / Resume / Snapshot 设计

> 状态标注约定见 [architecture.md](architecture.md) §0;状态机见 architecture.md §4.3。fc 为默认主档，snapshot 主路径是 FC 原生
> snapshot;容器档（runc/runsc）的 checkpoint 路径服务降级/GPU 场景。

> **先读 §0。** resume、restore、fork 是三个不同的操作,本文其余部分都假定读者已经
> 分清了它们。

## 0. resume、restore、fork 是三件不同的事

**resume 把同一个 sandbox 唤回来,restore 造出另一个。** 下面所有内容都从这一句
推出,而把它搞混已经不止一次需要口头解释。

| | **resume** | **restore** | **fork** |
|---|---|---|---|
| 起点 | 一个 vCPU 被冻住、进程仍活着的 firecracker | 磁盘 / S3 上的一份快照 blob | 一个**正在运行**的 sandbox |
| 产出 | **同一个** sandbox,同一个 id | **新的** sandbox,新 id | N 个新 sandbox,各自新 id |
| guest 内存 | 从未离开宿主 RAM | 由 UFFD 从解包后的镜像按缺页供给 | 同 restore |
| 持久对象 | 无 | 一个 `snap_...`,比由它造出的任何 sandbox 都活得久 | 不产生(要留就用 snapshot) |
| 开销 | 毫秒级 —— 一次 `PATCH /vm {Resumed}` | 节点本地缓存命中 **392 ms** | restore 的开销,减去打包与传输 |
| 熬得过 noded 重启 | ❌ 进程一死就没了 | ✅ blob 本身就是状态 | ❌ 派生自一个活进程 |
| 能跨机器 | ❌ 绑在那个进程的宿主上 | ✅ 这正是它的用途 | ❌ 同节点;跨节点走 snapshot+restore |
| 扇出(1 → N) | ❌ 从头到尾只有一个 | ✅ **一份快照造出 N 个互相独立的 sandbox** | ✅ 天生如此 |
| 约束 | 除「进程还在」之外没有 | 钉死在采集内存时那颗 CPU 的 vendor+family 上 | 同 restore |
| 配对操作 | `pause` | `snapshot` | — |

### 为什么这个区分重要

**扇出只有 restore 做得到。** 一份快照 restore N 次得到 N 个互相独立的 sandbox,
这正是 eval 的核心负载:环境只装一次,然后跑 N 个实验,而它们不能看见彼此的写入。
resume 根本做不到这件事 —— 一个 paused sandbox 就是一个 sandbox,resume 它得到的
就是那一个。引用计数恰好反映了这点:快照上的 `ref_count` 是**计数器而不是标志位**,
因为同一份快照被同时 restore 好几次是常态而非冲突(`AcquireSnapshot`,`store.go`)。

**pause/resume 是省成本的手段,不是扩容手段。** 它的存在是让闲置 sandbox 停止烧
CPU、同时保持随时可用。它不释放内存 —— 调度器仍按整份额度记账(§2)—— 所以它只是
拿 CPU 换延迟,别无其他。

**混淆两者会让所有性能数字失去意义。**
[competitive-analysis.md](competitive-analysis.md) 里各家「~100 ms 启动」指的都是
**restore**。它既不是 create(真开机:952 ms、5 CPU 秒,见 [status.md](status.md)),
也不是 resume(那只是解冻 vCPU,因此比两者都快,但做的事也少得多)。拿 resume 的延迟
去对标别人的 restore 延迟,等于把「解冻」和「造一台机器」放在一起比。

## 1. 能力分级

| 能力 | 语义 | 实现 | 状态 |
|---|---|---|---|
| **pause/resume（本节点）** | 冻结执行，保留内存与状态 | fc:pause vCPU | ✅ |
| **snapshot** | 完整状态持久化到 S3，sandbox 可销毁 | fc:memory + CoW extent 列表 | ✅ 三种形态 |
| **restore（跨节点）** | 从 snapshot 在任意节点重建出一个**新的** sandbox | UFFD 供页 + CoW 回填 | ⚠️ 见下 |
| **fork（毫秒级克隆）** | 一母多子、agent 分支探索 | 与 restore 共用同一套机制;缺的只有 API | ⚠️ 见 §4.5 |
| **容器档 checkpoint** | CRIU / gVisor save | — | 📐 未实现 |

⚠️ **跨节点 restore 未实测**:只有一台 fc 机器。逻辑上是通的(快照记录产出它的
CPU,调度器按 vendor+family 硬过滤),且已用「改写快照记录冒充 GenuineIntel」验证过
409 会正确返回 —— 但「同 family 跨 model 真的能恢复」没有实证,
`--cpu-template portable` 存在的理由正是这个。见 `docs/decisions.md` §3.6。

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
- fork ⚠️ **机制已实装,API 未实现**(见 §4.5)。计划的表面是
  `POST /sandboxes/{id}/fork {count}`:瞬时 CoW 快照 + N 次
  LoadSnapshot → 一母多子,不产生持久 snapshot 对象（要留存用 /snapshot）。
  在它出现之前,`snapshot create` + N 次 `run --snapshot` 有完全相同的语义,只是绕了持久对象一圈。
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

### 3.5.5 节点本地解包缓存的回收 ✅

`snapCache` 让同一 leaf 的多次 restore 复用一份解包内存镜像,
这是 fan-out 便宜的原因。代价是它**每恢复一个新快照就多占约一份 guest 内存**,
而这部分空间不占任何承诺量 —— 调度器看不见,所以节点能在「账面充足」时把盘写满。
实测一台开发机积累到 4.6 GB / 9 条目。

高低水位 + LRU,默认关(`--snapshot-cache-high-mib` / `--snapshot-cache-low-mib`):

| 机制 | 取值 | 理由 |
|---|---|---|
| 触发/回收线成对 | 低位默认 = 高位 80% | 抄 kubelet image GC。单阈值让触发后每次 restore 都付一次回收 |
| 大小按已分配块 | `st_blocks * 512` | 合并出的镜像是稀疏的,按名义大小会为回收零字节而淘汰条目 |
| 淘汰顺序 | 目录 mtime,命中时 `Touch` | atime 在 relatime 下一天才更新一次,热条目会被当成冷的 |
| 删除方式 | 先 rename 到临时目录再删 | 原地删会经过「vmstate 没了但内存镜像还在」的状态,`Lookup` 撞上会认为条目可用 |

**pin 只护 `Lookup`→`open` 这一段。** restore 先拿到路径,过一会儿才打开内存镜像;
在这两点之间被淘汰,open 就是 ENOENT,而那时 stream 已消费完、无从重建。
一旦打开就安全了 —— unlink 一个已 mmap 的文件不影响读(实测验证,decisions §3.7),
所以 `stage.Close()` 就释放 pin,不必等 VM 结束。

上报为 `bean_node_snapshot_cache_bytes`,通过可选接口 `runtime.CacheReporter` ——
不占额度的空间至少要可见。

### 3.6 生命周期（两档共通）⚠️

- snapshot 独立对象、独立配额（总字节数 per key）；TTL 可选，S3 lifecycle 兜底
- 引用计数：有 RESTORING 进行中的 snapshot 不可删。这个计数是**计数器而非标志位**,正因为同一快照被并发 restore 才是预期情形
- 同一 snapshot 可多次 restore、每次产出一个独立 sandbox → 这就是「装好环境 snapshot 一次,跑 N 个实验」所需要的扇出,也是 eval 场景的核心价值点。resume 替代不了:它只会还你被 pause 的那一个

## 4. 两档对比与接口统一 ⚠️

| 维度 | 容器档（CRIU/gVisor save） | fc 档（主路径） |
|---|---|---|
| 一致性 | 进程级，外部状态尽力而为 | 整机级（内存+设备+vCPU） |
| 速度 | 分钟级（大内存） | pause 百 ms;diff snapshot 增量 |
| restore | 重建进程树 | load snapshot + resume vCPU，百 ms 级 |
| fork 克隆 | 不支持 | CoW memory → 一母多子（AgentENV 单节点 16 子实证） |

接口统一（用户无感）：

- Runtime 接口 Checkpoint/Restore 签名两档通用（io.Reader/Writer 流式）
- manifest 的 `runtime` 字段区分格式，restore 调度按格式匹配节点能力
- snapshot API 语义一致，档位差异只体现在速度与 fork 式扇出是否可用

## 4.5 fork:机制已经在了,缺的是 API 表面 ⚠️

> **缺的是一个 API 调用,不是一项能力。** restore 本身**就是** fork:
> `snapshot create` 之后跑 N 次 `run --snapshot`,得到的就是 N 个共用一份内存镜像
> 的独立 sandbox。fork 需要的每一项机制都已实装并实测。不存在的只是「对着一个运行
> 中的 sandbox 一次调用就完成这两步、并省掉绕持久对象那一圈」。

### restore 已经是 fork/clone 语义 ✅

三条性质合起来就是 fork 的定义,而三条都在代码里:

| 性质 | 位置 | 依据 |
|---|---|---|
| 不可变部分**共享** | `uffd_linux.go` | 内存镜像以 `PROT_READ \| MAP_SHARED` 映射,注释写明「让从同一快照恢复的多个 VM 共用一份 page cache 而不是每 VM 一份」 |
| 可变部分**每实例私有拷贝** | `fc_lifecycle_linux.go` | `snapshotState`:「可写层总是解包出来……因为它无法共享:从同一个 checkpoint 恢复的两个 sandbox 一旦有一方写入就分叉」 |
| 结果是 **N 个互相独立的实例** | `store.go` | `ref_count` 是计数器而非标志位;`AcquireSnapshot` 递增它,其注释用的是复数的 restore |

共享不可变的、私有拷贝可变的,产出彼此观察不到的实例 —— 这就是 fork。

不只是读代码,还有实证:`hack/restore-repeat-check.sh` 把同一份快照 restore 若干次,
并在 **`drop_caches` 之后**让每个 sandbox 各自读回自己的 marker(drop 很关键 ——
走 page cache 的读即便设备在供 base image 也会通过,见 [decisions.md](decisions.md)
§3.0)。解包缓存的回收检查在六次 restore 上做了同样的断言
(见 [tech-stack.md](tech-stack.md) §3.2)。

节点本地这条路是**为这个形态设计的**,不只是恰好能用。`snapCache` 让一条链在每个
节点只合并一次,该叶子之后的每次 restore 都跳过合并;那个分支的注释直接点名了这个
场景 —— 「这就是扇出便宜的原因」。

### 一个 fork API 真正会带来什么

不是共享,也不是独立性 —— 这两样已经有了。只有这些:

- **一次调用而不是两次。** 用 `POST /sandboxes/{id}/fork {count: N}` 取代
  `snapshot create` 加 N 次 `run --snapshot`。
- **不产生持久对象。** fork 不会造出一个需要命名、计配额、引用计数、回收的
  `snap_...`。今天那个中间快照必须先创建再删除。
- **省掉打包/传输这一圈。** 母体就在本节点、内存也已经驻留,所以同节点 fork 可以
  跳过打包上 S3 再读回来。

这一节后面的内容都是关于那层表面,以及上线前值得先测的两个数。

### 为什么内存镜像可以被 N 个子实例共享 ✅(机制已验证)

这是 fork 便宜的全部原因,而它在 snapCache 里已经是承重的:

```
UFFD handler:  mmap(PROT_READ | MAP_SHARED)     ← 我们只读
Firecracker:   guest 内存是匿名映射
供页:          UFFDIO_COPY 把页 *拷进* guest 的匿名页
```

guest 写自己的内存,写的是它自己的匿名页,**碰不到我们的镜像文件** ——
已用「guest 写 64MB 后宿主文件校验和不变」实测过(snapcache_linux.go 的注释)。

所以**一份 unpacked memory 能服务任意多个实例**,不需要拷贝。
snapCache 现在就是靠这条让同一快照的多次 restore 复用一份内存镜像;
fork 只是把「多次 restore」变成「一次派生 N 个」。

### 每个子实例必须新分配什么

| 资源 | 能否共享 | 理由 |
|---|---|---|
| 内存镜像 | ✅ 共享 | 见上,UFFDIO_COPY 语义保证 |
| vmstate | ✅ 共享 | 只读加载 |
| **CoW 层** | ❌ 每个新建 | 一写就分叉,这是 sandbox 的定义 |
| **vsock UDS 路径** | ❌ 每个独立 | 路径相对于 sandbox 目录(vm-assembly §5) |
| sandbox id / token | ❌ 每个独立 | 身份 |
| dm 映射名 | ❌ 每个独立 | `bean-<id>`,flat namespace |

`guestCID` 与 vsock port 可以都用常量,因为每个 VM 有自己的 vsock 命名空间
(vm-assembly §7)—— 这一点让 fork 少一层分配。

网络实现之后会多出 MAC/IP 需要 guest 内重配,当前没有网络所以没有这个问题。

### 与 snapCache 的关系

fork 天然是「同一个 leaf 被恢复很多次」—— **正是 snapCache 设计针对的场景**。
所以 fork 的实现应该复用它而不是新建一条路径:第一个子实例填缓存,
其余直接命中。

`Fill` 的语义已经支持这个:每个 caller 都会被调用(处理自己的 CoW 层),
但只有一个真正构建共享 entry。这个语义是修一个 bug 时明确下来的 ——
等待方原本直接返回缓存 entry 而不执行回调,于是既不 drain 自己的流也不 stage
自己的可写层。

### 要压测的风险

**N 个子实例同时缺页,会不会打爆 UFFD handler?**

当前 handler 的 `serve()` 是**单个 goroutine 的循环**:读一个 fault 事件、
`UFFDIO_COPY` 填一页、继续。每个 VM 有自己的 handler 实例,所以 N 个 fork
是 N 个 handler —— 不共享那个循环。

但它们共享:
- 同一份 mmap 的镜像(page cache 层面是好事:只有一份)
- 宿主的内存带宽与 page fault 处理能力

所以风险不在 handler 的串行度,而在**同时冷启动 N 个 guest 时宿主的缺页吞吐**。
这个数字没有测过 —— 属于 GitHub #18 压测的范围,而且是 fork 实装前应该先知道的。

**第二个风险**:fork 出来的 N 个实例内存承诺量是 N 倍,而实际 RSS 因为按需供页
远低于此。这正是超卖(noded-design §3.2)想利用的富余,但内存超卖当前默认关 ——
所以 fork N 个会按 N 倍承诺量占额度,可能比实际需要保守很多。

### 实现顺序建议

1. 先测 §上面那两个数字(缺页吞吐、RSS 与承诺量的偏差)
2. 再实装 fork —— 因为如果缺页吞吐是瓶颈,fork 的 API 形态可能要带并发上限

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
POST   /v1/sandboxes { "snapshot": "snap_..." }  ✅  restore:一个**新的** sandbox、新 id。
                                                    不兼容 CPU 时 409 INCOMPATIBLE_CPU。
                                                    调 N 次就是 N 路扇出
POST   /v1/sandboxes/{id}/fork               ⚠️  无 API;机制就是上面那两个调用(§4.5)
```

注意 `pause`/`resume` 作用在 `{id}` 上、还你同一个 sandbox,而 restore 是
`POST /v1/sandboxes` —— 一次创建 —— 还你另一个。URL 的形状已经说明了这件事。

CLI:`bean snapshot create SBX [--name N] [--no-memory] [--base SNAP] [--no-keep-running]`,
`bean snapshot ls|rm`,`bean run --snapshot SNAP`(restore:每次调用产出一个新 sandbox),
`bean pause SBX` / `bean resume SBX`(同一个 sandbox)。没有 `bean fork`。

SDK 形态见 [sdk-cli-design.md](sdk-cli-design.md)。

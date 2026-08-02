# 技术选型与方案对比

> 每条决策记录:实测数据、竞品做法(e2b / tensorlake / agentenv)、以及为什么选这个。
> 没有实测数据支撑的条目标注「未验证」,不当成结论。

## 1. 启动优化

### 1.1 串口:默认关闭

**实测**(真 KVM 机器,alpine 3.19,VMM 启动到 agent 可连):

```
console=ttyS0    1193 / 1195 / 1210 ms
quiet             700 /  700 /  711 ms
```

摘掉串口省 493ms(41%)。8250 UART 写入是同步的,内核每打一行日志都要等硬件。

**竞品**:e2b 的 `fc-kernels` config 里 `CONFIG_SERIAL_8250=y` 是**开着的** ——
即编译进内核但 boot args 不挂 `console=`。需要调试时才挂,同一个内核既能快启也能调试。

**选择**:学 e2b。内核保留驱动,`--debug-console` 控制是否挂载。
理由:失败的 boot 没有别的证据来源,这个能力不能丢,但不该全量买单。

### 1.2 gRPC 重连退避

**实测**:agent 在 ~700ms 就 listen 了,但 `agent_ready` 报 1493ms。

原因:agent 要等 guest 启动完才能 listen,所以**第一次 dial 必然失败**。
gRPC 默认 `BaseDelay` 是 1s,失败后连接就在退避里躺满一秒,
上层 50ms 轮询完全空转。

**选择**:`BaseDelay` 改 20ms,`MaxDelay` 1s。轮询粒度 50ms → 10ms。
理由:重试间隔应该匹配「一次 boot」的时间尺度,不是「远端服务故障」的尺度。

**结果**:create 2.2s → 1.04s(`runtime_create` 234ms + `agent_ready` 770ms)。

### 1.3 guest 内核:用 CI prebuilt,不 fork,不自建编译流程

**调研**:
| repo | 内容 | 是否 fork |
|---|---|---|
| `e2b-dev/firecracker` | VMM 源码 | **是**(加了 gdb feature 等) |
| `e2b-dev/fc-versions` | 编 VMM 的 pipeline | 否 |
| `e2b-dev/fc-kernels` | 内核 config + patch + build.sh | **否** |

`fc-kernels` 运行时 `git clone amazonlinux/linux`(Firecracker 官方 `rebuild.sh` 的同一个源),
repo 里只放 config(3094 行)+ 一个 virtio_balloon patch。
**e2b 的内核维护面 = 一个 config 文件,没有 rebase 负担。**

**选择**:用 `firecracker-ci/v1.11/x86_64/vmlinux-6.1.102`,`.config` 一起入库
(CI 把 config 单独发布,所以「用 prebuilt」和「拿到自己的 config」不是二选一)。
`hack/build-assets.sh kernel` 负责下载并校验是 ELF —— 那个 bucket 见过截断,
而短了的内核表现为「boot 挂住」,不是下载报错。

理由:容器编译要先付出成本(工具链 + 拉源码 + 编 20min)才拿到第一个数据点,
而当时连「换内核有没有用」都还没测。先测,收益够再建编译流程 —— config 已在手上。

**实测**(quiet,VMM 启动到 agent 可连,各三次):
```
vmlinux-6.1.175   690 / 689 / 715 ms   (来源 agentenv R2 站,config 未知)
vmlinux-6.1.102   603 / 613 / 601 ms   (Firecracker CI,config 已知)
```
快 ~90ms(13%)。全链路 create 从 1040ms → 952ms,snapshot/restore 正常。

**但要注意收益的来源**:CI config 里 `CONFIG_SCSI_ISCSI_ATTRS`、`CONFIG_BPFILTER`、
`CONFIG_SQUASHFS`、`CONFIG_XFS_FS`、`CONFIG_NFS_FS` **全都是 =y**。
所以这 90ms 不是「裁掉了没用的驱动」——CI 内核也没裁。
差异主要是镜像更小(40.8MB vs 44.5MB)和版本本身。

**推论**:自己编一个精简 config 的收益上限比预期低。内核日志里那些
iSCSI / bpfilter 探测在 CI 内核里同样存在,而它已经更快了。
真要继续压启动时间,`quiet`(-493ms)和 gRPC 退避(-800ms)那种量级的收益
不在内核裁剪里。**所以暂不建编译流程。**

## 2. snapshot restore:UFFD 而非缓存解包结果

**实测**(restore 一个 512MiB 内存的 sandbox):

```
restore 总计      1400 ms
├─ restore_load   1303 ms   ← 拉 blob + 解 gzip + 落盘 memory/rootfs
└─ agent 等待       97 ms   ← 内存已恢复,进程还活着
```

对比冷启动 1040ms:**restore 比冷启动还慢**。93% 的成本在 `restore_load`。
落盘的 memory 文件实占 513MB(非稀疏),每次 restore 都完整重写一遍。

**验证过的一个事实**:Firecracker 对 memory 文件是 `MAP_PRIVATE`(写时复制)。
实测 guest 内写 64MB 随机数据后,宿主上的 memory 文件 md5 不变。
所以多个 restore **可以共享同一份解开的 memory 文件**。

**竞品做法**(三家一致):
- **e2b**:`packages/orchestrator/pkg/sandbox/uffd/` —— 完整的 UFFD handler,
  含 `memory/`、`prefetch/`、`userfaultfd/`(cgo)。
- **agentenv**:`storage/uffd-core/`(Rust)—— 还把 UFFD 后端接到了 overlaybd,
  缺页直接从镜像读。
- **tensorlake**:公开博客讲 sub-second cold start,并把磁盘快照做成
  O(changed bytes)(单文件改动 167ms / 105MB)。

**选择**:UFFD。Firecracker 的 `snapshot/load` 支持 `backend_type: Uffd` + UDS 路径,
VM 不读 memory 文件,缺页时由 handler 进程按需提供。**restore 时零落盘。**

被否掉的方案:「按 snapshot ID 缓存解开的 memory 文件」。它能省掉重复解压,
但第一次仍要落盘 512MB,而且占磁盘;UFFD 直接消除了这个成本,是竞品的共同选择。

被否掉的方案:「池化 restore-ready VM」。每个池成员要占一份内存,
而实测表明瓶颈在解包落盘而非 VM 恢复(agent 只等 97ms),池化解决的不是真问题。

**前提已确认**:FC v1.15.1-patch-v1 支持;宿主 `CONFIG_USERFAULTFD=y`;
`unprivileged_userfaultfd=0` 但 noded 以 root 运行,可用。
5.15 走 `userfaultfd` syscall,6.1+ 走 `/dev/userfaultfd`。

### 2.1 实测结果与两个只能靠跑才发现的协议细节

`/snapshot/load` 从 **1303ms → 7ms**。真机验证,按需供页。

两个坑,文档里都没写清楚:

1. **fd 和 region layout 不一定在同一个 datagram 里。** 单次 `ReadMsgUnix`
   拿到 fd 但 body 是空的 → JSON 解析失败 → handler 死掉,而 Firecracker
   在缺页上永久阻塞。必须循环收齐两者。agentenv 的 Rust 实现也是循环。
2. **Firecracker 递过来的 fd 是非阻塞的。** 直接 `read` 立刻返回 EAGAIN,
   fault 循环当场退出。必须 `poll` 等可读。
   这个错误的表现就是「`snapshot/load` 永久挂起」,和 handler 崩溃无法区分 ——
   所以 handler 必须有 `Err()` 通道,否则只能看到一个 hang。

### 2.2 unpack 缓存:每节点每快照一次

UFFD 干掉 load 成本后,剩下的 1060ms 全是 unpack(解 gzip + 落盘 512MB memory)。
同一个快照的每次 restore 解出的字节完全相同,所以按 snapshot ID 缓存。

**安全性已验证**:Firecracker 对 memory 文件是 `MAP_PRIVATE`。
guest 内写 64MB 后宿主文件 md5 不变 → 一份解开的 memory 可以服务任意多次 restore。

**writable rootfs 故意不缓存**:同一快照恢复出的两个 sandbox 一写就分叉,
必须各有自己的设备。好在它是 sparse extent list,新 sandbox 几乎没写过东西,
所以很小 —— 这才使得「memory 共享 + rootfs 独立」的拆分成立。

**实测**:
```
首次 restore   1617 ms   (付 unpack 代价)
后续 restore    ~950 ms
```

并发正确性:同一快照的并发 restore 只 unpack 一次。
写测试时发现一个真实竞态 —— 等待者在 `wg.Done()` 和自己重新查缓存之间,
会看到「盘上还没出现」而重新 unpack 一遍。改成等待者直接读 in-flight 结果。
publish 用「写临时目录 + rename」,所以中断的 unpack 不会留下残缺条目。

### 2.3 还没做:剩下的 ~950ms

命中缓存后仍要**把整个 16MB bundle 从 gateway streaming 过来并解 gzip**,
只为了取出 rootfs 那个 member。正确做法是让节点在命中时告诉控制面
「别发了」,或者把 rootfs 拆成独立对象。**未实现。**

**已知风险**(来自 Firecracker 官方文档):handler 进程死掉会让 Firecracker
在下次缺页时**永久挂起**,所以必须有存活监控。balloon 的 `MADV_DONTNEED`
会产生 `UFFD_EVENT_REMOVE`,handler 必须把对应页面置零而不是回读文件
(否则会复活脏数据)。

### 2.4 三家竞品对照

| 维度 | e2b | agentenv | tensorlake | bean(现状) |
|---|---|---|---|---|
| VMM | fork 了 firecracker(私有,加 gdb feature) | 上游 FC | 未公开 | 上游 FC 1.15.1 |
| guest 内核 | 自己 config + patch,源码取 `amazonlinux/linux`,**不 fork** | prebuilt(R2 站) | 未公开 | **FC CI prebuilt + config 入库** |
| 内存恢复 | UFFD(`uffd/` + `prefetch/`,cgo) | UFFD(`uffd-core/`,Rust) | 未公开细节,声称 sub-second | **UFFD(已实测 7ms load)** |
| rootfs 按需 | 未见 | UFFD 后端接 overlaybd | 磁盘快照 O(changed bytes),单文件改动 167ms | dm-snapshot CoW(8 KiB/sandbox),**lazy-pull 未做** |
| 磁盘快照增量 | 未见 | 未见 | **有**(他们的差异化点) | 无(full snapshot) |

**从对照里得到的三个判断:**

1. **UFFD 是共识,不是选项。** 三家全都做了,而且 e2b/agentenv 都各写了一个
   完整的 handler 包。我们原来打算的「缓存解开的 memory 文件」只是把成本
   从「每次」降到「每快照一次」,UFFD 才是把成本降到「每个实际访问的页」。
   两者不冲突 —— 我们现在两个都有。
2. **不要 fork 内核。** e2b fork 了 VMM 但**没有** fork 内核,只维护一个 config。
   这是维护面最小的做法,我们跟。
3. **磁盘增量快照是我们最大的缺口。** tensorlake 把它当核心卖点
   (O(changed bytes) vs O(disk size))。我们的 rootfs 已经走 sparse extent list,
   所以成本跟着「写了多少」而不是「provision 了多少」—— 方向对了,
   但还是 full snapshot,没有基于上一次快照的增量。Firecracker 原生支持
   diff snapshot,接口不用改。

## 3. rootfs:dm-snapshot 而非 overlaybd/TCMU

**已实测**:每 sandbox 磁盘成本 8 KiB(共享只读 base + 每 sandbox CoW)。

TCMU 需要每 sandbox 一套 SCSI fabric(loopback nexus),脆弱且慢;
dm-snapshot 只要 `dm_snapshot` 模块。

**overlaybd 的真正价值**在「首次拉取按需读块」,不在「每 sandbox 成本」——
后者 CoW 已经解决。所以 overlaybd 该做,但理由是**首次使用大镜像的等待时间**,
不是磁盘占用。

agentenv 的 `uffd-core/src/overlaybd.rs` 表明这两件事可以合并:
UFFD 缺页直接从 overlaybd 镜像读。这是比我们现在更远的一步。

### 3.0 restore 必须在设备组装**之前**恢复 CoW

dm-snapshot 在 `dmsetup create` 那一刻把 exception table 读进内核内存,之后不再回读。
往一个**已激活**设备的 CoW 后端补写,内核不认这些 chunk —— 设备继续供 base image。

原来的 restore 正是这么做的:`Prepare()` 组好设备,再把快照 extents 写进 `cow.img`。

**这个 bug 在 full snapshot 上是静默的**,实测(Zen 2 / 6.1.102):

```
恢复后立即读:  cat /root/marker  →  survives      ← 命中内存快照带回的 page cache
drop_caches 后: cat /root/marker  →  (9 个 \0)     ← 真去读块设备
                ls -la            →  size = 9      ← 元数据在内存里,是对的
                dmesg             →  无任何错误
```

文件系统没有任何异常信号:size 对、无 EIO、无 dmesg。只有内容是零。
元数据活在内存镜像里,数据活在块设备上,两边不一致而 ext4 没有理由怀疑。
memoryless 快照没有 page cache 可依赖,所以**立刻**暴露成「文件不见了」——
是它把这个一直存在的缺陷翻出来的。

**修法**:`PrepareOptions.SeedWritable` 回调,provider 在「CoW 建好」与
「组装设备」之间调用它。restore 因此改成先把 bundle 落到 staging 目录,
再交给 `Prepare`,extent 流原样暂存、只在写进设备时解码一次。

竞品对照:**没人往已激活设备的 CoW 里补写**。firecracker-containerd 的
devmapper snapshotter 是 thin-pool 先派生再 activate,顺序天生正确;
Lambda SnapStart 用 chunk 化的惰性加载块设备供给;E2B 的 rootfs 就是宿主文件,
CoW 在文件系统层。Firecracker 上游文档则直接把磁盘状态甩给调用方保证 ——
我们踩的是它警告过的那一类。

**测试为什么之前没抓到**:三层验证全在错误的抽象层。单测测 tar 进出(数据确实写进了
文件,bug 在文件之下);e2e 读的是 guest 里的文件(命中 page cache);
`dmsetup status` 看的是快照**源**的设备。没有一层去读**恢复后的块设备本身**。

现在 `TestDevMapperSeedIsVisibleThroughDevice` 挂载 `/dev/mapper/...` 读回,
不经过 guest。已验证把 seed 挪回 `dmsetup create` 之后该测试立刻失败。

**由此得到一条通用规则**:状态同时存在于内存和磁盘时,只读内存的测试是假的。
任何「恢复后数据还在」的断言,必须先 `drop_caches`。这条同样适用于后面的 diff snapshot。

### 3.0.1 diff snapshot:恢复时物化,不在缺页路径分层

Firecracker 的 diff 内存文件**不自包含** —— 是稀疏文件,必须叠到 base 上。
所以真正的设计问题不在「怎么产出 diff」,在「什么时候、在哪里合并」。

**竞品选了相反的两条路,都在生产跑:**

- **E2B**:fault 时分层查找。UFFD handler 经 `block.Slicer` 走 base + 各层,
  K 次 pause/resume 后一次读要「chase K different BuildId references」。
  不设链深上限,只有 `NormalizeMappings` 合并相邻同 build 段。
  公开分析明确指出 **cross-build fragmentation 随时间增长**,读放大与深度成正比。
- **Cognition blockdiff**:链只作血缘,运行前 flatten 成 raw。
  `apply` 是纯元数据操作(XFS reflink),128 GB `cp --reflink=always` 测得
  0.008s vs 24.5s。他们的 flatten 几乎免费,所以文章完全不谈读放大 ——
  **运行时没有链可走**。
- **Firecracker 上游**:`snapshot-editor edit-memory rebase` 就是 flatten,
  要求按创建顺序逐层叠加。

**我们选 flatten,理由不止「跟多数」:**

我们有 E2B 没有的结构优势 —— `snapCache` 已按 snapshot id 缓存解包结果。
E2B 每次 resume 自己走链;我们只在**某个 leaf 首次在某节点恢复时**付一次合并,
之后该节点所有 restore 复用。fan-out 正是「同一 leaf 恢复很多次」,合并被完全摊掉。

更重要的是 **UFFD 缺页路径零改动**。`fill()` 是全系统最热、出错最隐蔽的代码 ——
一个 bug 就是一页错内存,而且不会有任何错误信号。full snapshot 走的是同一条码路。

**链深超 8 自动转 full**。E2B 不设限并且确实吃到了 fragmentation 增长,
这是支持设限的证据。自动转让恢复成本有上界、祖先可回收,且调用方永远不用关心链深 ——
请求 diff 永远成功,只是偶尔更贵。

**三个不能静默的地方:**

1. `track_dirty_pages` 必须 boot 前设好且不存进快照。没开的 guest 请求 diff
   **明确报错**,不降级成 full —— 调用方以为省了空间,实际没省,而尺寸本身不解释原因。
2. diff 内存用**独立成员名** `memory.diff`,不是给 `memory` 加标志位。
   混淆的后果不对称且都很糟:full 当 diff 叠加会擦掉 base 未触碰的页;
   diff 当 full 加载会给 guest 一份带空洞的内存。按成员名分派两种错误都不可能。
3. 删 base 会让整条链失效,所以有子代时拒绝删除(复用 `ErrInUse` → 409)。
   否则失败在时间和空间上都很远:现在删成功,以后在另一台机器上恢复失败。

**顺序是 caller 的契约,数据里恢复不出来** —— 后层的页合法地覆盖前层,
所以反序会产出「结构完好但由旧页拼成」的镜像,下游无法检测。
因此 `store.SnapshotChain()` 一次性定序,链在 spec 里声明而不是从流里发现
(节点必须先为每层建 reader 才能开始读:每层是独立 gzip 流)。

**实测(Zen 2 / 6.1.102,drop_caches 后):**

```
base(full)     15,586,720 B   depth 0
diff #1           298,778 B   depth 1   ← 52×
diff #2           241,917 B   depth 2
```

深度 2 的链恢复后 a/b/c 三个文件全在,`uptime 57` 说明是 resume 而非重启。

### 3.1 overlaybd lazy-pull:已在 tcmu 后端实测跑通

**实测**(2026-08-02,Ubuntu 20.04 / 内核 5.15 / tcmu 后端 / alpine 3.20):

```
挂载耗时                        7 ms
挂载 + 读 /etc/os-release       1014 KiB / 5175 KiB = 19.6% of layer
读完整个文件系统                1270 KiB(zfile 压缩,只传访问到的块)
registry 响应                   8 × HTTP 206 Partial Content
可写上层实占                    40 KiB(表面 1.1 GB,真稀疏)
```

链路:`overlaybd-create --mkfs` 建空层 → `overlaybd-apply` 把 OCI tar 写进去 →
`overlaybd-commit -z -t` 封成 zfile blob → 推 registry → tcmu 以
`repoBlobUrl` 挂载。overlaybd 日志里的 `__open_ro_remote` 确认它打开的是
**HTTP URL 而不是本地文件**,25ms 就 ready,没有下载整层。

**验证过程踩到两个坑,都会在生产上复现:**

**(1) LUN 必须在 nexus 之后建。** 顺序错了内核报
`TCM_Loop I_T Nexus does not exist`,SCSI host 注册时就去扫 LUN,那时 nexus
还是空的,扫描失败后**再写 nexus 也不会重扫** —— 设备永远不出现,而 configfs
看起来完全正常(`enable=1`、`info` 显示 ACTIVATED、overlaybd 侧 `result=success`)。
正确顺序:backstore → tpgt → **nexus** → LUN 链接。

**(2) 必须设 `wwn/vpd_unit_serial`,否则 multipathd 会静默合并设备。**
TCMU 默认不给唯一序列号,两个内容完全不同的 overlaybd 设备 WWID 都是
`36001405` + 全零,multipathd 把它们当成同一 LUN 的两条路径合成 `mpatha`。
后果不是报错而是**读到另一个镜像的数据**,而且原设备变 busy 无法直接挂载
(mount 报 "already mounted or mount point busy",极具误导性)。
每个 backstore 写一个唯一 serial 即可。

**结论**:tcmu 后端功能完备,不必先升内核。ublk(≥6.0)只是性能更好。
接 `image.Provider` 接口时这两条必须编码进去,靠文档记不住。

## 3.5 trace:OTel + W3C traceparent,但 agent 不链 SDK

**为什么 request id 不够**:字段化日志能回答「这次请求发生了什么」,
不能回答「1.2 秒花在哪一层」。后者需要父子关系,而关系存在于
**进程之间**,任何单个进程的日志里都没有。

实测第一棵树就给出了一个此前无人知晓的数字:

```
POST /v1/sandboxes            bean-api   1196.0ms
  CreateSandbox               noded      1110.2ms   ← 差 86ms
    runtime.Create            noded       324.2ms
    agent.WaitHealthy         noded       785.8ms
```

那 86ms 是调度 + 落库,之前没有任何指标覆盖它。这正是 trace 的价值:
它暴露的是**没人想到要去测的那一段**。

**竞品对照**:

| | trace 方案 | guest 内 |
|---|---|---|
| e2b | OTel,`traceparent` 贯穿 | agent 出 span(envd 有出网路径) |
| agentenv | OTel | 同上 |
| tensorlake | 自建 timing 上报 | — |
| **bean** | OTel + W3C traceparent | **只采纳 trace id,不出 span** |

**bean 与 e2b 的差异是有意的**:e2b 的 envd 能直连 collector,我们的
beand 只有一条入向 vsock,没有出网路径。给它加一条反向通道要么破坏
「入站零暴露」,要么需要在 noded 里做一层 OTLP 中继 —— 后者可行但
不是现在的瓶颈。所以选择是:beand 采纳调用方的 trace id 写进自己的日志,
**并刻意不链 OTel SDK**(`go list -deps ./cmd/beand` 为 0)。理由是
agent 盘挂在每一个 microVM 上,体积按 boot 次数计价,而那份 SDK 服务的
遥测数据根本出不了 guest。

**代价必须说清**:guest 的 stderr 只在 `--debug-console` 下经串口出来,
而串口默认关闭(§1.1,省 493ms)。所以那条带 trace id 的日志
**默认看不到**。要真正闭环,得走 vsock 把 guest 日志收到节点侧 —— 未做。

**request id 就是 trace id,不另发号**。两套 id 意味着每次关联都要 join,
而它们必然在跨进程那一跳上分叉 —— 那一跳恰恰是唯一需要关联的地方。

**一个只有真机能暴露的 bug**:`resource.Merge(resource.Default(), ...)`
在 pinned semconv 版本与 SDK 不一致时直接返回错误,进程起不来。
所有单测都通过,因为它们把 `Endpoint` 留空、在那一行之前就 return 了。
补的测试专门走带 endpoint 的路径(exporter 懒连接,所以不需要真 collector)。

## 3.6 内存快照的 CPU 绑定:自定义 template + 调度器过滤

**问题**:guest 启动时读一次 CPUID 就缓存下来(glibc 据此选 string routine),
迁到没有该特征的机器上**不会在 restore 时失败**,而是之后在某段代码里崩。
所以掩码只能在 guest 启动前做 —— 快照时补不了。

### 为什么不用 Firecracker 的静态 template

在验证机(AMD EPYC 7542,family 23 / Zen 2)上实测,五个内置 template
**一个都启不来**:

```
T2 / C3 / T2S / T2CL  →  "CPU vendor mismatched"(都是 Intel 专用)
T2A                   →  "current CPU model is not permitted"(限 Milan/Zen 3)
```

注意 `PUT /machine-config` 对**所有** template 名都返回成功 ——
厂商校验发生在 `InstanceStart`。只测配置会得到「全部支持」的假结论。

改用 `/cpu-config` 自定义 template,顺带也不必把可移植性绑在
AWS 选择支持哪些 CPU 型号上。

### 两个只有真机能发现的细节

**(1) bitmap 宽度是 31 而不是 32。** 32 位报 `string is too long`。
单测按 32 位全过,真机第一次创建就失败。后果是 **bit 31 无法掩** ——
`avx512vl` 正在那一位,所以它被单列进 `UnmaskableCPUFeatures` 并写进启动日志,
而不是谎称已掩。

**(2) 不掩 xsave。** 掩 leaf 1 ECX bit26 确实让 `xsave` 消失,但 XSAVE
子特征在 leaf 0xD,实测仍然可见 —— guest 会看到有 `xsaveopt` 却没有 `xsave`
的 CPUID,那不对应任何真实处理器。且所有能跑 FC 的机器都有 xsave。

### 掩不掉的东西:vendor 与 family

CPUID leaf 0 的 vendor 字符串和 family 都无法掩,guest 内核要据此做 errata
处理和 MSR 访问。所以 **template 只在同厂商同 family 内提供可移植性**,
跨越必须由调度器拒绝 —— 见 `scheduler.CPUConstraint`。

**故意不记录 model**:掩指令集特征正是为了让快照跨型号可用,
按 model 匹配会把 template 的价值抹掉。

### 竞品对照

| | 内存快照的 CPU 处理 |
|---|---|
| e2b | CPU template 固定 baseline,节点池按 CPU 型号分组 |
| agentenv | 同上;以单节点 fork 为主(16 子实例),跨节点靠同型号池 |
| tensorlake | 磁盘增量为主卖点,内存快照限本机/同型号 |
| **bean** | 自定义 template + 调度器按 vendor/family 硬过滤,不兼容回 409 |

### 摸底脚本

`hack/cpu-template-probe.sh` 把上面所有探测固化下来:哪些内置 template
能启动、bitmap 宽度上限、本机有哪些会被掩的特征。
**换机器必须重跑** —— 这些答案都是 per-host 的,而且失败是静默的
(被拒的 `/cpu-config` 让 guest 完全不设掩码)。
脚本在与代码里 `cpuBitmapWidth` 不一致时以 70 退出。

它还揭示了一个验证边界:**这台验证机没有 AVX-512**,
所以 mask 表里那 5 个 avx512 位从未被真正验证过 ——
只有 `avx avx2 fma f16c` 是实测掩掉的。

## 3.7 容量记账:名义 vs 实际,以及为什么并发上不去

两个问题看起来都是「并发只有 16」,实际是**互相独立**的两件事,
分开归因之后结论完全不同(实测数据见 status.md 规模压测)。

### 并发慢:每次 boot 5 CPU-秒,与我们的代码无关 ✅

`agent_ready` 占 create 的 94%,而 `runtime_create`(dm-snapshot 组装 + VMM spawn)
在 1→16 并发之间只从 241ms 涨到 369ms。压测中 `vmstat` 给出
`r=16 / id=0 / us+sy=100% / wa≈0 / b=0`,逐进程确认每个 firecracker 烧
**5 CPU-秒**且 boot 之后停止增长。

**所以吞吐上限 ≈ 核数 ÷ 5 CPU-秒,是 guest boot 的固有成本。**
加大 `max_creates` 只会让每个请求更慢而不会提高吞吐 —— 这是排队论,不是调参。
真正的杠杆是**减少每次 boot 的 CPU**,或者**不 boot**:

- 从快照 restore 跳过内核初始化,这是 restore 相对 create 的真实价值
  (也是 e2b/Morph 都把 resume 当主路径的原因)
- guest 内核裁剪能降低这 5 秒,但需要自建编译流程(§1.3 决定不做)

**推论**:`max_creates` 的正确语义是「排队深度」而不是「拒绝阈值」。
批量 eval 的调用方拿到 503 之后只能自己重试,而重试风暴让情况更糟;
排队则让延迟可预测。GitHub #19。

### 磁盘记账:名义 20 GiB vs 实际 44 KiB ⚠️

限制器会随配置迁移,而**先撞上的总是记账最粗的资源** ——
默认配置下是磁盘(`102400 / 20480 = 5`),不是 `max_creates`。

友商调研的结论是:**没有任何公开证据显示有平台按「实际已分配块」做调度记账。**
业界的做法是三件事组合,而不是把名义值改准:

| 手段 | 谁在用 | 关键数字 |
|---|---|---|
| 超卖 + 池级记账 | containerd devmapper snapshotter、Kata | `base_image_size` 是虚拟大小,文档示例直接在 100 GB 池上开 8–10 GB/设备,超卖是默认姿态 |
| 每 sandbox 硬配额 | dm-thin 每设备尺寸、XFS project quota | 靠配额兜住单个 sandbox 写爆盘 |
| 节点水位停止接单 | Kubernetes kubelet | `nodefs.available<10%` 触发 DiskPressure;`imageGCHighThresholdPercent=85` **刻意低于**驱逐线,让回收先于驱逐 |

**e2b 做的和我们一样**:`dd if=/dev/zero ... count=5120` 建 5 GB 稀疏 overlay
叠在只读 squashfs 上,公开文章里**完全没有配额、记账、超卖策略的讨论**。
所以「稀疏层 + 不精确记账」不是我们的疏漏,是这条路线的常态;
区别在于我们把名义值当成了调度依据。

### 宿主盘写满时会发生什么:已实测 ✅

`hack/enospc-probe.sh`。在一个 64 MiB 的 loopback 文件系统上组 dm-snapshot
(base 256M,CoW 落在这个小盘上),然后往 guest 侧写到宿主盘撑满:

```
RESULT: the write FAILED with exit 1
  dd: error writing '...': Input/output error
kernel: blk_update_request: I/O error, dev loop9, sector 116032 op 0x1:(WRITE)
kernel: device-mapper: snapshots: Invalidating snapshot: Error reading/writing.
dmsetup status: 0 524288 snapshot Invalid
```

**结论比 dm-thin 那两种模式都更硬:**

| | 实测结果 |
|---|---|
| guest 是挂死还是报错 | **报错**(EIO),不是无限排队 —— 比 dm-thin 默认的 `queue_if_no_space` 好 |
| 设备状态 | dm-snapshot 目标转 **`Invalid`**,不可恢复 |
| 之后还能写吗 | `write()` **仍然返回成功** ← 危险的地方 |
| 那次写活下来了吗 | **没有**。remount 直接 `can't read superblock` |
| 共享 base 呢 | **完好**,只读 base 能干净挂上 —— 爆炸半径是单个 sandbox |

**「`write()` 成功但数据没了」这一条决定了设计。** 设备已经 `Invalid` 之后,
guest 侧的写调用照样返回 0,数据只落在 page cache;等到需要真正读设备时
superblock 都读不回来。**这和 §3.0 那个静默损坏是同一类错误** ——
上层看不到任何异常,直到 page cache 失效。

所以**不能指望在盘写满之后做补救**:那时 sandbox 已经不可恢复,
唯一正确的动作是销毁它。防线必须在写满之前。

好消息是 base 完好,所以爆炸半径是一个 sandbox 而不是「该镜像上的所有 sandbox」——
这让「宁可拒绝新建也不要写满」的取舍成立:代价是拒绝几个请求,
而不是丢掉一批正在跑的 eval。

### 由此得出的决定

**不做「磁盘超卖系数」。** 超卖系数是让运维猜一个倍数,而
**稀疏文件的名义大小本来就不该是记账依据** —— 与其猜倍数,不如上报真实占用。
所以:心跳报实际占用,调度器按真实水位判断。

**水位停止接单是必需的而不是可选的。** 一旦按实际占用记账,超卖就是隐式无限的,
而上面实测的失败模式是不可恢复 + 静默。dm-thin 的先例同样难看:
`queue_if_no_space`(内核默认)下 guest 挂死,元数据耗尽更是要离线
`thin_check`/`thin_repair`,加数据空间也修不回来。

抄 kubelet 的分层次序:**回收(缓存 LRU)的触发线要低于停止接单的线**,
否则一进入压力就直接拒绝服务而没给回收留机会。

### snapshot 缓存无界增长 ✅(已修)

`sandboxes/.snapshots/` 实测 **4.6 GB / 9 个条目**(每个约等于 guest 内存大小),
**完全在记账之外且没有任何回收**。这比记账高估更危险:

- 记账高估是**保守**的 —— 少放 sandbox,不会撑爆磁盘
- 缓存无界是**不受控**的 —— 不占额度所以调度器看不见,但真实占盘

已实装高低水位 + LRU(`--snapshot-cache-high-mib` / `--snapshot-cache-low-mib`,
默认关)。真机实测:600/300 MiB 水位下 6 次不同快照的 restore 把
**4.83 GB / 9 条目降到 537 MB / 1 条目**,且 `drop_caches` 之后每个 sandbox
都读到自己的 marker。

#### 三个决定值得记下来

**水位是一对而不是一个**,抄的是 kubelet image GC(85/80):
单阈值会让触发之后的**每一次** restore 都付一次回收,成对才让回收是偶发的批处理。
低水位默认取高水位的 80% —— 比例是运维没有依据去选的部分,
而「缓存最多占多大」是他们有依据的部分。

**记账用已分配块而不是名义大小**(`st_blocks * 512`)。合并出的内存镜像
在没有祖先写过的地方是稀疏的,按名义大小记账会为了回收「零字节」而淘汰条目 ——
这和 §3.7 磁盘记账犯的是同一个错,只是方向相反。

**淘汰只需要护住 lookup→open 这一段,不是 VM 的整个生命周期。**
最初我以为「正在被 UFFD mmap 的条目不能删」是主要风险,写进了 GitHub #25。
**这个判断是错的** —— 写了个 C 程序实测:mmap 一个文件、unlink 掉、
再把每一字节读回来,数据完整(inode 活到最后一个映射消失)。

真正的窗口窄得多也真实得多:restore 先 `Lookup` 拿到路径,
过一会儿才 `newUffdHandler` 打开内存镜像。**在这两点之间 unlink,
open 就是 ENOENT,而此时该 restore 的 stream 已经消费完 —— 没有东西可以重建。**
所以 pin 只跨 `stageSnapshot` 到 `loadSnapshot`,`stage.Close()` 就释放,
即使 VM 还在跑也安全。

pin 是计数的,因为并发 restore 会同时持有同一个 leaf;
`unpin` 幂等,因为 `Close()` 会走错误返回和 defer 两条路径。
故意把 pin 检查短路掉之后,两个测试立刻变红 —— 这是唯一能确认测试有效的办法。

## 4. 未决 / 待验证

- **overlaybd lazy-pull 接入 `image.Provider`**:能力本身已实测跑通(§3.1),
  剩下的是写 `OverlaybdProvider` —— configfs 编排 + registry 推送 + 生命周期。
  不再是「能不能用」的问题,是「接进来」的工程量。
- **升级宿主内核到 6.8**:20.04 的 apt 里没有 6.x(HWE 终点是 5.15)。
  需要 mainline PPA 或升级发行版。收益是 ublk(overlaybd 的更快后端)。
  这台机器是 VM(`/dev/vda2`),换内核后 nested KVM 能否保持可用未验证。
  **优先级已下调** —— tcmu 后端功能完备,ublk 只是性能优化。
- **AVX-512 掩码未实测**:验证机(Zen 2)没有 AVX-512,
  所以 mask 表里 5 个 avx512 位只是「按 CPUID 规范写对了」,
  没有在真硬件上验证过掩掉的效果。需要一台有 AVX-512 的机器跑
  `hack/cpu-template-probe.sh` + guest 对比。
- **同 family 跨 model 的 restore 未实测**:只有一台 fc 机器,
  无法验证 template 是否真的让快照跨型号可用 —— 这正是它存在的理由。
  逻辑上正确(掩掉了型号差异的来源),但没有实证。
- **`--track-dirty-pages` 的开销未实测**:diff snapshot 已实装(§3.0.1)但该开关
  默认关,因为 KVM 脏页记账的代价没量过。需要同镜像同内核、开/关各跑 N 次,
  对比 boot-to-agent 与一个 CPU-bound + 一个 memory-bound 的 exec 吞吐。
  回归 < 2% 就改默认开。
- **日志与 CLI 输出标准化**:日志全是 `log.Printf`(71 处),无结构化、
  无级别、不带 request_id。CLI exit code 只有 0 和 125,无 `--json`。
- **稀疏 CoW 层写满宿主盘时 guest 看到什么**:未实测。dm-thin 有明确的两种模式
  (排队挂死 / EIO),但我们是 ext4 上的稀疏文件,`fallocate` 失败发生在
  写入路径而非设备层。这个失败模式决定了接单水位要留多少余量(§3.7)。

## 5. 启动优化总账

按贡献排序,全部真机实测:

```
gRPC 重连退避         -800 ms   改 BaseDelay 1s → 20ms
关串口 (quiet)        -493 ms   8250 UART 同步写
换 CI 内核             -90 ms   6.1.175 → 6.1.102
health 轮询粒度        -40 ms   50ms → 10ms
─────────────────────────────
create   2200 ms → 952 ms

UFFD 按需供页        -1296 ms   /snapshot/load 1303ms → 7ms
unpack 缓存           -550 ms   每快照解一次而非每次 restore
─────────────────────────────
restore  1500 ms → 950 ms(首次 1617ms)
```

**教训:最大的两块都不在「内核/虚拟化」层,而在我们自己的代码里。**
gRPC 退避和串口加起来 1293ms,占冷启动优化的 96%。
一开始我以为瓶颈是 guest 内核启动,归因之后才发现内核只占其中 90ms。
先测再改,这条在这次是决定性的。

## 参考

- [firecracker: handling page faults on snapshot resume](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/handling-page-faults-on-snapshot-resume.md)
- [firecracker: guest_configs](https://github.com/firecracker-microvm/firecracker/tree/main/resources/guest_configs)
- [e2b-dev/fc-kernels](https://github.com/e2b-dev/fc-kernels)
- [tensorlake: Firecracker disk snapshots in O(changed bytes)](https://tensorlake.ai/blog/firecracker-disk-snapshots-o-changed-bytes)
- [Restoring Uniqueness in MicroVM Snapshots (AWS)](https://arxiv.org/pdf/2102.12892)

# 技术选型全景:每一项用了什么、为什么是它

> English: [../tech-stack.md](../tech-stack.md)

> 状态标注约定见 [architecture.md](architecture.md) §0。
> **权威顺序:代码 > [status.md](status.md) > [decisions.md](decisions.md) > 设计文档。**

本文梳理 `bean` 依赖的每一项技术:它是什么、在这里做什么、**为什么选它**、
**放弃了什么以及为什么**。有实测数据的地方直接给数字;属于判断而没有实测支撑的,
明说是判断,不借用它没有的权威性。

两点声明。第一,本文不是交付声明 —— 标记区分「跑得起来的」和「设计好的」,
而 jailer 在后者。第二,本文不是依赖清单:
`go.mod` 只有**四个直接依赖**,下面多数有意思的决定,恰恰是**决定不引依赖**。

全文实测机器:AMD EPYC 7542(Zen 2),16 物理核,24 GB,
guest kernel 6.1.102,Alpine 3.20。

---

## 1. 隔离:Firecracker microVM ✅

VMM 是 Firecracker。`internal/node/runtime/fc_linux.go` 每个 sandbox 拉起一个进程,
通过它的 Unix socket HTTP API 驱动 —— 配置文件能描述初始机器,但 pause、resume、
snapshot 只能走 API,所以一个 client 覆盖整个生命周期(`fc_api.go`)。

是负载决定的。sandbox 里跑的是 eval harness 生成的代码,**按定义就是不可信的**,
而「不可信代码」最便宜的正确答案是硬件虚拟化边界,不是共享内核。
另外 Firecracker 原生支持平台组织方式所依赖的两件事:内存快照与按需供页。

**放弃了什么:**

- **普通容器(runc,或一个任务一个 Pod)。** 共享一个内核,所以容器逃逸就是宿主失陷。
  边界是 syscall 过滤器而不是硬件。对一个专门跑没人审过的代码的 harness 来说,
  这是错误的默认值。
- **gVisor(runsc)。** 设计里保留它作为无 `/dev/kvm` 宿主的兜底档(architecture D3),
  不是主档。syscall 模拟层就是一层兼容性表面,而 eval 镜像是任意的 ——
  任何要编内核模块、用了冷门 syscall、以不常见方式读 `/proc` 的镜像都会变成一个支持问题。
  真内核没有这层表面。**✅ 已实现**:`--runtime runsc` 经 `NewOCITier` 直驱 OCI runtime(无 containerd),共用 fc 档的 rootfs providers;runc 与之同一套实现,只差二进制。
- **Kata Containers。** 被「直接驱动 Firecracker」取代。Kata 的价值是一个 CRI 兼容的
  VM runtime;平台不说 CRI、也不想让 containerd 上热路径,所以 Kata 只会是一层纯翻译。
- **QEMU。** 功能完备因而庞大:完整设备模型、大得多的攻击面、boot 时间以秒计而不是
  几百毫秒。Firecracker 存在的理由正是这个取舍对短命 sandbox 不成立。
  **这一条没有我们自己的对比实测**,依据的是 Firecracker 公开的设计理由。

**实际有的隔离,如实说**(⚠️):VMM 以 root 跑在宿主 mount namespace 里,
只有 Firecracker 内建的 seccomp。**jailer 和宿主侧 cgroup 包装都没实现**(GitHub #20)。
硬件边界仍然在,但纵深防御比应有的薄:一个 Firecracker/KVM 漏洞的后果是宿主 root,
而不是一个被 chroot 的降权用户。

**还有第三档,而它根本不是隔离。** `LocalRuntime`(`internal/node/runtime/local.go`)
把 sandbox 跑成宿主进程树,用真的 `beand` 二进制限制在一个目录里。它存在的目的是让
macOS 上的开发与 CI 能在没有 KVM 的情况下跑通同一套 agent gRPC 接口。
它不是安全边界,也从不作为安全边界提供给调用方。

### 这层边界的代价,实测

一次 create 到 agent 可达是 **952 ms**(234 ms runtime + 770 ms 等 agent),
并发下每个 `firecracker` 进程 boot 烧掉 **5 CPU 秒**,boot 完就不涨了。
16 并发时 `vmstat` 报 `r=16 / id=0 / wa=0`(16 核)。
所以**吞吐 ≈ 核数 ÷ 5 CPU 秒 ≈ 2.3 creates/s**,而且瓶颈是 guest boot 不是我们的代码:
并发 1 → 16,`runtime_create`(dm 组装 + VMM 拉起)只从 241 ms 走到 369 ms,
而 `agent_ready` 从 627 ms 走到 5710 ms。

这个数字就是快照恢复重要的原因。恢复完全跳过内核 init,
而这是绕开那 5 CPU 秒的唯一办法 —— 除了裁剪 guest 内核。

---

## 2. Rootfs:device-mapper snapshot ✅

每个 sandbox 需要一个从镜像派生出的可写根文件系统。当前路径是 device-mapper 的
`snapshot` target:每个镜像一个只读 loop device,节点上所有 sandbox 共享,
外加每 sandbox 一个稀疏 CoW 存储(`internal/node/image/devmapper_linux.go`)。
表是 `0 <base_sectors> snapshot <base_loop> <cow_loop> P 8` ——
`P` 是 persistent,exception 存进 CoW 元数据区,所以设备可以拆掉再组装,
这是「快照能捕获 CoW 层并在别处重放」的前提;`8` 是 chunk size(sector 单位,即 4 KiB),
够小,所以单块写只 copy 4 KiB 而不是几十 KiB。

**实测:每 sandbox 实占 44 KiB**,对应的名义申请是 20 GiB。
(早先的版本有些地方写 8 KiB、代码注释里写 80 KiB;那些测量点在 sandbox
生命周期的不同位置 —— 空的 CoW 层 vs sandbox 跑起来写过之后。44 KiB 是应该引用的值,
重点是数量级,细节见 `status.md`。)一个镜像 fan-out 一百份的成本就是一百个稀疏文件 ——
这正是平台存在的批量 eval 场景。

Provider 是分层而不是揉在一起:`PullingProvider` 包住任意一个内层块设备后端,
负责「首次使用时拉取」并做并发去重;`DevMapperProvider` / `FileProvider` 负责
「设备怎么组」。**镜像从哪来**和**块设备怎么组**是两件事,所以换后端不用重写拉取。

**放弃了什么:**

- **每 sandbox 全量拷贝。** 就是 `FileProvider`,只留作没有 `dm_snapshot` 时的兜底。
  512 MiB 镜像每 sandbox 就是 512 MiB 磁盘加上写它的时间。一百份的差别是
  「一百个稀疏文件」对「50 GB」。
- **overlayfs。** 文件系统层 union,产物是目录树而不是块设备。microVM 需要的是
  virtio-blk 上的块设备;替代方案是 virtiofs,而 Firecracker 对它支持很弱。
  把组合放在块层意味着一条镜像链路同时服务 microVM 档和(设计中的)容器档。
- **dm-thin。** 能力更强 —— 真的按设备定容、真的配额 —— 但在这里它的失效模式比
  dm-snapshot 更糟。内核默认 `queue_if_no_space` 下,把池写满的 guest 会**挂死**,
  而元数据耗尽要离线 `thin_check`/`thin_repair`,加数据空间修不回来。
  实测下 dm-snapshot 失败得更硬,但更可读(见下面 §7)。
- **每 sandbox 一套 TCMU/SCSI。** 每个 sandbox 一整套 SCSI fabric(loopback nexus):
  脆弱且慢,对比 `dm_snapshot` 只要一个内核模块。

### overlaybd:已接进 `image.Provider`,dm-snapshot 仍默认 ⚠️

这一条值得单独解释,因为「跑通了却不设默认」看起来像疏漏,但不是。

overlaybd(DADI,阿里)是块级 lazy-pull:层在 registry 里是块设备 diff,
挂载只 range 读实际访问到的块。2026-08-02 实测
(Ubuntu 20.04 / kernel 5.15 / tcmu backend / alpine 3.20):

```
挂载时间                          7 ms
挂载 + 读 /etc/os-release         1014 KiB / 5175 KiB = 层的 19.6%
读完整个文件系统                  1270 KiB(zfile 压缩)
registry 响应                     8 × HTTP 206 Partial Content
可写上层实占                      40 KiB(名义 1.1 GB,真稀疏)
```

overlaybd 日志里的 `__open_ro_remote` 证实它打开的是 HTTP URL 而不是本地文件。
25 ms 就绪,没有全层下载。

**为什么它不是默认路径。** 决定排序的那个认识是:**overlaybd 的价值在「首次使用大镜像
的等待时间」,不在「每 sandbox 成本」—— 后者 CoW 已经用 44 KiB 解决了。**
所以它是冷镜像路径上的优化项,不是平台立起来必须的基础设施,因此排在快照能力之后。
它要打的那个冷镜像数字是真实的(busybox 5–10 s,网络差时 alpine 到 2 m 45 s),
但对一批**事先知道自己要用哪些镜像**的 eval(它确实知道),
prewarm + 镜像亲和调度覆盖了同一个场景。

它现在已作为 `OverlaybdProvider` 接进同一个四方法 `Provider` 接口:
configfs 编排、registry 推送、生命周期,用 `--fc-overlaybd` 开启、真机验证过;
dm-snapshot 仍是默认。验证中撞到的两个陷阱都在那份代码里,
因为它们在生产会复现,而文档不会替我们记住:

1. **LUN 必须在 nexus 之后链接。** 顺序错了内核报
   `TCM_Loop I_T Nexus does not exist` —— SCSI host 在注册时扫 LUN,那时 nexus 还是空的,
   而事后写 nexus **不触发 rescan**。设备永远不出现,而 configfs 看起来完全正常。
   正确顺序:backstore → tpgt → nexus → LUN link。
2. **每个 backstore 必须设 `wwn/vpd_unit_serial`。** TCMU 默认不给唯一序列号,
   于是两个内容完全不同的 overlaybd 设备都拿到 WWID `36001405` + 全零,
   `multipathd` 把它们合并成一个 `mpatha`。症状不是报错,而是**读到另一个镜像的数据**,
   外加原设备变 busy、没法直接挂载。

tcmu 后端功能完整,所以**不需要先升级宿主内核**;ublk(≥ 6.0)只是更快。
接入 overlaybd 还会改变层的故事:今天转换会把镜像压平成单个 ext4、丢掉层结构,
所以 `commit` 是读出一个全量镜像而不是 seal 一个增量层。有了 overlaybd,
`overlaybd-commit` 直接 seal LSMT 可写层,「零转换」这个承诺才变成字面意义上的真。

**Nydus 被否掉的理由和 overlayfs 一样**:它是文件级的,文件系统语义进不了 microVM,
fc 档就得用 virtiofs。它作为容器档的备选保留。

---

## 3. 快照与 restore

> 本节讲的全部是 **restore** —— 从盘上的快照造出一个新 sandbox。resume(给一个从未
> 停止运行的进程解冻 vCPU)与这里的任何机制都无关。见
> [snapshot-resume.md](snapshot-resume.md) §0。

### 3.1 UFFD(userfaultfd)做内存恢复 ✅

Firecracker 的 `/snapshot/load` 有两种内存后端。`File` 在 VM 跑之前把整个内存镜像读进来;
`Uffd` 把 guest 内存匿名映射,由一个 handler 进程在 guest 触发缺页时按需供页。

**实测,恢复一个 512 MiB 的 guest:**

```
restore 总计       1400 ms
├─ restore_load    1303 ms   ← 拉 blob + gunzip + 把内存/rootfs 写盘
└─ 等 agent           97 ms   ← 内存已恢复,进程还活着
```

对比 1040 ms 的冷启动,**恢复比从头 boot 还慢**,而其中 93% 是 `restore_load`。
换 `Uffd` 后 `/snapshot/load` 从 **1303 ms → 7 ms**,恢复过程**零写盘**。
成本随 guest 实际触及的页数走,而不是随 guest 大小走。

`File` 是被这个实测否掉的。另外两个选项是被分析否掉的:

- **按 snapshot id 缓存解包后的内存文件。** 去掉了重复解压,但首次恢复仍然要写 512 MB,
  而且长期占盘。这原本是计划;UFFD 直接消灭了这项成本。两者不冲突,
  现在系统里两个都有(§3.2)。
- **池化预恢复好的 VM。** 每个池成员都持一份内存副本,而实测说明瓶颈在解包与写盘
  而不在 VM 恢复本身 —— agent 只等了 97 ms。池化是花内存去解决一个不是问题的问题。

UFFD 也不是赌注而是共识:e2b 有完整 handler
(`packages/orchestrator/pkg/sandbox/uffd/`,含 cgo),agentenv 有 Rust 的
`storage/uffd-core/`,tensorlake 公开宣称亚秒冷启动。

**共享之所以安全,是因为 Firecracker 用 `MAP_PRIVATE` 映射内存文件。**
这一条是验证过的而不是假设的:在 guest 里写 64 MB 随机数据后,
宿主上那个内存文件的 md5 不变。这才使得一份解包后的内存镜像可以服务任意多次恢复。

**两个只有真跑起来才会出现的协议细节**(都在
`internal/node/runtime/uffd_linux.go`):

1. **fd 和 region 布局不一定在同一个数据报里。** 一次 `ReadMsgUnix` 可能返回 fd
   但 body 是空的 → JSON 解析失败 → handler 死掉 → Firecracker 在第一次缺页上
   永久阻塞。必须循环读到两者都到。agentenv 的 Rust 实现也是循环。
2. **Firecracker 交过来的 fd 是非阻塞的。** 直接 `read` 立刻返回 `EAGAIN`,
   缺页循环当场退出。必须先 `poll` 等可读。

两个错误的表现都是「`snapshot/load` 永久挂住」,而这和「handler 崩了」无法区分 ——
这就是 handler 必须有 `Err()` channel 的理由。Firecracker 自己的文档也写了:
handler 死掉,**VM 会在下一次缺页时永久挂住**,所以存活监控是必需的。
balloon 的 `MADV_DONTNEED` 会产生 `UFFD_EVENT_REMOVE`,handler 必须把对应页**清零**
而不是重读文件,否则会把旧数据复活。

### 3.2 解包缓存,以及为什么 rootfs 不进缓存 ✅

load 降到 7 ms 之后,剩下的 ~1060 ms 全是解包(gunzip 加写 512 MB)。
同一个快照每次解包的输出逐字节相同,所以按 snapshot id 缓存(`snapcache_linux.go`):
**首次恢复 1617 ms,之后 ~950 ms。**

**可写 rootfs 刻意不缓存。** 从同一个快照恢复的两个 sandbox 在第一次写入时就分叉了,
所以各自必须有独立设备。它之所以负担得起,是因为它是稀疏 extent 列表、
而新 sandbox 几乎没写过东西 —— 这个不对称正是「共享内存 + 分开 rootfs」能成立的原因。

缓存回收是高低双水位 + LRU,**按已分配块计账(`st_blocks * 512`)而不是名义大小** ——
合并后的内存镜像在所有祖先都没写过的位置是稀疏的,按名义计账会为了回收零字节而驱逐条目。
双水位这个形态抄的是 kubelet 的镜像 GC:单阈值会让触发之后的**每一次**恢复都付回收成本,
双水位让回收保持为偶发的批处理。600/300 MiB 水位下实测,6 次不同快照的恢复从
**4.83 GB / 9 条降到 537 MB / 1 条**,`drop_caches` 之后每个 sandbox 仍能读回自己的 marker。

这里有一个信念被实测证伪了,值得记下来:原本假设的风险是「正被 UFFD mmap 的条目不能删」。
一个 C 程序把它推翻了 —— mmap 一个文件、unlink 它、再读回每一个字节,数据完好,
因为 inode 活到最后一个映射消失。真正的窗口窄得多:一次恢复先 `Lookup` 拿到路径,
之后才 open 镜像,所以在这两点之间 unlink 会得到 `ENOENT`,
而那次恢复的流已经被消费掉了 —— 没有任何东西可以重建。
所以 pin 只覆盖 `stageSnapshot` → `loadSnapshot`。

### 3.3 增量快照:在恢复时物化 ✅

Firecracker 原生支持 diff 快照;平台以 `--base SNAP` 暴露它,
实测 **298 KB 对 15.5 MB 全量(52×)**,depth 2 验证过能恢复出预期的三个文件,
`uptime 57` 证明是 resume 而不是 reboot。

Firecracker 的 diff 内存文件**不自包含** —— 它是稀疏文件,必须叠到一个 base 上。
所以真正的问题不是「怎么产生 diff」,而是**什么时候、在哪里合并**,
而业界正好在这一点上分成两派,**两派都在生产跑着**:

- **E2B** 在缺页时做分层查找:UFFD handler 穿过 `block.Slicer` 走 base 加每一层,
  于是 K 次 pause/resume 之后一次读要「追 K 个不同的 BuildId 引用」。
  链深无上限,只有 `NormalizeMappings` 合并同一 build 的相邻段。
  他们自己的公开分析明说**跨 build 碎片随时间增长**,读放大与深度成正比。
- **Cognition 的 blockdiff** 把链只当血缘,运行前压平成 raw。
  `apply` 是纯元数据操作(XFS reflink)—— 128 GB 的 `cp --reflink=always` 实测
  **0.008 s 对 24.5 s**。他们的压平本质免费,所以文章从头到尾没讨论读放大:
  运行时根本没有链可走。
- **Firecracker 上游** 有 `snapshot-editor edit-memory rebase`,就是压平,
  而且要求按创建顺序叠加。

**我们选压平,而理由不止「跟多数」。** 我们有一个 E2B 没有的结构性优势:
`snapCache` 已经按 snapshot id 缓存解包结果,所以合并是**每个 leaf 每个节点付一次**,
该节点之后的每次恢复都复用。fan-out 恰恰就是「同一个 leaf 被恢复很多次」,
所以在 diff 存在的意义所在的那个场景上,合并被完全摊平。

更重要的理由是**缺页路径完全不变**。`fill()` 是整个系统里最热、也最阴险易错的代码 ——
那里的一个 bug 是一页错的内存,而且**没有任何错误信号**。
压平让 `uffd_linux.go` 仍然只服务一个扁平镜像,和全量快照一直走的是同一份代码。

**链深上限 8**,超过就静默转成全量。E2B 不设上限并因此承担了增长的碎片,
这本身就是设上限的依据。它给恢复成本设了界、让祖先可以被回收,
也让调用方永远不用考虑链深:diff 请求永远成功,只是偶尔更贵。

有三件事绝不能静默,而它们没有静默:

1. `track_dirty_pages` 必须在 boot 前设置且不存进快照,所以它是节点配置
   (`--track-dirty-pages`,**默认关**,因为它的开销从未被量化)。
   没开它的 guest 请求 diff 会得到**显式报错**,绝不降级成全量 ——
   否则调用方以为省了空间而其实没有,而单看大小解释不了为什么。
2. diff 内存用**独立成员名** `memory.diff`,而不是在 `memory` 上加个 flag。
   混淆两者的后果在两个方向上都很坏:把全量当 diff 叠会抹掉 base 从未触及的页,
   把 diff 当全量加载会给 guest 一片布满空洞的内存。按成员名分派让两种错误都不可能发生。
3. 删除有后代的 base 会被拒(409)。否则失败在时间和空间上都很远:
   现在删成功了,之后在另一台机器上恢复失败。

**顺序是调用方的契约,且无法从数据里恢复** —— 后面的层合法地覆盖前面的,
所以顺序反了会得到一个「结构完好但由过期页拼出来」的镜像,下游检测不到。
所以 `store.SnapshotChain()` 一次性定序,链在 spec 里声明而不是从流里发现。

### 3.4 顺序:CoW 必须在设备组装之前回填 ✅

这不是技术选型,但它是上面那些选型可信的理由,所以放在这里。dm-snapshot 在
`dmsetup create` 那一刻把 exception table 读进内核内存,之后再也不读回。
往一个**已激活**设备的 CoW 后备存储里写,内核不认那些 chunk,设备继续供 base 镜像。

最初的 restore 就是这么做的,而**在全量快照上这个 bug 完全静默:**

```
恢复后立刻读:      cat /root/marker  →  在      ← 命中内存快照带回来的 page cache
drop_caches 之后:  cat /root/marker  →  9 × \0  ← 真的读块设备了
                   ls -la            →  size = 9 ← 元数据在内存里,而且是对的
                   dmesg             →  什么都没有
```

元数据在内存镜像里,数据在块设备上,两者不一致,而 ext4 没有理由起疑。
修法是一个 `PrepareOptions.SeedWritable` 回调,由 provider 在「CoW 已创建」与
「设备已组装」之间调用 —— 这迫使 restore 改成先把 bundle 落到 staging 目录,
并且 extent 流只解码一次,就在写进设备的那一次。

**没有别人往已激活设备的 CoW 里写**:firecracker-containerd 的 devmapper snapshotter
从 thin pool 派生、之后才激活,所以顺序天然是对的;Lambda SnapStart 提供的是
分块懒加载的块设备;E2B 的 rootfs 就是宿主上的一个文件,CoW 在文件系统层。
Firecracker 上游文档干脆把磁盘状态甩回给调用方保证 —— 我们撞到的正是它警告的那一类。

### 3.5 CPU 模板 ✅

guest 在 boot 时读一次 CPUID 就把答案缓存了 —— glibc 据此挑它的字符串例程。
把这个 guest 恢复到缺少某个特性的宿主上**不会在恢复时失败**,
它会在之后、在恰好接下来运行的那段代码里崩。所以掩码只在 boot 前有效,
在快照时补不上。

**Firecracker 的五个内建静态模板是被实测否掉的。** 在验证机(EPYC 7542,family 23)上
**一个都起不来:**

```
T2 / C3 / T2S / T2CL  →  "CPU vendor mismatched"              (全是 Intel-only)
T2A                   →  "current CPU model is not permitted" (仅 Milan/Zen 3)
```

值得注意它怎么藏起来的:`PUT /machine-config` 对**每一个**模板名都返回成功,
vendor 校验发生在 `InstanceStart`。只测配置会得出「五个全支持」这个错误结论。

所以:走 `/cpu-config` 的自定义模板。这也让平台的可移植性不再绑在 AWS
选择支持哪些 CPU 型号上。两个只有真机才暴露的细节:

- **bitmap 宽度是 31,不是 32。** 32 位报 `string is too long`。单测在 32 位下全绿;
  真机第一次 create 就失败。后果是**第 31 位无法掩码**,而 `avx512vl` 正好在那儿 ——
  所以它被单列进 `UnmaskableCPUFeatures` 并写进启动日志,而不是被谎称已掩码。
- **不要掩 xsave。** 掩掉 leaf 1 ECX bit 26 确实能让 `xsave` 消失,
  但 XSAVE 子特性在 leaf 0xD 里,实际仍然可见,于是 guest 会看到一份
  不对应任何真实处理器的 CPUID。而且能跑 Firecracker 的机器都有 xsave。

**vendor 和 family 掩不掉** —— leaf 0 带 vendor 字符串,guest 内核据此做 errata
处理和 MSR 访问。所以模板买到的是**同 vendor 同 family 内**的可移植性,
跨过这个边界必须由调度器拒绝(`409 INCOMPATIBLE_CPU`),
而不是放上去让 guest 之后乱来。**model 刻意不记录**:
掩掉指令集特性正是让快照跨 model 可用的手段,按 model 匹配会把模板的价值全部抹掉。

`hack/cpu-template-probe.sh` 把这套探测冻成脚本,它与代码里的 `cpuBitmapWidth`
不一致时退出 70。**换机器必须重跑** —— 这些答案都是每台机器一份,而且失败是静默的:
`/cpu-config` 被拒会让 guest 完全没有掩码。它也暴露了验证的一个边界:
这台机器没有 AVX-512,所以掩码表里的 5 个 avx512 位从未被真正执行过,
只有 `avx avx2 fma f16c` 被实测掩掉了。

---

## 4. Guest 侧

### 4.1 内核:用 Firecracker CI 预编译版,不 fork,不建编译流水线 ✅

`hack/build-assets.sh kernel` 下载
`firecracker-ci/v1.11/x86_64/vmlinux-6.1.102`,并把 CI 单独发布的 `.config`
一起放在旁边 —— 所以「用预编译」和「自己手上有 config」不是互斥的。
脚本会校验下载物是 ELF,因为那个 bucket 被观察到会发截断文件,
而一个短内核的表现是「boot 挂住」,不是下载错误。

**调研结果:**

| 仓库 | 内容 | 是否 fork |
|---|---|---|
| `e2b-dev/firecracker` | VMM 源码 | **是**(加了 gdb feature 等) |
| `e2b-dev/fc-versions` | 编 VMM 的流水线 | 否 |
| `e2b-dev/fc-kernels` | 内核 config + patch + build.sh | **否** |

`fc-kernels` 在运行时 `git clone amazonlinux/linux` —— 和 Firecracker 官方
`rebuild.sh` 用的是同一份源 —— 而仓库本身只放一个 config 加一个 virtio_balloon patch。
所以 **e2b 的内核维护面就是一个 config 文件,没有 rebase 负担**,
这就是值得抄的那个面。e2b fork 了 VMM 但没 fork 内核;我们两个都不 fork。

自己编被否掉的理由是「先付成本再拿证据」:容器里编意味着要先付工具链、
拉源码、二十分钟构建,才能拿到第一个数据点,而当时甚至还没确立「换内核有用」这件事。

**然后实测**(quiet,VMM 启动到 agent 可连,各三次):

```
vmlinux-6.1.175   690 / 689 / 715 ms   (agentenv R2 站点,config 未知)
vmlinux-6.1.102   603 / 613 / 601 ms   (Firecracker CI,config 已知)
```

快 ~90 ms(13%),端到端 create 从 1040 ms 到 952 ms,快照/恢复不受影响。
**但要注意这个收益不是从哪来的**:CI 的 config 里 `CONFIG_SCSI_ISCSI_ATTRS`、
`CONFIG_BPFILTER`、`CONFIG_SQUASHFS`、`CONFIG_XFS_FS`、`CONFIG_NFS_FS` **全是 =y**。
CI 内核也没裁掉这些;差别主要是镜像更小(40.8MB 对 44.5MB)和版本本身。
**所以自己裁 config 的天花板比看起来低** —— `quiet`(−493 ms)或
gRPC backoff(−800 ms)那个量级的收益不藏在内核裁剪里。
所以不建编译流水线,而 config 已经在手上,万一情况变了随时能用。

### 4.2 启动参数 ✅

```
quiet reboot=k panic=-1 pci=off init=/bean/beand -- --listen vsock:1024 --pivot /dev/vdb
```

`quiet` 是有实测的那个。**不挂串口省 493 ms(41%)**:

```
console=ttyS0    1193 / 1195 / 1210 ms
quiet             700 /  700 /  711 ms
```

8250 UART 写入是同步的 —— 内核每打一行日志都等硬件。其余几个:
`reboot=k` 因为 Firecracker 没有 ACPI,keyboard reset 是最小可用路径;
`panic=-1` 让崩掉的 guest 保持可检查而不是进重启循环;
`pci=off` 因为根本没有 PCI 总线可枚举。

**这个取舍抄的是 e2b 的做法**:它的 `fc-kernels` config 里 `CONFIG_SERIAL_8250=y`
是开着的,但启动参数里不带 `console=`,所以一个内核既 boot 得快又能调试。
这里 `--debug-console` 把 `console=ttyS0` 加回去。失败的 boot 没有别的证据来源,
所以这个能力不能丢,但不该每次 boot 都付 493 ms。**代价必须说清楚**:
默认关串口,意味着 guest 写到 stderr 的一切 —— 包括 agent 那行带 trace id 的日志 ——
**默认是看不见的**。

### 4.3 agent 作 PID 1,住在自己的盘上 ✅

`beand` 是静态链接的 Go 二进制(`CGO_ENABLED=0`、`-ldflags="-s -w"`),
装在一个 32 MiB 的只读 ext4 镜像里,作为 guest 的 **root** 设备挂载:

```
/drives/agent   agent.ext4      IsRootDevice: true,  ReadOnly: true   → /dev/vda
/drives/rootfs  <cow device>    IsRootDevice: false                   → /dev/vdb
```

内核从它挂成 root 的那个设备上 exec init,所以把 agent 放在那儿意味着
**用户镜像不承担任何义务** —— 不用内嵌 `beand`、不用有 init 系统、不用改 entrypoint。
agent 起来之后自己 pivot 到 `/dev/vdb`。这就是「零镜像转换」在 agent 侧的全部内容。

**Firecracker 按注册顺序命名 drive**,而 `--pivot /dev/vdb` 是常量,
所以 agent 盘必须先注册。注册反了的表现是 guest 挂载失败,
而在默认不挂串口的配置下**没有任何输出** —— 这就是 `--debug-console` 存在的具体理由。

agent 盘用 symlink 链进每个 sandbox 目录而不是拷贝:一个 inode、零拷贝,
而且让它的 drive path 可以是相对的。

**所有路径都是相对的**,VMM 的工作目录设成 sandbox 自己的目录。
这不是整洁癖,是快照可移植性:Firecracker 把设备路径和 vsock UDS 路径
**存进 machine state** 并在 load 时重新解析,而且拒绝在 load 时覆盖 vsock 路径。
绝对路径会让恢复出的 VM 去找**源 sandbox** 的文件,而源可能已经销毁了。
相对路径则解析到「VMM 是在哪个 sandbox 目录里启动的」。
所以跨机恢复是从一个决定里掉出来的 —— cwd 加相对路径 —— 全系统没有任何路径重写逻辑。

被否掉的:exec/PTY 接口用 **CRI streaming exec**。性能差、没有文件 API,
而且要依赖一条很长的组件链。

### 4.4 vsock 作控制通道 ✅

microVM 在 guest 自己配好网络之前没有宿主可达的网络,而为了让宿主能跟 agent 说话
就给每个 sandbox 挂一张 tap,会让控制路径依赖宿主网络。
`AF_VSOCK` 在 VM 一 boot 就存在,所以 agent 在早期 boot 阶段就可达,
并且在 guest 网络损坏或不存在时仍然可达 —— 而今天它一直是不存在的。

两个标识都是常量:`agentVsockPort = 1024`、`guestCID = 3`。都不需要分配,
因为**每个 VM 有自己的 vsock 命名空间**,没有可冲突的对象
(CID 3 是 guest 可用的最小值,0–2 被协议保留)。
常量还让 guest 的 cmdline 不依赖宿主状态,所以它在快照前后完全一致 ——
少一个恢复时要对齐的东西。

同一套 `AgentService` gRPC 接口在设计上要能跑在容器档的 unix socket 上,
所以传输是抽象出来的而不是硬编码的(`internal/node/vsock/`)。

---

## 5. 语言与运行时:Go ✅

四个二进制、一种语言:`bean`(CLI)、`bean-api`(gateway,内嵌 scheduler /
image / snapshot 模块)、`noded`(节点守护进程)、`beand`(sandbox 内 agent)。
Go 1.26.1。

理由是具体的而不是泛泛的。`beand` 装在挂给**每一个** microVM 的盘上,
所以它的体积是按 boot 计价的,而一个不依赖 libc 的静态二进制正是这个要求所需 ——
`CGO_ENABLED=0` 让它在任何 guest 镜像上都能跑,glibc 或 musl 都行。
节点守护进程是 I/O 并发密集的:很多 sandbox、很多 gRPC 流、每次恢复一个缺页处理
goroutine,而 goroutine 正合这个形状。而且整个系统是一次构建,
所以 agent、节点、控制面共享同一份生成的 protobuf 类型,不用维护三套视图。

**Linux-only 的代码由 build tag 承载。** device-mapper、userfaultfd、Firecracker、
`SEEK_HOLE` 稀疏文件遍历都是 Linux 内核接口,所以那些文件是 `//go:build linux`
并配 `_other.go` 对应物。这不是可移植性表演:它是让 `go build ./...` 和整套单测
能在 macOS 笔记本上跑起来的原因,而 `LocalRuntime` 档在那里执行同一套 agent gRPC 接口。
`golang.org/x/sys/unix` 是由此引入的唯一直接依赖,因为 userfaultfd 及其 ioctl
不在标准库里。

### 标准库是默认值,`go.mod` 能证明

四个直接依赖:`golang.org/x/sys`、`google.golang.org/grpc`、
`google.golang.org/protobuf`、`modernc.org/sqlite`。文件里其余全是传递依赖。

自己写而不是引进来的东西:

- **Prometheus 暴露格式**(`internal/obs/metrics.go`)—— counter、gauge、histogram
  直接渲染,让二进制保持无依赖,而同一个 registry 之后可以被 OTLP exporter 包起来。
- **OCI distribution 客户端**(`internal/node/image/registry.go`)—— manifest、
  层 blob、token 认证,全在 `net/http` 上。
- **结构化日志**用标准库的 `log/slog`,不是第三方 logger;`log.Printf` 只剩 1 处,
  对应 92 处 `slog` 调用。(`decisions.md` §4 和 `status.md` 都还写着
  「71 处 `log.Printf`,无结构」——**这个缺口已经补上了,那两处文档过期了**。)
- **Python SDK** 用标准库 `urllib.request`,不是 `httpx` 也不是 `requests`,
  所以安装它不拉任何东西。(architecture.md §7 说它是「手写 httpx」——也过期了。)
- **S3 与 SigV4**,这是最清楚的一例,单独一节。

### 手写 SigV4,以及理由 ✅

`aws-sdk-go-v2` 是几十个模块、上百个传递依赖,而平台用到的是
GET / PUT / DELETE / HEAD 加分片上传 —— 五个操作。

不引它的真实代价是 SigV4 必须写对,而那不是「算个 HMAC」那么简单。
**兼容性几乎总是丢在规范化(canonicalisation)那一步**,尤其是对非 AWS 实现
(MinIO、Ceph RGW、各家云的 S3 兼容层)。算法是 AWS 完全指定的;
`sign.go` 的价值在于把规范化做对。

除了依赖体积之外的好处是具体的:请求路径上没有隐藏的重试、连接池、region 解析,
所以出问题时看得见全部;以及能精确控制哪些 header 参与签名 ——
这是对接非 AWS 实现时最需要调的地方。只签 `host`、`content-type` 和 `x-amz-*`:
签的 header 越多,一个加 `X-Forwarded-For` 或规范化 `Accept-Encoding` 的代理
就越容易让签名失效。

做错了只会得到签名不匹配而不是清晰报错的几个细节:header 名小写、排序、去重
(Go 的 `http.Header` 是 canonical-MIME 形式);值要 `TrimSpace`,因为服务端会 trim;
`host` 要从 `req.Host` 取,因为 Go 不把它放进 header map;
空字符串的 SHA-256 硬编码,因为 S3 **要求** `X-Amz-Content-Sha256` 存在,哪怕 body 是空的;
用 `EscapedPath()` 而不是原始 path,否则 key 含空格或非 ASCII 会失败。

**刻意不做时钟校正。** 偏移超过 ±15 分钟会拿到 `RequestTimeTooSkewed`。
偏这么多的机器有更严重的问题(TLS、租约、日志时序),
在 S3 客户端里补偿只会掩盖它。

分片上传和 range 读在同一个 client 上实现。失败绝不能留下可读的半成品,
所以中断的上传不留任何对象 —— 这是对着真 MinIO 在 CI 里验证的而不是假服务器,
因为 `ErrBlobNotFound` 映射、abort 语义、range 读边界都是**服务端行为**,
假服务器只会确认我们自己的假设。

⚠️ **凭证是缺口**:节点从环境变量取 S3 凭证。设计里是控制面签发 presigned URL、
节点不持长期凭证;这部分未实现。

---

## 6. 控制面

### 6.1 SQLite 或 Postgres,由 flag 决定 ✅

热状态 —— sandbox 元数据、租约、调度承诺 —— 落关系库而不是 S3,
因为承诺需要事务:调度决策、资源扣减、命令记录必须原子提交,
否则两个副本会重复放置。

今天是 `modernc.org/sqlite`:**纯 Go,无 cgo**,这一点重要是因为构建里别处要求
`CGO_ENABLED=0`,而一个 cgo 版 SQLite 会把工具链故事劈成两半。
`SetMaxOpenConns(1)` 强制单写者。

Postgres 现在是一个 flag,不是一个项目:`bean-api --postgres <dsn>`。这才是多副本的
前提 —— SQLite 是一个文件,两个副本没法共享。

**是方言,不是第二套实现。** 一套用 `?` 写的语句,按引擎改写。规模是实测出来的
(103 处占位符加少数 DDL 构造,八条 `ON CONFLICT` 原样可移植),不是凭口味定的。
两套必须保持一致的 SQL、再配一个只能事后告诉你哪一套漂了的套件,是更糟的处境。

**真正让换引擎成立的是原子性,不是接口。** 本节早先的版本担心的是抽出一个 `Store`
接口,那反而是容易的一半。难的一半是:39 个方法里有 37 个依赖进程内的 mutex 保证
原子性 —— 而它本来就无法为第二个副本的写入定序,还掩盖了一个真实的丢更新 bug
(去掉它之后实测:200 次更新丢了 194 次)。现在每个操作的条件都在它自己的语句里,
store 里没有任何 mutex。

**光读 SQL 不足以完成移植。** 对真 Postgres 跑一遍,比事前梳理多找出四处差异,
其中 `INTEGER` 这条读代码根本读不出来 —— SQLite 里是 64 位,Postgres 里是 32 位,
而所有时间戳都是 Unix 毫秒:拼写完全一样,含义不一样。它还顺带抓出一个两个引擎共有的
真 bug:`Reserve` 有 8 个占位符、9 个实参,也就是 GPU 那条守卫根本不存在,而 SQLite
默默吞掉了多余的实参。完整清单、以及为什么现在有一个逐方法的 smoke test 加漂移守卫,
见 `status.md`。

被否掉的:**用 etcd 或 K8s API server 当存储**。调度器刻意是自己的
(architecture D7),因为 eval 调度足够简单,自己写反而能做 K8s 做不到的优化:
按「某镜像已在某节点缓存中的字节占比」给亲和性打分,以及同一次 eval 内的反亲和,
让一个节点故障不吞掉一整批。而一旦调度器是自己的,
一个通用分布式存储就买不到事务已经给的东西。

### 6.2 组件间 gRPC,边缘 REST ✅

两个内部接口:控制面 ↔ `noded` 之间的 `NodeService` + `SandboxService`,
以及 `noded` ↔ sandbox 内 agent 之间的 `AgentService`。`proto/` 下的 `.proto`
是唯一事实来源,用 `protoc-gen-go` 和 `protoc-gen-go-grpc` 生成,
版本**在 Makefile 里钉住**,让生成代码在各机器与 CI 上可复现。

gRPC 在这个系统实际要做的事情上挣到了位置:stdout/stderr 交错的流式 exec、
文件传输、长连心跳流,以及从一份定义为 agent 和节点同时生成客户端。
在手写 HTTP 上做双向流意味着自己写一套分帧协议。

一个有实测的调参决定。agent 在 ~700 ms 就在监听了,但 `agent_ready` 报 1493 ms,
因为 agent 在 guest boot 完之前无法监听,所以**第一次 dial 必然失败** ——
而 gRPC 默认 `BaseDelay` 1 秒,于是连接在退避里躺满一秒,上面的轮询空转。
改成 `BaseDelay` 20ms / `MaxDelay` 1s,轮询粒度 50ms → 10ms:
**create 从 2.2 s 到 1.04 s**。这个道理可以推广:
重试间隔应该匹配「一次 boot」的时间尺度,而不是「一个远程服务挂了」的时间尺度。

**边缘是 REST**,手写在 `net/http` 上(`internal/control/api/`)。
调用方是 Python 写的 eval harness 和一个 CLI,所以 `curl` 和 `urllib.request`
必须在没有 codegen 的情况下能用。没有用 grpc-gateway:
它出现在 `go.sum` 里只是因为 `hack/tracedump` 拉了 OTLP proto 包,
`go mod why` 确认主模块不需要它。

### 6.3 调度与容量,以及为什么必须做归因

这不是依赖,但它是若干技术决定被逼出来的地方。

**`max_creates=16` 从来不是真的限制。** 三种配置,同样的 30 并发:

| 节点配置 | 成功数 | 真正的限制者 |
|---|---|---|
| disk 100 GiB, cpu 8 | **5** | `102400 / 20480` = 名义磁盘计账 |
| 同上,申请 2 GiB 磁盘 | **8** | `cpuAllocatable 8 / 1 vCPU` |
| disk 1 TiB, cpu 32 | **16** | 这次才真是 `max_creates` |

限制者会跑到**计账最粗的那个资源**上,而默认配置下那是磁盘。
把最初那个「成功 16 个」归给 `max_creates` 是个巧合 —— 那台机器恰好 16 核。
所以现在的拒绝会说出是哪个资源用尽、卡住了几个节点、最接近的节点差多少,
因为面对一个没有归因的容量错误,最可能的反应是去调错的那个限制。

**没有磁盘超卖系数。** 系数是要求运维猜一个乘数,而稀疏文件的名义大小
本来就不该是计账输入。改成 `statfs` 上报实际占用 —— 差距很刺眼,
一个节点实测 `diskCommittedMiB: 0` 对 `diskUsedMiB: 76200`。
放置**仍然用承诺账本**,因为账本不会被突发超卖,而实测占用是滞后的,
按滞后的数字放置会让一批 sandbox 在同时开始写的时候一起撞墙。

**只有 create 并发排队,其余一律拒绝。** 排队把 30 并发从 **16/30 变成 30/30**,
墙上时间 8 s → 13 s —— 吞吐没变,还是 ≈2.3 creates/s,拒绝被换成了延迟。
对一个按定义就是突发、且被拒调用方会以另一个突发重试的负载,这是正确的取舍。
与 CPU/内存/磁盘的区别是**时长而不是严重性**:create 并发几秒内自己排空,
而资源承诺要持有 sandbox 的整个生命周期,等它只会晚一点返回同样的拒绝,
外加还占住了一个客户端。超时回 **504 `QUEUE_TIMEOUT`** 而不是 503,
因为请求是可受理的,只是节点忙得比调用方愿意等的时间更久 ——
503 会暗示集群太小。

---

## 7. 磁盘写满时会发生什么,以及它如何约束选型 ✅

`hack/enospc-probe.sh` 把 dm-snapshot 的 CoW 放在一个 64 MiB 的 loopback 文件系统上,
从 guest 侧一直写到宿主磁盘满:

```
RESULT: the write FAILED with exit 1
  dd: error writing '...': Input/output error
kernel: device-mapper: snapshots: Invalidating snapshot: Error reading/writing.
dmsetup status: 0 524288 snapshot Invalid
```

| | 实测 |
|---|---|
| guest 是挂住还是报错 | **报错**(EIO)—— 比 dm-thin 默认的 `queue_if_no_space` 好 |
| 设备状态 | target 变成 **`Invalid`**,不可恢复 |
| 之后还能写吗 | `write()` **仍然返回成功** ← 危险的地方 |
| 那些写活下来了吗 | **没有** —— remount 直接 `can't read superblock` |
| 共享 base | **完好**,能干净挂载;爆炸半径是一个 sandbox |

**「`write()` 成功而数据没了」这一行定下了设计。** 这和 CoW 回填那个 bug 是同一类静默失效:
上层看不到任何异常,直到 page cache 失效。所以**指望写满之后再补救是没有意义的** ——
到那时 sandbox 已不可恢复,唯一正确的动作是销毁它。防线必须在线之前:
节点在 `--min-free-disk-mib` / `--min-free-disk-percent`(默认关)以下拒绝受理,
回 **503 `NO_CAPACITY`** 并带上路径、当前剩余、水位线和后果,
不留 VM、不留映射、不留目录。base 完好是这个取舍显然便宜的原因 ——
代价是拒掉几个 create,不是丢掉一批正在跑的 eval。

分层顺序抄 kubelet:**回收触发线必须在停止受理线之下**,
否则一进入压力就直接拒绝服务,不给回收机会。

⚠️ **仍未验证**:guest 侧在自己的层无法分配时看到什么。宿主侧已实测,
但没人从里面看过。如果 guest 自己不报错,调用方拿到的是一个看起来健康、
实际在丢数据的 sandbox —— 这比拒绝 create 更糟,而答案决定了
这类 sandbox 是否该被主动标记 FAILED。

---

## 8. 镜像链路 ✅

```
① ParseReference     解析 ref
② 查 ImageDir        已有则直接返回  ← 不可变语义
③ FetchManifest      registry 认证 + manifest 解析
④ sizeFor(manifest)  从压缩层大小估算文件系统大小 ⚠️
⑤ writeBaseImage     建稀疏文件 → mkfs.ext4 → 挂载
⑥ applyLayer × N     按顺序解压,处理 whiteout
⑦ 补 guest 必需目录  /proc /sys /dev 等挂载点
⑧ 卸载 → rename      原子发布
⑨ 写元数据文件       记录这个文件来自哪个 ref
```

**节点自己拉镜像**,不 shell out 给容器运行时。sandbox 的 rootfs 是文件系统镜像
而不是容器快照,所以运行时里有用的那些部分 —— snapshotter、content store、daemon ——
都不可复用;需要的只是 manifest、层 blob 和一个解压的地方。
自己做还意味着节点不依赖 docker 或 containerd 装好且健康。
这与 architecture D2 说的「热路径无 containerd」是同一个结论,
而不同于「直接驱动 overlaybd」那一半,**这一半是交付了的**。

两处刻意的顺序:**元数据文件在镜像之后写**,因为 `Cached()` 据它上报,
先写会让节点宣称持有一个还不能用的镜像 —— 调度器据此把工作放过来,然后 create 失败。
以及**rename 才让镜像可见**,所以中断的转换不会留下一个看起来完整的半成品。

层路径在解压前做逃逸检查;`refToFilename` 把不同分隔符映射成不同字符,
所以 `a:b` 与 `a/b` 不会撞。

⚠️ **有一处文档行为与代码不符。** `image-pipeline.md` §2 说 gzip 判定不信 media type、
由 magic bytes 决定,并引用了 `applyLayer` 上方的注释 —— 但代码只按 `layer.MediaType`
分支(`convert_linux.go`:110),整个包里没有任何 magic byte 探测。
注释描述的是一个实现没有执行的意图,所以一个 media type 标错的 registry 会失败,
而不是被容错。

⚠️ **文件系统大小是估算的**,从压缩层大小推,是启发式而非测量。
**📐 没有缓存回收**:`ImageDir` 里的 base 镜像不会被清理,长期跑的节点会填满磁盘。
**📐 拉取时不校验 digest** —— registry 可信的前提下问题不大,但这是供应链防护缺的一环。

### 构建用 BuildKit ✅

`bean build` shell out 给 `buildctl`,对着一个 `buildkitd` socket。理由写在代码里:
COPY 和 ADD 语义、多阶段构建、ARG 插值、构建缓存、`.dockerignore`、heredoc
加起来是好几个月的工作,而且仍然会是一个不完整的模仿。e2b 和 Daytona 得出同样结论。

平台保留的是**输出形态**:BuildKit 能导出**扁平 rootfs tar**,
而这恰好就是 base 镜像需要的 —— 所以没有层组装、没有 registry 往返,
产物走的是和拉取镜像同一个 writer。`commit` 是反向路径 ——
从合成设备上读出完整 ext4 —— 而它产出的是全量镜像而非增量层,
因为 dm-snapshot 的 CoW 层不是 OCI 层格式。

⚠️ 构建日志不报进度,也无法取消。📐 构建产物只留在构建它的那个节点上
(GitHub #22),在多节点集群里这基本等于不可用。

私有 registry 凭证用标准库的 **AES-256-GCM** 静态加密,
节点从控制面获取而不是把长期 secret 放盘上。

---

## 9. 可观测性

### 9.1 OpenTelemetry + W3C traceparent ✅

request id 能回答「这次请求里发生了什么」,回答不了「那 1.2 秒去了哪一层」——
后者需要父子关系,而这些关系存在于**进程之间**,所以任何单个进程的日志里都没有。

第一棵测出来的 trace 给出了一个没人知道的数字:

```
POST /v1/sandboxes            bean-api   1196.0ms
  CreateSandbox               noded      1110.2ms   ← 86ms 空隙
    runtime.Create            noded       324.2ms
    agent.WaitHealthy         noded       785.8ms
```

那 86 ms 是调度加数据库写入,之前没有任何指标覆盖。
这正是 trace 的价值:它暴露的是**没人想到要去测的那一段**。

**request id 就是 trace id。** 两套 id 意味着每次关联都要 join,
而它们必然在跨进程那一跳分叉 —— 而那恰恰是唯一需要关联的地方。

**agent 刻意不链接 tracing SDK。** e2b 的 `envd` 能直连 collector;
`beand` 只有一条入向 vsock 连接、没有出向通路,
所以加一条反向通道要么破坏「零入向暴露」,要么需要在 `noded` 里做一个 OTLP 中继。
它只提取 `traceparent`、把 trace id 用在自己的日志行上,别的都不做,
因为 agent 装在挂给每个 microVM 的盘上 —— 体积按 boot 计价,
而一个 exporter 要服务的遥测数据本来就出不了 guest。

**⚠️ `decisions.md` §3.5 里有一个数字现在是错的。** 它写
`go list -deps ./cmd/beand` 返回 0 个 OpenTelemetry 包;实际返回 **12** 个。
这个决定的实质成立,而且比那个说法更精确:`beand` 链接了 `otel/trace`、
`otel/propagation`、`attribute`、`baggage`、`codes`、`semconv` ——
也就是 API 与上下文传播部分 —— 而链接了**零个** `otel/sdk` 或 OTLP exporter。
提取一个 `traceparent` 本身就需要 propagation API,所以 0 从来不可能达到;
达到的是「无 SDK、无 exporter」,而重量正是在那里。

一个只有真机暴露的 bug:`resource.Merge(resource.Default(), ...)`
在钉住的 semconv 版本与 SDK 不匹配时直接返回 error,进程起不来。
所有单测都过了,因为它们把 `Endpoint` 留空、在到那一行之前就返回了。
为它加的回归测试刻意设了 endpoint —— exporter 是惰性连接的,所以不需要真的 collector。

⚠️ 另一处值得标出来:五个 OTel 模块在 `go.mod` 里被标成 `// indirect`,
而 `internal/obs` 和 `internal/beand` 直接 import 它们。`go mod tidy` 会把它们
移进直接依赖块。纯属外观问题,但它让这个文件对「项目依赖什么」的表述是有误导的。

### 9.2 Prometheus 格式,不用客户端库 ✅

`internal/obs/metrics.go` 直接实现文本暴露格式 —— counter、gauge、histogram ——
让二进制保持无依赖,而同一个 registry 之后可以被 OTLP exporter 包起来。
抓取面包含 `bean_node_disk_{free,used}_bytes` 和快照缓存大小。

有一个工具的存在源于一个值得记录的测量陷阱:`hack/phase-delta.py`。
累积 histogram 的 `_sum/_count` 给的是生命周期平均值,没法归因单次运行 ——
26 个快的 create 会把 16 个慢的藏起来。对两次抓取做差分,得到的是仅该区间的平均值。

### 9.3 日志:`log/slog` ✅

标准库上的结构化日志,给人看用 text、给采集器用 json,trace id 作为 request 字段带上。
无法识别的 level 回落到 info 而不是拒绝启动,因为一个日志级别的拼写错误
不该把一个节点挡在集群外面。

---

## 10. 测试

测试套按「机器能做什么」切分,而这是 build tag 布局的刻意后果:

| 在哪跑 | 测什么 | 怎么跑 |
|---|---|---|
| 任何地方,含 macOS | 单测、`local` runtime 档、完整 agent gRPC 接口 | `make test` |
| Linux + root | device-mapper 组装与 CoW 回填 | `os.Geteuid() != 0` 时 skip |
| 真 KVM 机器 | Firecracker create/snapshot/restore、对活 VMM 的 UFFD | `-tags=fcintegration` |
| gateway 可达处 | 7 个端到端测试,跑在 local 档 | `-tags=e2e` |
| CI + MinIO | 对真 S3 服务器的 SigV4 | 环境变量门控,不满足则静默 skip |

S3 集成测试是唯一能证明手写 SigV4 产出的签名被服务端接受的检查 ——
单测只能证明规范化与自己自洽。它们由 `BEAN_S3_ENDPOINT` 门控,
所以 `go test ./...` 在没有基础设施时保持全绿。
⚠️ `tests/e2e` 里没有压测/负载测试;§1 的数字来自 `hack/stress-fc.sh`。

### 这个项目用代价换来的两条规则

**验证要穿透到真实持久层。** 当状态同时存在于内存和磁盘上时,
读内存的测试什么也证明不了。那个静默文件系统损坏的 bug 过了三层测试:
单测检查 tar 往返(是对的 —— 数据**确实**写进了文件,bug 在文件之下),
端到端测试在 guest 里读文件(命中 page cache),
`dmsetup status` 看的是作为快照**源**的那个设备。**没有一层读过恢复出的块设备。**
所以任何「恢复之后数据还在」形式的断言都必须先 `drop_caches`,
而 `TestDevMapperSeedIsVisibleThroughDevice` 直接挂 `/dev/mapper/...` 读回,
完全绕过 guest。

**然后把修复改回去,确认测试会失败。** 对那个 bug,所有文件级断言在坏实现下都是绿的,
所以这是唯一能知道新测试是否值钱的办法 —— 把 seed 移回 `dmsetup create` 之后,
它立刻失败。此后应用到了 loop device 泄漏、快照合并顺序、
快照缓存 pin(短路 pin 检查让两个测试立刻变红)、以及队列的「瞬时 vs 生命周期」区分上。

---

## 11. 启动优化账本

按贡献排序,全部在真机实测:

```
gRPC 重连退避              -800 ms   BaseDelay 1s → 20ms
关串口(quiet)             -493 ms   8250 UART 同步写
换 CI 内核                  -90 ms   6.1.175 → 6.1.102
健康轮询粒度                -40 ms   50ms → 10ms
─────────────────────────────
create   2200 ms → 952 ms

UFFD 按需供页             -1296 ms   /snapshot/load 1303ms → 7ms
解包缓存                   -550 ms   每快照解包一次而非每次恢复
─────────────────────────────
restore  1500 ms → 950 ms(首次 1617ms)
```

**最大的两项不在虚拟化层,在我们自己的代码里。** gRPC 退避加串口合计 1293 ms,
占冷启动优化的 96%。最初的假设是瓶颈在 guest 内核 boot;归因之后内核只占 90 ms。
这是本文里关于操作顺序最强的论据:先测量、再归因、然后选技术。

---

## 12. 什么建成了、什么没有

| 层 | 选择 | 状态 |
|---|---|---|
| VMM | Firecracker(上游,未 fork) | ✅ |
| jailer / 宿主 cgroup | — | 📐 |
| 容器档 | runc / gVisor | ✅ noded 直驱 OCI runtime,无 containerd |
| 开发/CI 档 | `local` 进程树,无隔离 | ✅ |
| Rootfs | device-mapper snapshot,共享 base + CoW | ✅ 每 sandbox 44 KiB |
| Rootfs 按需拉取 | overlaybd | ⚠️ 已接在 `--fc-overlaybd` 后面(走 TCMU);lazy pull 本身未对真 registry 测过 |
| 内存恢复 | Firecracker UFFD 后端 | ✅ load 7 ms |
| 增量快照 | Firecracker diff + 恢复时合并 | ✅ 298 KB,链深上限 8 |
| `--track-dirty-pages` | | ⚠️ 已实现,默认关,开销未测 |
| CPU 可移植性 | 自定义 `/cpu-config` 模板 + 调度器过滤 | ✅ 同 vendor+family 内 |
| Guest 内核 | Firecracker CI `vmlinux-6.1.102` + 入库 config | ✅ |
| Agent | 静态 Go,只读 ext4 作 root 设备 | ✅ |
| 控制通道 | vsock + gRPC | ✅ |
| Sandbox 网络 | veth + netns + nftables | 📐 地址池已建,管道未通 |
| 语言 | Go,标准库优先 | ✅ |
| 状态存储 | SQLite(`modernc.org/sqlite`,纯 Go) | ✅ |
| 状态存储 | Postgres | ✅ SQLite 或 Postgres 由 flag 决定;requirement 对真 Postgres 16 跑过 |
| 内部 RPC | gRPC + protobuf,生成器钉版本 | ✅ |
| 外部 API | `net/http` 上的 REST | ✅ |
| 对象存储 | S3 兼容,手写 SigV4、分片、range 读 | ✅ |
| S3 凭证 | presigned / STS 轮转 | 📐 目前是环境变量 |
| 镜像拉取 | 自己的 OCI distribution 客户端 | ✅ |
| 镜像构建 | 经 `buildctl` 的 BuildKit | ✅ 日志与取消 ⚠️ |
| Trace | OpenTelemetry + W3C traceparent | ✅ agent 只采纳 id,不发 span |
| 指标 | 手写 Prometheus 暴露格式 | ✅ |
| 日志 | `log/slog` | ✅ |

## 参考

- [decisions.md](decisions.md) —— 每个选择及其实测数据
- [status.md](status.md) —— 实际建成了什么
- [architecture.md](architecture.md) —— 组件及其关系
- [noded-design.md](noded-design.md)、[vm-assembly.md](vm-assembly.md)、
  [image-pipeline.md](image-pipeline.md)、[s3-storage.md](s3-storage.md)、
  [snapshot-resume.md](snapshot-resume.md)、
  [competitive-analysis.md](competitive-analysis.md)
- [firecracker: handling page faults on snapshot resume](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/handling-page-faults-on-snapshot-resume.md)
- [e2b-dev/fc-kernels](https://github.com/e2b-dev/fc-kernels)
- [tensorlake: Firecracker disk snapshots in O(changed bytes)](https://tensorlake.ai/blog/firecracker-disk-snapshots-o-changed-bytes)
- [Restoring Uniqueness in MicroVM Snapshots (AWS)](https://arxiv.org/pdf/2102.12892)

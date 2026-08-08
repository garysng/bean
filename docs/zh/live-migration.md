# 热迁移可行性 —— 技术报告

> 状态:📐 **调研 / 仅设计。** 本文尚无任何实现。它评估的是:把一个运行中的沙箱从
> 节点 A 热迁移到节点 B,在 bean 现有机制上是否可行、需要什么、硬限制在哪。权威顺序
> 依旧成立:代码 > `status.md` > `decisions.md` > 设计文档 > 本报告。

> English: [../live-migration.md](../live-migration.md)

---

## 0. 先把问题说准

这里的**热迁移(live migration)**指:把一个*运行中*的沙箱从宿主 A 移到宿主 B,使得
从 guest 和客户端的视角看,它一直在运行 —— 进程树存活、guest 内状态保留、停机时间短
到开着的连接和负载都察觉不到一次故障。

这比 bean 今天做的事更强。今天的跨机器路径是**快照 + 从快照创建**:在 A 上捕获一份持久
blob,在 B 上从它创建一个*新*沙箱。那是冷的 —— 产出的是另一个沙箱、新 id,且源被预期停止。
热迁移是同一套物理过程(把内存 + 磁盘 + 设备状态跨网络搬走),但多两个要求:

1. **连续性** —— 同一个沙箱身份、连接存活、客户端看不到重启。
2. **有界停机** —— stop-the-world 窗口是毫秒级,而不是整个传输时长。

本报告余下部分,衡量的就是"在另一台机器上 snapshot + restore"(已经能做)与这两个要求
之间的差距。

---

## 1. bean 已经具备的构件

热迁移不是单一功能,而是一层编排,而它所需的构件大多数 bean 为了 snapshot/restore/fork
已经 shipped。对着代码盘点:

| 构件 | 位置 | 状态 | 与热迁移的关系 |
|---|---|---|---|
| 一致性捕获 | `Checkpoint` — `fc_lifecycle_linux.go:155`(pause → `/snapshot/create` → 稀疏 tar bundle) | ✅ shipped | 任何迁移的 stop-and-copy 步 |
| 三档快照 | 全量 / `--no-memory` / `--base` 增量 — `fc_lifecycle_linux.go:178-207` | ✅ shipped | 增量是迭代 pre-copy 的原料 |
| 脏页跟踪 | `--track-dirty-pages`、`EnableDiffSnaps` — `fc_lifecycle_linux.go:510` | ⚠️ 默认关,开销未测 | pre-copy 收敛所需的信号 |
| UFFD 精确复活 | `uffd_linux.go:47-101`、`/snapshot/load ResumeVM` | ✅ shipped,`/snapshot/load` = 7 ms | 目标端的按需供页引擎 |
| 共享只读内存映像 | `MAP_SHARED` — `uffd_linux.go:93-97` | ✅ shipped | 多 VM 共用一份 page cache(fork) |
| 增量链合并 | `snapmerge_linux.go:37-139` | ✅ shipped | 把 base + diff 平铺成一份映像 |
| 按 id 的 bundle 缓存 | `snapcache_linux.go` | ✅ shipped | 首次解包 950 ms,之后 392 ms |
| CoW rootfs | dm-snapshot `devmapper_linux.go`;overlaybd/TCMU `obdtcmu_linux.go` | ✅ / ⚠️ | 本地共享 base + 每沙箱 CoW |
| 地址保持 | restore 故意留空 `NetworkOverrides` — `fc_lifecycle_linux.go:513-520` | ✅ confirmed | guest 带回原 IP/MAC |
| 每沙箱 netns | `network.md` §1、`runtime/netns_linux.go` | ✅ confirmed | 相同地址跨节点共存 |
| CPU 硬过滤 | `INCOMPATIBLE_CPU` — `cpucompat.go:33`,409 见 `snapshots.go:366` | ✅ shipped | 带内存 guest 的落点约束 |
| CPU template 预启动 | `cpu_template.go` | ✅ shipped | 跨机可移植性的杠杆 |
| S3 blob 传输 | multipart + range + SigV4 — `s3blobs.go`、`s3/` | ✅ shipped | 在机器间搬运捕获态 |

地址这一条值得单说:因为每个沙箱有自己的 netns、tap 在每个 netns 里都叫 `beantap0`,恢复出的
guest 快照已经指向新 netns 里正确的设备 —— restore 故意**不**覆盖网络配置
(`fc_lifecycle_linux.go:513-520`)。"恢复的 guest 保留原 IP/MAC"正是把同一 guest 在另一台
机器复活所需,而这里几乎免费。

## 2. 差距 —— 热迁移比 bean 多出的东西

代码里**没有任何 live-migration / pre-copy / post-copy 实现**(全库检索只命中 DB schema 迁移
和 CPU template 注释)。今天的跨机模型是:全量捕获 → **经 S3** → 目标机整链取回 → 本地 UFFD
复活。那就是 snapshot + create-from-snapshot,而且是*冷*的:新沙箱、新 id、源被预期停止。
具体缺失的是:

1. **没有迭代式 pre-copy 循环。** 增量作为*存储*概念存在(相对某个存储 base 拍差量),但没有
   任何东西在*运行中*的 guest 上反复读脏位图、多轮收敛。位图在 load 时被重置
   (`fc_lifecycle_linux.go:510`),每份 diff apply 后即删(`snapmerge_linux.go:139`)。
   pre-copy 需要"采样脏页、传走、重复,直到工作集足够小"。
2. **UFFD 只从本地文件供页。** `newUffdHandler(uds, memImagePath)` 打开本地路径并 mmap
   (`uffd_linux.go:79-101`)。**没有网络供页** —— 映像必须先完整落到目标机。post-copy(目标
   边跑边从源拉页)需要一条远端页通道。
3. **blob 走 S3 中转,而非源→目标。** 首字节延迟受对象存储限制,不是节点到节点的直连。
4. **停机不可控。** Checkpoint 全程 pause;没有"最后一轮小差量 + 毫秒级 stop-the-world 切换"。
5. **VMM 侧脏页未跟踪**(见 §4 —— 这是 bean 盘点没发现、而 Cloud Hypervisor 不得不建的构件)。

## 3. 业界实际怎么做

**Firecracker 官方不做 live migration,是取舍** —— 讨论
[#3119](https://github.com/firecracker-microvm/firecracker/discussions/3119):microVM
毫秒级启动与快照,所以官方把 snapshot/restore 定位为"覆盖 live migration 大多数用途的更简单
的东西",拒绝经典在线内存迁移。所有人的"迁移"都建在 snapshot + UFFD 之上,且几乎都带一次显式
pause —— 这个领域没人在 Firecracker 上做零停机 pre-copy。

- **E2B** —— pause 写一份完整快照(Firecracker snapfile + 内存 diff + rootfs diff,存为内容
  寻址块),resume 经 UFFD lazy loading 在另一台机复活。这几乎就是 bean 的模型。
- **fly.io** —— suspend/resume 把含内存的完整状态 dump 到持久存储;跨主机 machine migration
  复用该快照机制 + dm-clone/iSCSI 搬卷。明确*不是*经典 live migration —— 机器停掉再重建。
- **Morph** —— Infinibranch 对整个环境 snapshot/branch(宣称 <250 ms,但那是快照/分支,不是
  跨主机迁移停机)。
- **gVisor(runsc)** —— 自研 checkpoint/restore(不是 CRIU,因为 Sentry 本就持有状态)。但快照
  只能由*同一 runsc 二进制*恢复,且网络连接与 GPU 状态不保留 —— 用于快启/池化
  ([腾讯百万级 agentic-RL](https://gvisor.dev/blog/2026/04/23/scaling-agentic-rl-sandboxes-to-the-millions-with-gvisor-at-tencent/)、
  Modal 亚秒启动),不是跨主机 live migration。
- **runc + CRIU** —— 真容器 live migration 存在但脆弱:established TCP 需 `--tcp-established`,
  而把*已建立*连接重定向到新主机是已知未解的边缘
  ([CRIU #1598](https://github.com/checkpoint-restore/criu/issues/1598))。

**唯一的直接参照是 Cloud Hypervisor** —— 与 Firecracker 同为 rust-vmm 血统,且*确实*
shipped 了生产级 pre-copy live migration
([文档](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/live_migration.md)),
并在 [v53.0](https://www.cloudhypervisor.org/blog/cloud-hypervisor-v53.0-released/) 加入了基于
userfaultfd 的远端 post-copy。它证明在这套 VMM 架构上路走得通;Firecracker 不做是产品决策,
不是技术墙。

## 4. 机制,以及 Cloud Hypervisor 的那条教训

**Pre-copy** 让源保持运行:第 0 轮拷全部内存,之后每轮只重传上一轮弄脏的页,直到工作集小到可以
停下,拷完残余页加 vCPU/设备状态,切换。停机只是最后那次 stop-and-copy —— 几十到几百 ms。它只
在传输带宽 `B` 大于脏页率 `R` 时收敛;write-heavy 的 guest 若 `R ≳ B` 永不收敛,必须强制切换
(停机飙升)或转 post-copy。

**Post-copy** 先切换:暂停源,传走最小 CPU/设备状态,目标机立即启动,之后 guest 缺页时按需从源
拉页 —— 这正是 userfaultfd 的用途。停机与内存大小解耦,但现在一次缺页要跨网络,而且迁移中任一端
或链路挂掉,guest 就不可恢复(内存被劈成两半分在两台机)。生产系统(QEMU)的做法是
**pre-copy 打底、post-copy 兜底**。

**bean 自己的盘点发现不了、来自 Cloud Hypervisor 的教训
([#2458](https://github.com/cloud-hypervisor/cloud-hypervisor/issues/2458)):脏页有两个来源。**
KVM 的 dirty log 捕获的是 *guest/vCPU* 写的页。但 *VMM 自身*也会写进 guest RAM —— VIRTIO 设备
仿真、DMA —— 这些写对 KVM 不可见。只跟踪 KVM log 的迁移会传出一份微妙不完整的内存映像。bean 的
`--track-dirty-pages` 是 KVM-log 那一半;VMM 侧跟踪是另一件必须补建的东西。这类陷阱只在真正
shipped 过迁移的 VMM 里才现形 —— 这就是为什么 Cloud Hypervisor 是该读的参照。

## 5. 一条契合 bean 构件的路径

缺的不是构件,而是把它们连起来的编排层:一个脏页迭代循环 + 一条节点到节点的页通道。按"复用现有
构件的程度"排序:

**阶段 0 —— 跨节点冷搬移(基本已可做)。** snapshot → 传输 → 在 B 上 create-from-snapshot,
源停止。这就是今天的能力加一层"然后销毁源"的薄封装,并让源的 id 退役。停机 = 捕获 + 传输 + 复活
(秒级)。诚实地说:这是*重定位*,不是热迁移 —— 新 id、连接断开 —— 但它是零新机制的基线,且与
fly.io/E2B 已 ship 的行为一致。

**阶段 1 —— 节点到节点直传。** 把 S3 中转换成源→目标的 bundle 通道(gRPC 的
`RestoreSandboxFrame` 流式已为链存在,把它指向点对点)。降低首字节延迟。仍是冷的,但传输通道已经
是热路径会用的那条。

**阶段 2 —— post-copy 复活(杠杆最高的一步)。** 把 UFFD handler 从"从本地文件供页"扩展到"从
远端源节点供页":捕获最小状态,在 B 上启动 guest,让 `uffd_linux.go` 的缺页路径经阶段 1 的通道
从 A 拉缺失的页,而不是本地 mmap。bean 已经拥有目标端的缺页机制 —— 增量是一个远端页源加上源端的
服务端。这正是 Cloud Hypervisor v53.0 的做法,也是 bean 构件最契合的方向。**要在设计之初就防的
风险:**无人应答的缺页会让 Firecracker 永久挂起(`decisions.md:143`),所以迁移中途的网络故障
必须有显式的中止-恢复,而不是挂死。

**阶段 3 —— 迭代 pre-copy + 有界切换。** 建 bean 缺的那个循环:在运行中的 guest 上读脏位图,经
阶段 1 通道传增量,重复直到残余低于阈值或达到轮次上限,再来一次短 stop-and-copy。要求位图跨轮
存活(load 时不重置、每份 diff 不删)并 —— 按 §4 —— 补 VMM 侧脏页跟踪。与阶段 2 组成 write-heavy
guest 的不收敛兜底。

**同样值得早做:同机 live upgrade。** Cloud Hypervisor 的 `--local` 和 fly.io 的进程内移交都是
在一台宿主上把 guest 在 VMM 进程间移交。对 sandbox 平台,这让 noded 升级不必丢沙箱,直接复用快照
构件,且远比跨主机路径简单 —— 是验证机制的好起点。

## 6. 硬限制与风险

- **CPU 兼容是硬过滤,不是可调项。** 带内存的快照只能在同 vendor + family 上复活;vendor 与
  family 无法屏蔽(`decisions.md:382`),所以调度器以 409 拒绝(`cpucompat.go:33`)。迁移目标被
  限制在 CPU 兼容的池里,且 CPU template 必须在启动时选定 —— 无法对运行中的 guest 追补。跨*型号*
  restore 今天也未验证(`status.md:290`,只有一台 fc host),且 model 有意不落库。
- **无人应答的 UFFD 缺页会让 Firecracker 永久挂起**(`decisions.md:143`)。post-copy 让缺页源变成
  远端节点,所以网络分区的失败模式是 guest 挂死 —— 除非有显式的 liveness 监控与中止路径。这是
  阶段 2/3 最大的可靠性风险。
- **收敛没有保证。** write-heavy guest 可能弄脏得比链路传得快;pre-copy 此时需要轮次上限或脏集
  阈值强制切换,用停机换终止。post-copy 是兜底,但带上面那个内存劈两半的失败域。
- **`--track-dirty-pages` 开销未测**(`status.md:286`)且默认关;它必须在*启动前*开,所以可迁移的
  沙箱从一开始就要付那份开销。量化它是前提。
- **VMM 侧脏页跟踪不存在**(§4)—— 一份*完整*的内存映像需要它,而不只是 KVM 可见的页。
- **跨 L3 的 TCP 连续性通常无解。** guest 保留 IP/MAC(bean 的地址保持有帮助),同网段切换可发
  gratuitous ARP 重定向流量,但跨网段需要 overlay。恢复出的 guest 内部 ARP 缓存也要清
  (`network.md:166`,该条本身标注需真机验证)。
- **容器档(gVisor/runc)是单独一条线。** runsc 只能同二进制恢复且丢网络/GPU 状态;那里近期现实
  的目标是 snapshot-restore 式重定位(接受连接中断),不是连接存活的热迁移。

## 7. 结论

bean 缺的是编排,不是构件。一致性捕获、UFFD 复活、增量链、CoW、CPU 硬过滤、S3 传输,以及 ——
几乎免费的 —— 网络地址保持,全都 shipped,且都是为 snapshot/restore/fork 建的。热迁移多出来的
是一个脏页迭代循环和一条节点到节点的页通道,外加 VMM 侧脏页跟踪、远端缺页的 liveness/中止,以及
一个测量过的 `--track-dirty-pages`。

可行的顺序是:**跨节点冷重定位(阶段 0,几乎免费)→ 直传(阶段 1)→ 经 UFFD 的 post-copy
(阶段 2,最契合 bean 构件)→ 迭代 pre-copy 加有界切换(阶段 3)**,并以同机 live upgrade 作为
早期、低风险的验证场。Cloud Hypervisor —— 同 rust-vmm 血统、生产 pre-copy、v53.0 远端 post-copy
—— 是该研读的参照实现,它表明这条路可行而非受阻。Firecracker 上的零停机热迁移不是官方功能,也不
该期待它成为;一条建在 bean 现有机制上、有界停机、post-copy 优先的路径,才是现实的目标。

---

## 来源

**Firecracker** — [snapshot-support](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/snapshot-support.md) ·
[Discussion #3119(拒绝 live migration)](https://github.com/firecracker-microvm/firecracker/discussions/3119) ·
[缺页处理 / UFFD](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/handling-page-faults-on-snapshot-resume.md) ·
[CPU templates](https://github.com/firecracker-microvm/firecracker/blob/main/docs/cpu_templates/cpu-templates.md)

**Cloud Hypervisor** — [live migration 文档](https://github.com/cloud-hypervisor/cloud-hypervisor/blob/main/docs/live_migration.md) ·
[#2458 VMM 侧脏页跟踪](https://github.com/cloud-hypervisor/cloud-hypervisor/issues/2458) ·
[v53.0 远端 post-copy](https://www.cloudhypervisor.org/blog/cloud-hypervisor-v53.0-released/)

**Post-copy / userfaultfd** — [QEMU post-copy](https://www.qemu.org/docs/master/devel/migration/postcopy.html) ·
[userfaultfd(2)](https://man7.org/linux/man-pages/man2/userfaultfd.2.html)

**容器** — [gVisor checkpoint/restore](https://gvisor.dev/docs/user_guide/checkpoint_restore/) ·
[gVisor 腾讯规模化](https://gvisor.dev/blog/2026/04/23/scaling-agentic-rl-sandboxes-to-the-millions-with-gvisor-at-tencent/) ·
[runc + CRIU](https://github.com/opencontainers/runc/blob/main/docs/checkpoint-restore.md) ·
[CRIU #1598(established-TCP 重定向)](https://github.com/checkpoint-restore/criu/issues/1598)

**竞品** — [E2B persistence](https://e2b.dev/docs/sandbox/persistence) ·
[fly.io Making Machines Move](https://fly.io/blog/machine-migrations/) ·
[fly.io suspend/resume](https://fly.io/docs/reference/suspend-resume/) ·
[Morph Infinibranch](https://morph.so/blog/infinibranch)

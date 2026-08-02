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

## 3. rootfs:dm-snapshot 而非 overlaybd/TCMU

**已实测**:每 sandbox 磁盘成本 8 KiB(共享只读 base + 每 sandbox CoW)。

TCMU 需要每 sandbox 一套 SCSI fabric(loopback nexus),脆弱且慢;
dm-snapshot 只要 `dm_snapshot` 模块。

**overlaybd 的真正价值**在「首次拉取按需读块」,不在「每 sandbox 成本」——
后者 CoW 已经解决。所以 overlaybd 该做,但理由是**首次使用大镜像的等待时间**,
不是磁盘占用。

agentenv 的 `uffd-core/src/overlaybd.rs` 表明这两件事可以合并:
UFFD 缺页直接从 overlaybd 镜像读。这是比我们现在更远的一步。

## 4. 未决 / 待验证

- **overlaybd lazy-pull**:组件已装(`/opt/overlaybd/bin`,tcmu 模块已加载),
  但**功能未验证**。ublk 后端需要 ≥6.0 内核,tcmu 在 5.15 可用 ——
  功能验证不需要升内核。
- **升级宿主内核到 6.8**:20.04 的 apt 里没有 6.x(HWE 终点是 5.15)。
  需要 mainline PPA 或升级发行版。收益是 ublk(overlaybd 的更快后端)。
  这台机器是 VM(`/dev/vda2`),换内核后 nested KVM 能否保持可用未验证。
- **destroy 耗时 5.2s**:比 create 慢 5 倍,独立问题,未归因。
- **内核对比**:CI prebuilt 6.1.102 vs 现有 6.1.175,未测。

## 参考

- [firecracker: handling page faults on snapshot resume](https://github.com/firecracker-microvm/firecracker/blob/main/docs/snapshotting/handling-page-faults-on-snapshot-resume.md)
- [firecracker: guest_configs](https://github.com/firecracker-microvm/firecracker/tree/main/resources/guest_configs)
- [e2b-dev/fc-kernels](https://github.com/e2b-dev/fc-kernels)
- [tensorlake: Firecracker disk snapshots in O(changed bytes)](https://tensorlake.ai/blog/firecracker-disk-snapshots-o-changed-bytes)
- [Restoring Uniqueness in MicroVM Snapshots (AWS)](https://arxiv.org/pdf/2102.12892)

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

**选择**:先用 `firecracker-ci/v1.11/x86_64/vmlinux-6.1.102`,同时把它的 `.config`
入库当资产(CI 把 config 单独发布,所以「用 prebuilt」和「拿到自己的 config」不是二选一)。

理由:容器编译要先付出成本(工具链 + 拉源码 + 编 20min)才拿到第一个数据点,
而我们连「换内核有没有用」都还没测。先测,收益够再建编译流程 —— config 已在手上。

**现状问题**:当前用的 `vmlinux-6.1.175` 来自 agentenv 的 R2 站,**config 未知**,
这是没法解释启动耗时构成的原因之一。内核日志里能看到 iSCSI transport、bpfilter
这些 microVM 里用不到的探测,还有 `IO read @ 0x87 failed: MissingAddressRange`
—— 通用内核在 microVM 里做无用探测的直接证据。

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

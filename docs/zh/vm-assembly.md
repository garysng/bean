# microVM 组装:从块设备到可连 agent

> 状态标注约定见 [architecture.md](architecture.md) §0。
> 实现:`internal/node/runtime/fc_linux.go`(组装)、`internal/node/image/devmapper_linux.go`(块设备)。

一次 create 是 **952ms**(镜像已缓存):`runtime_create` 234ms + `agent_ready` 770ms。
本文讲这 234ms 里发生了什么,以及**哪些步骤的顺序是不能动的**。

后者是本文存在的主要理由 —— 有两处顺序约束,改错的后果都是**静默的错误行为**
而不是报错。

## 1. 全序 ✅

```
① image.Prepare        组出 /dev/mapper/bean-<id>(共享 base + 每 sandbox CoW)
                       restore 时:CoW 必须在此步之内回填 ← 顺序约束 A
② os.Symlink           把 agent 盘链进 sandbox 目录
③ exec firecracker     cwd = sandbox 目录 ← 相对路径的前提
④ waitAPIReady         轮询 API socket 出现
⑤ PUT /machine-config  vCPU / 内存 / track_dirty_pages
⑥ PUT /cpu-config      CPU 特征掩码 ← 顺序约束 B(必须在 ⑨ 之前)
⑦ PUT /boot-source     内核 + cmdline
⑧ PUT /drives/agent    agent 盘,root device
   PUT /drives/rootfs   用户镜像,第二盘
   PUT /vsock           CID 3,UDS 相对路径
⑨ PUT /actions         InstanceStart
```

restore 走的是同一条 ①–④,然后换成单个 `PUT /snapshot/load`(带 `ResumeVM`)——
因为快照里已经含了整机配置。**这也是为什么 ⑤–⑧ 一个都不能在 load 之前做**:
Firecracker 会拒绝一个已配置了 boot 资源的实例去 load 快照。

## 2. 顺序约束 A:CoW 必须在设备组装之前回填 ✅

dm-snapshot 在 `dmsetup create` 那一刻把 exception table 读进内核内存,**之后不再回读**。

所以往一个**已激活**设备的 CoW 后端写字节,内核不认这些 chunk,设备继续供 base image。
`image.PrepareOptions.SeedWritable` 存在就是为了在正确的时刻插入:

```go
createSparse(cowPath, sizeMiB)
opts.SeedWritable(cowPath)      // ← 回填在这里
attachLoop(cowPath, false)
dmsetup create ...              // ← exception table 在这一刻定型
```

**改错的后果**:full snapshot 上完全静默。恢复后立即读命中的是内存快照带回的
page cache,`drop_caches` 之后同一个文件读出全零,而 `ls` 仍显示正确 size、
无 EIO、无 dmesg。元数据活在内存镜像里、数据活在块设备上,两边不一致而 ext4
没有理由怀疑。完整归因见 [decisions §3.0](decisions.md)。

## 3. 顺序约束 B:CPU 掩码必须在 InstanceStart 之前 ✅

guest 在早期 boot **读一次 CPUID 就缓存下来** —— glibc 据此选 string routine
(`memcpy` 走 AVX2 还是 SSE2)。之后再改 CPUID 视图,guest 已经在用它以为存在的指令了。

```go
// Masking has to happen before InstanceStart. A guest reads CPUID once
// during early boot and caches what it found ...
if cfg := cpuConfigFor(r.CPUTemplate); cfg != nil {
    vm.client.put(ctx, "/cpu-config", cfg)
}
```

`track_dirty_pages` 是同一类约束的另一个例子,但机制不同:它要 KVM 从 guest
启动的第一条指令起就在记账,而且**不存进快照**。所以:

- 它只能是**节点配置**(`--track-dirty-pages`),不能是 per-snapshot 参数
- 没开的 guest 请求 diff 必须**明确报错**而不是降级成 full
- restore 时要重新传 `EnableDiffSnaps`,否则从快照恢复的 sandbox 再快照只能出 full ——
  而那恰好是最需要增量的场景

## 4. 为什么 agent 盘是 root device ✅

```
/drives/agent   agent.ext4      IsRootDevice: true,  ReadOnly: true   → /dev/vda
/drives/rootfs  <cow device>    IsRootDevice: false                   → /dev/vdb
```

内核从它挂成 root 的那个设备上 exec init。把 agent 放在那里,**用户镜像就不承担
任何义务** —— 不用内嵌 `beand`、不用有 init 系统、不用改 entrypoint。
agent 起来之后自己 pivot 到 `/dev/vdb`:

```
init=/bean/beand -- --listen vsock:1024 --pivot /dev/vdb
```

**顺序决定命名**:Firecracker 按注册顺序给 `vda`/`vdb`,而 `--pivot /dev/vdb`
是硬编码的,所以 agent 盘必须先注册。注册顺序反了的表现是 guest 挂载失败,
在没有串口的默认配置下**没有任何输出** —— 这是 `--debug-console` 存在的理由。

agent 盘用 symlink 链进 sandbox 目录而不是拷贝:一个 inode,零拷贝,
而且让它的 drive path 能是相对的(见 §5)。

## 5. 所有路径都是相对的 ✅

drive path、vsock UDS 全部相对于 VMM 的工作目录,而工作目录设成 sandbox 自己的目录:

```go
cmd.Dir = vm.dir
```

**理由是快照可移植性。** Firecracker 把设备路径与 vsock UDS 路径**存进 machine state**,
并在 load 时重新解析 —— 而且拒绝在 load 时覆盖 vsock 路径。

所以:
- 绝对路径 → 恢复出的 VM 会去找**源 sandbox** 的文件(源可能已经销毁)
- 相对路径 → 在哪个 sandbox 目录里启动 VMM,就解析到哪个 sandbox 的文件

这是「快照能在另一个 sandbox、另一台机器上恢复」的基础,
而它完全由「cwd + 相对路径」这一个决定实现,没有额外的路径重写逻辑。

## 6. cmdline 的每一项 ✅

```
quiet reboot=k panic=-1 pci=off init=/bean/beand -- --listen vsock:1024 --pivot /dev/vdb
```

| 参数 | 作用 | 依据 |
|---|---|---|
| `quiet` | 不挂串口 | **实测省 493ms**(1193ms → 700ms)。8250 UART 写入是同步的,内核每打一行都等硬件 |
| `reboot=k` | 用 keyboard reset | FC 无 ACPI,这是最小可用的 reset 方式 |
| `panic=-1` | panic 不重启 | 崩掉的 guest 保持可检查,不进重启循环 |
| `pci=off` | 跳过 PCI 枚举 | FC 没有 PCI 总线,枚举纯属浪费 |
| `init=/bean/beand` | agent 作 PID 1 | 见 §4 |
| `--` 之后 | 传给 beand 的参数 | 内核把 `--` 后的部分原样交给 init |

**`quiet` 与可调试性的取舍**:内核仍然编进了 8250 驱动 ——
`--debug-console` 只是把 `console=ttyS0` 加回去。失败的 boot 没有别的证据来源,
所以这个能力不能丢,但不该每次 boot 都付 493ms。这一点学的是 e2b
(它的 `fc-kernels` config 里 `CONFIG_SERIAL_8250=y` 是开着的)。

## 7. agent 的地址为什么可以是常量 ✅

```go
const agentVsockPort = 1024   // 无网络的 sandbox
const guestCID = 3
const AgentGuestPort = 10001  // 有网络的 sandbox
```

三个都不需要分配:**每个 VM 有自己的 vsock 命名空间和自己的网络命名空间**,
所以没有可冲突的对象。CID 3 是 guest 可用的最小值(0–2 被协议保留)。

**用哪一个取决于 sandbox 有没有网络**,而这是安全边界而非偏好:

| | 无网络 | 有网络 |
|---|---|---|
| agent 监听在 | `vsock:1024` | `tcp:0.0.0.0:10001` |
| sandbox 内部能否访问 | **不能** —— 该地址族是 host-to-guest | **能** |
| 靠什么把 sandbox 挡在外面 | 内核 | 每 sandbox 一个 token(A7) |

能用 vsock 的地方就保留 vsock,因为结构性保证胜过凭证。有网络的地方用 TCP,
因为那让 agent 成为 *guest 上的一个端口* —— 这才使得一套寻址方案同时覆盖它和用户暴露的
任意端口,见 api-design.md §6。因此 `10001` 是保留端口:用户暴露它就等于暴露 agent。

常量的好处是 guest 的 cmdline 不依赖宿主状态 —— 这让 cmdline 在快照前后完全一致,
少一个恢复时要对齐的东西。

## 8. dm-snapshot 表 ✅

```
0 <base_sectors> snapshot <base_loop> <cow_loop> P 8
```

- **`P`** = persistent。exception 存进 CoW 的元数据区,所以设备可以拆掉再重组 ——
  这是快照能捕获 CoW 层并在别处重放的前提。`N`(non-persistent)只活在内存里
- **`8`** = chunk size,单位 sector,即 4 KiB。选它是因为够小:
  一次单块写只 copy 4 KiB 而不是几十 KiB。代价是 exception table 条目更多,
  但实测每 sandbox 只占 44 KiB,没到需要权衡的量级

**base 是共享的**:一个只读 loop device 服务节点上所有用该镜像的 sandbox。
这是「每 sandbox 44 KiB」的来源 —— 对比 `FileProvider` 的每 sandbox 全量拷贝。

引用计数活在进程内存里,所以重启后要**接管**已有的 loop device 而不是新建
(否则每次重启泄漏一个,已修,见 GitHub #16)。

## 9. cleanup:注册顺序、反序执行 ✅

```go
var cleanup []func()
defer func() {
    if err == nil { return }
    for i := len(cleanup) - 1; i >= 0; i-- { cleanup[i]() }
}()
```

每一步成功后立刻把自己的 undo 压栈,失败时反序执行。**反序是必需的** ——
dm 映射持有 loop device,loop device 持有文件,所以必须先拆映射再 detach 再删文件。
顺序对了,失败的 create 不留 VMM 进程、不留设备、不留文件。

这条为什么重要:**泄漏的 microVM 占着调度器认为空闲的内存**。
一个孤儿 FC 进程不会自己消失,而调度器只看承诺量账本,
所以泄漏表现为「节点看起来有容量但实际没有」。

## 10. waitAPIReady 为什么必要 ✅

Firecracker 启动到创建 API socket 之间有个窗口,这期间发请求得到的是
「connection refused」——**一个和「配置错误」难以区分的错误**。

所以轮询 socket 出现(5ms 间隔,5s 上限)而不是直接发第一个请求。
5ms 是因为这个等待通常是几十毫秒的量级,固定 sleep 要么浪费要么不稳。

## 11. 还没有的东西 📐

组装链路里**没有**:

- ~~**网卡**~~。已做:网卡在 `InstanceStart` 之前注册,VMM 跑在该 sandbox 的命名空间里,
  MMDS 绑在那张网卡上。见 network.md
- **balloon**。内存回收靠不了它,所以内存超卖缺一个手段(见 noded-design §3.2)
- ~~**cgroup**~~。已做:`--fc-cgroups`,仅 v2,每 sandbox 的内存上限、CPU 配额、pid 上限
- ~~**降权**~~。已做:`--fc-vmm-uid`,VMM 不再是 root
- **chroot 和设备白名单**。仍然没有,这是 GitHub #20 剩下的那一半

**一个 FC/KVM 漏洞的后果不再是宿主 root。** 它是一个非特权 uid,在自己的 pid 命名空间里,
在该 sandbox 的网络命名空间里(去 RFC1918 和元数据地址段的出向流量被丢弃),
在一个限制其内存和 pid 的 cgroup 之下。这份清单里缺的是它自己的文件系统视图 ——
私有 mount 命名空间现在默认开(见 §12)。

补上这块的通常做法叫 jailer,而「直接加 jailer」并不成立,值得说清原因:jailer 的
`pivot_root` 要求把设备节点 **mknod** 进每 sandbox 的 jail,因为设备节点无法用符号链接进
chroot —— 而 bean 的 rootfs 正是一个 device-mapper 节点。e2b 不做这些也拿到了命名空间那一半:
`unshare` 一个 mount 命名空间,再用 tmpfs 加符号链接,这在 chroot 里行不通而在命名空间里可行。
bean 已经有 e2b 那样拿到的命名空间隔离,只是用 clone flags 而非包装进程实现(见 §12)。

## 12. 不用包装进程做隔离 ✅

VMM 是用 clone flags 起的,而不是套在 `unshare` 下面。差别在于 **noded 记下的是哪个 pid**,
不在于存在哪些命名空间。

e2b 的等价物是一条三层深的命令(`packages/orchestrator/internal/sandbox/fc/process.go`):

```
unshare -pfm --kill-child -- bash -c "mount --make-rprivate / && ... && ip netns exec <ns> firecracker"
```

那样能用,`--kill-child` 也覆盖了父进程死亡。但 `cmd.Process.Pid` 指的是 `unshare`,
于是给它发信号能不能到达 Firecracker,取决于每一层是原地 exec 还是 fork。
这种安排的失败模式很具体:**destroy 报成功而 microVM 还在跑**,
占着调度器已经许给别人的内存。

bean 改成在 fork 期间向内核索取同样的命名空间:

| | e2b | bean |
|---|---|---|
| pid 命名空间 | `unshare -p` | `Cloneflags: CLONE_NEWPID` |
| mount 命名空间 | `unshare -m` + `mount --make-rprivate /` | `Cloneflags` + `Unshareflags: CLONE_NEWNS` |
| 父进程死亡 | `--kill-child` | `Pdeathsig: SIGKILL` |
| 网络命名空间 | `ip netns exec` | 在钉住的线程上 `setns`(见 netns_linux.go) |
| noded 与 VMM 之间的进程数 | 2 | **0** |

**网络命名空间为什么是例外**:`CLONE_NEWNET` 创建的是一个*空*命名空间,
而这个 sandbox 的命名空间已经存在、里面已经有它的 tap。加入一个已存在的命名空间要用
`setns`,而 `setns` 是按线程生效的 —— 所以先钉住线程、加入命名空间、再在同一个线程上 fork。
clone flags 于是在那次 fork 期间生效,这就是两者能同时成立的原因。
在运行中的 VMM 上按 inode 实测过:它的 pid 命名空间与宿主不同,
同时它的网络命名空间是该 sandbox 的。

**`Pdeathsig` 为什么是 SIGKILL** 而不是 SIGTERM:在 pid 命名空间里 VMM 是 pid 1,
而 pid 1 会忽略自己没装处理器的信号。可捕获的信号恰好会在 sandbox 最需要死掉的时候被丢弃。

三个开关**默认都开**:`--fc-pid-namespace`、`--fc-kill-on-exit`、`--fc-mount-namespace`。
被压着最久的是 mount 命名空间,当时预期 bean 的 device-mapper rootfs 在里面会打不开;
那个预期是错的,起来的 guest 同时拥有可用的 `eth0` 和自己的 mnt / pid / net 命名空间。
它之所以必须用 guest 而不是靠检查来验证:VMM 解析不到 rootfs 时什么都不报,
只表现为一次没走完的启动。

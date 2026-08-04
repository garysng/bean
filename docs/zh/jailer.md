# jailer:代价是什么、会破坏什么、以及先做哪一步

> English: [../jailer.md](../jailer.md)
> 状态标注约定见 [architecture.md](architecture.md) §0。
> 实现:暂无。`grep -rn jailer --include='*.go'` 只有 1 处命中,且是注释。
> 相关:`internal/node/runtime/fc_linux.go`(startVMM、drive/vsock 注册)、
> `internal/node/runtime/uffd_linux.go`(缺页处理器)、
> `internal/node/network/setup_linux.go`(netns 创建)、GitHub #20。

这份文档存在的目的是在动手写代码之前先回答一个问题:**能不能在不破坏快照可移植性的前提下
采用 jailer?** 答案是能,但理由不是当前代码注释给出的那个,而且必须改掉三处今天承重的东西。

最重要的更正:写进 `startVMM` 和 [vm-assembly.md](vm-assembly.md) §5 的那个前提 ——
即"相对路径是快照在沙箱之间移动的**唯一**办法,因为 Firecracker 在 load 时拒绝覆盖 vsock
路径" —— **在上游已经不成立了**;而且另一件事是,**那套相对路径方案今天做的事情本来就不是
注释声称的那样**。下面两点都由源码与 API spec 建立,不是推断。

## 0. 状态汇总

| 项 | 状态 | 说明 |
|---|---|---|
| jailer 接进 noded | 📐 | 无代码。属第二阶段,且仍被 §8 第 4 项卡着 |
| VMM 外面的宿主 cgroup | ⚠️ | **第一阶段已交付**,不给 `--fc-cgroups` 则关闭。`internal/node/runtime/cgroup.go`。**只支持 cgroup v2 且强制要求**:v1 节点会拒绝启动而不是无限制地跑,因为 v1 无法限制 swap(见 §7) |
| 降权 | ⚠️ | **第一阶段已交付**,不给 `--fc-vmm-uid` 则关闭。`internal/node/runtime/vmmcreds.go`。只降了 uid —— mount namespace 与设备白名单仍属第二阶段,所以这个进程是非特权的,但对宿主文件系统有完整视野 |
| rlimit(`nofile`、`nproc`) | ⚠️ | 第一阶段已交付,随降权一起施加 |
| 设备白名单 | 📐 | 无代码,且没有 mount namespace 就做不到(§7 第 2 项) |
| 相对路径的快照可移植性 | ⚠️ | 能用,但三条"相对"路径里有两条是指向沙箱目录之外绝对目标的符号链接(§3)。这一点今天不可见,在 chroot 下是致命的 |
| load 时的 `vsock_override` | ✅ 上游(FC 1.16.0),📐 本项目 | 它移除了当前设计所围绕的那个约束 |

## 1. jailer 实际做了什么 📐

通过阅读 `firecracker-microvm/firecracker@main` 的 `src/jailer/src/env.rs` 与
`src/jailer/src/chroot.rs` 确认。这是源码,不是对文档的转述。

**chroot 布局。** `<chroot-base-dir>/<exec-file-name>/<id>/root`,默认 base 是 `/srv/jailer`
(`env.rs:181-190`)。对 bean 来说会是 `/srv/jailer/firecracker/<sandbox-id>/root`。

**二进制是被复制的,不是链接或 bind-mount。** `copy_exec_to_chroot`(`env.rs:490`)执行的是
真正的 `fs::copy`,复制进 `<chroot_dir>/firecracker`。上游给出的理由是内存隔离:复制意味着
新进程不与任何其他 Firecracker 共享 page-cache 支撑的代码段。代价是每次创建沙箱一次二进制
复制 —— 每次 create 几 MiB 的写,而 `runtime_create` 的预算是 234ms。**本项目未实测。**
采用之前必须测,因为它直接落在 create 路径上。

**顺序。** `run()`(`env.rs:647`)做的是:复制 exec → `join_netns` → `setrlimit` → 设置
cgroup → 若守护化则打开 `/dev/null` → `chroot()` → 以 0700 创建 `/`、`/dev`、`/dev/net`、
`/run` → `mknod` `/dev/kvm`(10:232)、`/dev/net/tun`(10:200)、`/dev/urandom`(1:9),
以及宿主上若存在则加 `/dev/userfaultfd`(次设备号通过解析 `/proc/misc` 发现,`env.rs:427`)
→ 降到 `--uid`/`--gid` → exec `/firecracker`。

这个顺序有两个后果要紧。**cgroup 与 netns 是在 chroot 之前加入的**,因为 chroot 之后就做不了
了。以及**降权是最后一步**,所以 jail 需要的一切都必须事先 chown 给目标 uid。

**工作目录会变成 `/`。** 这是关键,而且在 `chroot.rs` 里是显式的:
`unshare(CLONE_NEWNS)` → `mount(NULL, "/", MS_SLAVE|MS_REC)` → 把 chroot 目录 bind-mount 到
它自己上 → `set_current_dir(path)` → `mkdir old_root` → `pivot_root(".", "old_root")` →
**`chdir("/")`** → `umount2("old_root", MNT_DETACH)` → `rmdir`。那个 `chdir` 上的注释写的是
"pivot_root 不保证此刻我们就在 `/`,所以显式切过去"。

所以 `cmd.Dir = vm.dir` 在 jailer 下没有对应物。**cwd 不可选。** 它永远是 jail root。

## 2. 相对路径这个性质还成立吗?✅(已验证)

成立 —— 而这是好消息,理由却很容易搞反。

相对路径方案依赖的是 cwd 是**那个沙箱自己的目录**,不管它在哪。jailer 下每个沙箱有自己的
jail root(`.../<sandbox-id>/root`),cwd 就是那个 root。所以"相对于 cwd 的路径落在这个沙箱
自己的空间里"这个性质**恰恰是 jailer 保证的东西**,而不是它破坏的东西。

换句话说:相对路径能活下来,不是因为 jailer 碰巧没动 cwd,而是因为 jailer 给每个沙箱一个
自己的根,而相对路径本来就是相对于"这个沙箱的根"在解析的。

会坏的是别的东西 —— 见 §3。

## 3. 真正的阻碍:三条不像它们看起来那样的路径 ⚠️

chroot 破坏 bean 不是通过 cwd,而是通过**可达性**。在 chroot 下,一个路径要么在 jail 内解析
出来,要么根本解析不出来。今天有三样东西逃出了沙箱目录:

**(a) 两个 drive 都是指向绝对目标的符号链接。** `PathOnHost` 是相对的
(`fc_linux.go:477`、`484`),但这些名字指向的文件并不在本地:

- `agent.ext4` 是 `os.Symlink(r.AgentDiskPath, ...)`(`fc_linux.go:309`)→
  `/var/lib/bean/assets/agent.ext4`
- `rootfs.img` 是 `os.Symlink("/dev/mapper/bean-<id>", ...)`
  (`image/devmapper_linux.go:157-162`)

一个相对的**名字**,其**目标**是绝对的:没有 chroot 时解析正常,有 chroot 时就是断链。
dm 设备比 agent 磁盘更麻烦:设备节点**根本不能靠符号链接进 jail**,必须用正确的
major:minor 在里面 `mknod`,或者 bind-mount 进去。jailer 创建
`/dev/kvm`、`/dev/net/tun`、`/dev/urandom` 和 `/dev/userfaultfd`,**别的什么都不建** ——
`FOLDER_HIERARCHY` 恰好就是 `["/", "/dev", "/dev/net", "/run"]`(`env.rs:65`)。上游明确表示
guest 资源是运维方的事:用户"必须为任何将通过 API 提供给 VM 的资源创建硬链接(或复制)"。
**每沙箱的 dm 设备节点得由 bean 自己放进去,而这件事没有任何代码。**

**(b) 内核路径是绝对的。** `KernelImagePath: r.KernelPath`(`fc_linux.go:461`)→
`/var/lib/bean/assets/vmlinux`。在 jail 里不可达。需要硬链接或 bind mount 进去。

**(c) `SnapshotPath` 是绝对的。** `fc_lifecycle_linux.go:485` 传的是 `entry.StatePath`,
它是共享 `.snapshots` 缓存下的 `filepath.Join(dir, snapshotStateFile)`
(`snapcache_linux.go:194`)—— 它被**故意**放在沙箱目录**旁边**,好让缓存条目比沙箱活得更久
(`fc_linux.go:156-158`)。内存镜像更麻烦:它根本不是以路径形式传进去的,而是由 noded
`mmap` 并通过 UFFD socket 提供缺页,而**这种共享正是要点** —— 一份 page-cache 副本服务某个
快照的所有 restore(`uffd_linux.go:95-99`),这才是 fork 便宜的原因。天真地为每个 jail 复制
一份内存镜像,会摧毁 `internal/control/api/fork.go` 的经济性。

所以诚实的结论是:**快照可移植性能活下来;资产可达性活不下来。** `startVMM` 里那条注释指错了
风险。

## 4. UFFD:socket 能活,共享要小心 ⚠️

UFFD socket 是 pathname AF_UNIX,而 pathname AF_UNIX 是按**文件系统**解析的,不是按网络
命名空间(这一点用 socat 验证过,见 [network.md](network.md))。所以只要 socket 文件出现在
jail root 内、且属主是被降到的 uid,VMM 就能连上它。

需要小心的是共享:内存镜像是被 noded `mmap` 的,由 noded 通过 UFFD socket 提供缺页,而
Firecracker 自己**从不打开**那个内存文件。所以内存镜像不需要进 jail;只有 socket 需要。
而机器状态文件是 Firecracker 按绝对路径打开的,所以它需要在 jail 内可达 —— 这就是
`ensureTraversable`(`vmmcreds.go`)存在的原因,而在 chroot 下它需要的是另一套办法。

## 5. jailer 与"每沙箱一个 netns"是可组合的 ✅(已验证)

干净地可组合,而且这是唯一一处不需要重新设计的地方。

`Env::join_netns`(`env.rs:651`)打开 `--netns` 给出的路径,调用 `setns(fd, CLONE_NEWNET)`,
然后关闭它。**jailer 加入一个已存在的命名空间;它从不创建。** bean 已经自己创建命名空间了
(`ip netns add bean-<n>`,`setup_linux.go:100-126`),而 `ip netns add` 会在
`/var/run/netns/bean-<n>` 放一个 handle,正是 `--netns` 想要的东西。两边都不用让步。

> **本节的历史注记。** 这份文档最初写道:运行时今天**从不进入 netns**,所以采用 jailer 才是
> 真正把 VMM 挂到它命名空间上的东西,于是 #20 与 #21 是互补而非竞争关系。那个空缺后来由
> #45(`internal/node/runtime/netns_linux.go`)独立补上了 —— 用 `LockOSThread` 加 `setns`
> 加 `Start`,因为 `setns` 是按线程生效的。所以"互补"这个结论仍然成立,但 jailer 不再是获得
> 这个性质的**唯一**途径。

`network.md` §4 担心的"命名空间组织方式必须改变"没有发生:tap 命名不受影响,`beantap0` 在新
命名空间里仍然是对的,`network_overrides` 仍然是一个没被用到的逃生口。注意 netns 的加入发生在
chroot **之前**,所以 tap 设备是在已加入的命名空间里查找的,而 `/dev/net/tun` 是之后
`mknod` 的 —— 这就是 jailer 要创建它的原因,上游也这么说:jailed 运行时"要使用多个 TAP 接口"
就需要它。

## 6. `vsock_override` 让既有前提失效 ✅(已验证)

FC **1.16.0** 给 `SnapshotLoadParams` 加了 `vsock_override`(CHANGELOG,PR #5323;存在于
`src/firecracker/swagger/firecracker.yaml:1716`,是一个带 `uds_path` 键的对象)。API 描述:
"在快照恢复时覆盖 vsock 设备的 UDS 路径。这对于用与创建快照时不同的 socket 路径来恢复快照
很有用。"

`docs/vsock.md` 把动机限定在恰好是 bean 的处境上:"在某些**不**使用 jailer 的环境中,恢复带
vsock 设备的快照可能很困难",因为同一个 UDS 路径"无法被复用"。有一个值得记下的注意点:这个
覆盖是**前缀** —— "恢复后的 VM 上所有连接都将以 `./v.sock.2` 作为前缀打开"。

所以 `startVMM` 注释里那句"load 时拒绝覆盖它,所以相对路径是唯一办法"是**依赖版本的,而且
现在是错的**。由此有两条更正:

1. 这个论断在代码注释里必须**标注日期**,不能绝对化地陈述。
2. **钉住的版本号是未知的,这是一个文档缺口。** `hack/build-assets.sh:119` 钉的是**内核**,
   指向 `firecracker-ci/v1.11` 存储桶,而仓库里没有任何东西下载或钉住 Firecracker 二进制本身
   —— `dev-fc-stack.sh:96` 只是指向 `$ASSETS/firecracker` 并假定它在那儿。如果部署的二进制
   低于 1.16.0,`vsock_override` 就不存在。**要在 KVM 宿主上跑:
   `/var/lib/bean/assets/firecracker --version`。** 这一点无法从 darwin 的 checkout 上确定。

## 7. 另一条路:cgroup + 凭据 + 设备白名单,不用 jailer 📐

Go 能直接做到其中一个真实的子集,而且值得精确说明是**哪个**子集,因为
[architecture.md](architecture.md) §0.1 禁止在没有逐项清单的情况下说"X 覆盖了 Y 所以 Z 可以等"。

用 `SysProcAttr` 加上由 noded 写 cgroup 文件可以做到:

| 控制项 | 机制 | 说明 |
|---|---|---|
| cpu/memory/pids 限制 | 在统一层级下的每沙箱组里写 `cpu.max`、`memory.max`、`memory.swap.max=0` 和 `pids.max`,然后把子进程 pid 放进 `cgroup.procs` | 这正是 `overcommit.go:30` 被卡住的那部分。**不改路径,无快照风险。** 已交付,**仅 v2**。这一行曾经在开发机还是 v1 的时候就写着 v2 的文件名,所以两套层级曾被同时支持过;那套支持已被移除,**v2 现在是节点的硬性要求**,理由只有一条:**v1 无法限制 swap。** `memory.memsw.limit_in_bytes` 需要启动时带 `swapaccount=1`(默认关闭),所以 v1 的上限只约束 RAM,一个撞到上限的 VMM 会被推进 swap 而不是被停下 —— 宿主开始抖动,而限制报告为"已生效",这恰恰是超售内存所依赖这条限制去避免的那个失败。因此 v1 宿主在**启动时被拒绝**,而不是静默降级成没有限制。下限:Ubuntu 22.04+、Debian 11+、RHEL 9+(systemd 从 v243 起默认统一层级;Ubuntu 20.04 是 v1)。注意父组上必须写 `cgroup.subtree_control`,否则子组根本没有控制器文件,限制会静默缺失 |
| 降权 | `SysProcAttr.Credential{Uid, Gid}` | 需要把沙箱目录、dm 设备节点和 `/dev/kvm` chown 给那个 uid |
| 禁止提权 | `prctl(PR_SET_NO_NEW_PRIVS)` —— 需要一个 `fork/exec` hook 或一个极小的 re-exec shim | ~~Go 的 `SysProcAttr` 在 Linux 上有 `NoNewPrivs`~~ **这句是错的。** 对着 go1.26.1 核对过:`syscall.SysProcAttr` 有 `Credential`、`AmbientCaps`、`Cloneflags`,没有 `NoNewPrivs`。这个 prctl 需要一个 shim,而 shim 被排除的理由跟 `netns_linux.go` 排除它的理由相同 —— noded 记录到的 pid 会是 shim 的,于是 `killVMM` 的 `kill(-pid)` 会给错误的进程组发信号。它会随 jailer 一起到来,jailer 自己做这个 prctl |
| netns | exec 前 `setns`,或继续用 `ip netns exec` | 不用 jailer 也能做到 |

**jailer 给的、上面这条路给不了的** —— 诚实的清单,因为这正是决定先后顺序的对比:

1. **mount namespace 收敛。** `unshare(CLONE_NEWNS)` + `pivot_root` + `umount old_root`。
   被攻破的 VMM 只能看见 jail。`SysProcAttr.Credential` 让整个宿主文件系统仍然**可见**,
   只是不全都可写。这是实质差别,也是 A3 真正在讲的那件事:"FC 或 KVM 的漏洞于是意味着拿到
   宿主 root,而不是拿到 chroot 里一个低权限用户"。单靠降 uid 给到的是"一个对宿主文件系统有
   完整视野的低权限用户"。
2. **一个只含四个设备节点的 `/dev`。** 设备白名单是 jail 的 `/dev` 的性质,不是某个 syscall
   的性质。没有 mount namespace,就没有办法只对那一个进程呈现一个收窄的 `/dev`。
3. **fd 卫生与环境变量清空。** jailer 会关掉除 stdio 之外每一个继承来的 fd(通过
   `/proc/<pid>/fd`),并清掉继承的环境变量。Go 的 `exec` 只要写得小心,两样都不会泄漏,
   但 jailer 是无条件做的。
4. **`setrlimit` 的 `fsize`/`no-file`**(`no-file` 默认 2048)。Go 里能做到,目前没做。
5. **不与其他 VMM 共享二进制代码段** —— 这就是那次复制的理由。

反过来,**只走 cgroup 这条路给的、jailer 给不了的**:没有新的路径解析语义,因此对 §3 的资产、
以及那份让 fork 便宜的共享内存镜像,都没有风险。零快照风险。

## 8. 建议 📐

**把 #20 拆开。cgroup 那一半独立于 jailer 那一半,而且它正是当下卡住别的东西的那一半。**

**第一阶段 —— cgroup + 降权 + rlimit,不做 chroot。⚠️ 已交付,默认关闭。**
交付了第 4 项,以及 `overcommit.go` 和 `cmd/noded/main.go:99` 都点名为"把内存 overcommit 抬到
1.0 以上的前提"的那个资源公平性强制。不碰任何路径。不可能破坏 restore 或 fork。cgroup 那一半
**要求 cgroup v2**,v1 节点给了 `--fc-cgroups` 会拒绝启动;降权和 rlimit 与层级无关。

已交付的形态里**不**包含的,对照上面的清单:第 3 项的 fd 卫生与环境变量清空(jailer 两样都
无条件做;这里两样都没做),以及 `PR_SET_NO_NEW_PRIVS`,理由见 §7 表格里那处更正。
`RLIMIT_FSIZE` 也没设 —— 可写层的大小本来就是磁盘上限
(见 [security-and-startup.md](security-and-startup.md) §A3),所以对同一件事再加一道界,
会是一个没有陈述依据的数字。

第一阶段必须为被降权的 uid 打通两样东西,而两样都是从 §3 和 §4 推出来的,不是跑出来的:
每沙箱的 dm 设备节点(通过解析 `rootfs.img` 来 chown —— 目录遍历**故意不跟随**那个符号链接,
否则一个共享资产会被 chown 成某一个沙箱的身份),以及 UFFD socket,它的失败模式是 §4 说的
挂住而不是报错。共享的只读资产 —— 内核与 agent 磁盘 —— 保持 world-readable 而不是被 chown,
并且在启动时检查,因为否则它们各自都会让每次 create 失败,而症状说不出原因。

**第二阶段 —— jailer,但要等资产可达性的工作先存在**,那才是采用它的真正内容,而且还没写:

1. 在 KVM 宿主上 `firecracker --version`;判断 `vsock_override` 到底有没有。
2. 把内核放进 jail(同文件系统就硬链接,否则 bind mount)。
3. 把 agent 磁盘放进 jail(硬链接;它是共享只读的,所以很便宜)。
4. **用 `/dev/mapper/bean-<id>` 的 major:minor 在 jail 内 `mknod` 出每沙箱的 dm 设备**,
   替掉那个符号链接。这一步没有任何原型,也是最可能出意外的一块。
5. 决定 `.snapshots` 怎么进 jail,而**不能**给每个 jail 复制一份内存镜像,因为共享的只读
   `mmap` 正是 fork 便宜的原因。大概是把快照缓存目录做只读 bind mount。**未验证。**
6. 对着 234ms 的预算测量每次 create 的二进制复制成本。

**对最初那个问题的结论**:jailer 可以在不破坏快照可移植性的前提下采用 —— 每沙箱一个 jail root
保住了相对路径这个性质,netns 也能组合。但**它不能原样采用**,因为 VMM 目前通过绝对路径或符号
链接访问的五个宿主资产在 jail 内不可达,而其中一个是设备节点。A3 点出的安全缺口是真实的;
针对它最快的诚实进展是第一阶段,而第一阶段不带那些风险。

## 9. 必须在 Linux KVM 宿主上跑的验证

```bash
# 1. 部署的是哪个 FC 版本 —— 决定 vsock_override 是否存在。
firecracker --version

# 2. dm 设备在 jail 里到底能不能用?这是单个风险最高的未知。
#    对比 jail 内外的 major:minor。
#    然后把它 mknod 进 jail root,再从它启动一个沙箱。

# 3. 每次 create 一次二进制复制的成本,对着 234ms 的 runtime_create。
#    同时测 jailer 自身的 setup 耗时,沙箱目录放在真实文件系统上。

# 4. 跨 jail 的 UFFD:以被降到的 uid 在 jail root 绑定那个 socket,
#    做一次 restore,确认缺页是被服务了而不是 load 挂住。
#    uffdHandler.Faults() 能区分"从未缺页"和"缺页从未被应答"。

# 5. network.md §4 的开放问题,与 jailer 无关但在同一条 restore 路径上:
#    恢复出的 guest 是否需要一次 `ip neigh flush`。
```

这五条里第 2 条是决定性的。其余四条都是"多少代价"的问题,只有它是"可行不可行"的问题。

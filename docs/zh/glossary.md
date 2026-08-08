# 术语表

bean 用到的术语,一次定义清楚。凡是踩过坑的地方,定义里直接写明,而不是留给下一个人重新发现。

**权威顺序依旧成立:代码 > `status.md` > `decisions.md` > 设计文档 > 本页。**
若本页定义与它们冲突,以它们为准 —— 请告诉我们来修。

---

## 核心对象

**sandbox(沙箱)** —— 一个隔离的执行环境:一份 rootfs、一个 network namespace、
一棵进程树,跑在某一个 runtime 档之下。它是其他一切所围绕的基本单位。一个沙箱有一条
生命周期(见下面的动词)和一个 id(`sbx_...`)。

**image(镜像)** —— 一个 OCI 镜像:*layers*(文件系统,一叠 tarball)加上一个
*配置 blob*(描述如何启动)。bean 拉取并转换镜像,从不需要每镜像一次模板构建。镜像是
沙箱的只读输入,不是运行中的东西。

**base image(基础镜像)** —— 每个节点 loop 挂载一次、在其上所有沙箱间共享的只读镜像。
一个沙箱**不会**拿到自己的副本;它在共享 base 之上获得一个写时复制层(见 *CoW 层*)。
`commit` 可以把运行中沙箱的文件系统冻结成一份新的可复用 base image。

**rootfs** —— 沙箱启动时的根文件系统,由共享 base image 加上沙箱自己的可写 CoW 层组装而成。

**CoW 层** —— 每个沙箱通过 device-mapper 在共享 base 之上获得的稀疏写时复制层。这正是
为什么拉起一百个沙箱的代价是一百个稀疏文件、而非一百份镜像副本 —— create 时每沙箱 44 KiB 磁盘。

**snapshot(快照)** —— 一份持久化的沙箱捕获,它比沙箱本身活得更久,且可被反复用于创建。
一个 `snap_...` 对象,以 blob 形式存储(节点本地和/或 S3)。三种,**语义不同,不只是尺寸不同**:

| 种类 | 参数 | 捕获什么 | 从快照创建后 | 可移植性 |
|---|---|---|---|---|
| 全量 | *(默认)* | 内存 + 文件系统 | 恢复运行,进程树完整 | 绑定 CPU vendor + family |
| 仅文件系统 | `--no-memory` | 只有文件系统 | 重新 boot,文件完好 | 任意 CPU |
| 增量 | `--base SNAP` | 相对父快照写过的页 | 恢复运行 | 绑定 CPU vendor + family |

捕获了内存的快照只能在同 vendor、同 family 的 CPU 上恢复 —— guest 内存记录了那颗 CPU
提供的东西,事后无法屏蔽,所以调度器把它作为硬过滤执行(`409 INCOMPATIBLE_CPU`)。

**bundle** —— 快照在磁盘上/传输时的打包形式(vmstate + 内存 + rootfs 成员)。每节点
解包一次、按 snapshot id 缓存;某节点上首次从快照创建要付解包代价(约 950 ms),之后
每次命中缓存(392 ms)。

---

## 生命周期动词

这些是沙箱上的**外部操作**。每个都映射到一个按 runtime 而定的实现(见 *runtime 档*)——
动词相同,底下发生的事随档而异,而且有些档并不实现全部动词。

**create(创建)** —— 造一个新沙箱。一个端点(`POST /v1/sandboxes`),按输入分支:给
**镜像**就冷启动;给**快照**就走从快照创建。没有单独的 `/restore` 端点。

**create-from-snapshot(从快照创建)** —— 从一份快照 blob 创建一个**新**沙箱(新 id)。
快照是持久的,可以这样用任意多次,每次调用产出一个互相独立的沙箱。这就是早期草稿里作为
用户面动词的所谓 "restore";那个词现在只保留给 runtime 机制,不再是 API 动词。

**resume(唤醒)** —— 唤醒一个 **PAUSED** 沙箱。同一个沙箱、同一个 id、同一棵进程树 ——
它的内存从没离开过宿主 RAM。毫秒级。它**不是**从快照创建:不解包任何东西,也不造新沙箱。
对一个 PAUSED 沙箱发请求会透明触发 resume。

**pause(暂停)** —— 冻结一个 RUNNING 沙箱的 vCPU 而不销毁它,好让之后的 resume 唤醒它。
`on_idle=pause` 策略会在空闲超时后自动做这件事。

**fork(派生)** —— 从**一个运行中的源**产出 N 个**新的**、互相独立的沙箱,每批只做一次
checkpoint,源沙箱保持运行。机制已实现;还没有专门的 API 动词 —— 其表面就是一次 snapshot
加 N 次从快照创建。

**snapshot(动词)** —— 把一个运行中或暂停的沙箱捕获成一个持久的 `snap_...` 对象。对大
内存 guest 是重操作(全部内存页都要写出)。

**destroy(销毁)** —— 拆掉沙箱、释放其资源。要么显式(`DELETE`),要么经 `on_idle=delete`
策略在空闲超时后触发。

沙箱经历的状态比动词少:`create` → **RUNNING** ↔ **PAUSED**,而仅有的出口是 idle 清扫
或一次显式 `DELETE`。见 README 里的状态图。

---

## Runtime 与组件

**runtime 档** —— 沙箱运行所在的隔离后端,用 `--runtime` 选择。存在三档,它们实现同一套
接口,但机制不同、支持程度也不同:

| 档 | 参数 | 隔离 | snapshot / fork |
|---|---|---|---|
| Firecracker microVM | `fc` | 硬件(KVM microVM) | 完整支持 |
| OCI + gVisor / runc | `runsc` / `runc` | gVisor sentry,或 runc 命名空间 | **不支持**(返回 unsupported) |
| local | `local` | 无 —— 仅开发用 | 有限 |

`fc` 是测得更充分的路径,所有实测数字都出自它。OCI 档服务 benchmark 负载(任意镜像、
不需每镜像一次模板构建),但没有 checkpoint 可供 fork。

**bean-api** —— 控制面(一个进程):API 网关、调度器(放置在同进程内,所以放置与承诺是
同一个事务)、镜像服务。由 SQLite 或 Postgres 支撑。

**noded** —— 节点守护进程,每宿主一个。运行 runtime 各档与镜像子系统(base image、CoW、
overlaybd/TCMU)。

**beand** —— 每个沙箱内的 PID 1,装在自己的只读磁盘上,所以用户镜像不需任何改动。它先建
挂载矩阵,再 pivot 进用户镜像。

**bean-proxy** —— 端口流量进入沙箱的数据面路径:`{port}-{sandbox}` 直达该 guest 的那个
端口,用户 server 或 agent 一视同仁。没有注册调用,没有宿主端口池。

**bean** —— CLI。

---

## 一条分层提醒

README 和 lifecycle 表停留在**外部接口**层:create / resume / pause / snapshot / fork /
destroy。像 Firecracker 的 `/snapshot/load` 这类名字是 **`fc` 档的实现细节**,不是外部
动词 —— 它们活在设计文档里,不在表面。当一个词读着像操作、实则命名的是一个机制(restore、
`/snapshot/load`),它属于某个 runtime,而不属于 API。

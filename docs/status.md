# 实现状态

> 快照日期：2026-08-02。CI 全绿（lint / race 单测 / e2e / SDK / proto drift），
> 覆盖率 80.5%。控制面与节点面均在 Linux x86_64 上验证过，无 darwin 平台假设。
>
> **microVM 档已实装并在真 KVM 机器上跑通**：alpine 创建 952ms、
> host 经 vsock exec 拿到输出、snapshot 16-20 MiB、restore 950ms。
>
> 启动优化的归因过程与被否掉的方案见 `docs/decisions.md` —— 那里是
> 「为什么这么选」的权威记录,本文只记「做到哪一步了」。
>
> 实现细节分三篇:`vm-assembly.md`(microVM 怎么组起来,含两处不能动的顺序约束)、
> `image-pipeline.md`(OCI ref 怎么变成块设备)、`s3-storage.md`(自实现 SigV4 与
> Blobs 抽象)。

## 1. 已完成

### 控制面

| 组件 | 状态 | 说明 |
|---|---|---|
| `bean-api` REST gateway | ✅ | sandboxes CRUD、exec、files、logs、events、pause/resume、snapshot、image、metrics;API key 鉴权、配额位、请求体限流、超时钳制 |
| `scheduler` | ✅ | 两级放置（region → 节点）、硬过滤（runtime 能力/labels/承诺量/创建并发）、打分（镜像亲和/装箱/NVMe/spread）;**承诺量落库**,事务内条件更新,多副本不会重复放置、重启不丢账 |
| `nodesvc` | ✅ | Register（bootstrap token 校验 + 签发 node token）、Heartbeat 双向流续租、SyncState、租约过期回调 |
| `store` | ✅ | SQLite（Postgres 接口已抽象）:sandbox / snapshot / image / prewarm job / 节点与预留 |
| 事件 | ✅ | 状态机统一发件 → 持久化 + SSE 实时订阅（按 sandbox/label 过滤,慢订阅者丢弃计数） |
| image API | ✅ | ref/digest/overlaybd 产物三层语义、状态机、prewarm job;registry 凭证 AES-256-GCM 加密存储 |
| snapshot | ✅ | 创建/列表/删除/引用计数、从 snapshot 创建;**blob 存 S3**（本地目录为 dev 默认） |
| S3 存储层 | ✅ | 标准库自实现 SigV4（不引 AWS SDK）、分片上传、range 读;集成测试在 CI 里打真 MinIO |
| 路由 | ✅ | `NodeRouter` per-node 连接池,数据面按记录里的 nodeID 解析 |

### 节点面

| 组件 | 状态 | 说明 |
|---|---|---|
| `noded` | ✅ | Manager（创建/销毁/pause/resume/snapshot/restore、透明唤醒、本地 idle 回收、in-flight 保护）、SandboxService gRPC、node token 鉴权、metrics |
| `Registrar` | ✅ | 出向注册（无需入站）、SyncState 对账销毁孤儿、心跳带状态与承诺量、指数退避重连 |
| `beand`（sandbox 内） | ✅ | 双档 listener（unix socket / **AF_VSOCK**）、**microVM 内作 PID 1**（挂伪文件系统 → pivot 用户镜像）、exec（超时/截断/进程组 kill）、文件（os.Root 防逃逸、原子写）、logs 环形缓冲 |
| `FCRuntime` | ✅ | **真 Firecracker microVM**:VMM 进程管理、agent 盘为 root device + 用户镜像为第二盘、vsock、pause/resume、full snapshot / restore、销毁清理 |
| `image.Provider` | ✅ | `DevMapperProvider`（**共享只读基础镜像 + 每 sandbox CoW,一个 sandbox 只占 8 KiB**）、`FileProvider`（全量拷贝,兜底）、`PullingProvider`（首次使用时拉取转换,并发去重） |
| OCI 镜像拉取与转换 | ✅ | 节点直接说 distribution API（不依赖 docker/containerd）:manifest / 多平台 index / token 挑战 / **layer 断点续传**;whiteout 语义、路径逃逸防护;转换产物带 sidecar 记录 ref |
| prewarm | ✅ | 控制面后台调 `PrewarmImage`,节点拉取转换;节点心跳上报 `cachedImages`,**镜像亲和打分与 prewarm 进度因此才真正生效**（之前从未被填充） |
| commit | ✅ | 把 sandbox 文件系统封成 base image（`CommitSandbox` RPC）。**先 sync guest 再 pause**——只 pause 的话 guest page cache 还是脏的,读块设备会丢掉刚写的东西 |
| build image（Dockerfile） | ✅ | `bean build --tag REF .`,BuildKit 在节点上执行。**导出 `type=tar` 扁平 rootfs**,不组装层也不过 registry,和拉取路径共用同一个 image writer |
| `LocalRuntime` | ✅ | 进程级 sandbox（dev/CI，含 darwin），跑真 beand 二进制,验证与 fc 档相同的 agent gRPC 面 |

### 客户端

Python SDK（create/exec/files/pause/resume/kill、snapshot、images、events 订阅、
context manager、错误分层）、Go CLI（run [--image|--snapshot]/ls/exec/cp/logs/kill/
pause/resume/events -f/snapshot/**commit**/image）。

Python SDK 也有 `sandbox.commit(tag)`。

### 可观测

`bean-api /metrics`：创建结果与延迟、exec 延迟、各状态 sandbox 数、事件计数与订阅数。
`noded /metrics`：创建阶段耗时、创建/销毁/idle/snapshot 计数、节点 sandbox 状态与 in-flight。

日志走 `log/slog`,字段化 + 分级,`--log-format json` 给采集器、
`--log-level` 控制粒度。字段名是共享常量(`internal/logging`),
否则按 sandbox 聚合会因为各组件拼写不同而失效。
context 里带 request id,便于跨组件关联同一次请求。

trace 走 OTel,`--otlp-endpoint` 指向 OTLP/gRPC collector(空则关闭,
装 no-op provider —— 埋点处不做条件判断)。request id **就是 trace id**,
不是另一套编号:两套 id 意味着每次关联都要 join,而它们必然在
跨进程那一跳上分叉。

实测(真机,`hack/tracedump` 收 span):

```
POST /v1/sandboxes                 bean-api   1196.0ms
  SandboxService/CreateSandbox     noded      1110.2ms
    node.Create                    noded      1110.1ms   events=[phase.*]
      runtime.Create               noded       324.2ms
      agent.WaitHealthy            noded       785.8ms

POST /v1/sandboxes/{id}/exec       bean-api     18.6ms
  SandboxService/Exec              noded        17.4ms
    (beand 日志 request=283a333e…)  guest         8.0ms
```

第一棵树立刻给出一个此前没有任何指标覆盖的数字:gateway 与 noded
之间差 **86ms**(1196 − 1110),那是调度 + 落库的开销。

**beand 只采纳 trace id,不导出 span**,而且刻意不链 OTel SDK
(`go list -deps` 验证过为 0):它在 guest 内没有到 collector 的路径,
且 agent 盘的体积按每次 boot 计价。它把调用方的 trace id 写进自己的
日志,所以「慢在 guest 内」可以被核对而不是猜测。
**但 guest 的 stderr 只在 `--debug-console` 下经串口出来** ——
默认关串口(省 493ms)的代价就是那条日志默认不可见。
guest 内日志的常规出口还没做。

阶段耗时的 metric 与 span event 由**同一次调用**产生
(`Manager.observePhase`),所以不会出现某个阶段只进了直方图、
在 trace 里却是一段空白。

fc 档实测(镜像已缓存):`runtime_create` ~234ms(起 VMM)、
`agent_ready` ~770ms(内核启动 + pivot + listen)、`total` ~952ms。
裸 Firecracker 到 agent 可连是 606ms,所以上层开销已基本挤干。

destroy **214ms**(曾是 5.25s)。原先销毁前用 ACPI 请 guest 关机并等它退出,
但 guest 内核没编 `CONFIG_ACPI_BUTTON`、beand 又是没有信号处理的 PID 1 ——
那 5 秒**每次必然超时**。改成经 agent 执行 `sync`:达成的是同一个目的
(可写层与 sandbox 写入一致),而且是确认而非假设。

snapshot:checkpoint 1.5s、bundle 约 16-20 MiB。
restore ~950ms(同一快照首次 1617ms,要付 unpack 代价);
其中 FC `/snapshot/load` 只占 7ms —— guest 内存按需供页(UFFD),
不再把整个内存镜像读进来。剩下的成本是解 bundle。

`--no-memory` 只存文件系统:实测 bundle **6109 字节**对全量 15.5 MB(2550×),
restore 重新 boot 但保留文件(`uptime 0` 且 marker 在),可落任意 CPU。

`--base SNAP` 走增量:只存自 base 以来 guest 写过的内存页。实测 298 KB 对
15.5 MB(52×)。恢复时按链从根到叶物化成平坦镜像再交给现有 UFFD handler ——
不在缺页路径上分层,因为那是全系统最热、出错最隐蔽的代码。合并结果按 leaf id
进 snapCache,所以 fan-out 场景每节点只付一次。链深超 8 自动转 full。
需要节点带 `--track-dirty-pages` 启动(默认关);没开的 guest 请求 diff 明确报错
而不降级,详见 `docs/decisions.md` §3.0.1。

**restore 曾经会静默损坏文件系统**,已修:dm-snapshot 在设备激活时就把
exception table 读进内核,而 restore 是在那之后才把 extents 写进 `cow.img` ——
内核不认,设备继续供 base image。full snapshot 上这不可见,因为读命中的是
内存快照带回的 page cache;`drop_caches` 之后同一个文件读出 9 个 `\0`,
而 `ls` 仍显示 size=9、无 EIO、无 dmesg。现在 CoW 在组装设备**之前**恢复,
两条路径都实测过 drop_caches 后仍正确。详见 `docs/decisions.md` §3.0。

**内存快照绑 CPU**,所以 restore 是受约束的:节点上报 vendor/family/template,
快照记下产出它的那三项,调度器按此硬过滤,不兼容返回 409 `INCOMPATIBLE_CPU`
而不是放置后让 guest 崩。`--cpu-template portable` 掩掉宽向量特征
(实测 `avx avx2 fma f16c` 消失,`sse2`/`xsave` 保留)让快照能跨 CPU 型号,
但 vendor 与 family 掩不掉 —— 详见 `docs/decisions.md` §3.6。

镜像首次拉取转换:busybox 5-10s,alpine 在网络不稳时 2m45s ——
所以 prewarm 是必需的,不是优化。

### 验证覆盖

- **microVM 全链路**（真 KVM 机器,经 Manager 与 CLI 两层）：create → exec →
  cp 双向 → pause → 透明唤醒 → snapshot → 从 snapshot 创建 → 验证时点语义
  （快照后写入不出现在克隆里）→ 克隆间互相独立 → destroy 无残留
- 单节点 / 多节点 e2e：真进程 gateway + noded + CLI（local 档）
- scheduler 持久化属性：两副本并发放置恰好 N 个成功、重启不丢承诺量、
  LOST 跨副本只报一次、孤儿预留可回收
- S3：真 MinIO 上分片上传、abort 不留可读对象、range 读、含空格的 key
- 安全回归：symlink 逃逸阻断、setuid 位剥离、host env 不泄漏、孤儿孙进程不挂起
- vsock：CONNECT 握手不过读、Close 唤醒阻塞的 Accept、端口可重绑

## 2. 与设计的差距

| 项 | 状态 |
|---|---|
| build image：声明式 steps（Modal 风格链式 API） | ⛔ 未开始;Dockerfile 路径已通,steps 只是另一个前端编译到同一个 plan（`docs/image-build.md` §3.2、§5） |
| overlaybd lazy-pull | ⚠️ **能力已实测跑通,尚未接入代码**。当前生产路径是「拉全量 + 转换 + CoW 共享」（每 sandbox 8 KiB）。overlaybd 侧已在验证机上验证:挂载 7ms、只传 19.6% 的层字节就能挂载并读文件、8 个 HTTP 206、可写上层实占 40 KiB（`docs/decisions.md` §3.1）。剩下的是写 `OverlaybdProvider` 接进 `image.Provider` |
| diff snapshot（增量） | ✅ `--base SNAP` 只存自 base 以来改动的 guest 内存。实测 base 15.5 MB → diff 298 KB(52×);深度 2 的链恢复后文件全在且 `uptime 57`(resume 非重启)。合并在 restore 时物化成平坦镜像,**UFFD 缺页路径零改动**;链深超 8 自动转 full;删 base 有子代时返回 409。需 `--track-dirty-pages`(默认关,boot 前生效) |
| fork / shared-fs 卷 / proxy 端口暴露 | ⛔ P3–P4 范围,未开始 |
| OTel trace | ✅ **已实装并实测**。一次 create/exec 是一棵跨进程 span 树(下方「可观测」段有实测树)。`--otlp-endpoint` 为空则装 no-op provider,埋点无需条件判断。**限制**:beand 在 guest 内无出网路径,只采纳 trace id 写进自己的日志、不导出 span;而 guest 的 stderr 只在 `--debug-console` 下经串口出来,所以默认配置看不到那条日志 |
| 资源超卖 | ✅ `--overcommit-cpu` / `--overcommit-memory`,节点侧算,上报已含系数。实测 `--cpu 8 --overcommit-cpu 3` → allocatable 24。CPU 超了只是变慢,内存超了是被杀,所以内存默认 1.0 —— 抬高它需要先实测 FC 按需供页的富余(#18)并给 VMM 进程加 cgroup(#20) |
| Postgres | ⚠️ 接口已抽象,当前 SQLite |
| 创建阶段指标 network | ⚠️ 埋点位已留,等网络实装 |

## 3. 节点前提

fc 档需要：

- `/dev/kvm`（Intel VT-x 或 AMD SVM）
- Firecracker 二进制、guest 内核镜像、agent 盘（`hack/build-assets.sh` 构建;
  `kernel` 子命令下载 Firecracker CI 的 `vmlinux-6.1.102` 及其 config）
- **userfaultfd**（`CONFIG_USERFAULTFD=y`）：restore 靠它按需供页。
  5.15 走 `userfaultfd` syscall,6.1+ 走 `/dev/userfaultfd`;
  `unprivileged_userfaultfd=0` 也可以,因为 noded 以 root 运行。
- **AMD 主机需 `kvm.ignore_msrs=Y`**：Firecracker 保存快照时读 Intel 专有的
  MSR 0x3a,AMD 上 KVM 会拒绝。`NewFCTier` 启动时检查并给出修复命令,
  而不是等到快照失败才暴露。

overlaybd 需要 ublk（内核 ≥ 6.0)或 tcmu 后端。当前验证机是 Ubuntu 20.04 +
内核 5.15,无 `/dev/ublk-control`,所以走 **tcmu**（`target_core_user` +
`tcm_loop` 模块）—— **已实测功能完备**,ublk 只是性能更好,不是前提。

tcmu 路径另需注意宿主上的 `multipathd`:TCMU 设备默认无唯一序列号,
multipathd 会把多个 overlaybd 设备合并成一条 multipath,
**读到的是别的镜像的数据**。必须给每个 backstore 写 `wwn/vpd_unit_serial`。

## 4. 下一步

1. **overlaybd 接入 `image.Provider`**:能力已在验证机上实测跑通
   (tcmu 后端,`docs/decisions.md` §3.1),不再是「能不能用」的问题。
   要写的是 `OverlaybdProvider`:configfs 编排(**LUN 必须在 nexus 之后建、
   必须设唯一 `vpd_unit_serial`**,两个坑都会静默失败)、
   转换产物推 registry、设备生命周期与释放。
   CoW 已经解决「每 sandbox 的成本」,overlaybd 解决的是
   「首次使用一个大镜像的等待时间」。
2. **build 的构建日志与取消**：现在 build 是「起了就等」,失败只能从 image state
   看到 FAILED。日志落存储 + 可流式查看 + `cancel` 才算完整（`docs/image-build.md` §6）。
3. **guest 内日志的出口**:beand 已把 trace id 写进日志,但默认关串口
   意味着那条日志没有出口(只有 `--debug-console` 能看到)。
   应该走 vsock 把 guest 日志收到节点侧,而不是靠串口 ——
   串口既慢(493ms/boot)又只能在调试时开。
4. **restore 剩下的 ~950ms**：命中 unpack 缓存后,仍要把整个 bundle 从
   gateway 传过来并解 gzip,只为取出 rootfs 那一个 member。
   要么命中时让节点告诉控制面「别发了」,要么把 rootfs 拆成独立对象。
5. **`--track-dirty-pages` 的开销未实测**:diff snapshot 需要它,但它默认关,
   因为 KVM 记账的代价没量过。Firecracker 文档只说「CPU cycles」外加
   「negates most of the benefits of huge pages」(我们没用 huge pages,不适用)。
   E2B 常开是先例,但先例不等于在我们的负载上可忽略 —— 要对比开/关的
   boot-to-agent 与 exec 吞吐才能决定要不要改成默认开。
6. **AVX-512 掩码与跨型号 restore 未实测**:验证机是 Zen 2,
   没有 AVX-512,也只有一台 fc 机器 —— 所以 CPU template 的
   「跨型号可移植」这个核心目的没有实证,只有逻辑推导。
   `hack/cpu-template-probe.sh` 会报告本机缺哪些特征。

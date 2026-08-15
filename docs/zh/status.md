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
| `store` | ✅ | SQLite:sandbox / snapshot / image / prewarm job / 节点与预留。**没有 `Store` 接口**,各调用点用的都是具体类型 `*store.Store`;真正成立的是 SQL 边界收在一个包里(`database/sql` 与驱动 import 只出现在 `internal/control/store`) |
| 事件 | ✅ | 状态机统一发件 → 持久化 + SSE 实时订阅（按 sandbox/label 过滤,慢订阅者丢弃计数） |
| image API | ✅ | ref/digest/overlaybd 产物三层语义、状态机、prewarm job;registry 凭证 AES-256-GCM 加密存储 |
| snapshot | ✅ | 创建/列表/删除/引用计数、从 snapshot 创建;**blob 存 S3**（本地目录为 dev 默认） |
| S3 存储层 | ✅ | 标准库自实现 SigV4（不引 AWS SDK）、分片上传、range 读;集成测试在 CI 里打真 MinIO |
| 路由 | ✅ | `NodeRouter` per-node 连接池,数据面按记录里的 nodeID 解析 |

### 节点面

| 组件 | 状态 | 说明 |
|---|---|---|
| `noded` | ✅ | Manager（创建 含从快照创建/销毁/pause/resume/snapshot、透明唤醒、本地 idle 回收、in-flight 保护）、SandboxService gRPC、node token 鉴权、metrics |
| `Registrar` | ✅ | 出向注册（无需入站）、SyncState 对账销毁孤儿、心跳带状态与承诺量、指数退避重连 |
| `beand`（sandbox 内） | ✅ | 双档 listener（unix socket / **AF_VSOCK**）、**microVM 内作 PID 1**（挂伪文件系统 → pivot 用户镜像）、exec（超时/截断/进程组 kill）、文件（os.Root 防逃逸、原子写）、logs 环形缓冲 |
| `FCRuntime` | ✅ | **真 Firecracker microVM**:VMM 进程管理、agent 盘为 root device + 用户镜像为第二盘、vsock、pause/resume、full snapshot + 从快照创建(内部 Fork 路径)、销毁清理 |
| `image.Provider` | ✅ | `DevMapperProvider`（**共享只读基础镜像 + 每 sandbox CoW,一个 sandbox 只占 44 KiB**）、`FileProvider`（全量拷贝,兜底）、`PullingProvider`（首次使用时拉取转换,并发去重） |
| OCI 镜像拉取与转换 | ✅ | 节点直接说 distribution API（不依赖 docker/containerd）:manifest / 多平台 index / token 挑战 / **layer 断点续传**;whiteout 语义、路径逃逸防护;转换产物带元数据文件记录 ref |
| prewarm | ✅ | 控制面后台调 `PrewarmImage`,节点拉取转换;节点心跳上报 `cachedImages`,**镜像亲和打分与 prewarm 进度因此才真正生效**（之前从未被填充） |
| build image（Dockerfile） | ✅ | `bean build --tag REF .`,BuildKit 在节点上执行。**导出 `type=tar` 扁平 rootfs**,不组装层也不过 registry,和拉取路径共用同一个 image writer |
| `OCITier`（容器档） | ✅ | `--runtime runc` / `--runtime runsc`:noded 直驱 OCI runtime(`NewOCITier`),**无 containerd** —— 两者同一套 bundle 与子命令,共用 fc 档的 rootfs providers。这是继 `fc`、`local` 之后的第三个已实装 runtime 档 |
| `LocalRuntime` | ✅ | 进程级 sandbox（dev/CI，含 darwin），跑真 beand 二进制,验证与 fc 档相同的 agent gRPC 面 |

### 客户端

Python SDK（create/exec/files/pause/resume/kill、snapshot、images、events 订阅、
context manager、错误分层）、Go CLI（run [--image|--snapshot]/ls/exec/cp/logs/kill/
pause/resume/events -f/snapshot/image）。

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
restore 每次产出的是一个**新的** sandbox(新 id),同一份快照 restore N 次就是 N 个
互相独立的 sandbox —— 这与 resume(把同一个 sandbox 唤回来)是两件事,见
[snapshot-resume.md](snapshot-resume.md) §0。
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

### 规模压测(2026-08-03)⚠️

`hack/stress-fc.sh` + `hack/phase-delta.py`(差分两次 metrics 抓取,
把单次压测的相位耗时从累计直方图里分离出来)。Zen 2 / **16 物理核** / 24 GB,
alpine:3.20:

| 并发 | p50 | agent_ready | runtime_create |
|---|---|---|---|
| 1 | 938ms | 627ms | 241ms |
| 2 | 1228ms | | |
| 4 | 2010ms | | |
| 8 | 3803ms | 2920ms | 272ms |
| 12 | 5556ms | | |
| 16 | 6805ms | 5710ms | 369ms |

#### 结论:瓶颈是 guest boot 抢 CPU,不是我们的代码 ✅(已归因)

**`agent_ready` 占 94%**(6079ms 里的 5710ms),而 `runtime_create`
(dm-snapshot 组装 + VMM spawn)从 241ms 只涨到 369ms —— **几乎不随并发变化**。
所以 dmsetup/losetup/稀疏文件那条链不是瓶颈,`DevMapperProvider.mu`
也不是(它只包 map 操作,里面那次 `losetup` 是每镜像一次而非每 sandbox 一次)。

压测中 `vmstat` 的读数是决定性的:

```
 r  b   bi    bo    in     cs    us sy id wa
16  0   0     20   5030   2048   62 38  0  0     ← 16 runnable / 16 核 / id=0
```

`r=16`、`id=0`、`us+sy=100%`、`wa≈0`、`b=0`:**16 个可运行线程占满 16 个核,
且没有 IO 等待**。逐进程确认:每个 firecracker 在 21s wall 里烧了
**5 CPU-秒**,16 × 5 = 80 CPU-秒挤进 16 核 → 每个 boot 被拉长约 5 倍。
且 21s→53s 之间 CPU 时间**停在 5s 不动**,说明这 5s 全是 boot,idle guest 不耗 CPU。

**所以吞吐上限约 2.3 creates/s,由「每次 boot 5 CPU-秒 ÷ 核数」决定。**
降低单次延迟必须减少每 boot 的 CPU 消耗(内核裁剪、更少的 guest 初始化),
而不是加大并发窗口。**从快照 restore 是绕开这 5 CPU-秒的正解** ——
这也是 restore 相对 create 的真实价值(是 restore 而非 resume:它造出一个新
sandbox,所以才能替代 create)。

#### 创建排队:30 并发从 16 成功变成 30 成功 ✅

`--create-wait 60s`(网关,默认 0 即立即拒绝)。同一台机器同一个压测:

| | 成功 | 失败 | wall | p50 | p95 |
|---|---|---|---|---|---|
| 拒绝(之前) | 16/30 | 14 | 8s | 6805ms | 7497ms |
| 排队(现在) | **30/30** | **0** | 13s | 7550ms | 13213ms |

**这正好验证了吞吐分析**:wall 从 8s 涨到 13s 而成功数从 16 到 30 ——
吞吐没变(仍是 ≈2.3 creates/s),排队只是把拒绝换成了延迟。
所以这不是性能优化,是**把「调用方自己重试」换成「可预测的等待」**:
批量 eval 拿到 503 之后只会再来一波 burst,而重试风暴让情况更糟。

**只对创建并发排队,不对 CPU/内存/磁盘排队。** 区别是时长而非严重程度:
创建并发几秒就自己空出来(那 16 个约 7 秒排完),而 CPU/内存/磁盘的承诺量
是按 sandbox 整个生命周期持有的 —— 等十秒还是不够,等待只会把一个快速清晰的
拒绝变成一个缓慢的、内容完全相同的拒绝。

超时返回 **504 `QUEUE_TIMEOUT`** 而非 503:请求本身是可接纳的,只是节点忙得
超过了调用方愿意等的时间。503 会让人以为集群不够大。

**一个只有真机能发现的 bug**:第一版实现在 30 并发下只到 24/30,剩下 6 个报
`createConcurrency blocked 1/1` —— 恰好是该排队的情况。原因是我把「已经没有
节点被阻塞」当成了停止等待的理由,而这个判断跑在一次失败的 `Schedule` 之后,
burst 期间 in-flight 计数经常正好在这中间掉下来。已补单测。

#### `max_creates=16` 从来不是真正的限制器 ⚠️(GitHub #19)

三次压测暴露了限制器会随配置迁移,而**先撞上的总是记账最粗的那个资源**:

| 节点配置 | 30 并发成功数 | 真正的限制器 |
|---|---|---|
| disk 100 GiB, cpu 8 | **5** | `102400 / 20480` = 磁盘名义记账 |
| disk 100 GiB, cpu 8, 请求 2 GiB 盘 | **8** | `cpuAllocatable 8 / 1 vCPU` |
| disk 1 TiB, cpu 32 | **16** | 这才是 `max_creates` |

**默认配置下先撞的是磁盘,不是 `max_creates`。** 之前把「成功 16 个」
归因给 `max_creates` 是巧合 —— 那台机器恰好 16 核。
`max_creates=16` 是 `store.go:112` 的硬编码默认值,与核数无关。

**磁盘按名义大小记账,高估约 47 万倍**(GitHub #24):CoW 实际 44 KiB,
记账 20 GiB。eval 负载几乎不写盘,所以这把节点密度压到实际能力的几百分之一。

**没有泄漏**:每轮压测后 dm 映射、firecracker 进程、持有已删除文件的
loop device 全部归零 —— loop 泄漏的修复(#16)在并发下成立。

### 磁盘:实际占用上报 + 低空间停止接单 ✅

承诺量与真实占用的差距现在两边都可见 —— 实测同一节点
`diskCommittedMiB: 0` 而 `diskUsedMiB: 76200`。**这个差距本身就是那个盲区**:
承诺量说节点空着,而盘上已经用掉 76 GB(base 镜像、快照缓存、别的服务)。

**不做超卖系数**:那是让运维猜一个倍数,而稀疏文件的名义大小本来就不该是记账依据。
改为 `statfs` 测真实占用并上报(`bean_node_disk_{free,used}_bytes` +
心跳 `disk_used_mib` + `/v1/nodes` 的 `diskUsedMiB`)。

**放置仍然走承诺量账本**,没有改成按真实水位判断 —— 账本不会被突发写满超卖,
而真实占用是滞后的。真正的防线放在节点侧:`--min-free-disk-mib` /
`--min-free-disk-percent`(两者取大者,默认关)。

理由是实测出来的失败模式(decisions §3.7):宿主盘满时 dm-snapshot 转 `Invalid`,
**guest 侧的 `write()` 仍然返回成功但数据全丢**,remount 直接读不出 superblock。
**盘满之后没有补救可做** —— 那时 sandbox 已不可恢复。好在共享 base 完好,
爆炸半径是单个 sandbox,所以「宁可拒绝新建」这个取舍是明确划算的。

真机验证:不可能满足的水位下 create 返回 **503 `NO_CAPACITY`**(不是 500),
消息带上路径、当前空闲、地板值和后果;没有留下 VM、dm 映射或目录;
`bean_node_creates_refused_total{reason="disk_pressure"}` 递增。
换成现实水位(5 GiB / 5%)后 6 并发全部成功、无泄漏。

**关于每 sandbox 的占用:统一引用 44 KiB。** 文档里此前还出现过 8 KiB 和 80 KiB,
三者并不是对同一次测量的分歧 —— 它们的测量点在 sandbox 生命周期的不同位置:
8 KiB 是刚组装好、还没写过的 CoW 层;44 KiB 是已经启动并写过的 sandbox;
80 KiB 是代码注释里某一次具体小写入的结果。有意义的对照是
`FileProvider` 每 sandbox 拷一份完整 base 镜像,在那个量级上三个数说的是同一件事。

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
- **store 的 requirement 对真 Postgres 16 跑过**（不是 mock）：九条 requirement
  加一个逐方法 smoke test,入口 `hack/postgres-conformance.sh`。并确认这个绿是
  数据库挣来的、不是引擎的锁掩盖出来的 —— 把条件 `UPDATE` 换成 `SELECT`,
  引用计数那条 requirement 在 Postgres 上同样失败。两个引擎都跑 `-race`,
  这一点重要是因为 store 现在没有任何 mutex

### 第三条测试规则：没有测试调用的语句,等于没有引擎解析过它

接 Postgres 时,范围是靠读 SQL 定的:103 处占位符、一个 `AUTOINCREMENT`、
一个当 bool 用的 `INTEGER`、`ON CONFLICT` 全部可移植。这个梳理对了形状,错了清单。
对真 Postgres 16 跑 `migrate()` 又找出四处差异,其中后两处读代码根本读不出来:

- `secret BLOB` —— Postgres 没这个类型,整个 schema 被拒
- `ADD COLUMN` 幂等 —— 只有 Postgres 能写 `IF NOT EXISTS`,所以重复列这件事必须
  按引擎分开处理,而不是拿一份错误文本去匹配两个引擎
- `INTEGER` —— SQLite 64 位,Postgres 32 位,而所有时间戳列存的是 Unix 毫秒。
  七条 requirement 里五条溢出失败。**这条靠看是看不出来的**:拼写一样,含义不一样
- **`Reserve` 从来就没有 GPU 守卫。** 8 个占位符、9 个实参,SQLite 默默吞掉多余的
  那个,所以 `gpu_committed` 从未与 `gpu_count` 比对过。单卡节点会把同一块 GPU 发给
  两个 guest,故障最后落在 guest 里表现成「设备已被占用」。只有会数占位符的引擎提出了异议

然后套件 8/8 全绿,而 `Release` 和 `FinishCreate` 在 Postgres 上从没执行过 ——
两者都用了 SQLite 的两参数 `MAX(x - ?, 0)`,Postgres 跑不了,而没有任何 requirement
调用它们。真上线的后果:容量在 `Reserve` 提交、永远还不回来,节点永久填满,
后续放置一律报 `NO_CAPACITY`,而那些资源没人在用 —— 报错说的是容量,不是那条语句。

事后实测:**38 个接口方法里,23 个从没被任何测试在 Postgres 上执行过。**
所以现在有一个把每个方法各调一次的 smoke test,加一个基于反射的漂移守卫 ——
手写的调用清单否则会跟接口一样腐烂。守卫立刻抓出它自己初稿漏掉的三个 snapshot 方法。

## 归因笔记:create 与 destroy 关键路径的实测

以下数字全部来自同一台 128 核机器(AMD EPYC,Ubuntu 22.04,503 GB),除注明外均为
256 并发 create。记下来是因为每一条都推翻了一个此前未经实测就成立的说法。

### create 的时间去哪了

`runtime_create` 以前是一个覆盖了几乎整个 create 的不透明数字,所以归因只能停在
「在这里面某处」。现在拆成六个子阶段:

| 阶段 | dm-snapshot | overlaybd |
|---|---|---|
| `fc_rootfs` | 3.809s | 0.908s |
| `fc_boot` | 0.133s | 0.050s |
| `fc_vmm_spawn` | 0.066s | 0.025s |
| `fc_api_ready` | 0.000s | 0.002s |
| `fc_cgroup` | 0.000s | 0.000s |
| `agent_ready` | 0.316s | 0.306s |
| **总计** | **4.512s** | **1.299s** |

`fc_rootfs` 是天花板,而且只有它随负载增长:n=16 时 0.863s,n=256 时 3.809s,
boot 和 spawn 基本不变。

成本是 subprocess 开销 —— 这是在宿主上把其他可能逐个排除掉得出的,不是推理出来的。
裸 dm-snapshot 创建能并行(串行 10/s,并发 65/s);`losetup --find` 不随设备数退化
(已有 0 / 100 / 200 个设备时每次调用都是 26ms)。所以是每个 sandbox 两次
`losetup` 加一次 `dmsetup` 的 `fork`/`exec`,每次约 26ms。`attachTCMU` 写 configfs、
完全不 fork,这就是 overlaybd 在这个阶段快 4.2 倍的原因。

### `cores / cpu-per-create` 那个说法是错的

README、本文和 `--create-wait` 的帮助文本都写着上限约 `cores / 5` —— 那是在一台
16 核机器上、当时每个 create 都要 boot 时量的。实测成本是**每个 create 0.31–0.44
CPU-秒**,小一个数量级;而实测吞吐只有该成本预测值的 0.16–0.28。也就是说在测过的
任何并发下,宿主 CPU 都不是约束。

在归因清楚之前有两个东西被错怪过,一并记下免得有人重走。**manager 的 mutex**:
create 只短暂持锁,而我为抓 2N+1 访问模式写的测试在坏代码上照样通过,所以那个测试
被删掉而不是留着。**SQLite 写争用**:300 个并发写入者下 `TouchNode` 从 119µs 涨到
25ms —— 214 倍,但离产生影响还差三个数量级。之后同一轮实测 Postgres 吞吐只有
SQLite 的**一半**(47.7 对 89.4 creates/s),这就彻底否掉了「单写连接在限制吞吐」
的想法。Postgres 的价值是多副本共享一份状态,不是让单节点更快。

### destroy 有一个永远不可能起作用的 2 秒地板

`killVMM` 先发 `SIGTERM`、最多等 2 秒、再发 `SIGKILL`。而 Firecracker 装了
`SIGTERM` 处理器且不会因它退出:活体 VMM 上实测 `SigCgt: 0000000441801449`,发信号
3 秒后仍然存活,`SIGKILL` 则 59ms 死亡。去掉这个无用等待后,单次 destroy
从 2184ms 降到约 200ms。

这是这条路径上**第二个**完全同形状的等待 —— 之前那个 ACPI poweroff 等待占了
5335ms destroy 中的 5001ms,同样不可能成功,因为 guest 内核没有编
`CONFIG_ACPI_BUTTON`。两次被藏住的原因也一样:destroy 只有一个数字,固定地板看起来
就像「拆设备本来就要这么久」。现在 `destroy_flush` 和 `destroy_network` 是独立阶段,
分别是 0.418s 和 0.000s。

**仍未解决**:高并发下 destroy 是串行的。wall 时间与数量成线性(31 → 1968ms、
64 → 3445ms、128 → 7372ms),每个恒定约 57ms,而 57ms × 128 正好是那 7372ms。
不是内核 —— 10 次串行 backstore `rmdir` 总共 15ms;`Destroy` 也在拆除之前就释放了
runtime 锁。所以串行点在 configfs 之上的某处,尚未定位。

### 一个健康的节点被判定为 LOST

对账跑在心跳流打开**之前**,而它的耗时无界:每个残留的 device-mapper 映射,
`dmsetup remove --retry` 要 4.806 秒才放弃,且严格串行。109 个残留就是 8.7 分钟才
发出第一次心跳,而 lease 是 45 秒。按时间顺序读日志时它是明的 ——
20:19:54 注册成功,20:20:44 lease 过期,中间一次心跳都没有。

## 2. 与设计的差距

| 项 | 状态 |
|---|---|
| build image：声明式 steps（Modal 风格链式 API） | ⛔ 未开始;Dockerfile 路径已通,steps 只是另一个前端编译到同一个 plan（`docs/image-build.md` §3.2、§5） |
| overlaybd | ⚠️ **已接入并在真机端到端验证**(PR #49)。`OverlaybdProvider` 走 TCMU,`--fc-overlaybd` 开启,**dm-snapshot 仍是默认**。实测:sandbox 从 overlaybd 设备启动、guest 从自己的 rootfs 读到 `PRETTY_NAME="Alpine Linux v3.20"`、写落在可写层、`bean kill` 后无 backstore 无 multipath 残留(`hack/overlaybd-e2e.sh`)。同机对比 dm-snapshot(`hack/overlaybd-bench.sh`):三个共享 base 的镜像 **392 MiB → 118 MiB**,转换 CPU 从每镜像平均 2.2 s 降到 1.37 / 0.49 / 0.44 s。**冷启动延迟没有改善** —— 这条路径依然先下载再转换每一层才能组设备,首次使用比拍平做的功还多,收益在第二个镜像和磁盘上。128 核机器 256 并发 create 是它真正发挥的地方:`fc_rootfs` 3.809 s → 0.908 s,吞吐 47.5 → 88.0 creates/s,零失败零泄漏 —— 原因是子进程数,dm-snapshot 每 sandbox fork 两次 `losetup` 一次 `dmsetup`(每次约 26 ms),而 `attachTCMU` 全是 configfs 写、完全不 fork。 |
| overlaybd lazy-pull | ⚠️ **已实现,未对真 registry 验证过**。`--fc-overlaybd-lazy-pull`。挂载 7ms、只传 19.6% 的层字节就能挂载并读文件、8 个 HTTP 206 —— 这些数字来自 `docs/decisions.md` §3.1 的手工验证,针对的是**已经封好的 overlaybd 层**,不是来自这份代码。普通 OCI 层是 gzip tar,没有可 seek 的块索引,所以指名这种镜像的 create 会被**拒绝**而不是悄悄建一个打不开的 config。产出封好的层是 `Prewarm` 的活:它转换镜像并把每一层按 OCI digest 发布到 bean 自己的对象存储(`--fc-overlaybd-s3-endpoint`),之后任何读同一个存储的节点直接远端读、不再转换。**create 从不发布** —— 几十 MiB 的 S3 上传不该压在 sandbox 的延迟路径上。所以真正冷的 create 仍然是一次转换,但那是**每机群每镜像一次**而不是每节点一次,前提是有人 prewarm |
| overlaybd over ublk | ⚠️ **已接线并在真机实测**。`--fc-overlaybd` 加 `--fc-ublk`:层的解析、按 digest 共享、缺层时转换全都和 TCMU 路线一模一样,变的只是由本进程读层并用 io_uring 建设备,不再把 config 交给 overlaybd 守护进程、也不再每 sandbox 组一套 SCSI fabric。这意味着层格式是用 Go 读的:`lsmt.go` 解 trailer 和位打包的索引,`zfile.go` 解块压缩数据,`lz4block.go` 解块本身(约 100 行;它在每次 guest 读的路径上,所以不引依赖),`lsmtstack.go` 按「新层胜出」合并整条链,`lsmtcow.go` 在上面盖一层稀疏 overlay。**做这件事的理由是 teardown**:拆 128 个 TCMU 设备要 4.0 s,而且 5.15 和 6.8 上都是 4.0 s —— 守护进程卡在一条上游明确警告不要并发使用的 netlink socket 上,而不随内核版本变化的开销就是传输层的开销。**已在真机对照 TCMU 实测**(`hack/obd-transport-bench.sh`,同机同镜像同档次一轮跑完,两边零失败零泄漏):p50 4 并发 461→334ms、16 并发 512→361ms、60 并发 642→420ms;60 并发吞吐 70.1→101.5 creates/s;`fc_rootfs` 0.227s→0.027s(**这才是这次改动真正拿到的 8 倍**),`runtime_create` 0.258s→0.057s。teardown 的主张在它被提出的地方成立:60 并发下 TCMU 的 `obd_detach` 平均 0.704s/sandbox,而 ublk 路径**根本没有这个阶段** —— 没有 fabric 要拆。guest 从本进程自己解码的层链启动:ext4 superblock 经 tar → zfile → lsmt extent → stack 合并读出 0xef53。**这轮真机抓到 5 个单元测试全绿时一个都没暴露的 bug**,详见英文版 status 的「overlaybd over ublk: what only hardware found」。**lazy pull 现在也走得通,并已在真机验证**。只存在于对象存储里的层可以直接背书一个 ublk 设备:`openLSMTStackFrom` 收本地/远端混合的层源,远端的那条走 range 请求读(`blobreader.go` 负责分块和缓存,`blobfetch.go` 对接 registry 或对象存储 URL)。格式代码一行没改 —— 因为传输层以下的每个 reader 本来就收 `io.ReaderAt`,这也正是本行早先把这个缺口写成「结构性限制」为什么是错的:那种说法会让下一个人不去做。**实测**:层在 create 前后都不在本地磁盘,guest **358ms** 起来,最多读了 **5.1 MiB 层的 60%**(这是上界 —— 那个数把所有 loopback 流量都算进去了,不只是 fetch),并正确读出 `/etc/alpine-release`、`uname -m` 和 `/bin/busybox`。为此修了三个 bug,三个都表现成同一个没信息量的症状,详见英文版「lazy pull over ublk: three bugs behind one symptom」。有两件真实存储会做的事被**拒绝**而不是绕过:**对 Range 请求回 200** 意味着 range 被忽略、整个 blob 从 0 字节开始发,那么读层中段会拿到层开头;**响应被截断**算错误而不是部分成功。另外对象存储必须允许匿名 GET —— overlaybd 自己的守护进程就是这么读的;不允许的话节点会在启动时报出来,否则它会静默地把每一层都转换而不是远端读。另外,只按 digest 指名某层的链会被拒绝,并把层和镜像都报出来,因为运维的下一步是在这台机器上 prewarm 那个镜像。`commit` 这个动词已经不存在了 —— 本行早先把它列为「两条路线都未测」,而 PR #61 已把它整条链路移除:文件系统快照和 commit 出来的镜像底层是同一批内容寻址的 overlaybd 层,所以「保存这个环境去分享」就是把快照提升进 template 命名空间。**给一个已删除的功能留注意事项比不留更糟** —— 它会让读者去找一个不存在的东西测。LZ4 解码器是拿 `lz4` CLI 产出的块校验的,不是只拿测试自己造的块 —— 手工造的向量抓不出编解码双方共享同一个误读 |
| 四条 rootfs 路径回归 | ✅ | worker 拆分和 CoW 互斥锁是为 lazy pull 加的,但它们在**每一次** ublk create 的路径上。修好一条路、弄坏另一条才是真风险,而唯一能看见的办法是把本来就通的那几条再跑一遍。`hack/rootfs-paths-regress.sh` 跑全部四种配置(不带 flag 的 dm-snapshot、只开 ublk、overlaybd 走 tcmu、overlaybd 走 ublk),每一种都要求**guest 回话**,而不只是 sandbox 到 RUNNING —— 这个区分就是重点:agent 不可达正是这轮 bug 从外面看的样子,而只查状态的东西会把它当成成功。每种配置还要写一个文件再读回来,因为互斥锁在写路径上。四条全过、每条两项检查都过,零 ublk 设备、零 dm 映射残留。并发数也没变:60 并发下 ublk 仍是 101.7 creates/s、`fc_rootfs` 0.022s,对比拆分前的 101.5 和 0.027s —— 所以这个交接在它被加进来的那个并发档上不花钱 |
| diff snapshot（增量） | ✅ `--base SNAP` 只存自 base 以来改动的 guest 内存。实测 base 15.5 MB → diff 298 KB(52×);深度 2 的链 restore 后文件全在且 `uptime 57`(载入内存态而非重新开机 —— 新 sandbox 接着被采集那个 guest 的 uptime 走)。合并在 restore 时物化成平坦镜像,**UFFD 缺页路径零改动**;链深超 8 自动转 full;删 base 有子代时返回 409。需 `--track-dirty-pages`(默认关,boot 前生效) |
| 端口暴露与数据面 | ✅ 一个机制而非两个:Host 里的 `{port}-{sandbox}` 直达该 guest 的该端口,用户的服务器和 agent 走同一条路。无需注册调用、无需宿主端口池 —— noded 进入 namespace 后直连。缺的是按端口的访问控制 |
| shared-fs 卷 | ⛔ P3–P4 范围,未开始 |
| OTel trace | ✅ **已实装并实测**。一次 create/exec 是一棵跨进程 span 树(下方「可观测」段有实测树)。`--otlp-endpoint` 为空则装 no-op provider,埋点无需条件判断。**限制**:beand 在 guest 内无出网路径,只采纳 trace id 写进自己的日志、不导出 span;而 guest 的 stderr 只在 `--debug-console` 下经串口出来,所以默认配置看不到那条日志 |
| 资源超卖 | ✅ `--overcommit-cpu` / `--overcommit-memory`,节点侧算,上报已含系数。实测 `--cpu 8 --overcommit-cpu 3` → allocatable 24。CPU 超了只是变慢,内存超了是被杀,所以内存默认 1.0 —— 抬高它需要先实测 FC 按需供页的富余(#18)并给 VMM 进程加 cgroup(#20) |
| Postgres | ✅ `bean-api --postgres`。这才是多副本的前提:SQLite 是一个文件,两个副本没法共享。做成方言而不是第二套实现,规模是实测出来的 —— 103 处占位符加少数 DDL 构造,八条 `ON CONFLICT` 原样可移植。`hack/postgres-conformance.sh` 对真 Postgres 16 跑全部 requirement;没跑过真库时套件会显式 skip,不会拿 SQLite 挣来的绿报成通过。**光读 SQL 不够** —— 见下文 |
| 创建阶段指标 network | ✅ 网络已实装,`network_setup` 阶段已上报 |

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

overlaybd 需要 ublk（内核 ≥ 6.0)或 tcmu 后端。已接入的那个后端是 **tcmu**
（`target_core_user` + `tcm_loop` 模块）—— 当时验证机是 Ubuntu 20.04 + 内核 5.15,
没有 `/dev/ublk-control`。**tcmu 已实测功能完备**。验证机现在已升到 6.8,
`/dev/ublk-control` 可用;overlaybd 加 `--fc-ublk` 现在可以走 ublk(层格式由本进程
用 Go 读),但那条路**还没上真机**,默认仍是 tcmu —— 见上表
「overlaybd over ublk」行。

**「ublk 只是性能更好」这句已被实测推翻。** tcmu 拆 128 个设备要 4.0 s,
而且在 5.15 和 6.8 上完全一样:daemon 卡在一条上游明确警告不要并发使用的
netlink socket 上。那是传输层的限制不是内核版本的问题,换内核修不掉,
所以 ublk 是目标传输层而不是可选优化(见上表)。

tcmu 路径另需注意宿主上的 `multipathd`:TCMU 设备默认无唯一序列号,
multipathd 会把多个 overlaybd 设备合并成一条 multipath,
**读到的是别的镜像的数据**。必须给每个 backstore 写 `wwn/vpd_unit_serial`。

## 4. 下一步

1. **在真机上量 overlaybd over ublk**:接线已经做完(层格式用 Go 读、`--fc-overlaybd`
   加 `--fc-ublk` 走这条路),但**只有单元测试和交叉编译,没上过硬件**。
   做它的**全部理由**是 TCMU 拆 128 个设备的 4.0 s,而那个数换内核修不掉
   (5.15 和 6.8 一样慢),所以现在缺的就是那个对照数 —— 没量出来就等于收益未证实。
   ublk 单独作为 dm-snapshot 的替代已经量过了:60 并发下 `fc_rootfs`
   2.461 s → 0.034 s、吞吐 18.3 → 101.7 creates/s,零失败零泄漏。
   顺带纠正一个早先写错的价值排序:overlaybd 的收益是**转换 CPU 和磁盘**,
   不是「首次使用一个大镜像的等待时间」—— 冷路径没有变快,
   真正能砍掉它的是 lazy pull,而那需要已经封好的层(且 lazy pull 与 ublk 互斥)。
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

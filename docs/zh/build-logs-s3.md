# 构建日志上 S3 —— 无状态、重启不丢的日志与取消方案

> 状态:✅ **步骤 A、B 均已实现。** 节点把构建日志上传到专用 S3
> 日志桶(`internal/control/s3/buildlog.go`,`BuildLogWriter`),网关按字节偏移无状态
> 读回(`internal/control/api/build.go` 的 `handleBuildLogs`,`BuildLogReader`),取消
> 从记录解析出构建所在节点并调用节点的 `CancelBuild` RPC(`internal/node/grpc.go`、
> `internal/node/buildreg.go`)。内存态 `buildTracker` 已删除。**步骤 B 已切断结果流:**
> 节点的 `BuildImage` 服务端流被移除,改为即发即返的 `StartBuild` + 轮询
> `GetBuildStatus`(节点在自持 context 下跑构建并缓存结果),bean-api 的
> `ReconcileBuilds` 在重启时重新挂接在飞构建。构建现在能扛过 bean-api 重启。按绑定规则,
> **.75 KVM 主机 e2e(§14)已通过——真实硬件上 4 个测试全绿(2026-08-15)**,
> 满足绑定规则;单元测试亦通过。状态标记约定见
> [architecture.md](../architecture.md) §0。
> **权威顺序:代码 > [status.md](../status.md) > [decisions.md](../decisions.md) > 设计文档。**
> English: [../build-logs-s3.md](../build-logs-s3.md)

## 1. 为什么要改

一次构建耗时数分钟,所以 `POST /v1/templates/build` 立即返回 `202`,构建在节点上脱离请求异步进行,调用方通过两个端点跟进:

- `GET  /v1/templates/build/logs?ref=` —— 拉取输出
- `POST /v1/templates/build/cancel?ref=` —— 停止构建

二者今天都由 `buildTracker` 支撑:**单个** bean-api 进程里的内存 `map[ref]*buildLog`,每次构建持有一个 4 MiB 环形缓冲区,以及一个能杀掉构建的 `context.CancelFunc`。节点通过 `BuildImage` gRPC 流把日志帧推上来,`drainBuildStream` 把每帧拷进环形缓冲,`handleBuildLogs` 再从中读出。单副本能用。但一旦有多于一个 bean-api 副本、或发生重启,就暴露三个缺陷:

1. **多副本 404。** `/logs` 和 `/cancel` 请求可能落到任意副本,但缓冲区和 `CancelFunc` 只存在于处理了 `/build` 的那个副本上。其余副本对一个正常运行的构建一律回 `BUILD_NOT_FOUND`。(`docs/build-service.md` §3.5 已点明过。)
2. **重启丢失。** 缓冲区在内存里。构建途中重启 bean-api,日志就没了;`/cancel` 也再够不到构建(`CancelFunc` 随进程一起死了),尽管构建本身还在节点上跑。
3. **双重转发。** 每个日志字节都要走 节点 → bean-api(gRPC)→ 客户端,中途还整段缓在 bean-api 内存里,只受那 4 MiB 窗口约束,而窗口一满就 **丢弃** 早期输出(`handleBuildLogs` 里的 "log truncated")。

修法是:别再让 bean-api 当日志的家。bean-api 不应持有 **任何** 构建状态,而应是一个面向持久存储的无状态读取器——这样任意副本都能服务任意构建的日志,重启也不丢任何东西。

## 2. 参考:E2B 怎么做

E2B(`infra/packages/`)是最接近的可用参考,它的形态就是目标:

- **日志** 写入 **Loki**,打标 `{service="template-manager", buildID=…, envID=…}`。API 按 `buildID` 加一个 `LogsOffset` 查询 Loki 来服务日志(`template_build_status.go` → `lokiClient.QueryRange(...)`)。API 什么都不持有,任意副本都能应答。
- **状态**(Building/Ready/Failed + 原因)存在 **Postgres**(`envbuild.Status`),不在日志存储里。是否终态是一次 DB 读取,而非日志流的属性。
- **取消** 归 **编排节点** 所有:构建跑在节点侧一个 goroutine 里,登记在每节点的 cache 中(`create_template.go`:`go func(){…}()`、`defer buildInfo.Cancel()`),API 通过给运行它的节点发 `DeleteBuild` gRPC 来取消(`template_start_build.go` → `delete_template.go`:`c.Cancel()`)。控制面不持有任何取消句柄。

要学的是这个 **三分**:日志进日志存储、状态进数据库、取消归节点。控制面每个构建什么都不留。

bean 采纳这个三分,但 **不** 采纳 Loki:bean 已经有一等的对象存储契约(`s3.ObjectStore`)在支撑快照 blob 和 overlaybd 层,带 `GetRange`/`Head`,还有开发用的 `DirStore`。构建日志是一段按偏移寻址的追加式字节流——恰好是这个契约所服务的,而范围读正是 `/logs?follow` 所需。引入 Loki 等于为一个对象存储已经装得下的负载,再多运维、加固、推理一套存储系统。所以 bean 的日志存储 **就是 S3**,放在一个专用桶里。

## 3. 决策(用户,2026-08-15)

1. **noded 直传。** 运行构建的节点自己把日志分片写进 S3——而不是 bean-api 转发。这消掉了双重转发(§1.3),再配合决策 2,把构建的生命周期从任何 bean-api 连接上解耦。代价:noded 需要日志桶的写凭据(§9)。
2. **取消轨一起落。** 取消在同一次改动里迁到节点,不拖到以后:持久化构建方 `nodeId`,取消句柄放在节点上,加一个节点 `CancelBuild` RPC(§8)。
3. **专用桶。** 构建日志放自己的 S3 桶(如 `bean-build-logs`),与 blob/overlaybd 桶分开——不同的生命周期(日志会过期;层是内容寻址、长期保留)、不同的保留策略、以及给节点更小的凭据爆炸半径(§9)。
4. **先写设计文档。** 本文在写码前敲定边界:桶内 key 布局、谁来写、无状态读路径、状态存储在 store、节点自持的取消轨,以及移除内存缓冲。

## 4. 架构

```
  bean build ──POST /build──▶ bean-api ──StartBuild──▶ noded  (构建的持有者)
                                 │                        │
                                 │                        ├─▶ buildctl / buildkitd
                                 │                        │
                                 │                        └─▶ S3 日志桶
                                 │                              buildlogs/<key>/NNNNNN
                                 ▼                              buildlogs/<key>/manifest
                          store.Template                        ▲
                          {State, NodeID, BuildID}              │
                                 ▲                              │
  bean build --follow ──GET /logs──▶ 任意 bean-api 副本 ─────────┘ (GetRange/Head)
  bean build cancel ────POST /cancel─▶ 任意副本 ──CancelBuild──▶ 持有该构建的 noded
```

三个存储,bean-api 里没有任何每构建状态:

- **日志 → S3**(专用桶),由 noded 写,由任意 bean-api 副本经 `GetRange`/`Head` 读。
- **状态 → `store.Template`**(`State`、`Reason`,加上记录里 **已存在** 的 `NodeID`、`BuildID`)。store 是"这个构建是否结束、如何结束"的唯一真相源。
- **取消 → 持有该构建的节点。** bean-api 解析 `Template.NodeID` 并发 `CancelBuild(ref)`;节点取消它正在跑的那个构建。

因为构建跑在节点自持的 context 下(登记在节点取消注册表里,而非挂在某条 bean-api 流上),它 **能扛过 bean-api 重启**,且任意副本都能跟进或取消。这正是当前设计缺的性质。

## 5. S3 布局 —— 专用日志桶

一个桶,每次构建一个扁平前缀:

```
buildlogs/<key>/000000        第一个分片(写完即不可变)
buildlogs/<key>/000001        下一个分片
buildlogs/<key>/...
buildlogs/<key>/manifest      小 JSON:{seq, done, failed, reason, updatedAt}
```

- **`<key>` 是构建 ref,经消毒。** ref 就是模板 tag(构建以 tag 为键——见 `build.go` 关于为何不另设 build id 的说明)。它含 `/`、`:`、`@`,所以走 **与 `refToFilename` 相同的方案**(`internal/node/image/file.go:15`):字母数字/`-`/`_` 原样通过,其余变成 `_<hex>`。结果无斜杠、防碰撞、稳定——干净的单个路径段。`refToFilename` 今天在 `internal/node/image` 里未导出;本设计把这个消毒器提取成一个小的共享辅助(如 `internal/control/s3.BuildLogKey(ref)` 或一个 `buildkey` 包),使 **写方(noded)与读方(bean-api)派生出完全一致的 key**,且谁都不必导入对方的包。

- **分片不可变、按序号追加。** 一个分片一次性整段写入,经 `s3.Put`(或 `Writer`+`Close`),此后不再重写——S3 的"`Close` 前不可读"保证意味着读方绝不会看到半个分片。新输出成为下一个 `NNNNNN`。这绕开了 S3 不支持对象追加的限制:不是往一个对象追加,而是新增对象。六位、零填充,字典序 = 时间序。

- **`manifest` 对象是日志存储自带的状态旁路**,很小,随构建推进被覆盖(后写者胜):有多少分片(读方无需 `LIST` 就知道停在哪),以及构建是否结束。**权威** 终态仍是 `store.Template`(§6);manifest 存在,是为了让 *日志读方* 从它所读字节的同一存储里判断"还有没有更多",而不必每次轮询都回控制面 DB 转一圈。二者若不一致,以 `store.Template` 为准。

不用 `LIST`:读方从当前序号起向上 `Head(buildlogs/<key>/NNNNNN)` 直到 `ErrNotFound`,或读 `manifest.seq`。`LIST` 是额外权限,且更慢、最终一致;顺序 `Head` 正是 lazy-pull 已经依赖的同一原语。

## 6. 读路径(bean-api,无状态)

`handleBuildLogs` 不再碰 `buildTracker`。它变为:

1. 按 ref 查 `store.Template`。不存在 → `404`。存在但不是构建(`Source != TemplateBuilt`)→ `400`。
2. 按序号从日志 `ObjectStore` 读分片,以分块 `text/plain` 逐段流给客户端(内容类型与分帧不变——`curl` 和 `bean build --follow` 仍无需解析器即可消费)。
3. **偏移。** 客户端的字节偏移映射到(分片序号,片内偏移);读方 `Head` 得知各分片大小,对首个部分分片 `GetRange` 其尾部,之后按整分片读。这与当前 API 一样按偏移寻址,所以 `--follow` 重连与断点续读照常工作。
4. **跟随。** `?follow=true`(默认):把已知分片排干后,按短轮询间隔重新 `Head` 下一序号 / 重读 `manifest`,直到 manifest(或 `store.Template`)显示终态,再把最后一个分片排干并停止。`?follow=false`:排干现有内容即停。
5. **终态结果写在 body 里**,一如今天:响应在结果已知前就已提交 `200`,所以成功/失败写进 body(`build succeeded` / `build failed: <reason>`),取自 `store.Template`。

没有环形缓冲,所以 **不会有 "log truncated" 断口**——每个分片在桶生命周期规则将其过期前都持久存在(§10)。读方能看到的唯一"截断"是整个构建被桶清掉,那是一个干净的 `404`,而非流中途的缺口。

任意副本都能服务:它什么都不持有,只需日志 `ObjectStore` 句柄和 store。这就是读侧的多副本修复(§1.1)与重启修复(§1.2)。

## 7. 写路径(noded)

节点持有构建及其日志上传。在 `Builder.Build` / `runBuildctl` 中,今天是 gRPC 流发送器的那个 `Logs io.Writer` 变成一个 **S3 分片写入器**:

- 一个 `logUploader` 包住日志 `ObjectStore` 并缓冲 BuildKit 的输出。当缓冲达到大小阈值(如 256 KiB–1 MiB)**或** 经过一小段时间(如 1–2 秒),以先到者为准,就刷出一个分片——这样安静的构建也能显示进度,话痨的构建也不会每行一个对象。每次刷出 `Put` 一个 `buildlogs/<key>/<seq>` 并推进 `manifest.seq`。
- 完成时写入终态 `manifest`(`done`、`failed`、`reason`)并刷出尾巴。
- 那个在构建 **错误** 里点名失败步骤的 40 行尾缓冲(`buildLogTailLines`)保留——它通过另一条路(RPC 的 result/error)到达 bean-api,是让失败在不拉全量日志时也可读的东西。

gRPC 日志帧(`BuildImageEvent.log`)对 **持久化不再必要**。选项见 §8:要么丢弃(节点上传,bean-api 从不见日志字节),要么保留为尽力而为的实时尾巴。本设计选择丢弃——日志一个写入方、一条读路径、没有双重转发。

## 8. 取消轨与 RPC 重塑

两个决策在此富有成效地相撞:**noded 直传**(于是 bean-api 不必持日志流)与 **取消归节点**(于是 bean-api 不必持 `CancelFunc`)。合起来意味着 bean-api 每个构建 **什么都不持有**,进而 `BuildImage` 服务端流——其全部职责就是把日志帧运给 bean-api、其 `ctx` 就是取消机制——已无事可做。于是重塑节点构建 RPC:

- **`StartBuild(BuildImageRequest) → StartBuildResponse{buildId}`** —— 在构建于节点上登记并跑起来时即返回,而非等它结束。节点在自己的 goroutine 里、在一个节点自持的 `context` 下跑构建,该 context 存在一个 **每节点取消注册表** 里、以 ref 为键(对应 E2B 的 `buildInfo`/`buildCache`)。这正是让构建活过任何 bean-api 连接的东西。
- **`CancelBuild(ref) → CancelBuildResponse`** —— 在注册表里查 ref 并取消其 context,这与今天脱离态 ctx 的取消一样杀掉 `buildctl`。取消一个未知/已结束的 ref 不算错(幂等),与对象存储 `Delete` 约定一致。
- **终态结果 —— bean-api 轮询节点。** 仍得有人把 `store.Template` 翻成 READY/FAILED 并带上产物坐标(`overlaybd_ref`、`size_bytes`、`layer_digests`、`config`)。节点把构建结果缓存在注册表里,并暴露 **`GetBuildStatus(ref) → {phase, result, reason}`**;bean-api 的每构建 goroutine 每秒轮询它,直到终态,再执行 `MarkReady` / `MarkFailed`。权威状态的写入留在控制侧(store 是 bean-api 的),节点只上报。这 **正是 E2B 的做法**——其 API 调用即发即返的 `TemplateCreate`,再由后台 `PollBuildStatus` 每秒轮询节点的 `GetStatus` 并在控制侧写 Postgres(`packages/api/internal/template-manager/{create_template,template_status}.go`)。它也是更小的改动:对 `nodesvc` **零改动**——不用注入 `image.Service`、不用 node→控制面结果 RPC、不用 ack——因为 bean-api 本就持有 `s.images` 和控制面→节点的 `SandboxServiceClient`。
  - (弃)节点走 heartbeat 推送 `ReportBuildResult`。节点确有一条已鉴权的 heartbeat,所以可行,但这不是 E2B 的做法,漏推送时仍需 reconciler,且把 `nodesvc` 耦合到模板存储。作为可选的低延迟快路径后置。
- **重启对账。** 重启后的 bean-api 必须重新挂接在飞构建:启动时 `ReconcileBuilds` 列出 `store.Template` 里处于 `BUILDING` 的,对每个在新的 `maxBuildDuration` 界内恢复 `pollBuild(NodeID, ref)`(没有 `NodeID` 的模板直接判失败——无节点持有它)。与在飞构建同一条轮询回路,一套机制两用。构建本身从未停;欠的只是一次状态写入。

**持久化 `NodeID`。** `store.Template` 已经有 `NodeID` 和 `BuildID`(`store/types.go:209`),但 `handleBuild` 今天并没在记录上设 `NodeID`。本设计在 `pickBuilder` 返回后、`StartBuild` 之前/之时设上它,于是任意副本上的 `/cancel` 都能从 store 解析出持有节点。近乎零成本——字段已存在。

### 分两个可验证的步骤落地

重塑是实打实的改动面,所以分两个可 e2e 的步骤,而非一刀切:

- **步骤 A —— 日志上 S3、取消归节点、保留流。** noded 上传日志分片到 S3(§7);`/logs` 从 S3 读(§6);加节点取消注册表 + `CancelBuild`;持久化 `NodeID`;`/cancel` 调节点。把 `BuildImage` 服务端流仅保留为 *结果载体*(bean-api 仍等它拿 result 帧并 `MarkReady`)。这已经修好了多副本 404、**日志** 的重启丢失、以及双重转发——且完全可测。
- **步骤 B —— 切断流(已完成)。** 用即发即返的 `StartBuild` + 轮询 `GetBuildStatus` + `ReconcileBuilds` 重启 reconciler 取代那条常驻的 `BuildImage` 流,使 bean-api 重启连一次可能漏掉的状态写入都不再欠——构建在节点自持 context 下跑,替补副本靠轮询重新挂接。这就是完全 E2B 形态的解耦。

`buildlog.go` 的内存态 `buildTracker`/`buildLog` 在步骤 A 删除;它提供的 `changed` 通道跟随被 S3 `Head`/manifest 轮询取代。

## 9. 凭据与 STS 欠账

noded 上传意味着 noded 需要日志桶的 **写** 凭据。[s3-storage.md](../s3-storage.md) §6 的规则依旧成立:

- **密钥只来自环境变量,绝不用 flag** —— `BEAN_S3_ACCESS_KEY` / `BEAN_S3_SECRET_KEY`(flag 会经 `/proc/<pid>/cmdline` 和 `ps` 泄露)。endpoint/region/bucket 可以是 flag。noded 已经为层存储以这种方式加载 S3 凭据(`cmd/noded/main.go:717`),所以日志桶复用这个模式——一个 `s3.Client`,再对日志桶建第二个 `NewBucketStore`(一个 client 可服务多个桶,如 `objectstore.go` 所述)。
- **专用桶缩小爆炸半径。** 因为日志自成一桶,节点的日志凭据可以只 scope 到那一个桶,且不必与层存储凭据是同一把 key。日志比层价值低,所以泄露日志 key 比泄露 blob key 问题小。
- **已知的现存欠账,本设计不改变但相关:** noded 持有长期 S3 凭据;STS 轮换与预签名 URL 上传尚未实现。本设计 *新增了一个节点要写的桶*,所以略微扩大了这笔欠账,应在 STS 落地时点出——最终形态是 bean-api 铸一个短期预签名 PUT(或 STS 会话)、scope 到 `buildlogs/<key>/*` 交给节点,使节点绝不持有常驻日志凭据。此处不在范围内;记下以免遗忘。

## 10. 保留 —— 桶生命周期规则,而非进程

内存态设计在 30 分钟后过期日志(`buildLogRetention`),因为它不得不这样——日志在 RAM 里。到了 S3,根本没理由把日志留在进程里,也没理由自造过期逻辑。保留是日志桶上的一条 **S3 生命周期规则**,由运维配置(如 N 天后过期 `buildlogs/` 对象)。专用桶(§3.3)正是让这一点干净的原因:规则作用于整个桶,而不碰内容寻址的层 blob——后者 **绝不可** 过期。开发/CI 用 `DirStore`,干脆全留(或用 cron 清目录);bean 里没有生命周期守护进程。

## 11. 配置

- **bean-api**:`--s3-logs-bucket`(或 `BEAN_S3_LOGS_BUCKET`),默认如 `bean-build-logs`。与现有 `--s3-endpoint` 一起设置时,bean-api 为读建第二个 `NewBucketStore`。未设 → 开发用日志目录下的 `DirStore`,与 blob 的回退方式(`snapshot.NewDirBlobs`)一致。
- **noded**:同一个日志桶名(flag/env),节点据此建 `BucketStore` 来写。endpoint/region 复用节点现有的 `--s3-*` flag;只有 bucket 不同。
- 两端都用共享的 `BuildLogKey(ref)` 辅助(§5)派生 key,使写方与读方逐字节一致。

## 12. 迁移 / 每一块变成什么

| 今天(`buildlog.go` / `build.go`) | 变成 |
|---|---|
| `buildTracker map[ref]*buildLog`(内存) | **删除** —— bean-api 里没有每构建状态 |
| 4 MiB 环形缓冲 + `maxBuildLogBytes` | S3 分片对象,无窗口、不丢弃 |
| `buildLogRetention`(30 分,进程内) | S3 桶生命周期规则(§10) |
| `changed` 通道(免轮询跟随) | `/logs` 里 `Head`/`manifest` 短轮询 |
| `log.cancel()` = 本地 `CancelFunc` | 向 `Template.NodeID` 发 `CancelBuild(ref)` RPC |
| `drainBuildStream` 等结果帧 | **删除** —— 节点上传分片;`pollBuild` 轮询 `GetBuildStatus`(步骤 B) |
| `handleBuildLogs` 读环形缓冲 | 读 S3 分片(无状态) |
| `handleBuildCancel` 调 `log.cancel()` | 解析 `NodeID`,调节点 RPC |
| 构建时 `Template.NodeID` 未设 | 在 `pickBuilder`/`StartBuild` 时设上 |

Proto:步骤 A 加了 `CancelBuild`;步骤 B 用 `StartBuild` + `GetBuildStatus` 取代 `BuildImage` 服务端流,并退役 `BuildImageEvent` 消息(保留 `BuildImageResponse`,复用在 `GetBuildStatusResponse` 里)。重新生成 `internal/gen`。

## 13. 文档标记修正(先做)

`status.md:54` 仍写着 `| Build logs and cancellation | ⚠️ | A build reports no progress and cannot be stopped |`。这有两重陈旧:日志/取消其实已经实现(单副本),而本设计要改的是"如何做"。把它更新为反映真实状态——已实现但单副本/内存态,并以本文为多副本方案——而非"无进度、无法停止"。同时对齐 `image-build.md` 和 `build-service.md` §3.5(后者已预言了多副本裂缝),让它们指向本文。

## 14. e2e(需要真实 KVM 主机 —— 绑定规则)

单元测试(对 `DirStore` 的 S3 分片、写读两端 key 派生一致性、偏移/跟随读取逻辑)必要但 **不充分**——每个阶段都在 `.75` KVM 主机上验证(见存储收敛的绑定规则;`docs/bean-75` 主机怪癖:buildctl 在 PATH 上、`docker.m.daocloud.io` 镜像、`vhost_vsock`)。e2e 证明:

e2e 在 `tests/e2e/buildlogs_test.go`(build tag `e2e`,未设 `BEAN_S3_ENDPOINT` 时跳过),经 `hack/buildlogs-e2e.sh <凭据env文件>` 运行。harness 用 `--runtime fc`(带 firecracker/kernel/agent-disk 资产),**不是** `--runtime local`:只有 fc tier 实现了 `runtime.ImageBuilder`(`internal/node/runtime/fc_linux.go`),local 节点根本不能构建(`runtime local cannot build images`)。构建本身不启动 microVM——它调 buildkit——但 `NewFCTier` 构造仍需 `/dev/kvm` 和这些资产。2026-08-15 已在真实硬件通过(4 个全绿,`ok tests/e2e`):

1. 在 KVM 主机上用真实 `--s3-logs-bucket`(MinIO)构建一个模板。确认分片对象落在 `buildlogs/<key>/NNNNNN`,`manifest` 在推进。(`TestBuildLogsLandInS3`)
2. 从 **第二个** bean-api 副本跟随日志:输出连续、无 `BUILD_NOT_FOUND`、无 `[log truncated]` 断口。这就是多副本读修复的实证。(`TestBuildLogsServedFromOtherReplica`)
3. 从并未发起该构建的副本执行 `bean build cancel`:构建停止(节点上 buildctl 死掉),`store.Template` 转 `FAILED`,ref 释放可重试。(`TestBuildCancelFromOtherReplica`)
4. **在构建途中杀掉发起该构建的 bean-api,并在同一端口拉起替补:构建在节点上继续跑,替补的 `ReconcileBuilds` 把模板推到 `READY` 且带真实 `overlaybd_ref`/`layer_digests`。** 这是步骤 B 的重启存活性质。(`TestBuildSurvivesReplicaRestart`)
5. 确认日志桶生命周期规则把旧构建的日志过期成干净的 `404`,而另一个桶里的层 blob 毫发无损。(手动 / 运维配置,不在自动化套件内)

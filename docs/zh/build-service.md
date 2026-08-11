# 镜像 build 该拆成独立服务吗?

> 状态:📐 **设计 / 讨论。** 这里暂不提议改代码。评估的是:镜像 build 路径是否该从 noded
> 里拆出、成为独立服务,拆能带来什么,以及今天真正卡在哪。权威顺序成立:代码 > `status.md`
> > `decisions.md` > 设计文档 > 本页。build *做什么*见 [image-build.md](image-build.md);
> 这里谈的是它*该在哪跑*。

> English: [../build-service.md](../build-service.md)

---

## 0. 问题

四个二进制是 `bean`(CLI)、`bean-api`、`noded`、`beand`。build 没有自己的节点 —— 那它在
哪跑,又该在哪跑?今天 build **在 noded 内部**执行,和服务 `create`/`exec` 的是同一个进程,
在 sandbox 旁边驱动一个 `buildkitd`。问题是:这种耦合是否该拆成一个独立的 build 服务。

先给出诚实的答案:**build 在"便宜的那些方面"已经解耦了,而在"要紧的那一个方面"仍然耦合。**
挡住一次干净拆分的不是调用图 —— 而是:build 出的镜像从不离开构建它的那台节点。

## 1. 今天 build 怎么跑

```
CLI/SDK ──► bean-api (build.go) ──gRPC 流──► noded ──► buildctl ──► buildkitd
                │ pickBuilder                 │ ImageBuilder            │
                │ (label 优先,否则任意)       │ (每节点可选)            │
                └─ 镜像标记 BUILDING           └─ 产物:node-local ImageDir 里的扁平 ext4
```

- **入口**:`handleBuild`(`internal/control/api/build.go:57`)解码 context tar(64 MiB 上限),
  把镜像记为 `ImageBuilding`,然后选节点。build 随后在后台以 `context.Background()` + 60 分钟
  上限运行,立即返回 `202`;日志与取消是按 image ref 索引的两个端点(`build.go:289`、`:358`)。
- **选节点已经独立于调度器。** `pickBuilder`(`build.go:148`)优先选带 `pool=builder` label 的
  Ready 节点,否则取任意 Ready 节点 —— 它完全不查 `scheduler.go` 里 create 路径的亲和打分。
- **执行**:noded 的 `BuildImage` RPC(`grpc.go:143`)是长活 server-stream;`Manager.BuildImage`
  (`manager.go:833`)类型断言 `runtime.ImageBuilder` 再委派。`image.Builder`(`build_linux.go:29`)
  shell 出 `buildctl` 连到 `buildkitd` socket。
- **buildkitd 是每节点可选依赖。** `--buildkit-addr` 默认空(`cmd/noded/main.go:81`);只有 fc 档
  装配 `Builder`,且仅当地址非空(`fc_tier_linux.go:215`)。地址为空的节点不接受 build。
  `pool=builder` label 已经让集群可以跑专用构建节点。

所以*执行*这条缝是干净的:一个可选接口、选节点已从调度分离、外加一个专用构建节点的 label 机制。

## 2. 真正耦合的是什么

三处真实耦合,按"挡拆分"的程度降序:

1. **产物是节点本地的,且从不上传。** 一次 build 把扁平 `.ext4` 落进该节点的 `ImageDir`
   (`build_linux.go:89`),被同一节点的 rootfs provider 直接读(`rootfs.go:194`)。`MarkReady`
   传的是空 overlaybd ref、size 0(`build.go:241`)—— 代码注释很直白:READY *夸大了可达范围*,
   该镜像"只存在于构建节点的 ImageDir 里、从不上传,所以别的节点无法从它启动"
   (`build.go:236-240`)。**这是硬阻塞。** 一个集中式 build 服务若产出的镜像没有 sandbox 节点
   能消费,就是没用的。
2. **build 与 sandbox 共享节点,且零资源隔离。** build 和 `create`/`exec` 在同一个 noded 进程、
   同一个 `Manager`(`manager.go`)里,`buildkitd` 抢同一台主机的 CPU、磁盘、IO。没有并发上限、
   没有限流、没有 cgroup —— 唯一护栏是 60 分钟超时(`build.go:143`)。一个重 build 能饿死同机的
   sandbox 启动。
3. **只有 fc 档能 build。** `OCIRuntime` 不装配任何 `Builder`(`oci_tier_linux.go` 把
   `BuildkitAddr` 透传但从不组装),所以在 OCI 档节点上 `m.rt.(runtime.ImageBuilder)` 断言失败。

## 3. 分发机制已经存在 —— 只是服务于别的镜像

bean 已经在节点间分发*导入的*镜像,build 只是还没走这条路:

- **S3 blob store**:`obdblobstore.go` 把 sealed overlaybd layer 按 OCI digest 为 key 推进
  S3 兼容 bucket(`:141`),overlaybd 守护进程匿名 range 读(`:166`)。seal 能力也已存在
  (`obdbuild_linux.go`)。
- **prewarm 才 publish,create 从不**(`status.md:55`):prewarm 转换镜像并把每个 sealed layer
  推到对象存储,任何读该 store 的节点都远程解析这些 layer 而非本地转换。
- **镜像亲和调度**:调度器给已缓存某镜像的节点加分(`scheduler.go:324`,`ImageAffinity:10`),
  数据来自心跳上报的 `CachedImages`。

要点:"seal → 推 S3 → 其他节点 range 读"这一跳**已经为导入/overlaybd 镜像建好并 shipped**。
build 产物只是没走它 —— 停在了本地 ext4。补上这个缺口用的是同一套机制,而 `build.go:238-240`
已经为此预留("上传可以之后再落")。

## 3.5 第二个、独立的问题:日志流

暴露 build 老化的不止分发。build 日志今天走**双重中转** —— noded 经 gRPC 把日志推给 bean-api,
bean-api 缓冲后再回吐给客户端 —— 而这个缓冲是**进程本地内存**:

- `s.builds` 是一个用 mutex 保护的 `map[string]*buildLog`(`buildlog.go:42-49`),上限 4 MiB、
  30 分钟,从不落 DB 或 S3。noded 也不留存 —— 每帧流一次、什么都不留(`node/buildlog.go:9-16`)。
- **这是真实的多副本裂缝。** bean-api 对着 Postgres 跑多副本。一次 build 落在副本 A,它的日志
  缓冲在 A 的内存里;客户端把 `GET .../build/logs?ref=` 或 `POST .../build/cancel?ref=` 打到
  副本 B,`s.builds.get(ref)` 查的是 B 自己的 map,miss,返回 404(`build.go:295`、`:364`)——
  尽管镜像的*状态*在 Postgres 里从任一副本都可见。日志和取消是一个本该水平扩展的系统里的副本
  本地状态,进程重启还会丢。

**能不能不中转、走 node-direct?** 本次盘点纠正了一个常见误解:exec 和文件传输今天*也*经
bean-api 中转(`cli.go:321` → `handleExec` → `router.Client`),尽管 README 暗示不是 —— 所以
build 在这点上并不特殊。真正 node-direct 的数据面(bean-proxy 和 noded 的 `PortForwarder`)
完全按 **sandbox id** 寻址(`ParseSandboxHost` 拒绝空 sandbox 段;`TargetFor` 查 `m.sandboxes`),
而 build 没有 sandbox —— 它跑的是 buildkitd。noded 侧也**没有按 ref 的 build 日志端点**,只有
那个一次性、不留存的 `BuildImage` stream。

所以 node-direct 化的 build 日志需要两样新东西:①一个 noded 侧按 build-ref 留存、可重连的端点;
②一个不是 sandbox id 的寻址 key。两条数据面里,**bean-proxy 的模型更贴合** —— 它已经在做
"id → node 地址 → node 上的端点",所以增量是把 id 空间从 sandbox 扩到 `build-{ref}`,而不是
从零造一条 client→noded 直连。但注意顺序:**多副本裂缝是正确性 bug**,本身就值得修(比如按 ref
做 sticky routing,或共享/DB 支撑的日志),与日志是否走 node-direct 无关。

## 4. 前置条件,以及它为何排在最前

**无论拆不拆,产物都必须变成可分发的。** 像 prewarm publish 一个导入镜像那样 publish build
结果:不再由 `writeBaseImage` 落一个本地 ext4,而是把 rootfs seal 成 overlaybd layer、
`BlobStore.Put` 到 S3、用真实 overlaybd ref 调 `MarkReady`。之后任何节点都能 range 读它,
和导入镜像一模一样。

这一个改动本身就比拆分更值:

- 它让 build 出的镜像全集群可用 —— 正是 `build.go:236` 说今天缺的东西。
- 它让 build 的*位置*变得无关紧要。产物一旦进了 blob store,它是在 noded 内、在 `pool=builder`
  节点上、还是在独立服务里产出的,就成了运维选择,而非架构问题。

所以顺序是刻意的:**先修分发,再决定是否拆** —— 因为分发修好之后,拆分就是一个部署决策,
而不是正确性决策。

## 5. 修好分发之后,拆分的两种形态

**形态 A —— 专用构建节点(小步)。** build 仍留在 noded 里,但只在带 `pool=builder` label 的
节点上跑 —— 这正是 `pickBuilder` 已支持的(`build.go:148`)。产物进 blob store(§4)。这把 build
从 sandbox 热路径上移开,基本不引入新组件 —— 它是调度与部署策略,不是新二进制。

- **利**:复用一切;没有新服务要运维;`pickBuilder` 和 label 机制已存在。
- **弊**:仍是 noded 二进制及其假设;构建节点上的资源隔离是粗粒度(整节点),不是每 build。

**形态 B —— 独立 build 服务(大步)。** 一个 `bean-build` 服务(或某个二进制的一种模式),它
拥有 buildkitd、暴露 build RPC、只写 blob store —— 从不写本地 `ImageDir`。bean-api 的
`pickBuilder` 变成"路由到 build 服务"。

- **利**:build 容量独立于 sandbox 容量扩缩;干净的资源与故障隔离;buildkitd 不再是每 noded 依赖。
- **弊**:一个新二进制和部署面;真正的工作量是把 `image.Builder` 对本地 `ImageDir` 的假设
  (`build_linux.go`)迁移到 blob-store writer —— 而这正是 §4 本来就要做的。

形态 B 的接口缝已经很窄:`runtime.ImageBuilder` + `runtime.BuildRequest`(带 `Logs io.Writer`
流,`runtime.go:294-320`),真正的逻辑在 `image.Builder`,只依赖一个 buildctl 地址和两个本地
目录。要动的是产物那一侧。

## 6. 建议

**先别拆。先做分发修复(§4),然后倾向形态 A。**

理由:

- **分发缺口是真正的问题**,而且它无论拓扑如何都挡着 build 的可用性。它本身就值得做、且能解锁
  其余一切。它复用已 shipped 的机制(seal + blob store + prewarm publish),是风险最低、价值
  最高的一步。
- 之后,**形态 A** 用已存在的 `pool=builder` label 把 build 从 sandbox 热路径移开,不引入新
  二进制。对一个重心在 microVM 热路径的项目,这以零头的代价拿到了大部分收益(隔离重 build 负载)。
- **形态 B**(真正的独立服务)只有当 build 量大到"独立扩缩 + 干净故障隔离"能抵得上一个新二进制
  和部署面时,才划算。在那之前它是投机 —— 而且因为接口缝已经很窄、§4 又已经做了产物侧的硬迁移,
  推迟它代价很小。

一个值得顺带收拾的清理:**OCI 档根本不能 build**(`oci_tier_linux.go` 从不装配 `Builder`)。
如果 build 应该跨档通用,这是个要补的缺口;如果 build 是有意只给 fc,文档该说清楚,而不是把
`BuildkitAddr` 接进来却不用。

以及本次盘点顺带发现的一处文档不准:build 的**日志流与取消其实已实现**(`build.go:289`、
`:358`、`grpc.go:143`),但 [image-build.md](image-build.md) §6 仍标 ⚠️ 未实现。这应单独修正。

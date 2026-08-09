# Image Build 设计

> 用户可以在平台侧构建镜像，而不只是引用已有的 OCI 镜像。
> 状态标注约定见 [architecture.md](architecture.md) §0。
> 术语沿用 [api-design.md](api-design.md) §3.5：`ref` 是用户唯一接触的标识。

## 1. 为什么需要

bean 的立足点是「任意 OCI 镜像零转换直启」，所以不需要 e2b 式的 per-image
template build。但完全没有构建能力留下两个真实缺口：

- **加依赖无处可去**：用户想在 `python:3.12` 上装 `requirements.txt`，只能自己
  维护一个外部 registry，或者每次 sandbox 起来后重复安装
- **「装好环境再复用」只能靠 snapshot**：snapshot 绑定 runtime 档、不可跨档，
  而镜像是通用的（见 §4 的区分）

## 2. 镜像的两种来源 ⚠️

`store.Image.Source` 区分它们，因为转换代价完全不同：

| Source | 来源 | 转换 | 状态流转 |
|---|---|---|---|
| `imported` | 用户给的 OCI ref | tar.gz layer → ext4 镜像/块设备，需 convertor | `PENDING → CONVERTING → READY` |
| `built` | 平台构建 | 见下 | `BUILDING → CONVERTING → READY`（commit 路径跳过 CONVERTING） |

**关于「built 是否零转换」——取决于构建路径**，这点必须说清，否则会误判成本：

| 构建路径 | 产物格式 | 转换 |
|---|---|---|
| **BuildKit**（Dockerfile / steps） | 标准 OCI layer | **仍需一次转换** |
| **commit**（提取运行中 sandbox 的可写层） | ⚠️ 当前是 **dm-snapshot 的 CoW 层**,不是 overlaybd LSMT | 把 `/dev/mapper` 上的合成设备读成一个新的 base ext4 镜像。overlaybd 接入后可改成 `overlaybd-commit` seal,届时才是真正的零转换 |

即便 BuildKit 路径要转换，相对 e2b 仍是改进：转换发生在 **build 时**（一次、
可缓存、不在用户等待路径上），而 e2b 是 build 完再花 5–15 分钟转 VM rootfs。

## 3. 三种构建形式

底层统一：**在容器里按序执行步骤，然后把结果 commit 成层**。差别只在如何描述步骤。

### 3.1 Dockerfile（完整语义）✅

用 **BuildKit** 而非自研 parser。COPY/ADD 语义、multi-stage、ARG 插值、
构建缓存、`.dockerignore`、heredoc 这些加起来是数月工作量且注定不完整；
e2b 与 Daytona 同样用 BuildKit。

```
bean build -f Dockerfile -t myteam/eval-base:v1 .
```

CLI 打包 build context（受 `.dockerignore` 约束）上传，**平台侧执行构建**——
用户无需本地装 Docker，且构建缓存在平台侧共享。

### 3.2 声明式 steps（Modal 风格）📐

eval 编排方本来就在写 Python，链式声明比维护 Dockerfile 更顺手，且每步天然
是一个缓存键：

```python
img = (client.images.build("myteam/eval-base:v1")
       .from_("python:3.12")
       .pip_install("-r", "requirements.txt")
       .run("apt-get update && apt-get install -y git")
       .env(PYTHONUNBUFFERED="1")
       .workdir("/app")
       .submit())
```

SDK 把链式调用编译成 §5 的 build plan；服务端不区分它来自 Dockerfile 还是 steps。

### 3.3 commit（快照当前 sandbox 为镜像）✅

「先交互式装环境，再固化成镜像」——探索性工作流的最短路径，且零转换：

```
bean commit sbx_abc -t myteam/explored:v1
```

## 4. build image 与 snapshot 的区别 ✅

两者共用「提取 sandbox 可写层」的机制，但**不是同一种东西**，混淆会让数据模型
和用户心智都乱掉：

| | snapshot | built image |
|---|---|---|
| 内容 | 文件系统 **+ 内存/设备态**（fc 档） | 只有文件系统 |
| 跨 runtime 档 | ❌ 格式绑定产出它的档 | ✅ 就是个镜像层 |
| 用途 | 把**一个** sandbox 的精确状态(含进程树)克隆成任意多个新 sandbox | 作为**别人的**基础镜像 |
| 标识 | `snap_...`，引用计数 + TTL 回收(是计数,因为一份快照可以同时被 restore 多次) | `ref` + digest，像镜像一样被引用 |
| 典型场景 | 「装好环境 → fan-out N 个实验」 | 「团队共用的 eval 基础镜像」 |

## 5. Build Plan：统一中间表示 ⚠️

> `store.BuildPlan` / `BuildStep` 类型已定义 ✅,但只有 `dockerfile` 与 `commit`
> 两种 kind 走通;`steps` kind 的编译器未实现。per-step cacheKey 字段存在但
> 未被使用。


三种形式都编译成同一个 plan，服务端只认 plan。这样加新前端（如 Bazel、
Nix）不动执行器，换执行器（BuildKit → 自研）不动 API。

```go
type BuildPlan struct {
    From      string       // 基础镜像 ref（imported 或 built 都可以）
    Steps     []BuildStep  // 有序
    Tag       string       // 产物 ref
    Env       map[string]string
    Workdir   string
    // Dockerfile 路径 + context digest；steps 形式为空
    Dockerfile      string
    ContextDigest   string
}

type BuildStep struct {
    Kind string  // run | copy | env | workdir | user
    // CacheKey 是 (前序步骤链 + 本步内容) 的哈希：内容寻址缓存的依据，
    // 也是 Modal 式「每步自动缓存」的实现方式
    CacheKey string
    Run  string
    Copy *CopyStep
    ...
}
```

## 6. API

> 构建的**日志流与取消**已实现:日志端点(`build.go:289`)与取消(`build.go:358`)
> 走 noded 的长活 `BuildImage` 流(`grpc.go:143`)。仅剩一个注意点 —— 日志缓冲是
> 每副本进程内内存(`buildlog.go`),所以多副本 bean-api 下,logs/cancel 请求必须打到
> 发起该 build 的那个副本;见 [build-service.md §3.5](build-service.md)。


```
POST /v1/images/build      Dockerfile 或 steps → 202 { buildId }
     { "tag": "...", "from": "...", "steps": [...],
       "dockerfile": "...", "contextRef": "..." }
POST /v1/images/build/{id}/context   上传 build context（tar）
GET  /v1/images/build/{id}           状态、日志位置、产出 digest
GET  /v1/images/build?label=          列表
POST /v1/images/build/{id}/cancel
POST /v1/sandboxes/{id}/commit  { "tag": "..." } → 202 { imageRef }
```

Build 状态机：`PENDING → RUNNING → CONVERTING → READY | FAILED | CANCELLED`
（commit 路径跳过 CONVERTING）。日志按 build 落存储，可流式查看。

## 7. 执行位置 ⚠️

构建跑在**节点上**（与 sandbox 同池，或标记 `pool=builder` 的专用节点），
理由：

- BuildKit 需要 containerd/OCI 环境，节点上本来就有
- 构建缓存与镜像块缓存共享本地盘，同一批 eval 的重复构建命中率高
- 调度器已有的 labels/nodeSelector 机制直接复用，无需新的编排层

`noded` 的 `BuildImage` RPC ✅ 已实装(BuildKit,`--buildkit-addr` 为空则该节点
不接构建)。⚠️ 构建产物当前落**节点本地 ImageDir** 作为 base ext4,
未 push 到 S3 blob —— 所以构建出的镜像目前只在构建它的那个节点可用。
原计划:push 到平台 S3（overlaybd blob），
元数据回写控制面。

## 7.5 Dockerfile 构建的实现细节 ✅

`internal/node/image/build_linux.go`。调用 buildctl 而不是链 BuildKit 的 Go 客户端:

```
buildctl --addr <buildkitd> build
  --frontend dockerfile.v0
  --local context=<dir> --local dockerfile=<dir>
  --output type=tar,dest=<out.tar>
  [--opt build-arg:K=V ...]
```

**为什么 `type=tar` 而不是 `type=image`**:base 镜像在我们这里就是一个扁平文件系统
(要 mkfs 成 ext4)。导出 image 意味着 BuildKit 组装出分层结构,我们再把它压平 ——
多一步且没有收益。`type=tar` 直接给出压平后的内容。

**stdout 与 stderr 合并收集**:BuildKit 把进度写 stderr,失败时那里面才有
「哪一步失败了」。所以两路都收进同一个 buffer,失败时取尾部 40 行 ——
全量输出对一个构建可能是几千行,而有用的信息在末尾。

**大小估算比转换那边准**:构建输出的 tar 是未压缩的,所以它的大小就是内容大小
(对比镜像转换只有压缩层大小可用,要 × 3 估,见 image-pipeline §2)。
仍要留 headroom 给文件系统开销与 sandbox 后续写入。

**`--frontend dockerfile.v0` 意味着完整 Dockerfile 语义** —— 多阶段构建、
`COPY --from`、缓存挂载都是 BuildKit 自己的能力,我们不解析 Dockerfile。
这是选 BuildKit 而不是自己实现构建的全部理由。

### cacheKey 字段存在但未使用 ⚠️

`store.BuildStep.CacheKey` 已定义,设计意图是「hash 前序步骤链 + 本步内容」,
让未变的前缀复用缓存。**当前没有代码计算或使用它** —— Dockerfile 构建的缓存
完全由 BuildKit 自己管(它有自己的内容寻址缓存),而 `steps` 形式未实现,
所以还没有需要我们自己算 cacheKey 的场景。

## 8. 不做（明确边界）

- **push 回外部 OCI registry**：built 镜像只在 bean 内部可用。反向转换或双格式
  存储成本明显更高，且当前场景（内部 eval）不需要。若将来要，影响的是 blob
  布局，需要重新设计
- **build 期间的任意网络访问策略**：沿用 sandbox 的 `egress-only`，不单独开口子
- **跨 region 构建编排**：built 镜像的 blob 复制走与 imported 镜像相同的路径（D11）

## 9. 与竞品对比

| 平台 | build 定义 | 执行 | 产物 |
|---|---|---|---|
| e2b | Dockerfile | BuildKit → 转 VM rootfs | template（5–15 分钟/个） |
| Daytona | Dockerfile / Declarative Builder | BuildKit | snapshot |
| Modal | Python 链式调用 | 自研构建器（要求镜像内有 Python） | 内容寻址层 |
| **bean** | Dockerfile ✅ / 声明式 steps 📐 / commit ✅ | BuildKit（平台侧）✅ / dm-snapshot CoW 读出 ⚠️ | ⚠️ 当前落节点本地 ext4;overlaybd layer on S3 是目标 |

bean 的差异：三种形式统一到一个 plan;commit 路径零转换;产物直接是 fc 档可用的
块设备格式，不需要再转一次。

# S3 存储层设计

> 状态标注约定见 [architecture.md](architecture.md) §0。
> 实现:`internal/control/s3/`(协议层)、`internal/control/snapshot/`(存储抽象)。

S3 是平台的统一持久化后端 —— 快照 blob 在这里,镜像 blob 与产物按设计也应该在这里。
本文讲**协议层怎么自己实现的**,以及**上层抽象为什么长这样**。

## 1. 为什么不引 AWS SDK ✅

`aws-sdk-go-v2` 拉进来是几十个模块、上百个传递依赖,而我们用到的是
GET / PUT / DELETE / HEAD 加分片上传 —— 五个操作。

代价是要自己处理 SigV4 的细节,而那不是「算个 HMAC」那么简单:
**兼容性几乎总是丢在规范化(canonicalisation)那一步**,尤其是对非 AWS 实现
(MinIO、Ceph RGW、各家云的 S3 兼容层)。所以 `sign.go` 的注释里写着:
算法是 AWS 完全指定的,这里的价值在于把规范化做对。

收益不只是依赖体积。自己实现意味着:
- 请求路径上没有隐藏的重试、连接池、区域解析逻辑 —— 出问题时看得见全部
- 可以精确控制哪些 header 参与签名(见 §2.2),这是对接非 AWS 实现时最需要调的地方

## 2. SigV4 的实现要点 ✅

### 2.1 签名密钥的四层派生

```
kDate    = HMAC("AWS4" + secretKey, dateStamp)
kRegion  = HMAC(kDate, region)
kService = HMAC(kRegion, "s3")
kSigning = HMAC(kService, "aws4_request")
```

每层都窄化一次作用域,所以泄漏一个 `kSigning` 只能在那一天、那个 region、
那个 service 里用。这是 SigV4 设计上的好处,也是为什么不能简化成一次 HMAC。

### 2.2 只签我们控制的 header

```go
if lower == "host" || lower == "content-type" || strings.HasPrefix(lower, "x-amz-") {
```

签的 header 越多,请求在传输途中被中间层改动就越容易签名失效
(代理加 `X-Forwarded-For`、加 `Via`、规范化 `Accept-Encoding` 都很常见)。
只签 `host`、`content-type` 和 `x-amz-*` —— 前两个是语义必需,后者是我们自己加的。

规范化的三个细节,任何一个错了都是签名不匹配而不是清晰的报错:

- **名字小写、按字典序排、去重**。Go 的 `http.Header` 是 canonical-MIME 形式
  (`X-Amz-Date`),必须转小写
- **值 `TrimSpace`**。服务端会 trim,客户端不 trim 就对不上
- **`host` 可能不在 `req.Header` 里**。Go 把它放在 `req.Host` 字段,
  所以要单独补进列表并从 `req.URL.Host` 取值

### 2.3 空 body 也要给 payload hash

```go
emptyPayload = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
```

这是空字节串的 SHA-256。S3 **要求** `X-Amz-Content-Sha256` 存在,
哪怕 body 是空的 —— 省掉这个 header 的表现是签名不匹配,而不是「缺少必需 header」。
硬编码这个常量比每次算一遍空 hash 更直白。

### 2.4 canonical URI 与 query

- URI 用 `req.URL.EscapedPath()`,空则补 `/`。用未转义的 path 会在
  key 含空格或中文时失败
- query 用 `req.URL.Query().Encode()` —— Go 的实现恰好满足 SigV4 要求的
  「按 key 排序、key 与 value 各自转义」

### 2.5 时钟偏移

签名带 `X-Amz-Date`,服务端通常容忍 ±15 分钟。**没有做时钟校正** ——
宿主时钟偏移超过 15 分钟时请求会被拒,报的是 `RequestTimeTooSkewed`。
这个决定是刻意的:偏移这么大的机器有更严重的问题(TLS、租约、日志时序),
在 S3 客户端里补偿只会掩盖它。

## 3. 分片上传 ✅

```go
// S3 requires at least 5 MiB for all but the final part; 16 MiB keeps the
// part count low without holding much memory.
const DefaultPartSize = 16 << 20
```

**为什么是 16 MiB 而不是 5 MiB**:分片数有上限(10000),5 MiB 的分片意味着
单个对象最大 50 GB;16 MiB 给到 160 GB,而内存占用只是一个分片的缓冲。
下限 5 MiB 是 S3 的硬性要求(最后一片除外),所以不能更小。

**为什么不流式签名(`STREAMING-AWS4-HMAC-SHA256`)**:那需要在每个 chunk 前
写签名头,而我们的写入方是 `io.Writer`(快照 bundle 直接流进来)。
分片上传用同样的代价换来了更简单的实现:每片独立签名,复用单发路径的 `signer`。

### 失败必须不留可读的半成品

这是 `Blobs` 接口的契约,也是分片上传的主要复杂度来源:

```go
// Abort discards the upload and its parts, so a failed write does not leave
// a partial object.
func (u *Uploader) Abort()
```

分片上传天然满足这条 —— **对象在 `CompleteMultipartUpload` 之前不存在**。
中断的上传留下的是不可见的分片,靠 bucket lifecycle 规则回收
(这一条需要运维配置,否则会积累计费)。

## 4. Blobs 抽象:为什么是这四个方法 ✅

```go
type Blobs interface {
    Writer(id string) (io.WriteCloser, error)
    Reader(id string) (io.ReadCloser, error)
    Size(id string) (int64, error)
    Delete(id string) error
}
```

**`Delete` 删不存在的 blob 不是错误。** 清理路径必须幂等 ——
快照记录删了但 blob 删失败、重试时 blob 已不在,这不该报错。
把「不存在」当成成功,清理逻辑就不需要区分「删掉了」和「本来就没有」。

**`Reader` 缺失时返回 `ErrBlobNotFound`(而非通用错误)。**
上层要据此返回 404 `SNAPSHOT_DATA_MISSING` —— 记录在但数据没了,
这和「快照不存在」是不同的故障,运维处理方式也不同。

**`Abort` 不在接口里。** 用类型断言:

```go
func AbortWrite(blobs Blobs, id string, w io.WriteCloser) {
    if a, ok := w.(Aborter); ok {
        a.Abort()
        return
    }
    _ = w.Close()
    _ = blobs.Delete(id)
}
```

理由:`DirBlobs` 的「abort」就是删掉临时文件,而它的 `Close` 本来就要 rename ——
两者是同一个动作的两面,硬塞进接口会让本地实现多一个只为满足接口而存在的方法。
断言让**能做得更好的实现**(S3 的 `AbortMultipartUpload` 一次调用清掉所有分片)
走快路径,其余走「关掉再删」的通用回退。

### 本地目录实现与 S3 的等价性

`DirBlobs` 写临时文件 + `rename`,S3 走分片提交。两者提供同一条保证:
**在写入完成之前,读方看不到任何东西**。所以 dev 环境用本地目录、
生产用 S3,上层代码与测试完全不需要区分。

## 5. range 读 ✅

`Client.GetRange` 是两条路径的基础:

- **快照 restore**:虽然当前是整份 bundle 流式读,但 `snapCache` 命中后
  只需要 rootfs 那个 member —— 拆成独立对象后就能只读需要的段
  (这是 restore 剩余 ~950ms 的优化方向,见 status.md)
- **overlaybd lazy-pull**:整个机制就是块级 range 读。已实测 8 个 HTTP 206
  就能挂载并读文件(decisions.md §3.1)

## 6. 凭证 ⚠️

**secret 只从环境变量读,endpoint 可以是 flag**:

```
--s3-endpoint(或 BEAN_S3_ENDPOINT)   # 非敏感,两种都行
BEAN_S3_ACCESS_KEY                    # 仅环境变量
BEAN_S3_SECRET_KEY                    # 仅环境变量
```

这个区分是刻意的:flag 会出现在 `/proc/<pid>/cmdline`,任何本地用户 `ps`
就能看到,所以密钥没有对应的 flag。endpoint 不敏感,给 flag 方便调试。

环境变量也不是强防护(`/proc/<pid>/environ` 同样可读,只是限制在同 uid),
但至少不在 `ps` 的默认输出里,也不会被记进 shell history。

**noded 在 `--fc-overlaybd` 下直连 S3**(`grep -rn BEAN_S3 cmd/noded/` 现在有 5 处命中)。
它从 `BEAN_S3_ACCESS_KEY` / `BEAN_S3_SECRET_KEY` 构造 S3 client
(`cmd/noded/main.go` 的 `s3.New(...)` → `NewS3BlobStore(...)`),由此得到的
`OverlaybdBlobs` store 发布并 range 读 sealed layers(`internal/node/image/obdblobstore.go`)。
dm-snapshot 路径的快照 blob 仍走 节点 → gRPC → gateway → S3,但 overlaybd 下的节点侧
S3 访问已经是真实存在的,随之而来的凭证管理需求也是真实的。

### 已知缺口 📐

设计里(security-and-startup §A5)节点在需要直传时**不应持有长期凭证**:

- **presigned URL** 未实装 —— 节点上传产物、sandbox 内直传产物都应该用控制面
  签发的、绑定 key 前缀与 content-length 的短时 URL
- **STS 只读角色轮换**未实装 —— 节点已经在 `--fc-overlaybd-lazy-pull` 下直接 range 读 blob,
  且用的是长期 `BEAN_S3_ACCESS_KEY` / `BEAN_S3_SECRET_KEY` 而非轮转的 STS 凭证。这才是当前
  真正的 gap:它需要的是 1h 轮换、限 blob bucket 前缀的只读临时凭证

换句话说:节点侧 S3 访问已不再是假设 —— 一旦开启 overlaybd,节点就持有长期凭证,
所以 STS 缺口是当下的隐患,不是将来的。构建产物上传(#22)的 presigned 仍是节点/sandbox
直传产物的前置条件;此刻偷懒给节点长期凭证,已经是实质的安全退步,而非推迟的问题。

## 7. 测试策略 ✅

三层:

| 层 | 位置 | 验的是什么 |
|---|---|---|
| 签名单测 | `sign_test.go` | canonical request 的字节级正确性 —— 这是最容易错且最难调的地方 |
| 协议单测 | `client_test.go` / `multipart_test.go` | 用 `httptest` 假服务端验请求形状、分片切分、abort 行为 |
| 集成测试 | `client_integration_test.go` / `s3blobs_test.go` | **打真 MinIO**,`BEAN_S3_ENDPOINT` 未设则 skip |

集成测试为什么必需:`ErrBlobNotFound` 的映射、abort 之后对象确实不存在、
range 读的边界 —— 这些是**服务端的行为**而不是我们包装层的行为,
假服务端只能验证我们发了什么,不能验证真实 S3 会怎么回应。

CI 里跑真 MinIO,所以这一层不是「可选的额外验证」。

## 8. 把对象存储在 snapshot、image、build 之间统一 📐

> 本节是四阶段收敛的设计:一套 object-store 契约,让 snapshot blob、overlaybd 层、
> build 产物都共用;之后 build 产物和 snapshot 文件系统进一步落到同一套内容寻址的
> 层存储上。先写设计再写代码,把边界定清楚。

### 8.1 现在已经共享什么、还没共享什么

**wire 层已经是单一的**:`internal/control/s3.Client`(SigV4、multipart、range 读)是唯一
一套 S3 实现,`bean-api` 和 `noded` 都在 import。没有重复的协议代码要合并。

**没共享的是它上面那一层** —— 三个互不相干的 facade 架在同一个 client 上:

| Facade | 侧 | key 方案 | 形状 |
|---|---|---|---|
| `snapshot.Blobs`(`snapshot/store.go:20`) | 控制面(`bean-api`) | `snapshots/<id>/data` | id 键,流式 `Writer`/`Reader`/`Size`/`Delete` |
| `image.BlobStore`(`image/obdblobstore.go:36`) | 节点(`noded`) | `blobs/<digest>` | digest 键,缓冲 `Put` + `BlobURL`/`CheckReadable` |
| `image.ImageIndex`(`image/obdindex.go:37`) | 节点(`noded`) | `manifests/<digest>`、`tags/...` | 带类型的 manifest/tag 对象 |

外加两套并行的配置命名空间,读同一批凭证:`-s3-*`(bean-api)与 `-fc-overlaybd-s3-*`
(noded),都来自 `BEAN_S3_ACCESS_KEY` / `BEAN_S3_SECRET_KEY`。

### 8.2 统一契约

一个 object-store 接口,**以流式 `Writer` 为写原语**,并把 range 读并进来,于是三个
facade 都变成它上面的薄 key 方案适配器:

```go
// ObjectStore 是每个产物存储最终落到的唯一契约。key 是不透明的,各调用方
// 自己拥有自己的 key 方案(见 8.3)。实现:生产用 BucketStore(基于 S3 client),
// 开发/CI 用 DirStore —— 就是 snapshot 已经依赖的那套 DirBlobs/S3 等价关系,收敛成一个类型。
type ObjectStore interface {
    // Writer 把对象流式写到 key。Close 返回 nil 之前那里读不到任何东西 —— 就是两个
    // 现有实现都已保证的「不留半成品」。若 writer 同时满足 Aborter,可丢弃半截写入。
    // 这是流原语:一个 snapshot bundle 可能有 guest 内存那么大,绝不整块缓冲。
    Writer(ctx context.Context, key string) (io.WriteCloser, error)
    Get(ctx context.Context, key string) (io.ReadCloser, error)
    GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, error)
    Head(ctx context.Context, key string) (size int64, err error) // 不存在返回 ErrNotFound
    Delete(ctx context.Context, key string) error                 // 不存在不算错误
}
```

`Put(ctx, store, key, r, size)` 是 `Writer` 之上的包级便捷函数,给已经持有 reader 的调用方
(一个封好的层、一个小 manifest)用;拷贝失败时它 abort 半截写入,不发布截断对象。几处接缝怎么解决:

- **流式,而非缓冲。** 早先的草稿在接口上放了个缓冲的 `Put(r, size)`;它被去掉了,因为一个
  memory snapshot 有 guest 内存那么大,绝不能整块驻留内存。`Writer` 是原语;overlaybd 层上传
  原先整块的 `io.ReadAll` 换成 `io.Copy` 进 writer,保留同样的声明长度校验,短读时 abort,
  key 上不留东西。
- **`BlobURL` / `CheckReadable` 留在 overlaybd 适配器上。** 它们编码的是 overlaybd 守护进程
  匿名读的要求,只对 overlaybd 读方式有意义,对 snapshot 和 build 没有意义,不属于共享核心,
  保留为 overlaybd 层适配器(`image.BlobStore`)的方法 —— 它现在持有一个 `ObjectStore` 存字节,
  只保留 bucket 和 read-URL 用来拼 `BlobURL`。
- **`Aborter` 仍是类型断言**,和 `AbortWrite` 一样:S3 路径 abort 它的 multipart 上传,本地
  路径删它的临时文件。`AbortWriter(ctx, store, key, w)` 是用它的包级 helper,writer 不实现时
  退化为 close+delete。

### 8.3 key 方案按产物分,放在适配器上

共享存储对 key 不持观点。每个产物在薄适配器上保留自己的方案,三块关注点保持清晰、可各自 GC:

| 产物 | key | 拥有方 |
|---|---|---|
| snapshot 字节 | `snapshots/<id>/data`(不变) | 控制面 |
| overlaybd 层 | `blobs/<digest>`(不变) | 节点 |
| manifest / tag | `manifests/<digest>`、`tags/<host>/<repo>/<tag>`(不变) | 节点 |
| **build 产物** | `blobs/<digest>` —— 与层**同一个**内容寻址空间(见 8.5) | 节点 |

key 原样保留,意味着对 snapshot 和 overlaybd 而言这次统一是纯重构、无数据迁移:同样的字节落到
同样的 key,只是上面的 Go 类型变了。

### 8.4 控制面 vs 节点侧是部署事实,不是障碍

builder 跑在 **noded** 上(`internal/node/image/build_linux.go`,在 `cmd/noded/main.go` 接线),
正好就是 overlaybd 存储已经有一套可用的、用同一批 `BEAN_S3_*` 凭证的节点侧 S3 client 的地方。
所以 build 产物上传和 overlaybd 上传在同一进程 —— 不用把 build 字节绕经控制面。snapshot 存储
留在控制面;它共享的是接口和低层 client,不是进程。统一抽象每进程实例化一次(`bean-api` 一次、
`noded` 一次),各带自己的 key 方案适配器。

配置收敛成一个命名空间:单一的 `--s3-endpoint` / `--s3-bucket` / `--s3-region` /
`--s3-path-style`(overlaybd 读 URL 作为唯一真正 overlaybd 特有的额外项保留),两个进程读同一批
`BEAN_S3_*` 凭证。阶段 1 交付了共享的 `ObjectStore` 契约,以及在接口之下支撑两个节点侧 facade 的
单一 `BucketStore`;**改名放在阶段 2 做**,把 noded 的 `-fc-overlaybd-s3-*` 退成 `-s3-*`。改名落在
阶段 2 而非阶段 1,是因为那一阶段 build 产物也封成 overlaybd 层进同一个存储,存储就明确是节点唯一的
产物存储,通用的 `-s3-*` 名才准确。

### 8.5 阶段 2-4:build 产物与 snapshot 文件系统落到共享层

统一存储使能的收敛,按顺序:

- **阶段 2 —— build 产物上 S3。** 今天构建产物是扁平的 `<ImageDir>/<name>.ext4`,**从不上传**
  (`internal/control/api/build.go:236` 明说:「只存在于构建节点的 ImageDir ...别的节点无法从它
  启动」)。有了节点侧存储,build 把产物发布到共享的 `blobs/<digest>` 空间并记录 digest,任意
  节点都能从它启动,`image` API 的 list/delete 操作在真实存储的产物上。这消除了单节点限制。
- **阶段 3 —— 文件系统层统一到 overlaybd,按 digest 去重。** image 的文件系统和 snapshot 的
  文件系统都变成按内容 digest 键的 overlaybd 层链。从某 image 拍的 snapshot 共享该 image 的层
  digest,于是 S3 只存一份 —— 就是让第二个 image 转化便宜的那套去重。snapshot 的文件系统成员
  从独立 bundle 迁到这个共享层空间;**内存和设备态仍是独立 blob**,因为那是任何 image 都没有的
  部分,也是区分内存快照与文件系统快照的唯一东西。
- **阶段 4 —— 移除 commit。** 一旦纯文件系统快照和一个 commit 出来的镜像在底层是同一批内容寻址
  层,`commit` 就冗余了:「保存这个环境去分享」就是把文件系统快照提升进 image 命名空间。专门的
  `commit` 动词、它的 handler 和 gRPC 都移除;该用例走 snapshot。

终态:**一个对象存储、所有文件系统一个层空间、内存态作为唯一区分产物的 blob** —— 这就是 glossary
定义的 `{文件系统, config, ?内存}` 统一模型,在存储层落地。

### 8.6 验证

每个阶段都在真实 KVM 主机(fc tier)上做端到端验证,不只是单测 —— 因为这里真正重要的性质
(真实 S3 接受上传、另一个节点从它没构建过的 digest 启动、overlaybd range 读一个共享层、
snapshot 从去重的层恢复)是服务端和 guest 的行为,假的显示不出来。现有打 MinIO 的集成测试(§7)
是单测层;`hack/` 下的 fc-tier 探针是 e2e 层。

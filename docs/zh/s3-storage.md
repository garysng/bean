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

**当前只有控制面碰 S3**(`grep -rn BEAN_S3 cmd/noded/` 为空)。
快照 blob 的流向是 节点 → gRPC → gateway → S3,节点不需要凭证也拿不到。
这不是设计上的克制,是「节点侧还没有需要直传 S3 的功能」的副产品。

### 已知缺口 📐

设计里(security-and-startup §A5)节点在需要直传时**不应持有长期凭证**:

- **presigned URL** 未实装 —— 节点上传产物、sandbox 内直传产物都应该用控制面
  签发的、绑定 key 前缀与 content-length 的短时 URL
- **STS 只读角色轮换**未实装 —— overlaybd 接入后节点要直接读 blob,
  那时才真正需要它(1h 轮换的只读临时凭证,限 blob bucket 前缀)

换句话说:这两条缺口目前**还没有造成风险**,因为节点不碰 S3。
但它们是 overlaybd 接入(节点直接 range 读 blob)与构建产物上传(#22)的前置条件 ——
届时如果偷懒直接给节点长期凭证,就会变成实质的安全退步。

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

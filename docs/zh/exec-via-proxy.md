# exec / 文件传输走数据面,不走网关

> 状态:📐 **设计。** 提议把 `exec` 和文件传输从 bean-api 控制面中转移到 bean-proxy 数据面,
> 让它们 node-direct 直达 agent —— 与 README 已有的声称对齐。权威顺序成立:代码 > `status.md`
> > `decisions.md` > 设计文档 > 本页。

> English: [../exec-via-proxy.md](../exec-via-proxy.md)

---

## 0. 为什么

README 说 `exec` 和文件传输"不再经控制面中转"(README §其他已可用)。代码不符:`exec` 今天
是三跳 gRPC,中间那跳就是 bean-api。

```
client ──HTTP/JSON──► bean-api ──gRPC──► noded ──gRPC (vsock/tcp)──► agent
         POST /exec    handleExec         Exec 透传                  AgentService/Exec
```

- `handleExec`(`server.go:764`)解码 JSON、`resolveNode`,再 `nodeClient.Exec` —— 打到节点的
  **控制**端口的 gRPC 调用。它自己的 metric 标注是 "Exec round-trip latency **through the
  gateway**"。
- noded 的 `Exec`(`grpc.go:185`)是纯透传到 agent(`AgentConn` → `AgentService/Exec`)。

所以每次 exec、每个文件字节都经控制面。这把数据面的吞吐和延迟耦合到了网关上,还让网关恰好成了
那些高频操作(流式文件、频繁 exec)的瓶颈 —— 而它本不该在这条路上。修好它,README 才为真。

## 1. 传输层已经存在

关键发现:**node-direct 传输已经完整建好**,而且已经端到端承载 gRPC。不需要给 agent 加 HTTP 面,
也不需要让 proxy 懂 exec。

```
client ──gRPC (h2c)──► bean-proxy ──h2c──► noded PortForwarder ──h2c 进 netns──► agent:10001
        authority                 反向代理             拨 GuestIP:10001        AgentService/Exec
        10001-{sandbox}
```

- **bean-proxy 已经会转发 gRPC。** 对 agent 端口(`AgentGuestPort=10001`)它选 h2c 的
  `http2.Transport`(`proxy.go:139-152`),整个 server 用 `h2c.NewHandler` 包着
  (`bean-proxy/main.go:81`)。"不说 gRPC"的注释意思是它不*发起或解释* gRPC —— 它字节透明地
  转发 HTTP/2,还特意对 10001 端口选 h2c。
- **noded 的 PortForwarder** 做同样的分流(`portforward.go:193`,`transportFor` → 10001 用
  h2c),并拨进 sandbox netns。
- **agent 的 exec API 就是那条 gRPC。** 10001 上的 `AgentService/Exec` 正是 forwarder 承载的
  东西。所以 exec 可以表达成"经 proxy 打到 `10001-{sandbox}` 的一次 gRPC 调用"。

**不**存在的是一个会说这套的 client。bean-api 拨的是 noded 的*控制* gRPC 端口、调
`SandboxService/Exec`(另一个 service);没有任何 client 用 authority `10001-{sandbox}` 拨 proxy
直接调 `AgentService/Exec`。

## 2. 死结:凭据 vs 可达性

这是让改造变得不 trivial 的部分,动任何代码前必须理解。agent 按档认证不同,而要紧的两个档正好
互相牵制:

| 档 | agent 监听 | 认证 | 有 guest IP? | PortForwarder 到得了? |
|---|---|---|---|---|
| networked fc | `tcp:0.0.0.0:10001` | **必需** —— per-sandbox token(`beand/auth.go`,fail-closed) | 有 | **能** |
| 无网 fc | `vsock:1024` | 无(vsock 隔离) | 无 | 不能 |
| local(开发) | unix socket | 无 | 无 | 不能 |

agent 的 token 校验(`auth.go`)验的是 **per-sandbox** token(`sbxtoken.Verify` 对 MMDS 里发布的
hash),且 fail-closed —— 凭据缺失即拒。它的全部意义(据代码注释):agent 一旦 over TCP,
沙箱*自己的 root* 就能拨 10001,只有 per-sandbox token 能区分 noded 和那个 root。

两个事实相撞:

1. **proxy 的 `bean-node-token` 满足不了 agent。** 那个 token 认证的是 proxy *到 noded 的
   forwarding port*(`proxy.go:242` 注释);agent 校验的是 per-sandbox token,是另一个凭据。
   把 node token 透传过去,请求能到 agent,但会被拒。
2. **per-sandbox token 的明文只在 noded 上。** 它在节点上铸造(`manager.go:213`);只有 hash
   进 MMDS。client 今天没有任何途径拿到它。

而可达性反着卡:**PortForwarder 需要一个可路由的 guest IP**(`TargetFor` 在 `net_ == nil` 报错,
`portforward.go:98`),只有 networked fc 档有 —— 而那正是需要 token 的档。免 token 的档
(vsock、local)没有 guest IP,proxy 到不了。

**现有代码里没有任何可运行的档位能"不带 token 走 proxy"**:有网就要 token,没网 forwarder 就
到不了 agent。所以凭据交付不是可选的润色 —— 它在关键路径上。

## 3. 设计

三部分。凭据交付是唯一有真实安全分量的;另两部分是机械的。

### 3.1 凭据交付

拨 proxy 的 client 必须带上 agent 期望的 per-sandbox token。两个选项及权衡:

- **create 时返回。** `POST /v1/sandboxes` 在响应里(一次性)带上 token;client 为该沙箱终生
  保留。最简单、一个往返,但 token 从此活在 client 里、并可能进 create 日志(除非小心处理)。
- **专用取回端点。** `GET /v1/sandboxes/{id}/agent-token` 按需返回,受 bean-api 上和其他一切
  相同的 API-key 认证保护。把它挡在 create 响应之外、多一个往返,并给出一个可审计、日后可吊销
  的单点。

无论哪种,今天从不离开 noded 的明文,都必须上浮到 bean-api 再到 client。这是对"秘密存放范围"
的一次刻意扩大,应明确点出:token 不再是 noded-only 的秘密,而成为数据面的 client 持有 bearer
凭据。推荐:**取回端点**,因为它可审计、且不给每个 create 响应塞进一个秘密。

### 3.2 客户端:一个数据面 gRPC client

CLI 和 SDK 今天只认 `BEAN_BASE_URL` → bean-api REST。新增:

- **`BEAN_PROXY_URL`** —— 数据面调用用的 proxy 地址。
- 一个 **gRPC client**,用 authority `10001-{sandbox}` 拨 proxy、直接调 `AgentService/Exec` /
  `ReadFile` / `WriteFile` / `ListDir` / `DeleteFile`,把 per-sandbox token 作为 gRPC metadata
  带上(agent 读的同一个 key)。
- 回退:若 `BEAN_PROXY_URL` 未设,保留当前 bean-api 路径,所以这是增量的,单节点/开发环境不受影响。

Python SDK 现在纯 `urllib`;gRPC client 意味着要么引 `grpcio` 依赖,要么另做 h2c/framed 方案。
这个成本是真实的,是"SDK 也改"的一部分。

### 3.3 寻址

proxy 和 forwarder 都无需协议改动 —— 两者对 10001 端口都已经做 h2c。client 构造 authority
`10001-{sandbox}`;proxy 经 `NodeAddrFor`(已实现)解析节点并转发;forwarder 在 netns 里拨
`GuestIP:10001`。这部分完全是复用。

## 4. 验证只能在 fc 档真机上做

这限制了"e2e 真实验证"本身如何可能,必须说清:

- **local 档没有 guest 网络**(`local.go`:unix-socket agent;`manager.go:197` 只在 `--guest-subnet`
  时才配网络)。`TargetFor` 对每个 local 沙箱都会命中 `net_ == nil` —— PortForwarder 到不了它。
- `--guest-subnet` 需要 Linux + KVM + `--uplink`(`cmd/noded/main.go:411`)。
- 当前 e2e 栈(`tests/e2e/e2e_test.go`)只起 bean-api + noded、`--runtime local`、没有 bean-proxy、
  没有 `--sandbox-port-listen`。

所以真正的"exec 经 proxy"e2e **必须在 fc 档、真 Linux/KVM 机上跑**,栈还要额外起 bean-proxy 并
让 noded 带 `--sandbox-port-listen`。它在 local 档 CI 栈里跑不了。因此验证方案是一个 `hack/`
脚本(形如 `guest-egress-probe.sh`),在真 microVM 机上:创建沙箱、取 token、经 proxy exec、断言
输出、经 proxy 往返一个文件、断言字节 —— 然后把 token 改坏、断言 agent 拒绝。最后这步要紧:证明
数据面路径上认证仍然 fail-closed,是整个安全论证的核心。

## 5. 分阶段

1. **凭据交付**(§3.1)—— bean-api 上的取回端点,token 从 noded 上浮。没有它其他都测不了。
2. **CLI 的数据面 gRPC client**(§3.2),挂在 `BEAN_PROXY_URL` 后,未设时回退网关路径。
3. **真机 e2e**(§4)—— `hack/` 探针断言经 proxy 的 exec + 文件往返,含 fail-closed 检查。
4. **SDK** —— 给 Python SDK 加数据面 gRPC client(`grpcio` 依赖的决定落在这)。
5. **文档** —— 一旦 ship,修正 README,让"不再经控制面中转"由"数据面路径成为默认"支撑,并注明
   网关路径作为回退。

会触及的文件:`internal/control/api/server.go`(token 端点)、`internal/node/manager.go`(上浮
token)、`cli/cli.go`(proxy client)、`sdk/python/bean/__init__.py`(gRPC client)、一个新的
`hack/` 探针,以及 e2e/文档更新。proxy(`proxy.go`)和 forwarder(`portforward.go`)无需改动 ——
这正是重点。

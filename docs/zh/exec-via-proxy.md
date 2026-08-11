# exec / 文件传输走数据面,不走网关

> 状态:✅ **已实现。** 配了 proxy 时,`exec` 与文件传输不再经 bean-api 中转,而是走 bean-proxy
> 数据面 node-direct 直达 agent。§5 的六个阶段都已落地:token 注入
> (`internal/node/portforward.go`)、create 响应带回 domain
> (`internal/control/api/server.go`)、CLI 客户端(`cli/dataplane.go`)、真机探针
> (`hack/exec-via-proxy-probe.sh`)、Python SDK 客户端(`sdk/python/bean/_dataplane.py`)。
> §5 保留为施工顺序的记录。权威顺序成立:代码 > `status.md` > `decisions.md` > 设计文档 > 本页。
>
> 这条路径是 opt-in,不是默认:不设 `BEAN_PROXY_URL` 时客户端仍走网关中转 —— 没有 proxy 的
> 单节点栈需要的正是这个回退。

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
到不了 agent。所以这条路上*总得有人*出示 per-sandbox token —— 唯一的问题是谁。下面的设计给出
答案:**noded 来注入 token**,client 全程不碰它。

## 3. 设计

定案:**client 全程不碰 per-sandbox token。** 由 **noded 在 forwarder 处注入**,因为 noded
是唯一本来就握着明文的进程。client 只对平台外层的 apikey 层认证;proxy 往里的一切都归节点信任
域管理。这是 Daytona 范式 —— proxy 层拥有 agent 认证、调用方只持一个平台凭据 —— 而不是 E2B
范式的"client 自己持 per-sandbox token 直连"。

三部分。注入认证是唯一有真实安全分量的;另两部分是机械的。

### 3.1 认证:noded 在 forwarder 处注入 token

凭据链变成:

```
client ──apikey──► bean-proxy ──node-token──► noded PortForwarder ──注入 per-sandbox token──► agent
        (平台外层)              (现有边界 F)           (noded 握着明文)
```

- **client → proxy:只需 apikey。** 我们假设每个到达 proxy 的请求都已过平台的 apikey 层(最终
  产品会包一层)。bean 本身在这里不再重复校验;client 不出示任何 per-sandbox 凭据,因为它根本
  没有。
- **proxy → noded:`bean-node-token`。** 不变 —— 现有的 forwarding port 边界
  (`portforward.go:295`)。
- **noded → agent:per-sandbox token,由 PortForwarder 注入。** 今天 PortForwarder 是纯透传,
  **不**注入 token(只有 noded 控制路径上的 `AgentConn` 才经 `sbxtoken.WithAgentToken` 注入)。
  改动是:forwarder 往 `GuestIP:10001` 转发某沙箱时,查出该沙箱的 `agentToken`(明文在
  `manager.go:213` 铸造后就存在 `sandbox.agentToken` 里),给出站请求带上 agent 的认证
  metadata/header。

这是**唯一**有安全分量的改动,且刻意只落在 noded。明文 token 一如今天仍是 noded-only 的秘密 ——
**不**上浮到 bean-api 或 client。agent 侧的校验(`beand/auth.go`)保持 fail-closed、原样不动:
不带 token 到达 agent 的请求(比如沙箱自己的 root 拨 10001)照样被拒。forwarder 注入并不削弱
这一点 —— 它只是让*合法*的 proxy 路径带上 agent 本就要求的凭据。

实现时要验证一点:PortForwarder 只对 agent 端口(10001)注入,不能对它同样转发的任意用户端口
注入。用户跑在 8080 上的自己的 server 绝不能收到 agent token。

### 3.2 寻址:sandbox 返回 domain,client 拼 URL

create 返回沙箱的 **domain**(或 client 该用的 proxy base)。CLI/SDK 在调用时据此拼出请求
URL —— 对该 domain 拼 `{port}-{sandbox}`。proxy 只转发,所有端口映射默认通(按端口访问控制是
另一个尚未构建的功能 —— [#50](https://github.com/garysng/bean/issues/50)),所以没有注册调用、
没有宿主端口池。这是 E2B 的 subdomain 范式,但 domain 由服务端返回,而非 client 端按约定拼。

- **`create` 响应**新增该沙箱的 domain/proxy base。
- **CLI/SDK** 对该 domain 拼 `10001-{sandbox}`(agent)或 `{port}-{sandbox}`(用户端口)再发起
  调用。
- 未配置 proxy domain 时回退当前 bean-api 路径,所以这是增量的,单节点/开发环境不受影响。

### 3.3 客户端:一个数据面 gRPC client

CLI 和 SDK 今天只认 `BEAN_BASE_URL` → bean-api REST。数据面路径新增一个 **gRPC client**,用
authority `10001-{sandbox}` 拨 proxy、直接调 `AgentService/Exec` / `ReadFile` / `WriteFile` /
`ListDir` / `DeleteFile`。**不**附带任何 per-sandbox token —— client 除了外层 apikey 层所需的,
什么都不出示。

传输层无需 proxy 或 forwarder 的协议改动 —— 两者对 10001 端口都已经做 h2c。proxy 经
`NodeAddrFor`(已实现)解析节点并转发;forwarder 在 netns 里拨 `GuestIP:10001`,并新增注入
agent token(§3.1)。传输完全复用,只有 token 注入是新的。

Python SDK 现在纯 `urllib`;gRPC client 意味着要么引 `grpcio` 依赖,要么另做 h2c/framed 方案。
这个成本是真实的,是"SDK 也改"的一部分。

## 4. 验证只能在 fc 档真机上做

这限制了"e2e 真实验证"本身如何可能,必须说清:

- **local 档没有 guest 网络**(`local.go`:unix-socket agent;`manager.go:197` 只在 `--guest-subnet`
  时才配网络)。`TargetFor` 对每个 local 沙箱都会命中 `net_ == nil` —— PortForwarder 到不了它。
- `--guest-subnet` 需要 Linux + KVM + `--uplink`(`cmd/noded/main.go:411`)。
- 当前 e2e 栈(`tests/e2e/e2e_test.go`)只起 bean-api + noded、`--runtime local`、没有 bean-proxy、
  没有 `--sandbox-port-listen`。

所以真正的"exec 经 proxy"e2e **必须在 fc 档、真 Linux/KVM 机上跑**,栈还要额外起 bean-proxy 并
让 noded 带 `--sandbox-port-listen`。它在 local 档 CI 栈里跑不了。因此验证方案是一个 `hack/`
脚本(形如 `guest-egress-probe.sh`),在真 microVM 机上:创建沙箱、经 proxy exec、断言输出、经
proxy 往返一个文件、断言字节 —— 然后为证明认证仍 fail-closed,走 forwarder 路径但**不**经 noded
注入(模拟沙箱自己的 root)拨 `10001-{sandbox}`、断言 agent 拒绝。最后这步要紧:整个安全论证就是
"只有 noded 注入的路径才带 token",而 agent 对其余一切照拒。

## 5. 分阶段

1. **forwarder 里注入 token**(§3.1)—— PortForwarder 只对 agent 端口注入该沙箱的
   `agentToken`。这是唯一有安全分量的改动,也是整条路径的依赖点。
2. **`create` 返回沙箱 domain**(§3.2)—— 在响应里带出,好让 client 拼数据面 URL。
3. **CLI 的数据面 gRPC client**(§3.3),对返回的 domain 拼 `{port}-{sandbox}`,未配置 proxy
   domain 时回退网关路径。
4. **真机 e2e**(§4)—— `hack/` 探针断言经 proxy 的 exec + 文件往返,含 fail-closed 检查
   (未注入的拨号被拒)。
5. **SDK** —— 给 Python SDK 加数据面 gRPC client(`grpcio` 依赖的决定落在这)。
6. **文档** —— 一旦 ship,修正 README,让"不再经控制面中转"由"数据面路径成为默认"支撑,并注明
   网关路径作为回退。

会触及的文件:`internal/node/portforward.go`(对 10001 端口注入 agent token —— 唯一有安全
分量的改动)、`internal/control/api/server.go`(create 响应返回沙箱 domain)、`cli/cli.go`
(proxy client)、`sdk/python/bean/__init__.py`(gRPC client)、一个新的 `hack/` 探针,以及
e2e/文档更新。注意与早先草稿的区别:client 不再取回或持有 per-sandbox token,所以没有 token
端点、也没有秘密上浮到 bean-api —— 明文始终 noded-only。proxy(`proxy.go`)无需改动;forwarder
(`portforward.go`)要改,而那处注入正是整个设计的关键。

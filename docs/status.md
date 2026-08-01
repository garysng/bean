# 实现状态

> 快照日期：2026-08-02。CI 全绿（lint / race 单测 / e2e / SDK / proto drift），
> 覆盖率 80.5%。控制面与节点面均在 Linux x86_64 上验证过，无 darwin 平台假设。
>
> **microVM 档已实装并在真 KVM 机器上跑通**：Alpine 3.20 启动 2.2s，
> host 经 vsock exec 拿到输出，snapshot 20 MiB、restore 1.8s。

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

fc 档实测（镜像已缓存）：`runtime_create` ~200-270ms（起 VMM）、
`agent_ready` ~1.9s（内核启动 + pivot + listen）、`total` ~2.2s。
**内核启动是主要成本**,后续加速从这里入手。

snapshot：checkpoint 1.5s、restore 1.8s、bundle 约 16-20 MiB。
镜像首次拉取转换：busybox 5-10s,alpine 在网络不稳时 2m45s ——
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
| overlaybd lazy-pull | ⚠️ 当前是「拉全量 + 转换 + CoW 共享」,已能用且成本低（每 sandbox 8 KiB）;overlaybd 的价值在于**首次拉取**也按需,节点已装好组件,接同一个 `image.Provider` 接口即可 |
| diff snapshot（增量） | ⚠️ 当前 full snapshot;Firecracker 支持 diff,接口无需改 |
| fork / shared-fs 卷 / proxy 端口暴露 | ⛔ P3–P4 范围,未开始 |
| OTLP 导出 | ⚠️ registry 已就位,包一层即可 |
| Postgres | ⚠️ 接口已抽象,当前 SQLite |
| 创建阶段指标 network | ⚠️ 埋点位已留,等网络实装 |

## 3. 节点前提

fc 档需要：

- `/dev/kvm`（Intel VT-x 或 AMD SVM）
- Firecracker 二进制、guest 内核镜像、agent 盘（`hack/build-assets.sh` 构建）
- **AMD 主机需 `kvm.ignore_msrs=Y`**：Firecracker 保存快照时读 Intel 专有的
  MSR 0x3a,AMD 上 KVM 会拒绝。`NewFCTier` 启动时检查并给出修复命令,
  而不是等到快照失败才暴露。

overlaybd 需要 ublk（内核 ≥ 6.0)或 tcmu 后端。当前验证机是 Ubuntu 20.04 +
内核 5.15,无 `/dev/ublk-control`,所以 overlaybd 走 **tcmu**;
换 22.04 + HWE 6.8 才有 ublk（性能更好）。

## 4. 下一步

1. **build 的构建日志与取消**：现在 build 是「起了就等」,失败只能从 image state
   看到 FAILED。日志落存储 + 可流式查看 + `cancel` 才算完整（`docs/image-build.md` §6）。
2. **加速创建**：2.2s 里 1.9s 是内核启动。精简 guest config,或维护一个
   snapshot 预热池（restore 1.8s 也不快,但可以在请求到来前就备好）。
3. **overlaybd lazy-pull**：让首次拉取也按需读块,而不是拉全量再转换。
   现在 CoW 已经解决了「每 sandbox 的成本」,overlaybd 解决的是
   「首次使用一个大镜像的等待时间」。

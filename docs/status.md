# 实现状态与 fcRuntime 实装计划

> 快照日期：2026-08-01。覆盖率 81.5%，CI 全绿（lint/race 单测/e2e/SDK/proto drift）。
> 已在 Linux x86_64 上验证（12 包 race 单测 + e2e + SDK 全通过），无 darwin 平台假设。

## 1. 已完成

### 控制面

| 组件 | 状态 | 说明 |
|---|---|---|
| `bean-api` REST gateway | ✅ | sandboxes CRUD、exec、files、logs、events、pause/resume、metrics;API key 鉴权、配额位、请求体限流、超时钳制 |
| `scheduler` | ✅ | 两级放置（region → 节点）、硬过滤（runtime 能力/labels/承诺量/创建并发）、打分（镜像亲和/装箱/NVMe/spread）、按承诺量记账不超卖、批量放置、READY/SUSPECT/LOST/DRAINING |
| `nodesvc` | ✅ | Register（bootstrap token 校验 + 签发 node token）、Heartbeat 双向流续租、SyncState、租约过期回调 |
| `store` | ✅ | SQLite（Postgres 接口已抽象）:sandbox 记录、事件历史 |
| 事件 | ✅ | 状态机统一发件 → 持久化 + SSE 实时订阅（按 sandbox/label 过滤,慢订阅者丢弃计数） |
| image API | ✅ | ref/digest/overlaybd 产物三层语义、状态机、prewarm job;registry 凭证 AES-256-GCM 加密存储 |
| snapshot | ✅ 控制面 | 创建/列表/删除/引用计数、从 snapshot 创建 sandbox;blob 存储接口化（本地目录,S3 待接） |
| 路由 | ✅ | `NodeRouter` per-node 连接池,数据面按记录里的 nodeID 解析 |

### 节点面

| 组件 | 状态 | 说明 |
|---|---|---|
| `noded` | ✅ | Manager（创建/销毁/pause/resume、透明唤醒、本地 idle 回收、in-flight 保护）、SandboxService gRPC、node token 鉴权、metrics |
| `Registrar` | ✅ | 出向注册（无需入站）、SyncState 对账销毁孤儿、心跳带状态与承诺量、指数退避重连 |
| `beand`（sandbox 内） | ✅ | exec（超时/截断/进程组 kill/WaitDelay）、文件（os.Root 防逃逸、原子写）、logs 环形缓冲、用户进程托管与回收 |
| `LocalRuntime` | ✅ | 进程级 sandbox（dev/CI），跑真 beand 二进制,验证与 fc 档相同的 agent gRPC 面 |
| `fcRuntime` | ⛔ 骨架 | 见 §3 |

### 客户端

Python SDK（create/exec/files/pause/resume/kill、snapshot、images、events 订阅、
context manager、错误分层）、Go CLI（run [--image|--snapshot]/ls/exec/cp/logs/kill/
pause/resume/events -f/snapshot/image）。

### 可观测

`bean-api /metrics`：创建结果与延迟、exec 延迟、各状态 sandbox 数、事件计数与订阅数。
`noded /metrics`：创建阶段耗时（runtime_create / agent_ready / total）、
创建/销毁/idle 动作计数、节点 sandbox 状态与 in-flight。

### 验证覆盖

- 单节点 e2e：真进程 gateway + noded + CLI，create→exec→files→pause→唤醒→events→destroy
- 多节点 e2e：1 gateway + 2 自注册 noded，放置分散、exec 路由正确、容量耗尽 503、释放后可再创建
- 安全回归：symlink 逃逸阻断、setuid 位剥离、host env 不泄漏、超时后孤儿孙进程不挂起
- 并发回归：并发放置恰好 N 个成功、并发 pause 只一个胜出、连接池无竞态
- snapshot e2e：写文件 → snapshot → 从 snapshot 创建 → exec 读回内容一致;
  克隆间互相独立、fan-out 多份、引用计数拒删、blob 随记录删除

## 2. 与设计的差距

| 项 | 状态 |
|---|---|
| fcRuntime（真 microVM） | ⛔ 未实装,**唯一实质缺口** |
| overlaybd 镜像链路 | ⛔ 未实装（依赖 fc 档）;image 状态机与 API 已就位 |
| prewarm 节点侧执行 | ⚠️ API 与 job 状态已就位,节点侧拉取随 overlaybd 落地 |
| snapshot 的 fc 档实现 | ⚠️ 控制面全通;LocalRuntime 用 tar 真跑通,fc 档 memory snapshot 待实装 |
| fork / shared-fs 卷 / proxy 端口暴露 | ⛔ P3–P4 范围,未开始 |
| OTLP 导出 | ⚠️ registry 已就位,包一层即可 |
| Postgres | ⚠️ 接口已抽象,当前 SQLite |
| 创建阶段指标 image_pull/rootfs/network | ⚠️ 埋点位已留,等 fc 档 |

## 3. fcRuntime 实装计划

### 3.1 前置环境

**已备好**（192.168.75.52，root 可登录）：Linux x86_64、16 核 / 23 GB、
`/dev/kvm` 可用（AMD svm）、Go 1.24.5、Firecracker + Jailer v1.10.1、TCMU 可用。

**限制**：内核 5.15（< 6.0）→ 无 `/dev/ublk-control`，overlaybd 需走 **tcmu 后端**
（功能完整，性能不如 ublk）。若要 ublk 需装 HWE 内核（`linux-generic-hwe-22.04`，6.8）
并重启。

**仍需**：`overlaybd` 组件、S3 或兼容对象存储。参考实现在本地
`/Users/mac/project/agentenv`（AgentENV，uvm-ublk 直驱 overlaybd + FC 的完整实证）。

### 3.2 实装顺序

1. **guest 内核 + agent 盘**（先手工构建，流水线后置）
   - 6.x 精简 config，内嵌 virtio-blk/net/vsock、erofs、nfs
   - agent 盘：erofs 只读镜像，含静态编译 `beand` + 最小工具
   - 验收：`firecracker` 手工起 VM，guest 内 `beand` 能 listen vsock
2. **overlaybd 块设备组装**（`internal/node/image`）
   - 预转换镜像 → ublk 设备（先全量本地，lazy-pull 后置）
   - 产出 `runtime.RootfsMount`（块设备路径），与 Runtime 解耦
   - 验收：给定镜像 ref 拿到可挂载块设备，`mount` 后内容正确
3. **`fcRuntime.Create`**
   - jailer + firecracker 进程管理、virtio-blk×2（rootfs + agent 盘）、vsock、tap
   - guest 内 beand 作 init：挂载矩阵 → 切根 → 应用 image config → listen
   - 验收：`Manager.Create` 走 fc 档拿到健康 agent，现有 manager 测试全过
4. **vsock transport**
   - beand 的 listener 抽象加 vsock 实现;noded 侧 dial vsock
   - 验收：`internal/beand` 现有测试在 vsock 传输下同样通过
5. **销毁与对账**
   - FC 进程/tap/ublk 设备/挂载点清理;reconcile 枚举存活 FC 进程
   - 验收：destroy 后无残留（e2e 已有断言，换 fc 档复用）
6. **补齐阶段指标**：image_pull / rootfs / network 三个 phase 埋点

### 3.3 可复用的现有资产

fcRuntime 只需实现 `runtime.Runtime` 接口（7 个方法，含 Checkpoint/Restore），
**上层完全不用改**：
Manager、gRPC 面、scheduler、gateway、SDK、CLI、e2e 断言都与 runtime 无关。
`LocalRuntime` 已证明这层抽象成立——同一套 manager/e2e 测试换实现即可复用。

### 3.4 风险

| 风险 | 缓解 |
|---|---|
| guest 内核 config 调试耗时 | 先用 AgentENV 的 config 起步 |
| vsock 与 unix socket 语义差异 | transport 抽象已就位,协议层不变 |
| ublk 内核要求 | 节点 OS 统一基线;tcmu 后端兜底 |
| 清理不彻底导致残留 | 沿用 `bean-<id>` 命名规约 + 现有孤儿扫描 |

## 4. 下一步

环境已就绪，控制面（含 image/snapshot/registry 认证）已完整且在 Linux 上验证过。
按 §3.2 顺序实装 fcRuntime 即可，第一步是 guest 内核 + agent 盘。

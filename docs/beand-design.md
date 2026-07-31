# beand（Node Daemon）与 bean-agent 详细设计

> beand：每节点一个的守护进程，sandbox 生命周期的实际执行者。
> bean-agent：sandbox 内 PID1，exec/文件/端口的执行末端。

## 1. beand 总体结构

```
beand
├── server/          gRPC server（NodeService client 侧 + 数据面 SandboxService 实现）
├── runtime/         Runtime 接口 + runc/runsc/kata/firecracker 实现
├── image/           镜像拉取、snapshotter 管理、本地缓存、prewarm
├── network/         netns/veth/bridge/nftables 编排
├── agentmgr/        agent 注入、socket 管理、健康探测
├── reconcile/       期望状态 vs containerd 实际状态对账
├── gc/              超时回收、孤儿资源清理、缓存 LRU
└── report/          能力探测、心跳、资源水位、缓存清单摘要
```

单二进制 `beand`，systemd 管理，配置文件 `/etc/bean/beand.yaml`：

```yaml
nodeId: auto            # 默认机器指纹生成
controlPlane: grpc://control.internal:7443
containerd: /run/bean/containerd.sock    # 独立 containerd 实例，专用 namespace "bean"
cidr: 10.100.0.0/24     # 本节点 sandbox 网段
cache:
  dir: /var/lib/bean/cache
  maxBytes: 800Gi        # 裸金属 NVMe 大盘 / 云 VM 小盘只差这个数
runtimes: auto           # 或显式列表覆盖探测结果
```

## 2. 能力探测（启动时）

| 探测项 | 方法 | 影响 |
|---|---|---|
| KVM | `/dev/kvm` 可打开 | kata/firecracker 档位 |
| runsc | 二进制存在 + `runsc --version` | standard 档；无 KVM 时自动 `--platform=ptrace` |
| NVMe/磁盘 | 缓存目录所在盘类型 + 可用空间 | 调度缓存盘权重 |
| GPU | NVML 枚举 | GPU 资源画像 + nvidia 运行时注入 |
| cgroup v2 | `/sys/fs/cgroup/cgroup.controllers` | 强制要求 v2，v1 直接拒绝启动 |
| 内核版本/erofs/overlayfs | `uname` + /proc/filesystems | snapshotter 选择 |

探测结果 → `Register` 上报，之后仅在变化时重报。

## 3. Runtime 抽象

```go
type Runtime interface {
    Create(ctx context.Context, spec *SandboxSpec, rootfs RootfsMount) (Handle, error)
    Destroy(ctx context.Context, id string, force bool) error
    Pause(ctx context.Context, id string) error
    Resume(ctx context.Context, id string) error
    Checkpoint(ctx context.Context, id string, w io.Writer) error   // → S3
    Restore(ctx context.Context, spec *SandboxSpec, rootfs RootfsMount, r io.Reader) (Handle, error)
    Stats(ctx context.Context, id string) (*Stats, error)
}
```

| 实现 | 底层 | 职责边界 |
|---|---|---|
| `runcRuntime` | containerd task API + runc shim | 基线；Checkpoint 用 CRIU |
| `runscRuntime` | containerd + runsc shim (`io.containerd.runsc.v1`) | 默认档；Checkpoint 用 gVisor 自带 save/restore |
| `kataRuntime` | containerd + kata shim | strong 档过渡方案 |
| `fcRuntime`（Phase 4+） | 自研：直接管 firecracker 进程 + jailer | 容器 rootfs 经 virtio-blk/virtiofs 挂入；vsock 通 agent；memory snapshot 原生 |

要点：

- 前三个实现共享 containerd 通道，差异只在 runtime handler 与 checkpoint 路径，代码复用率高
- `RootfsMount` 由 image 模块产出（见 §4），与 Runtime 解耦——这是 fcRuntime 复用镜像链路的关键
- OCI spec 生成集中在一处（资源限制、mount、seccomp、hostname、agent 注入），runtime 实现只做增量修改

## 4. 镜像模块

### 4.1 双格式支持

| 格式 | snapshotter | 场景 |
|---|---|---|
| 标准 OCI（gzip 层） | overlayfs | 兜底；小镜像、prewarm 已完成时性能最佳 |
| Nydus（RAFS） | nydus-snapshotter | 大镜像 lazy-pull 主路径；blob 存 S3 |

- image-service 负责把高频镜像**离线转换**为 Nydus 格式（转换在服务端做一次，
  节点侧零转换开销；未转换的镜像自动走 overlayfs 路径）
- 转换非阻塞：镜像首次使用时若无 Nydus 版本，直接标准拉取，同时后台触发转换

### 4.2 缓存管理

```
/var/lib/bean/cache/
├── content/        containerd content store（层 blob）
├── snapshots/      overlayfs/nydus 快照目录
└── nydus-chunks/   nydus blob chunk 缓存（S3 range-read 结果）
```

- LRU 以「镜像」为粒度记账（层被多镜像共享时引用计数），chunk 缓存独立 LRU
- 水位控制：>85% 触发后台 GC，>95% 拒绝新 PULLING 并上报调度器
- 缓存清单摘要：心跳携带本地镜像 ref 集合的增量 + 布隆过滤器（调度器做镜像亲和用）

### 4.3 Prewarm

- 收到 PrewarmImage 指令后按 priority 入队，受专用带宽/并发限制（不与在线 PULLING 抢）
- Nydus 镜像 prewarm = 拉元数据 + 预取热 chunk（有 access pattern 记录时）或全量 chunk

## 5. 网络模块

### 5.1 数据面

```
创建：
1. ip netns add bean-<id>
2. veth 对：veth-<id> (host) ↔ eth0 (netns)，母桥 bean0 (10.100.0.1/24)
3. netns 内配 IP（节点本地 IPAM，位图分配）、默认路由 → 10.100.0.1
4. resolv.conf 生成挂入（上游 DNS 可配，默认节点 systemd-resolved 或 1.1.1.1）
5. /etc/hosts 注入 sandbox hostname
```

### 5.2 nftables 规则（每节点一套 + per-sandbox 链）

```
table inet bean {
  chain forward {
    # sandbox → 公网：SNAT 出网（masquerade 在 nat 表）
    iifname "bean0" oifname != "bean0" ct state new accept
    # 禁止 sandbox ↔ sandbox
    iifname "bean0" oifname "bean0" drop
    # 禁止访问节点内网段与元数据服务
    iifname "bean0" ip daddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16, 169.254.169.254 } drop
      # 注：10.100.0.1（网关自身）与 DNS 例外在前置 accept 规则处理
  }
  chain input {
    # 仅放行 sandbox → 网关的 DNS/agent 必要端口
  }
}
```

- `networkPolicy: none` → netns 无默认路由，纯本地回环
- `allow-list`（预留）→ per-sandbox 链插入目标 CIDR accept
- 端口暴露不开入站 DNAT——数据面走 agent ForwardPort（见 api-design.md §6），
  节点防火墙入站只对 control plane/gateway 内网开放

### 5.3 fcRuntime 兼容

FC microVM 用 tap 设备替代 veth 的 netns 端，同样挂 bean0 桥，nftables 规则不变。

## 6. bean-agent

### 6.1 注入与启动

1. beand 发布目录 `/var/lib/bean/agent/<version>/bean-agent`（静态编译，musl，≈8 MiB）
2. OCI spec 增加只读 bind mount：`/var/lib/bean/agent/<ver>/bean-agent → /.bean/agent`
   以及 socket 目录 `/run/bean/<id>/ → /.bean/run/`（读写）
3. entrypoint override 为 `/.bean/agent`；原 image 的 entrypoint/cmd/env/user/workdir
   序列化进 spec annotation，由 agent 读取
4. agent 启动即 listen unix socket `/.bean/run/agent.sock`（beand 从 host 侧
   `/run/bean/<id>/agent.sock` 直连），上报 Ready
5. `autoStartCmd=true` 或收到 StartUserProcess 时，agent 按原 entrypoint 语义
   fork 用户进程（setuid 到镜像 USER、应用 env/workdir）

版本升级：agent 随 beand 包发布，目录带版本号，运行中 sandbox 不受影响（旧版本目录保留至无引用）。

路径冲突：`/.bean` 若与镜像内容冲突（极罕见），创建失败并明确报错，可配置备用挂载点。

### 6.2 PID1 职责

- **僵尸回收**：`SIGCHLD` reap 所有孤儿
- **信号转发**：SIGTERM → 用户进程组，graceful 超时后 SIGKILL
- **进程管理**：exec 会话表（id → 进程组），支持 signal/kill 单会话

### 6.3 Exec / PTY

- 普通 exec：`os/exec` + pipe，stdout/stderr 分流，输出限额截断
- PTY：`creack/pty`，resize 帧调 `TIOCSWINSZ`；会话绑定 WS 连接，
  连接断开保留会话 60s 可重连（reconnect token）
- 并发 exec 无全局锁，per-sandbox 上限（默认 32 会话）

### 6.4 文件操作

- 流式 gRPC chunk（1 MiB/帧），保留 mode/uid/gid；目录树操作提供 `tar` 模式
  （上传 tar 自动解包、下载目录打 tar）——eval 批量注入 repo 快照的主路径
- 大文件产物直推 S3：agent 收到含 presigned URL 的指令后在容器内直接
  PUT（走 sandbox 出网路径），不占 beand 带宽

### 6.5 日志

- 用户进程 stdout/stderr → 环形缓冲（8 MiB）+ 可选实时流
- 销毁前 beand 触发 agent 将全量日志经 presigned URL 归档 S3

### 6.6 传输层抽象

agent 的 gRPC listener 抽象为 `Transport`（unix socket 实现 / vsock 实现），
fcRuntime 落地时 agent 代码零改动，只换 transport 与注入方式（initrd 或 virtiofs）。

## 7. 心跳、租约与 reconcile

### 7.1 心跳

- 双向流，间隔 3s;携带:资源水位、各 sandbox {id, state, 资源用量摘要}、
  镜像缓存增量、正在执行的 command ids
- 控制面 15s（5 个周期）未收到 → 节点 SUSPECT → 30s → LOST：
  其上 RUNNING sandbox 标 LOST、调度停止派发
- 网络闪断恢复：流重建后全量状态上报一次

### 7.2 beand 重启 reconcile

```
1. 读 containerd namespace "bean" 全量 task/container 列表
2. PullCommands 拿控制面期望状态
3. 三向对账：
   - 双方都有 & 状态一致 → 重挂 agent socket、恢复监控
   - 控制面有、本地无 → 上报 FAILED（由上层决定重建）
   - 本地有、控制面无（孤儿）→ 销毁 + 清理 netns/挂载/IPAM
4. 全量上报，恢复心跳
```

netns/veth/nftables 链均带 `bean-<id>` 命名规约，孤儿扫描按前缀比对存活 sandbox 集合。

### 7.3 GC 触发器

| 对象 | 策略 |
|---|---|
| sandbox 超时 | beand 本地定时器（expiresAt 随指令下发），到期即销毁并上报——不依赖控制面在线 |
| 镜像/chunk 缓存 | §4.2 水位 LRU |
| exec 会话 | 断连 60s 无重连 |
| 临时文件（S3 暂存下载） | S3 lifecycle 规则 1 天 |
| Postgres 终态 sandbox 记录 | 控制面归档任务，30 天转冷 |

## 8. beand 自身可观测

- Prometheus exporter：sandbox 创建各阶段耗时直方图（拉镜像/rootfs/容器启动/agent ready）、
  缓存命中率、nftables 规则数、IPAM 使用率
- 结构化日志（zap），request_id 透传
- pprof 端口（内网）

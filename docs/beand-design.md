# beand（Node Daemon）与 bean-agent 详细设计

> beand：每节点一个的守护进程，sandbox 生命周期的实际执行者。
> bean-agent：sandbox 内 PID1，exec/文件/端口的执行末端。

## 1. beand 总体结构

```
beand
├── server/          gRPC server（NodeService client 侧 + 数据面 SandboxService 实现）
├── runtime/         Runtime 接口 + fc/runc/runsc 实现
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
| KVM | `/dev/kvm` 可打开 | fc 档位（默认主档） |
| runsc | 二进制存在 + `runsc --version` | 无 KVM 降级档；无 KVM 时自动 `--platform=ptrace` |
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
| `fcRuntime`（主档） | 自研：beand 直接管 firecracker 进程 + jailer | overlaybd 块设备 virtio-blk 直挂;vsock 通 agent;memory snapshot/fork 原生（见 §3.1） |
| `runcRuntime` | containerd task API + runc shim | GPU 路径 + 可信任务;P0 链路基线;Checkpoint 用 CRIU |
| `runscRuntime` | containerd + runsc shim (`io.containerd.runsc.v1`) | 无 KVM 节点降级档;Checkpoint 用 gVisor 自带 save/restore |

要点：

- containerd 对 fcRuntime 只提供**镜像半边**（overlaybd-snapshotter 组装块设备），
  VM 生命周期由 beand 自管——不走 kata/firecracker-containerd 的 shim 嵌套
- `RootfsMount` 由 image 模块产出（见 §4），与 Runtime 解耦：容器档拿到 overlayfs
  挂载点，fcRuntime 拿到块设备路径——同一条镜像链路
- OCI spec / VM spec 生成集中在一处（资源限制、mount、seccomp、agent 注入），
  runtime 实现只做增量修改

### 3.1 fcRuntime 细节

**启动链路**

```
1. image 模块产出块设备：overlaybd base（只读，lazy-pull S3）+ 可写 overlay 盘
2. beand 起 jailer→firecracker：guest 内核（平台统一打包，6.x 全功能配置）、
   virtio-blk×2、vsock、tap 网卡、virtio-balloon
3. guest 内 bean-agent 作为 init：
   a. 挂载矩阵：/proc /sys /dev /dev/shm /dev/pts /dev/mqueue /tmp
      （按 OCI runtime spec 默认 mounts 复刻）
   b. 切根到镜像 rootfs（base+overlay 已在 host 侧组好，guest 见单一块设备）
   c. 读启动参数（vsock 首连推送）：image config + sandbox spec
   d. 应用 ENV/hostname/resolv.conf/hosts;listen vsock
   e. autoStartCmd → 按 USER/WORKDIR/Entrypoint+Cmd 语义拉起用户进程
```

**容器兼容性矩阵**

| 项 | 兼容性 | 说明 |
|---|---|---|
| ENV/ENTRYPOINT/CMD/USER/WORKDIR | ✅ | agent 按 image config 复刻，与容器档同一份代码 |
| 文件系统/权限/uid-gid | ✅ | 块设备原样挂载 |
| /proc /sys /dev | ✅ | 真内核，比 gVisor 模拟更全 |
| 动态链接/glibc/musl | ✅ | 用户态不变 |
| 内核版本 | ⚠️ | guest 内核由平台提供，`uname -r` 非宿主;纯用户态负载无感 |
| VOLUME/EXPOSE/HEALTHCHECK | ➖ | 同容器档：忽略/仅元数据/不执行 |
| 镜像架构 | ❗ | 必须匹配节点 arch，无模拟（容器档同） |
| GPU | ❌ | FC 无 passthrough → auto 解析自动落容器档 |

## 3.2 资源模型（cpu / mem 配置）

**API 层**：`resources: {cpu, memoryMiB, gpu}` 创建时声明、**不可变**（FC 不支持热调整,
容器档为保持语义一致同样不开放热调整）。

| 档 | cpu 执行 | mem 执行 |
|---|---|---|
| 容器档 | cgroup v2 `cpu.max`（硬）+ `cpu.weight` | `memory.max` + `memory.swap.max=0` |
| fc 档 | vCPU 数 = ceil(cpu)，宿主侧 FC 进程再包 cgroup（cpu.max 双保险 + weight 公平） | guest 内存 = memoryMiB;virtio-balloon 空闲回收 |

**超卖策略（全部为配置项，非硬编码）**

```yaml
# beand.yaml（节点级覆盖）/ 调度器全局默认
overcommit:
  cpu: 3.0        # allocatable = 物理核 × 该系数;1.0 = 不超卖
  memory: 1.0     # 内存默认不超卖（fc 档 balloon 回收不改承诺量记账）
```

- CPU：eval 突发型负载默认 3.0;CPU 密集型节点池可配 1.0;cgroup cpu.weight 按
  规格比例分配保公平;`dedicated: true`（预留字段）→ vCPU pin，不参与超卖
- 内存：容器档按 RSS 实际水位天然复用;fc 档靠 balloon——beand 周期驱动
  气球回收空闲 guest 内存,调度器按「规格承诺量」与「气球后实际占用」双水位记账:
  新建看承诺量（保证 resume/突发有量），告警看实际占用
- 系数变更仅影响后续调度决策，存量 sandbox 不受影响;调低导致承诺量超出新
  allocatable 时节点标记「不再接单」自然排空
- PAUSED：两档都不释放内存额度（防 resume OOM）;要释放走 snapshot
- 规格上限：单 sandbox ≤ 32 vCPU / 128 GiB(fc 档 FC 自身约束;容器档同限保持一致)

**存储（disk）**

API 层增加 `resources.diskMiB`（可写层上限，默认 20 GiB）：

| 档 | 可写层实现 | 配额执行 |
|---|---|---|
| 容器档 | overlayfs upper dir | XFS project quota（硬限制） |
| fc 档 | 可写 overlay 盘（sparse 稀疏文件，预建 ext4） | 盘大小即上限，天然硬限;稀疏文件按实际写入占宿主空间 |

- 节点盘分池：`cache/`（镜像 chunk，LRU 可回收）与 `sandboxes/`（可写层，
  生命周期绑定 sandbox）分开记账——缓存永远可牺牲，可写层不可
- 可写层用量随心跳上报（调度器盘水位依据）;写满行为：容器档 ENOSPC、
  fc 档 guest 内 ENOSPC，均不影响宿主
- tmpfs：`/dev/shm` 默认 64 MiB 计入内存配额，可配

## 3.3 Volume（独立的一等资源）

**资源模型**：镜像与卷是两种正交资源——镜像定义环境（rootfs，不可变，随 sandbox
销毁），卷承载数据（独立生命周期，先于 sandbox 存在、后于 sandbox 留存，可被
多个 sandbox 同时挂载）。卷有独立的 CRUD API 与配额：

```
POST   /volumes        { "name": "swebench-data", "type": "shared-fs"|"dataset",
                         "quotaMiB": ..., "readOnly": ... }
GET    /volumes / DELETE /volumes/{id}

POST /sandboxes { ..., "volumes": [
  { "volume": "vol_...", "subPath": "run-0731", "mountPath": "/workspace", "readOnly": false }
] }
```

**两种类型：**

| 类型 | 后端 | 语义 | 场景 |
|---|---|---|---|
| `shared-fs` | JuiceFS（on S3+Redis，与 S3 底座一致）或 CephFS，平台配置，用户不感知 | POSIX 读写共享 | 持久工作区、跨 sandbox 共享数据 |
| `dataset` | overlaybd 只读块（实现上复用镜像的 S3 传输/缓存管道，但资源上是独立对象） | 只读、版本化发布 | 数据集/模型权重海量只读消费 |

**挂载路径按档位分流：**

| 档 | shared-fs | dataset |
|---|---|---|
| 容器档 | 宿主挂载（每节点一个挂载点，beand 管理）→ bind mount subPath | 块设备挂载 → bind mount |
| fc 档 | **guest 内直挂**：agent 跑 JuiceFS/ceph 客户端，走 sandbox 出网连元数据服务与 S3（FC 无 virtiofs，宿主透传不可行） | 多挂一块 virtio-blk，天然支持 |

fc 档 shared-fs 要点：

- 客户端二进制随 agent 盘注入（不进用户镜像）;挂载凭证为 volume 级短时 token
  （控制面签发，只授权该 volume 目录），经 vsock 下发,不落盘
- 网络策略例外：`egress-only`/`none` 均放行 guest → 文件系统元数据服务/S3 的
  白名单地址（nftables per-sandbox 链插入）
- 性能预期明示：guest 内 FUSE + 网络路径吞吐低于容器档 bind mount;
  只读大数据集优先用 dataset 卷
- 挂载失败属 sandbox 创建失败（FAILED,带明确 reason）

**不做**：sandbox 间实时协作锁语义——共享写一律经 shared-fs 后端落盘,
一致性由后端文件系统保证。

**调度联动**：shared-fs 无节点亲和（网络文件系统全节点可达）;dataset 卷参与
镜像亲和打分（块缓存命中）。

## 4. 镜像模块

### 4.1 overlaybd 主路线 + 双格式兜底

| 格式 | 消费方式 | 场景 |
|---|---|---|
| overlaybd（块级，DADI） | 容器档：overlaybd-snapshotter;fc 档：块设备 virtio-blk 直挂 | 主路径;blob 存 S3，ublk 按需 range-read |
| 标准 OCI（gzip 层） | overlayfs snapshotter（仅容器档） | 兜底：未转换镜像、overlaybd 故障降级 |

- image-service（control plane 逻辑模块，见 4.4）负责把镜像**离线转换**为
  overlaybd 格式（`convertor` 工具，层级转换可增量）;转换在服务端做一次，
  节点侧零转换开销
- 转换非阻塞：镜像首次使用时若无 overlaybd 版本，容器档走标准拉取先跑起来
  （fc 档等待转换完成或直接报错提示 prewarm），同时后台触发转换
- 可写层：容器档 overlayfs upper（XFS quota）;fc 档独立稀疏 overlay 盘（见 §3.2）

### 4.2 缓存管理

```
/var/lib/bean/
├── cache/               # 可牺牲池（LRU）
│   ├── content/         #   containerd content store（标准层 blob，兜底路径）
│   ├── snapshots/       #   overlayfs/overlaybd 快照目录
│   └── obd-cache/       #   overlaybd 块 chunk 缓存（S3 range-read 结果）
└── sandboxes/           # 不可牺牲池：可写层/overlay 盘，生命周期绑定 sandbox
```

- LRU 以「镜像」为粒度记账（块被多镜像共享时引用计数），chunk 缓存独立 LRU
- 水位控制：cache 池 >85% 触发后台 GC，>95% 拒绝新 PULLING 并上报调度器;
  sandboxes 池余量是调度硬约束
- 缓存清单摘要：心跳携带本地镜像 ref 集合的增量 + 布隆过滤器 + 字节占比（调度器镜像亲和用）

### 4.3 Prewarm

- 收到 PrewarmImage 指令后按 priority 入队，受专用带宽/并发限制（不与在线 PULLING 抢）
- overlaybd prewarm = 拉元数据 + 预取热块（有 access trace 时按 trace,否则全量）
- overlaybd 原生支持记录启动 IO trace（`record-trace`）,首次运行采集、
  后续按 trace 精准预取——对固定 eval 镜像集效果显著

### 4.4 image-service 部署形态

image-service 是 **control plane 的逻辑模块**（`internal/control/image`），非独立
部署服务;P0–P2 内嵌 bean-api 进程。职责需要全局视角所以不能下放节点：

- 格式转换全局去重（一个镜像只转一次，多节点不打架）
- prewarm 编排需要全节点缓存视图
- S3 blob GC 需要全局引用计数（镜像 ↔ blob ↔ 运行中 sandbox/snapshot）

转换任务 CPU 重,量大后可拆独立 worker 池水平扩展（接口已按模块边界隔离）。

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
fc 档 agent 代码零改动，只换 transport（vsock）与注入方式（agent 盘/initrd，见 §3.1）。

## 7. 心跳、租约与 reconcile

### 7.1 心跳

- 双向流，间隔 3s;携带:资源水位、各 sandbox {id, state, 资源用量摘要}、
  镜像缓存增量、正在执行的 command ids
- 控制面 15s（5 个周期）未收到 → 节点 SUSPECT → 30s → LOST：
  其上 RUNNING sandbox 标 LOST、调度停止派发
- 网络闪断恢复：流重建后全量状态上报一次;控制面在此期间的直连指令失败会重试,超过阈值触发重调度

### 7.2 beand 重启 reconcile

```
1. 读 containerd namespace "bean" 全量 task/container 列表
2. SyncState 拿控制面期望状态
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

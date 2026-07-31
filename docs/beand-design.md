# beand（Node Daemon）与 bean-agent 详细设计

> beand：每节点一个的守护进程，sandbox 生命周期的实际执行者。
> bean-agent：sandbox 内 PID1，exec/文件/端口的执行末端。

## 1. beand 总体结构

```
beand
├── server/          gRPC server（NodeService client 侧 + 数据面 SandboxService 实现）
├── runtime/         Runtime 接口 + fc/runc/runsc 实现
├── image/           ublk 设备/overlaybd 配置管理、S3 blob 缓存、prewarm
├── network/         netns/veth/bridge/nftables/tc 编排
├── volume/          shared-fs 宿主挂载 + NFS 导出、dataset 块设备 attach
├── sbxproxy/        节点侧端口反代：regional proxy → 直连 sandbox IP:port
├── agentmgr/        agent 注入（bind mount / agent 盘）、socket/vsock 管理、健康探测
├── reconcile/       期望状态 vs 本地实际状态（containerd ∪ FC 进程）对账
├── gc/              超时回收、孤儿资源清理、缓存 LRU
└── report/          能力探测、心跳、资源水位、缓存清单摘要
```

单二进制 `beand`，systemd 管理，配置文件 `/etc/bean/beand.yaml`：

```yaml
nodeId: auto            # 默认机器指纹生成
region: ap-east-1       # 一级字段（数据域/故障域）,Register 时控制面校验:
                        #   region 必须已注册（S3/proxy 组已配置）,否则拒绝加入;
                        #   节点生命周期内不可变（迁 region = 退出重注册）
labels:                 # 自由标签,调度 nodeSelector 过滤/打分用（可运维更新）
  pool: gpu-a100
  disk: nvme
bootstrapToken: <region bootstrap token>        # 首次注册用,见 §7.0
controlPlane: grpcs://<hosted-gateway>:443
                        # 云上托管 gRPC 接入层（nexus 同款模式,示例:
                        # grpc-bean.internal....:443）,TLS 由托管网关终结;
                        # beand 出向长连,指令经 CommandChannel 多路复用下发
s3:
  endpoint: https://s3.ap-east-1.example.com    # 本 region S3 backend
containerd: null        # 可选:仅容器档节点配置（GPU/无 KVM,P5）;纯 fc 节点不装
cidr: 10.100.0.0/24     # 本节点 sandbox 网段（节点间可复用同段——跨节点 sandbox 互通是非目标）
cache:
  dir: /var/lib/bean/cache
  maxBytes: 800Gi        # 裸金属 NVMe 大盘 / 云 VM 小盘只差这个数
runtimes: auto           # 或显式列表覆盖探测结果
overcommit:              # 见 §3.2，节点级覆盖调度器全局默认
  cpu: 3.0
  memory: 1.0
network:
  egressRateMbps: 100    # per-sandbox tc 限速
  dnsUpstream: []        # 默认节点转发器,上游可配
```

（示例为节选,完整 schema 见 deploy/）

## 2. 能力探测（启动时）

| 探测项 | 方法 | 影响 |
|---|---|---|
| KVM | `/dev/kvm` 可打开 | fc 档位（默认主档） |
| runsc | 二进制存在 + `runsc --version` | 无 KVM 降级档；无 KVM 时自动 `--platform=ptrace` |
| NVMe/磁盘 | 缓存目录所在盘类型 + 可用空间 | 调度缓存盘权重 |
| GPU | NVML 枚举 | GPU 资源画像 + nvidia 运行时注入 |
| cgroup v2 | `/sys/fs/cgroup/cgroup.controllers` | 强制要求 v2，v1 直接拒绝启动 |
| ublk/tcmu | /dev/ublk-control、target_core_user | overlaybd 后端选择;两者皆无 → 不上报 fc 能力（fc 依赖块设备） |
| 内核版本/erofs | `uname` + /proc/filesystems | agent 盘（erofs）;overlayfs 仅容器档 P5 |

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
| `fcRuntime`（主档,P0 起点） | 自研：beand 直接管 firecracker 进程 + jailer;**无 containerd** | overlaybd ublk 块设备 virtio-blk 直挂;vsock 通 agent;memory snapshot/fork 原生（见 §3.1） |
| `runcRuntime`（P5 按需,GPU/可信） | containerd task API + runc shim | Checkpoint 用 CRIU;需节点装 containerd |
| `runscRuntime`（P5,无 KVM 降级） | containerd + runsc shim | Checkpoint 用 gVisor save/restore |

要点：

- **fc 热路径零 containerd**：image 模块直驱 overlaybd ublk daemon（AgentENV
  的 uvm-ublk/uvm-ublk-daemon 同款,本地源码 /Users/mac/project/agentenv 可参考）;
  containerd 是容器档的可选依赖,纯 fc 节点不装
- `RootfsMount` 由 image 模块产出（见 §4）,与 Runtime 解耦：fcRuntime 拿块设备
  路径,容器档拿 overlayfs 挂载点——同一条镜像链路
- VM spec / OCI spec 生成集中在一处,runtime 实现只做增量修改

### 3.1 fcRuntime 细节

**启动链路**

```
1. image 模块产出 rootfs 块设备：overlaybd base 层（lazy-pull S3）+ overlaybd
   可写层在宿主合成【单一块设备】（拍板：host 侧组装,e2b/AgentENV 同款;
   配额=可写层文件大小,snapshot disk-diff 直接取宿主层）
2. beand 起 jailer→firecracker：
   virtio-blk: rootfs 盘 + agent 盘(只读 erofs,含 bean-agent 与工具) 
               + N 个 dataset 卷盘（如有）
   vsock、tap 网卡、virtio-balloon;guest 内核（平台统一打包,见 §3.4）
   kernel cmdline: init=/run/bean-agent（agent 盘由内核挂载后执行）
3. guest 内 bean-agent 作为 init：
   a. 挂载矩阵：/proc /sys /dev /dev/shm /dev/pts /dev/mqueue /tmp
      （按 OCI runtime spec 默认 mounts 复刻）
   b. 挂载 rootfs 盘并切根（guest 只见一块 rootfs 盘，零 union 逻辑）
   c. 读启动参数（vsock 首连推送）：image config + sandbox spec + 卷挂载表
   d. 应用 ENV/hostname/resolv.conf/hosts;挂载卷（dataset 盘 / NFS）;listen vsock
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

**类型：首期仅 `shared-fs`**;`dataset`（overlaybd 只读块，复用镜像管道）为预留
类型暂不排期——大数据集只读场景先用 shared-fs 或打进镜像，需求明确后再启用。

| 类型 | 后端 | 语义 | 场景 |
|---|---|---|---|
| `shared-fs` | JuiceFS（on S3+Redis，与 S3 底座一致）或 CephFS，平台配置，用户不感知 | POSIX 读写共享 | 持久工作区、跨 sandbox 共享数据 |
| `dataset`（预留） | overlaybd 只读块 | 只读、版本化发布 | 数据集/权重海量只读消费 |

**shared-fs 数据面：宿主 NFS 导出（e2b 同款路线,经其源码验证）**

```
后端（宿主挂载,beand volume 模块管理）：JuiceFS(on S3+Redis) / CephFS / 本地盘
    ▼
宿主内核 nfsd 导出 per-volume 目录（拍板：内核 nfsd,零用户态开销、成熟度最高;
    配额由后端执行——JuiceFS 目录配额/CephFS quota）
    ▼  NFS 流量仅走 sandbox→宿主网关（virtio-net/veth,不出节点）
guest/容器内 agent 执行: mount -t nfs -o fg,hard <宿主网关IP>:/<volumeName> <mountPath>
```

选宿主 NFS 而非 guest 内跑 JuiceFS 客户端的理由：

- **guest 零凭证零额外二进制**——只需内核 NFS client（Linux 标配）;存储凭证只在宿主
- **`none` 策略天然兼容**——NFS 目标是宿主网关,与「出公网」正交,零外传承诺不破
- **宿主客户端缓存全 sandbox 共享**——同批 eval 读同数据,宿主拉一次全员命中
  （guest 内独立客户端则 N 份缓存 N 份回源）
- 后端可换（JuiceFS/CephFS/本地盘），beand 只见宿主路径
- 代价：多一跳 NFS 协议;小文件/元数据密集负载偏慢——该类负载引导到可写层
  （dataset 卷启用后,大流量只读再迁过去）

**挂载矩阵：**

| 档 | shared-fs | dataset（预留） |
|---|---|---|
| 容器档 | 直接 bind mount 宿主挂载点 subPath（跳过 NFS） | 块设备挂载 → bind mount |
| fc 档 | guest 内核 NFS client 挂宿主导出 | 附加一块 virtio-blk |

- 配额：后端执行（JuiceFS 目录配额 / CephFS quota）,nfsd 层不做拦截
- 挂载失败属 sandbox 创建失败（FAILED,带明确 reason）
- nftables：sandbox → 宿主网关 NFS 端口的 accept 规则仅对挂了 shared-fs 卷的
  sandbox 插入（per-sandbox 链）

**不做**：sandbox 间实时协作锁语义——共享写一律经后端文件系统落盘保证一致性。

**调度联动**：shared-fs 无节点亲和（后端全节点可达）。

## 3.4 Guest 内核与 agent 盘的构建发布

fc 档两个平台工件,均由 CI 构建、S3 分发、beand 启动时按版本拉取到本地：

| 工件 | 内容 | 构建 | 版本策略 |
|---|---|---|---|
| guest 内核 | 6.x LTS,内嵌 virtio/vsock/nfs/overlayfs 等必需项的精简 config,bzImage | 内核源码 + config 入库,CI 复现构建 | 独立版本号;manifest 记录,snapshot restore 校验一致性 |
| agent 盘 | erofs 只读镜像:bean-agent 静态二进制 + busybox 级工具 | CI 打包,与 beand 同版本发布 | 随 beand 版本;旧版本保留至无运行中引用 |

- 存放：`s3://bean/artifacts/{kernel,agent-disk}/<version>/` + sha256 校验
- beand 配置声明版本（默认跟随 beand 发布版），本地缓存 `/var/lib/bean/artifacts/`
- 容器档的 agent 直接用 agent 盘内同一个二进制 bind mount，两档单一构建产物

## 4. 镜像模块

### 4.1 overlaybd 直驱主路线

image 模块直接管理 overlaybd ublk daemon（不经 containerd snapshotter）：
按镜像元数据（控制面下发的层清单 + S3 blob 引用）生成 overlaybd config →
ublk 设备就绪 → 交给 runtime。实证细节参考本地 AgentENV 源码
（`src/overlaybd/`、crates 下 uvm-ublk,以及 registryfs_v2 远端直读模式）。

| 格式 | 消费方式 | 场景 |
|---|---|---|
| overlaybd（块级，DADI） | fc 档：ublk 块设备 virtio-blk 直挂;容器档（P5）：同设备挂载 | 主路径;blob 存 S3，ublk 按需 range-read |
| 标准 OCI（gzip 层） | containerd overlayfs（仅容器档,P5） | 兜底：未转换镜像 |

- image-service（control plane 逻辑模块，见 4.4）负责把镜像**离线转换**为
  overlaybd 格式（`convertor` 工具，层级转换可增量）;转换在服务端做一次，
  节点侧零转换开销
- 转换非阻塞：镜像首次使用时若无 overlaybd 版本,fc 档（主）等待转换或明确报错
  提示先 prewarm,同时后台触发转换（容器档标准拉取兜底,P5）
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
4. resolv.conf 指向节点 DNS 转发器（可审计;上游可配,默认 1.1.1.1）
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

- 前提：启用 `br_netfilter`（桥接流量过 forward 链,否则同桥 sandbox↔sandbox
  二层直通绕过规则）;节点 bootstrap 脚本固化该 sysctl
- 带宽/连接数：per-sandbox tc 出口限速（配置项,默认 100 Mbps）+ conntrack
  连接数上限（防扫描/DDoS 放大）
- `networkPolicy: none` → netns 无默认路由，纯本地回环（宿主 NFS/网关地址除外,见 §3.3）
- `allow-list`（预留）→ per-sandbox 链插入目标 CIDR accept
- 端口暴露不开入站 DNAT——regional proxy → beand sbxproxy → 直连 sandbox IP
  （见 api-design.md §6.2）;节点防火墙入站仅对 control plane/proxy 开放

### 5.3 fcRuntime 兼容

FC microVM 用 tap 设备替代 veth 的 netns 端，同样挂 bean0 桥，nftables 规则不变。

## 6. bean-agent

### 6.1 注入与启动

> 本节描述**容器档注入**（bind mount + entrypoint override,随 P5 引入）;
> fc 主路径的 agent 盘注入见 §3.1/§3.4。

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
fc 档 agent 代码零改动，只换 transport（vsock）与注入载体（agent 盘，拍板见 §3.1/§3.4）。

## 7. 注册、心跳、租约与 reconcile

### 7.0 节点注册与凭证分层

```
管理员：控制面注册 region（S3 endpoint、proxy 组、BYOC token 服务地址）
      → 生成 region bootstrap token（短 TTL 24h,可限次数,可撤销）
节点：beand 配置 token 启动 → Register(token, region, capabilities, labels)
    → 控制面校验 region 已注册 + token 有效（BYOC region 可配人工 approve）
    → 控制面签发 node token（短期,绑定 nodeId+region）
    → 后续所有 RPC 携带 node token metadata,心跳自动续期
```

**传输与身份分层（顺应云上托管接入层,不引入 mTLS）**：

- 传输层：TLS 单向——控制面经托管 gRPC 接入层暴露（网关终结 TLS）,
  节点用系统 CA 验证服务端,**零证书配置**（与现有 nexus 边缘节点模式一致）
- 节点身份：应用层 node token（内存持有不落盘,重启重新 Register）;
  控制面按 token↔nodeId 绑定校验——节点只能上报/操作调度到自己的 sandbox
- token 泄漏面：短期 + 绑定 nodeId,冒用需同时伪造心跳流;可即时吊销

凭证三层,职责不重叠：

| 凭证 | 权限 | 生命周期 |
|---|---|---|
| region bootstrap token | **仅 Register**（registration-only,无任何数据读写） | 短 TTL + 限次;泄漏最坏结果=注册假节点,而任务 presigned 只授权自身路径,再加 approve 闭环 |
| node token | 心跳/指令/SyncState 身份,绑定 nodeId+region | 短期,心跳续期;内存持有;退出/异常即吊销 |
| S3 访问 | 镜像 blob=STS 只读（限 region bucket 前缀）;写产物/snapshot=per-操作 presigned | STS 1h;presigned 15min |

BYOC：客户节点出向连托管接入层即可（443,零证书配置）,身份全在应用层 token。

### 7.1 心跳

- 双向流，间隔 3s;携带:资源水位、各 sandbox {id, state, 资源用量摘要}、
  镜像缓存增量、正在执行的 command ids
- 控制面 15s（5 个周期）未收到 → 节点 SUSPECT → 30s → LOST：
  其上 RUNNING sandbox 标 LOST、调度停止派发
- 网络闪断恢复：流重建后全量状态上报一次;控制面在此期间的直连指令失败会重试,超过阈值触发重调度

### 7.2 beand 重启 reconcile

```
1. 枚举本地实际状态：存活 firecracker 进程（jailer 目录 /run/bean/fc/<id>/ +
   pidfile 规约,fc 档主路径）∪ containerd task（容器档,如启用）
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
| sandbox idle | beand 本地 idle 检测（lifecycle 随 create 下发）:无 exec/端口/文件活动持续 idleTimeout → 执行 onIdle(pause/kill) 并发 event——不依赖控制面在线 |
| PAUSED 滞留 | 默认不回收;管理员可选开启全局策略（P4 后由 snapshot 归档替代） |
| 镜像/chunk 缓存 | §4.2 水位 LRU |
| exec 会话 | 断连 60s 无重连 |
| 临时文件（S3 暂存下载） | S3 lifecycle 规则 1 天 |
| Postgres 终态 sandbox 记录 | 控制面归档任务，30 天转冷 |

## 8. beand 自身可观测

- OTLP 导出（Prometheus 端点保留）：sandbox 创建各阶段耗时直方图（拉镜像/rootfs/启动/agent ready）、
  缓存命中率、nftables 规则数、IPAM 使用率
- per-sandbox 资源时序（cgroup/FC stats → OTLP,attributes 带 sandbox_id/labels）;
  agent 可选透传 sandbox 内应用 OTLP（localhost:4317 → vsock 转发）
- 结构化日志（zap），request_id 透传
- pprof 端口（内网）

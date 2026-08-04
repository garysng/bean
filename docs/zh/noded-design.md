# noded（Node Daemon）与 beand（In-Sandbox Daemon）详细设计

> **noded**：每节点一个的守护进程（二进制 `noded`），sandbox 生命周期的实际执行者。
> **beand**：sandbox 内的 init/PID1（二进制 `beand`），exec/文件/端口的执行末端。
> 命名约定：**noded 在宿主上，beand 在 sandbox 内**。

> 状态标注约定见 [architecture.md](architecture.md) §0。

## 1. noded 总体结构 ⚠️

**实际的包结构**(`internal/node/`):

```
internal/node/
├── manager.go       ✅ Manager:生命周期编排(创建/销毁/pause/resume/snapshot/
│                       restore、透明唤醒、idle 回收、in-flight 保护)
├── grpc.go          ✅ SandboxService 实现 + 数据面透传到 agent
├── register.go      ✅ 出向注册、心跳、SyncState 对账
├── auth.go / dial.go ✅ node token 鉴权、agent 连接
├── runtime/         ✅ Runtime 接口 + fc(真 microVM)与 local(进程级,仅开发)
│                       含 UFFD 供页、快照 bundle、CPU template、diff 合并
├── image/           ✅ image.Provider:DevMapperProvider(共享 base + CoW)、
│                       FileProvider、PullingProvider;OCI 拉取转换、commit、build
├── vsock/           ✅ AF_VSOCK 拨号
├── agentmgr/        📐 空目录
└── lifecycle/       📐 空目录
```

**之前这里列的 `network/`、`volume/`、`sbxproxy/`、`reconcile/`、`gc/`、`report/`
都不存在**,而写法和已交付模块完全一样。这是本批文档最误导的一处:读者会认为
网络隔离已经在跑。其中 `reconcile` 与 `report` 的职责实际落在 `register.go`,
`gc` 的部分落在 `manager.go`(idle 回收),其余三个没有任何代码。

**配置方式:flag,不是 YAML。** 全仓库 `grep -rn yaml --include='*.go'` 为空,
不存在 `/etc/bean/noded.yaml`。实际参数(`cmd/noded/main.go`):

```
--listen / --control-plane / --node-token / --bootstrap-token / --region
--runtime fc|local          # 档位,不自动探测
--firecracker-bin / --kernel / --agent-disk
--base-dir / --image-dir    # sandbox 状态与基础镜像
--cpu / --memory-mib        # 可分配量,直接给数不是探测
--labels / --metrics
--cpu-template none|portable    # 掩 CPU 特征让内存快照可跨型号
--track-dirty-pages             # 允许增量快照,须 boot 前生效
--buildkit-addr                 # 空则本节点不接构建
--otlp-endpoint                 # 空则装 no-op tracer
--log-format / --log-level
```

下面这份 YAML 是**计划形态**(📐),保留是因为它记录了配置项的意图 ——
`overcommit` 一节现已实装(见 §3.2),其余尚未:

```yaml
nodeId: auto            # 📐 当前无此概念,节点 id 由控制面注册时分配
region: ap-east-1       # ✅ 有,是 --region
labels:                 # ✅ 有,是 --labels
  pool: gpu-a100
bootstrapToken: <...>   # ✅ 有,是 --bootstrap-token
controlPlane: grpcs://<hosted-gateway>:443   # ⚠️ 有 --control-plane,但无 TLS
s3:
  endpoint: https://...  # ⚠️ 走环境变量而非配置(凭证不进命令行)
containerd: null        # 📐 容器档未实现
cidr: 10.100.0.0/24     # 📐 无网络栈
cache:
  dir: /var/lib/bean/cache
  maxBytes: 800Gi        # 📐 无缓存 LRU;当前基础镜像不自动回收
runtimes: auto           # 📐 不探测,靠 --runtime 显式指定
overcommit:              # ✅ 已实装,见 §3.2
  cpu: 3.0
  memory: 1.0
network:                 # 📐 无网络栈
  egressRateMbps: 100
```

## 2. 能力探测（启动时）📐

**当前不探测。** 档位靠 `--runtime fc|local` 显式指定,可分配量靠 `--cpu`/`--memory-mib`
直接给数。唯一的运行时检查是 `DevMapperProvider.Available()`(查 dmsetup/losetup
存在且 `dmsetup targets` 里有 snapshot 目标),不上报也不影响档位选择 ——
缺了就是启动失败而非降级。

下表是计划:


| 探测项 | 方法 | 影响 |
|---|---|---|
| KVM | `/dev/kvm` 可打开 | fc 档位（默认主档） |
| runsc | 二进制存在 + `runsc --version` | 无 KVM 降级档；无 KVM 时自动 `--platform=ptrace` |
| NVMe/磁盘 | 缓存目录所在盘类型 + 可用空间 | 调度缓存盘权重 |
| GPU | NVML 枚举 | GPU 资源画像 + nvidia 运行时注入 |
| cgroup v2 | `/sys/fs/cgroup/cgroup.controllers` | 强制要求 v2，v1 直接拒绝启动 |
| ublk/tcmu | /dev/ublk-control、target_core_user | overlaybd 后端选择;两者皆无 → 不上报 fc 能力（fc 依赖块设备） |
| 内核版本/ext4 | `uname` + /proc/filesystems | agent 盘（ext4）;overlayfs 仅容器档 P5 |

探测结果 → `Register` 上报，之后仅在变化时重报。

## 3. Runtime 抽象 ✅

实际接口(`internal/node/runtime/runtime.go`):

```go
type Runtime interface {
    Name() string
    Create(ctx context.Context, spec *Spec) (*Handle, error)
    Destroy(ctx context.Context, id string, force bool) error
    Pause(ctx context.Context, id string) error
    Resume(ctx context.Context, id string) error
    Checkpoint(ctx context.Context, id string, w io.Writer, opts CheckpointOptions) error
    Restore(ctx context.Context, spec *Spec, layers []SnapshotLayer) (*Handle, error)
}
```

`Resume` 收一个 `id`、什么都不返回:它作用在本进程已经持有的那台 VM 上,还回来的是
同一台。`Restore` 收一整个 `*Spec`、返回一个新的 `*Handle`,因为它造出的 sandbox
本来不存在 —— 这也是为什么一份快照可以驱动任意多个并发的 `Restore`。签名本身就带着
这个区分,见 [snapshot-resume.md](snapshot-resume.md) §0。

与早先设计的三处差异,都是实现过程中发现原设计不对:

- **`Create` 不收 rootfs**。image provider 是 runtime 的字段而非参数 ——
  因为 rootfs 的组装时机与 runtime 内部状态耦合:restore 必须在**设备组装之前**
  把 CoW 填好(否则 dm-snapshot 的 exception table 已进内核,静默损坏文件系统,
  见 `docs/decisions.md` §3.0)。参数式接口无法表达这个顺序。
- **`Restore` 收 `[]SnapshotLayer` 而不是单个 reader**。增量快照要replay 整条链,
  每层是独立 gzip 流,一个 reader 读到第一层结束就停了。
- **没有 `Stats`**。没有实现,也没有调用方 —— 资源水位目前从心跳的承诺量记账走。

另有三个**可选**接口,runtime 按能力实现,调用方类型断言:
`ImageWarmer`(prewarm)、`ImageLister`(缓存清单)、`ImageBuilder`(构建)、
`SandboxCommitter`(封装成镜像)。分开而非塞进 `Runtime` 的理由:
local 档跑宿主进程,没有「缓存镜像」这个概念,让它 stub 掉四个方法
比让调用方判断能力说明的信息更少。

| 实现 | 状态 | 底层 | 职责边界 |
|---|---|---|---|
| `FCRuntime`（主档） | ✅ | noded 直接 exec firecracker 进程,**无 jailer**(见 security §A3)、无 containerd | dm-snapshot 块设备 virtio-blk 直挂;vsock 通 agent;full/diff/memoryless 快照 |
| `LocalRuntime`（开发/CI） | ✅ | 宿主进程 | **无隔离**,不可用于不可信代码 |
| `runcRuntime` | 📐 | containerd + runc shim | 未实现 |
| `runscRuntime` | 📐 | containerd + runsc shim | 未实现 |

要点：

- **fc 热路径零 containerd** ✅:image 模块自己管块设备,纯 fc 节点不装 containerd。
  但当前后端是 **dm-snapshot 而非 overlaybd ublk** —— overlaybd 能力已实测跑通
  (挂载 7ms、只传 19.6% 层字节)但尚未接进 `image.Provider`
  (`docs/decisions.md` §3.1)。
- `image.Rootfs` 由 image 模块产出(见 §4),带 `Device`(VM 挂的路径)与
  `Writable`(快照要抓的 CoW 层)两个字段。**`Writable` 是关键**:
  快照捕获它,而 restore 通过 `PrepareOptions.SeedWritable` 在设备组装前回填。

### 3.1 fcRuntime 细节 ⚠️

**启动链路**

```
1. image 模块产出 rootfs 块设备:**dm-snapshot** —— 共享只读 base(loop 挂载)
   + 每 sandbox 稀疏 CoW 文件,合成单一 `/dev/mapper/bean-<id>`。
   配额 = CoW 文件大小;快照抓的就是这个 CoW 层。
   (overlaybd lazy-pull 是目标形态,能力已实测但未接入)
2. noded 直接 exec firecracker(**无 jailer**,见 security §A3):
   virtio-blk: **agent 盘为 root device**(`agent.ext4`,含 beand)
               + 用户镜像为第二盘(guest 内 `/dev/vdb`)
   vsock;**无网卡、无 balloon**(网络栈未实现,balloon 未接)
   kernel cmdline: `init=/bean/beand -- --listen vsock:1024 --pivot ...`
3. guest 内 beand 作为 init：
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
| ENV/ENTRYPOINT/CMD/WORKDIR | ✅ | 转换时记录在镜像旁，创建时与请求合并;规则见 [image-pipeline §5](image-pipeline.md) |
| USER | 📐 | 已记录并携带，**未生效** —— 一切仍以 root 运行。需要在子进程里 fork-then-setuid，并读 guest 的 `/etc/passwd`，见 image-pipeline §5 |
| 文件系统/权限/uid-gid | ✅ | 块设备原样挂载 |
| /proc /sys /dev | ✅ | 真内核，比 gVisor 模拟更全 |
| 动态链接/glibc/musl | ✅ | 用户态不变 |
| 内核版本 | ⚠️ | guest 内核由平台提供，`uname -r` 非宿主;纯用户态负载无感 |
| VOLUME/EXPOSE/HEALTHCHECK | ➖ | 同容器档：忽略/仅元数据/不执行 |
| 镜像架构 | ❗ | 必须匹配节点 arch，无模拟（容器档同） |
| GPU | ❌ | FC 无 passthrough → auto 解析自动落容器档 |

## 3.2 资源模型（cpu / mem 配置）⚠️

**API 层**：`resources: {cpu, memoryMiB, gpu}` 创建时声明、**不可变**（FC 不支持热调整,
容器档为保持语义一致同样不开放热调整）。

| 档 | cpu 执行 | mem 执行 |
|---|---|---|
| 容器档 | cgroup v2 `cpu.max`（硬）+ `cpu.weight` | `memory.max` + `memory.swap.max=0` |
| fc 档 | vCPU 数 = ceil(cpu)，宿主侧 FC 进程再包 cgroup（cpu.max 双保险 + weight 公平） | guest 内存 = memoryMiB;virtio-balloon 空闲回收 |

**超卖策略 ✅ 已实装**(flag,不是 YAML):

```
--overcommit-cpu 3.0        # 上报的 allocatable = --cpu × 该系数;1.0 = 不超卖
--overcommit-memory 1.0     # 内存默认不超卖
```

实测:`--cpu 8 --overcommit-cpu 3.0` → `/v1/nodes` 报 `cpuAllocatable: 24`。

**为什么在节点侧而不是调度器侧算**:合适的系数取决于这个节点是干什么的
(CPU 密集池要 1.0,通用池可以更高),而 `NodeRecord.CPUAllocatable` 的注释
本来就写着「已包含超卖系数」—— 保持只有一处地方做这个决定。

**为什么 CPU 与内存默认值不同**:CPU 超了只是变慢(内核分时),内存超了是进程被杀。
fc 档理论上内存有富余(FC 按需供页,guest 实际 RSS 远低于声明值),
但那个偏差**没有实测过**,而且宿主侧没有 cgroup 包裹 VMM 进程(security §A3),
压力下没有内核层面的公平性保证。这两条是抬高内存系数的前置条件。

**拒绝 < 1.0 而不是钳制**:想「留出四分之一」的人会写 0.75,
钳到 1.0 是无视他,照收则上报得比实际少而日志里没有任何解释。
少报容量应该用 `--cpu` 直接给小一点的数,错误信息里说了这一点。
上限 20 是为了让小数点打错(3.0 → 30)当场报错,
否则它表现为无法解释的超时而不是配置错误。

- CPU：eval 突发型负载默认 3.0;CPU 密集型节点池可配 1.0;cgroup cpu.weight 按
  规格比例分配保公平;`dedicated: true`（预留字段）→ vCPU pin，不参与超卖
- 内存：容器档按 RSS 实际水位天然复用;fc 档靠 balloon——noded 周期驱动
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

## 3.3 Volume（独立的一等资源）📐

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
后端（宿主挂载,noded volume 模块管理）：JuiceFS(on S3+Redis) / CephFS / 本地盘
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
- 后端可换（JuiceFS/CephFS/本地盘），noded 只见宿主路径
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

## 3.4 Guest 内核与 agent 盘的构建发布 ✅

fc 档两个平台工件,均由 CI 构建、S3 分发、noded 启动时按版本拉取到本地：

| 工件 | 内容 | 构建 | 版本策略 |
|---|---|---|---|
| guest 内核 | 6.x LTS,内嵌 virtio/vsock/nfs/overlayfs 等必需项的精简 config,bzImage | 内核源码 + config 入库,CI 复现构建 | 独立版本号;manifest 记录,snapshot restore 校验一致性 |
| agent 盘 | ext4 只读镜像:beand 静态二进制 + busybox 级工具 | CI 打包,与 noded 同版本发布 | 随 noded 版本;旧版本保留至无运行中引用 |

- 存放：`s3://bean/artifacts/{kernel,agent-disk}/<version>/` + sha256 校验
- noded 配置声明版本（默认跟随 noded 发布版），本地缓存 `/var/lib/bean/artifacts/`
- 容器档的 agent 直接用 agent 盘内同一个二进制 bind mount，两档单一构建产物

## 4. 镜像模块 ⚠️

### 4.1 overlaybd 直驱主路线 📐

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

### 4.2 缓存管理 📐

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

### 4.3 Prewarm ✅

- 收到 PrewarmImage 指令后按 priority 入队，受专用带宽/并发限制（不与在线 PULLING 抢）
- overlaybd prewarm = 拉元数据 + 预取热块（有 access trace 时按 trace,否则全量）
- overlaybd 原生支持记录启动 IO trace（`record-trace`）,首次运行采集、
  后续按 trace 精准预取——对固定 eval 镜像集效果显著

### 4.4 image-service 部署形态 ⚠️

image-service 是 **control plane 的逻辑模块**（`internal/control/image`），非独立
部署服务;P0–P2 内嵌 bean-api 进程。职责需要全局视角所以不能下放节点：

- 格式转换全局去重（一个镜像只转一次，多节点不打架）
- prewarm 编排需要全节点缓存视图
- S3 blob GC 需要全局引用计数（镜像 ↔ blob ↔ 运行中 sandbox/snapshot）

转换任务 CPU 重,量大后可拆独立 worker 池水平扩展（接口已按模块边界隔离）。

## 5. 网络模块 📐

> **整节没有任何代码。** `grep -rn 'nftables\|netns\|veth\|bean0'` 全仓库为 0。
> sandbox 当前**没有网络** —— fc 档只有 vsock 通到 agent,那是控制通道。
> 下面是设计意图,不是当前行为;安全语义见 security-and-startup.md §A4。


### 5.1 数据面 📐

```
创建：
1. ip netns add bean-<id>
2. veth 对：veth-<id> (host) ↔ eth0 (netns)，母桥 bean0 (10.100.0.1/24)
3. netns 内配 IP（节点本地 IPAM，位图分配）、默认路由 → 10.100.0.1
4. resolv.conf 指向节点 DNS 转发器（可审计;上游可配,默认 1.1.1.1）
5. /etc/hosts 注入 sandbox hostname
```

### 5.2 nftables 规则（每节点一套 + per-sandbox 链）📐

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
- 端口暴露不开入站 DNAT——regional proxy → noded sbxproxy → 直连 sandbox IP
  （见 api-design.md §6.2）;节点防火墙入站仅对 control plane/proxy 开放

### 5.3 fcRuntime 兼容 📐

当前 FC 不配 tap 网卡,`fcMachineConfig` 里没有网络设备。


FC microVM 用 tap 设备替代 veth 的 netns 端，同样挂 bean0 桥，nftables 规则不变。

## 6. beand ✅

### 6.1 注入与启动 ✅

> 本节描述**容器档注入**（bind mount + entrypoint override,随 P5 引入）;
> fc 主路径的 agent 盘注入见 §3.1/§3.4。

1. noded 发布目录 `/var/lib/bean/agent/<version>/beand`（静态编译，musl，≈8 MiB）
2. OCI spec 增加只读 bind mount：`/var/lib/bean/agent/<ver>/beand → /.bean/agent`
   以及 socket 目录 `/run/bean/<id>/ → /.bean/run/`（读写）
3. entrypoint override 为 `/.bean/agent`；原 image 的 entrypoint/cmd/env/user/workdir
   序列化进 spec annotation，由 agent 读取
4. agent 启动即 listen unix socket `/.bean/run/agent.sock`（noded 从 host 侧
   `/run/bean/<id>/agent.sock` 直连），上报 Ready
5. `autoStartCmd=true` 或收到 StartUserProcess 时，agent 按原 entrypoint 语义
   fork 用户进程（setuid 到镜像 USER、应用 env/workdir）

版本升级：agent 随 noded 包发布，目录带版本号，运行中 sandbox 不受影响（旧版本目录保留至无引用）。

路径冲突：`/.bean` 若与镜像内容冲突（极罕见），创建失败并明确报错，可配置备用挂载点。

### 6.2 PID1 职责 ✅

- **僵尸回收**：`SIGCHLD` reap 所有孤儿
- **信号转发**：SIGTERM → 用户进程组，graceful 超时后 SIGKILL
- **进程管理**：exec 会话表（id → 进程组），支持 signal/kill 单会话

### 6.3 Exec / PTY ⚠️

- 普通 exec：`os/exec` + pipe，stdout/stderr 分流，输出限额截断
- PTY：`creack/pty`，resize 帧调 `TIOCSWINSZ`；会话绑定 WS 连接，
  连接断开保留会话 60s 可重连（reconnect token）
- 并发 exec 无全局锁，per-sandbox 上限（默认 32 会话）

### 6.4 文件操作 ✅

- 流式 gRPC chunk（1 MiB/帧），保留 mode/uid/gid；目录树操作提供 `tar` 模式
  （上传 tar 自动解包、下载目录打 tar）——eval 批量注入 repo 快照的主路径
- 大文件产物直推 S3：agent 收到含 presigned URL 的指令后在容器内直接
  PUT（走 sandbox 出网路径），不占 noded 带宽

### 6.5 日志 ✅

- 用户进程 stdout/stderr → 环形缓冲（8 MiB）+ 可选实时流
- 销毁前 noded 触发 agent 将全量日志经 presigned URL 归档 S3

### 6.6 传输层抽象 ✅

agent 的 gRPC listener 抽象为 `Transport`（unix socket 实现 / vsock 实现），
fc 档 agent 代码零改动，只换 transport（vsock）与注入载体（agent 盘，拍板见 §3.1/§3.4）。

## 7. 注册、心跳、租约与 reconcile ✅

### 7.0 节点注册与凭证分层 ✅

```
管理员：控制面注册 region（S3 endpoint、proxy 组、BYOC token 服务地址）
      → 生成 region bootstrap token（短 TTL 24h,可限次数,可撤销）
节点：noded 配置 token 启动 → Register(token, region, capabilities, labels)
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

### 7.1 心跳 ✅

- 双向流，间隔 3s;携带:资源水位、各 sandbox {id, state, 资源用量摘要}、
  镜像缓存增量、正在执行的 command ids
- 控制面 15s（5 个周期）未收到 → 节点 SUSPECT → 30s → LOST：
  其上 RUNNING sandbox 标 LOST、调度停止派发
- 网络闪断恢复：流重建后全量状态上报一次;控制面在此期间的直连指令失败会重试,超过阈值触发重调度

### 7.2 noded 重启 reconcile ⚠️

已实现的是**控制面侧**对账:`SyncState` 拉期望列表,销毁本地不该有的 sandbox
(`register.go`)。**未实现的是宿主资源对账** —— 重启后不扫 `losetup -a` /
`dmsetup ls`,所以上一代进程留下的 loop device 与 dm 映射不会被回收。
这已经造成实测到的泄漏:noded 重启一次,共享 base image 就多一个 loop device
(见 `docs/status.md` 待办)。


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

### 7.3 GC 触发器 ⚠️

| 对象 | 策略 |
|---|---|
| sandbox idle | noded 本地 idle 检测（lifecycle 随 create 下发）:无 exec/端口/文件活动持续 idleTimeout → 执行 onIdle(pause/kill) 并发 event——不依赖控制面在线 |
| PAUSED 滞留 | 默认不回收;管理员可选开启全局策略（P4 后由 snapshot 归档替代） |
| 镜像/chunk 缓存 | §4.2 水位 LRU |
| exec 会话 | 断连 60s 无重连 |
| 临时文件（S3 暂存下载） | S3 lifecycle 规则 1 天 |
| Postgres 终态 sandbox 记录 | 控制面归档任务，30 天转冷 |

## 8. noded 自身可观测 ✅

- Prometheus 端点 `--metrics <addr>` → `GET /metrics`（免鉴权,本地采集）;OTLP 导出后续包同一 registry：
  - `bean_node_create_phase_seconds{phase,runtime}` 创建各阶段耗时直方图
    （phase: runtime_create / agent_ready / total;后续补 image_pull / rootfs / network）
  - `bean_node_creates_total{outcome,runtime}`、`bean_node_destroys_total{outcome,runtime}`
  - `bean_node_idle_actions_total{action,outcome}` idle 回收动作
  - `bean_node_sandboxes{state}`、`bean_node_requests_in_flight`（scrape 时重算）
  - 待补：缓存命中率、nftables 规则数、IPAM 使用率
- per-sandbox 资源时序（cgroup/FC stats → OTLP,attributes 带 sandbox_id/labels）;
  agent 可选透传 sandbox 内应用 OTLP（localhost:4317 → vsock 转发）
- 结构化日志（zap），request_id 透传
- pprof 端口（内网）

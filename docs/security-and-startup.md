# 安全模型与快速启动设计

## Part A — 安全模型

### A1. 威胁模型

sandbox 内运行的是 **AI 生成的不可信代码**（eval 任务、agent rollout），假设攻击者完全控制 sandbox 内进程。需要防御：

| 威胁 | 后果 | 防线 |
|---|---|---|
| 内核逃逸 | 接管节点 | FC microVM / gVisor 隔离档（A2） |
| 横向移动 | 访问其他 sandbox / 内网服务 | 📐 网络栈未实现,当前 sandbox 无网络（A4） |
| 凭证窃取 | 拿到 S3/控制面凭证 | 零长期凭证（A5） |
| 资源滥用 | 挖矿、fork 炸弹、磁盘写满 | ⚠️ 当前靠 guest 内核自限与 CoW 盘大小;宿主 cgroup 未实现（A3） |
| 出网滥用 | 作为跳板攻击外部、DDoS | 📐 同上,未实现（A4） |
| 恶意镜像 | 供应链投毒 | 镜像来源控制（A6） |
| agent 攻击面 | 从容器内攻击 agent → noded | 最小 API + socket 权限（A7） |

### A2. 隔离档位 ⚠️（内部机制，不对外暴露;分档规则见 architecture.md D3）

**当前只有 `fc` 和 `local` 两档存在。** `local` 是进程级 sandbox,仅供开发与
CI 使用,**没有任何隔离**,不在下表里也不该用于不可信代码。容器档(runc/runsc)
未实现。

| 实际档 | 运行时 | 逃逸防线 | 何时使用 |
|---|---|---|---|
| `fc`（默认主档） | Firecracker microVM | 硬件虚拟化边界,宿主暴露面最小（FC 设备模型极简 + 内置 seccomp）。**jailer 尚未接入**,见 A3 | KVM 节点——常规 eval/rollout |
| `runsc` 📐 | gVisor | 用户态内核拦截 syscall，宿主内核面≈70 个 syscall | 无 KVM 节点的降级档。**未实现** |
| `runc` 📐 | runc | 仅 namespace/seccomp/caps | 内部可信任务 + GPU（内部预留）。**未实现** |

- fc 档 guest 是真内核，无 gVisor 的 syscall 兼容性问题
- runc 承载 GPU 意味着 **GPU eval 的隔离弱于默认档**——GPU 节点独立节点池 +
  镜像白名单收紧作为补偿控制;gVisor GPU 支持（nvproxy）作为 P5 演进项
- runsc 降级档兼容性回归集随容器档 P5 引入，不兼容镜像显式豁免，不静默降级

### A3. 加固基线 ⚠️

**当前真实状态,先说清楚:**

| 加固项 | 状态 | 说明 |
|---|---|---|
| 硬件虚拟化边界 | ✅ | 真 Firecracker microVM,这是主防线 |
| FC 进程自身 seccomp | ✅ | Firecracker 内置严格 profile,不传 `--no-seccomp` 即生效 |
| 可写层盘大小硬限 | ✅ | 宿主组装 CoW 文件,大小天然是上限 |
| guest 内资源自限 | ✅ | guest 内核自管,能耗尽的只有自己 VM 的资源 |
| **jailer(chroot + 独立 uid/gid + 设备白名单)** | 📐 | **未实现**。noded 直接 exec firecracker 二进制,`grep -rn jailer` 全仓库为 0 |
| **宿主侧 cgroup 包裹 FC 进程** | 📐 | **未实现**。cpu/mem 的双保险目前只有调度器的承诺量记账,没有内核强制 |

之前这一节把 jailer 和 cgroup 写成「P2 交付」,是错的 —— 它们从未实现。
这类错误在安全文档里代价最高:读者会据此判断可以跑什么代码。

**缺 jailer 的实际含义**:FC 进程以 root 跑在宿主的 mount namespace 里,
没有 chroot、没有降权、没有设备白名单。一个 FC 或 KVM 的漏洞,其后果是
「拿到宿主 root」而不是「拿到一个被 chroot 的低权用户」。硬件虚拟化边界还在,
但纵深防御少了一层 —— 而 Firecracker 官方把 jailer 列为生产部署的建议做法。

**缺 cgroup 包裹的实际含义**:承诺量只是调度器的账本。一个 guest 无法超过
自己 VM 的内存配置(FC 强制),但 **FC 进程本身**在宿主上没有 cgroup 限制,
所以宿主内存压力下没有内核层面的公平性保证。这在超卖场景下更要紧
(见 architecture.md D12)。

**容器档**（runc/runsc,📐 未实现,随 P5 引入）：

- cgroup v2 硬限制：cpu.max、memory.max（+ memory.swap.max=0）、pids.max（默认 4096，防 fork 炸弹）、io 权重
- 磁盘写入上限：rootfs 可写层 XFS project quota（默认 20 GiB，可配）
- `no_new_privileges=true`；全部 capability drop 后按需加回（默认仅 CHOWN/SETUID/SETGID/DAC_OVERRIDE/FOWNER/KILL——满足包管理器与常规构建）
- 默认 seccomp profile（runc 档用 containerd 默认 + 加黑 keyctl/bpf/userfaultfd 等；runsc 档 gVisor 自身已收敛）
- `/proc`、`/sys` 按 OCI 默认 masked/readonly 路径处理
- 不挂 docker.sock、不开 privileged、拒绝 host network/pid/ipc（API 层无此选项）

**fc 档**（guest 内 agent 即 root init，容器加固项不适用，防线在宿主侧）：

- ✅ FC 进程自身 seccomp（Firecracker 内置严格 profile,默认生效）
- ✅ guest 磁盘写入上限 = 可写层文件大小（宿主组装，天然硬限）
- ✅ pids/fork 炸弹：guest 内核自限（能耗尽的只有自己 VM 的资源）
- 📐 jailer：chroot + 独立 uid/gid + 设备白名单 —— **未实现**
- 📐 宿主侧 cgroup 包裹 FC 进程（cpu/mem 双保险）—— **未实现**

### A4. 网络安全 📐

**整节未实现。** `grep -rn 'nftables\|netns\|veth\|bean0'` 全仓库为 0 ——
没有网络模块,sandbox 目前没有任何网络能力(fc 档只有 vsock 到 agent,
那是控制通道不是数据网络)。

这意味着下面每一条都是**计划**,不是当前的安全承诺。特别是「默认 egress-only」——
当前既没有 egress 也没有隔离规则,因为根本没有网络栈。**不要**把这一节当成
「sandbox 已被限制在这些规则内」来读。

见 noded-design.md §5(同样未实现)。计划中的安全语义:

- 默认 `egress-only`：可出公网（拉依赖是 eval 刚需），**禁止**：sandbox 间互访、节点内网段（RFC1918）、云元数据（169.254.169.254 / fd00:ec2::254）
- 出网带宽 per-sandbox 限速（tc，默认 100 Mbps）+ conntrack 连接数上限（防端口扫描/DDoS 放大）
- `none` 策略供纯离线 eval：无默认路由，杜绝数据外传（模型作弊检测场景有用）;
  卷不破坏该承诺——dataset 卷是本地块设备,shared-fs 卷走宿主 NFS（流量仅达宿主网关,
  不出节点）,均与「出公网」正交。若连宿主共享存储也要禁,创建时不挂卷即可
- DNS 走节点转发器，可记录审计日志
- 入站零暴露：无 DNAT，唯一入口是 proxy → noded → agent 的应用层链路

### A5. 凭证与信任链 ⚠️

已实现:bootstrap token 注册 + node token 鉴权、registry 凭证 AES-256-GCM 加密
落库、S3 长期凭证仅控制面持有、fc 档 vsock(host 侧 FC API socket 仅 noded 可达)。
未实现:presigned URL 注入 sandbox、STS 只读角色轮换、sandbox JWT、TLS 终结层。


```
S3 长期凭证：仅 control plane 持有
   ├── 节点产物上传/snapshot：presigned URL（TTL 15min，绑定 key 前缀 + content-length）
   ├── overlaybd 块读取：noded 持 STS 只读角色（限 blob bucket 前缀，1h 轮换）
   └── sandbox 内直传产物：presigned PUT URL 注入（即使泄漏也只能写指定 key）
控制面 ↔ noded：TLS 单向（云上托管 gRPC 接入层终结,节点零证书配置）
   + 应用层 node token（短期,内存持有,绑定 nodeId——控制面校验节点只能
   操作自己的 sandbox）;注册凭 bootstrap token,凭证分层见 noded-design §7.0
noded ↔ agent：容器档 unix socket（0700，host 侧仅 noded 用户可达;容器内挂载点
   仅 root 可读）;fc 档 vsock（host 侧 FC API socket 仅 noded 可达,guest 内
   /dev/vsock 默认仅 root 可开——非 root 用户进程无法调用 agent API）
sandbox token（JWT）：签名密钥控制面持有，绑定 sandbox-id + 过期时间
```

### A6. 镜像来源 ⚠️

- 首期：仅允许配置白名单内的 registry / S3 blob 源
- 镜像 digest 固定：调度与缓存全部按 digest（tag 仅入口解析一次），保证 eval 可复现
- 预留：镜像签名校验（cosign）接入点在 image-service 解析层

### A7. agent 攻击面控制 ✅

- agent 对 sandbox 内进程暴露的唯一接口是 unix socket（容器档）/ vsock（fc 档），均 root-only（A5）
- agent 以 root 跑（需 setuid 到镜像 USER），但其 API 只允许来自 noded 侧 socket 的指令——容器内即使 root 也只能调用与自己权限等价的操作，无提权增益
- agent 二进制只读挂载，容器内不可替换
- noded 侧对 agent 响应做长度/速率限制，防被攻陷的 agent 反打 noded

### A8. 平台面 📐

- API 全写操作审计（who/what/when，Postgres + S3 归档）
- 节点最小化：专用 OS 镜像、无多余服务、noded 非 root 化评估（P3;containerd 如启用,P5）
- 每周期跑 sandbox 逃逸回归测试集（FC/KVM 攻击面为主;容器档引入后加 gVisor exploit suite 子集）

---

## Part B — 快速启动

### B1. 冷启动预算 ⚠️

**实测(真 KVM 机器,alpine,命中缓存)**:create 全链路 **952ms**,
其中 `runtime_create` 234ms + `agent_ready` 770ms(重叠)。达成了「命中 P50 < 2s」。

冷镜像目标未达成也未走 lazy-pull:当前是「拉全量 + 转换 + CoW 共享」,
实测 busybox 5-10s、alpine 在网络不稳时 **2m45s** —— 所以 prewarm 是必需的
而不是优化。overlaybd lazy-pull 能力已实测(B2),但尚未接进 `image.Provider`。

**这些数字都是单个 sandbox 手工测的。** 并发下如何退化没有测过 ——
见 `docs/status.md` 的压测待办。

原目标:**缓存命中 P50 < 2s；冷镜像（overlaybd lazy-pull）P50 < 10s**。分解（fc 档为例，容器档少 VM 启动项更快）：

| 阶段 | 缓存命中目标 | 冷路径目标 | 手段 |
|---|---|---|---|
| API + 调度 | 50 ms | 50 ms | 内存化调度器状态，无同步外呼 |
| 指令送达 noded | 50 ms | 50 ms | push 直连 gRPC（控制面→noded） |
| 镜像就绪 | ~0（已缓存） | 2–6 s | overlaybd：仅拉元数据+启动热块（见 B2） |
| rootfs 设备就绪 | 100 ms | 200 ms | ublk 设备组装、overlaybd 元数据缓存 |
| netns/网络 | 50 ms | 50 ms | veth/nftables 批量原子操作;IPAM 内存位图 |
| sandbox 启动 | 200–500 ms | 200–500 ms | FC microVM 启动≈125ms+内核引导;容器档 runc≈100ms/runsc≈300ms |
| agent ready | 100 ms | 100 ms | 静态二进制,无依赖加载 |
| **合计** | **≈1–1.2 s** | **≈4–8 s** | |

每阶段打点进创建耗时直方图（noded exporter），回归监控。

**实测(2026-08-02,真 KVM 机器,镜像已缓存,无网络项)**:
`runtime_create` 234ms + `agent_ready` 770ms = **952ms**,落在预算内。
但预算表里的归因是错的:它把成本压在「VM 启动 + 内核引导」上,
而实测最大的两块开销都在我们自己的代码里 —— gRPC 重连退避(800ms)
和 guest 串口同步写(493ms),内核本身只值 90ms。
详见 `docs/decisions.md` §5。

restore 路径:950ms(首次 1617ms)。guest 内存靠 userfaultfd 按需供页,
FC `/snapshot/load` 只占 7ms。

### B2. overlaybd lazy-pull from S3 ⚠️

**能力已在验证机实测跑通,但尚未接入代码**。当前生产路径是 dm-snapshot:
拉全量 + 转换 + 共享只读 base + 每 sandbox CoW(实测每 sandbox 8 KiB)。
overlaybd 侧实测:挂载 7ms、只传 19.6% 的层字节就能挂载并读文件、8 个 HTTP 206、
可写上层实占 40 KiB(`docs/decisions.md` §3.1)。剩下的是写 `OverlaybdProvider`
接进 `image.Provider`。

下面描述的是那个目标形态,不是当前形态。

```
镜像发布链路（image-service，离线一次）：
OCI 镜像 → overlaybd convertor（层级增量转换）→ 块设备层 blobs → S3
                                     │
节点使用链路：                          ▼
CreateSandbox → overlaybd/ublk 组装块设备（元数据数 MiB）→ 立即可挂
             → 容器档挂 overlayfs / fc 档 virtio-blk 直挂 guest
             → IO 访问触发块按需 range-read S3 → 本地 obd-cache
```

- 「启动」只需元数据 + entrypoint 路径热块，SWE-bench 类镜像启动所需数据
  通常 < 全镜像的 5%;overlaybd `record-trace` 采集启动 IO 序列后可精准预取
- 块级 dedup：2000+ 评测镜像共享基础层（ubuntu/python）时 S3 存储与节点缓存
  都大幅缩减
- 该路线已被 AgentENV 在 FC + 海量 OCI 镜像场景生产验证（本地盘做有界缓存，
  镜像总量可超磁盘容量）
- 风险与对策：
  - S3 首字节延迟波动 → 按 trace 预取 + obd-cache 命中兜底
  - ublk 依赖较新内核（6.0+）→ 节点 OS 统一基线;**tcmu 后端在 5.15 上已实测
    功能完备**（挂载 7ms、只传 19.6% 层字节、HTTP 206 range read,
    见 `docs/decisions.md` §3.1）,是可用的主路径而非降级路径,ublk 仅性能更优;
    两者皆不可用的节点不上报 fc 能力（fc 依赖块设备后端），仅容器档 overlayfs 兜底
  - tcmu 需给每个 backstore 设唯一 `vpd_unit_serial`,否则宿主 `multipathd`
    会合并不同镜像的设备并返回错误数据（静默,不报错）
  - 运行中 S3 不可达 → 块读失败重试 + sandbox 级 IO 错误上报（区别于任务自身失败）

### B3. 缓存与预热策略 ⚠️

1. **节点缓存**（noded-design §4.2）：镜像粒度 LRU + chunk LRU，S3 为 source of truth
2. **prewarm API**：eval 批次开始前，编排层按「批次镜像清单 × 目标并发」计算
   节点覆盖数下发预热;image-service 按节点缓存水位挑目标节点
3. **镜像亲和调度**：score = w1·(已缓存层字节占比) + w2·(空闲资源匹配) + w3·(缓存盘类型)
   —— 同一镜像的重复 eval run 天然命中同批节点
4. **基础层常驻**：统计 top 共享层（ubuntu、conda、python），标记 pin 不参与 LRU
5. **IO trace 记录**：首次运行 `record-trace` 采集块访问序列存 S3 元数据，
   后续 prewarm/启动按 trace 预取（overlaybd 原生能力）

### B4. 批量拉起（eval 风暴）⚠️

2000 sandbox 同时创建的路径保护：

- gateway `batchCreate` → 调度器批量决策（单次锁内完成 bin-packing，避免 2000 次抢锁）
- per-node 并发创建上限（默认 16）。⚠️ **当前是硬过滤而非排队**:in-flight 满了
  该节点直接判为不可用,单节点集群会返回 `NO_CAPACITY`。对批量场景这可能是错的
  语义(调用方看到失败而非变慢),且 16 这个值没有实测依据 —— 见 `docs/status.md`
  待办与压测任务
- S3 天然抗并发读；registry 不在热路径（blob 全在 S3）
- 复用连接：noded 的 S3 client 连接池 + HTTP/2

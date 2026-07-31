# 安全模型与快速启动设计

## Part A — 安全模型

### A1. 威胁模型

sandbox 内运行的是 **AI 生成的不可信代码**（eval 任务、agent rollout），假设攻击者完全控制 sandbox 内进程。需要防御：

| 威胁 | 后果 | 防线 |
|---|---|---|
| 内核逃逸 | 接管节点 | FC microVM / gVisor 隔离档（A2） |
| 横向移动 | 访问其他 sandbox / 内网服务 | 网络隔离（A4） |
| 凭证窃取 | 拿到 S3/控制面凭证 | 零长期凭证（A5） |
| 资源滥用 | 挖矿、fork 炸弹、磁盘写满 | cgroup 硬限制（A3） |
| 出网滥用 | 作为跳板攻击外部、DDoS | egress 策略 + 带宽限速（A4） |
| 恶意镜像 | 供应链投毒 | 镜像来源控制（A6） |
| agent 攻击面 | 从容器内攻击 agent → beand | 最小 API + socket 权限（A7） |

### A2. 隔离档位（内部机制，不对外暴露;分档规则见 architecture.md D3）

| 实际档 | 运行时 | 逃逸防线 | 何时使用 |
|---|---|---|---|
| `fc`（默认主档） | Firecracker microVM | 硬件虚拟化边界，宿主暴露面最小（FC 设备模型极简 + jailer + seccomp） | KVM 节点——常规 eval/rollout |
| `runsc` | gVisor | 用户态内核拦截 syscall，宿主内核面≈70 个 syscall | 无 KVM 节点的降级档 |
| `runc` | runc | 仅 namespace/seccomp/caps | 内部可信任务 + GPU（内部预留，不对外开放） |

- fc 档 guest 是真内核，无 gVisor 的 syscall 兼容性问题
- runc 承载 GPU 意味着 **GPU eval 的隔离弱于默认档**——GPU 节点独立节点池 +
  镜像白名单收紧作为补偿控制;gVisor GPU 支持（nvproxy）作为 P5 演进项
- runsc 降级档的兼容性回归集仍需建立（P2），不兼容镜像显式豁免，不静默降级

### A3. 加固基线

**容器档**（runc/runsc）：

- cgroup v2 硬限制：cpu.max、memory.max（+ memory.swap.max=0）、pids.max（默认 4096，防 fork 炸弹）、io 权重
- 磁盘写入上限：rootfs 可写层 XFS project quota（默认 20 GiB，可配）
- `no_new_privileges=true`；全部 capability drop 后按需加回（默认仅 CHOWN/SETUID/SETGID/DAC_OVERRIDE/FOWNER/KILL——满足包管理器与常规构建）
- 默认 seccomp profile（runc 档用 containerd 默认 + 加黑 keyctl/bpf/userfaultfd 等；runsc 档 gVisor 自身已收敛）
- `/proc`、`/sys` 按 OCI 默认 masked/readonly 路径处理
- 不挂 docker.sock、不开 privileged、拒绝 host network/pid/ipc（API 层无此选项）

**fc 档**（guest 内 agent 即 root init，容器加固项不适用，防线在宿主侧）：

- jailer：chroot + 独立 uid/gid + cgroup + 设备白名单
- FC 进程自身 seccomp（Firecracker 内置严格 profile）
- 宿主侧 cgroup 包裹 FC 进程（cpu/mem 双保险）
- guest 磁盘写入上限 = 可写层文件大小（宿主组装，天然硬限）
- pids/fork 炸弹：guest 内核自限（能耗尽的只有自己 VM 的资源）

### A4. 网络安全

见 beand-design.md §5，安全语义汇总：

- 默认 `egress-only`：可出公网（拉依赖是 eval 刚需），**禁止**：sandbox 间互访、节点内网段（RFC1918）、云元数据（169.254.169.254 / fd00:ec2::254）
- 出网带宽 per-sandbox 限速（tc，默认 100 Mbps）+ conntrack 连接数上限（防端口扫描/DDoS 放大）
- `none` 策略供纯离线 eval：无默认路由，杜绝数据外传（模型作弊检测场景有用）;
  卷不破坏该承诺——dataset 卷是本地块设备,shared-fs 卷走宿主 NFS（流量仅达宿主网关,
  不出节点）,均与「出公网」正交。若连宿主共享存储也要禁,创建时不挂卷即可
- DNS 走节点转发器，可记录审计日志
- 入站零暴露：无 DNAT，唯一入口是 proxy → beand → agent 的应用层链路

### A5. 凭证与信任链

```
S3 长期凭证：仅 control plane 持有
   ├── 节点产物上传/snapshot：presigned URL（TTL 15min，绑定 key 前缀 + content-length）
   ├── overlaybd 块读取：beand 持 STS 只读角色（限 blob bucket 前缀，1h 轮换）
   └── sandbox 内直传产物：presigned PUT URL 注入（即使泄漏也只能写指定 key）
控制面 ↔ beand：mTLS（云托管私有 CA;证书运行时拉取、内存持有、不落盘——
   节点磁盘零凭证残留;注册凭 region bootstrap token,凭证分层见 beand-design §7.0）
beand ↔ agent：容器档 unix socket（0700，host 侧仅 beand 用户可达;容器内挂载点
   仅 root 可读）;fc 档 vsock（host 侧 FC API socket 仅 beand 可达,guest 内
   /dev/vsock 默认仅 root 可开——非 root 用户进程无法调用 agent API）
sandbox token（JWT）：签名密钥控制面持有，绑定 sandbox-id + 过期时间
```

### A6. 镜像来源

- 首期：仅允许配置白名单内的 registry / S3 blob 源
- 镜像 digest 固定：调度与缓存全部按 digest（tag 仅入口解析一次），保证 eval 可复现
- 预留：镜像签名校验（cosign）接入点在 image-service 解析层

### A7. agent 攻击面控制

- agent 对 sandbox 内进程暴露的唯一接口是 unix socket（容器档）/ vsock（fc 档），均 root-only（A5）
- agent 以 root 跑（需 setuid 到镜像 USER），但其 API 只允许来自 beand 侧 socket 的指令——容器内即使 root 也只能调用与自己权限等价的操作，无提权增益
- agent 二进制只读挂载，容器内不可替换
- beand 侧对 agent 响应做长度/速率限制，防被攻陷的 agent 反打 beand

### A8. 平台面

- API 全写操作审计（who/what/when，Postgres + S3 归档）
- 节点最小化：专用 OS 镜像、无多余服务、beand/containerd 非 root 化评估（P3）
- 每周期跑 sandbox 逃逸回归测试集（gVisor exploit suite 子集）

---

## Part B — 快速启动

### B1. 冷启动预算

目标：**缓存命中 P50 < 2s；冷镜像（overlaybd lazy-pull）P50 < 10s**。分解（fc 档为例，容器档少 VM 启动项更快）：

| 阶段 | 缓存命中目标 | 冷路径目标 | 手段 |
|---|---|---|---|
| API + 调度 | 50 ms | 50 ms | 内存化调度器状态，无同步外呼 |
| 指令送达 beand | 50 ms | 50 ms | push 直连 gRPC（控制面→beand） |
| 镜像就绪 | ~0（已缓存） | 2–6 s | overlaybd：仅拉元数据+启动热块（见 B2） |
| rootfs 挂载 | 100 ms | 200 ms | snapshotter 预热、erofs 元数据缓存 |
| netns/网络 | 50 ms | 50 ms | veth/nftables 批量原子操作;IPAM 内存位图 |
| sandbox 启动 | 200–500 ms | 200–500 ms | FC microVM 启动≈125ms+内核引导;容器档 runc≈100ms/runsc≈300ms |
| agent ready | 100 ms | 100 ms | 静态二进制,无依赖加载 |
| **合计** | **≈1–1.2 s** | **≈4–8 s** | |

每阶段打点进创建耗时直方图（beand exporter），回归监控。

### B2. overlaybd lazy-pull from S3

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
  - ublk 依赖较新内核（6.0+）→ 节点 OS 统一基线;老内核退 overlaybd tcmu 后端;
    两者皆不可用的节点不上报 fc 能力（fc 依赖块设备后端），仅容器档 overlayfs 兜底
  - 运行中 S3 不可达 → 块读失败重试 + sandbox 级 IO 错误上报（区别于任务自身失败）

### B3. 缓存与预热策略

1. **节点缓存**（beand-design §4.2）：镜像粒度 LRU + chunk LRU，S3 为 source of truth
2. **prewarm API**：eval 批次开始前，编排层按「批次镜像清单 × 目标并发」计算
   节点覆盖数下发预热;image-service 按节点缓存水位挑目标节点
3. **镜像亲和调度**：score = w1·(已缓存层字节占比) + w2·(空闲资源匹配) + w3·(缓存盘类型)
   —— 同一镜像的重复 eval run 天然命中同批节点
4. **基础层常驻**：统计 top 共享层（ubuntu、conda、python），标记 pin 不参与 LRU
5. **IO trace 记录**：首次运行 `record-trace` 采集块访问序列存 S3 元数据，
   后续 prewarm/启动按 trace 预取（overlaybd 原生能力）

### B4. 批量拉起（eval 风暴）

2000 sandbox 同时创建的路径保护：

- gateway `batchCreate` → 调度器批量决策（单次锁内完成 bin-packing，避免 2000 次抢锁）
- per-node 并发创建上限（默认 16），超出排队——瞬时风暴变节点内流水线
- S3 天然抗并发读；registry 不在热路径（blob 全在 S3）
- 复用连接：beand 的 S3 client 连接池 + HTTP/2

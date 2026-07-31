# bean

Container-native sandbox platform for AI evaluation workloads.

任意 OCI 镜像**零转换**直接作为 sandbox 启动，S3 lazy-pull 镜像分发，
面向 AI evaluation / agent rollout 的批量异构镜像场景（SWE-bench 类 2000+ 镜像）。
全自研栈：control plane、node daemon（beand）、sandbox agent、proxy、SDK、CLI，
不依赖 K8s，同时支持裸金属与云 VM 节点。

## 核心特性

- **镜像即环境**：无 e2b 式 template build，Docker 镜像直接启动
- **秒级冷启动**：overlaybd 块级 lazy-pull from S3 + 节点缓存 + prewarm + 镜像亲和调度
- **隔离自动分档（内部机制）**：Firecracker microVM 默认档（rootfs 直挂，零嵌套），无 KVM 节点降级 gVisor;GPU 走 runc 容器档（内部预留，不对外）
- **批量原语**：batchCreate、标签批量销毁、eval 批量 SDK helper
- **Volume 一等资源**：shared-fs 卷（宿主 nfsd 导出，跨 sandbox 持久工作区;dataset 只读块卷预留）
- **S3 统一存储**：镜像 blob、日志产物、snapshot 全部落 S3，节点无状态
- **多区域 / BYOC**：控制面全局一份、数据面按 region 自治（独立 S3 + regional proxy）;BYOC 客户数据不出自有环境
- **pause/resume/snapshot/fork**：FC 原生 memory snapshot;fork 独立 API（CoW 一母多子,瞬时 fan-out）;容器档 freezer/checkpoint 兜底,跨节点 restore

## 文档

| 文档 | 内容 |
|---|---|
| [docs/architecture.md](docs/architecture.md) | 总体架构、核心设计决策（D1–D10）、API 概览、状态机 |
| [docs/api-design.md](docs/api-design.md) | REST/gRPC 详细定义、鉴权、bean-proxy 端口反代、配额限流 |
| [docs/beand-design.md](docs/beand-design.md) | node daemon：Runtime 抽象、镜像缓存、网络编排、reconcile;bean-agent：PID1 注入、exec/PTY/文件 |
| [docs/security-and-startup.md](docs/security-and-startup.md) | 威胁模型、隔离与加固基线、凭证链;冷启动预算与 lazy-pull 细节 |
| [docs/snapshot-resume.md](docs/snapshot-resume.md) | pause/resume/snapshot/restore 实现,FC 档最终形态 |
| [docs/sdk-cli-design.md](docs/sdk-cli-design.md) | Python/TS SDK 接口、CLI 命令面、代码生成策略 |
| [docs/competitive-analysis.md](docs/competitive-analysis.md) | AgentENV/CubeSandbox/e2b/Daytona/Modal/Morph 等竞品对比与差异化定位 |
| [docs/roadmap.md](docs/roadmap.md) | P0–P5 实施路线、验收标准、风险登记簿 |

## 规划中的 Repo 结构

```
bean/
├── proto/                  # gRPC 定义（single source of truth）
├── cmd/                    # bean-api / bean-scheduler / bean-proxy / beand / bean-agent
├── internal/               # control / node / agent / store
├── sdk/                    # python / typescript
├── cli/                    # bean CLI
├── deploy/                 # 节点 bootstrap、systemd、S3/DB 初始化
└── docs/
```

## Status

设计阶段。实施从 [roadmap](docs/roadmap.md) P0 开始。

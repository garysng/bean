# Warm snapshot:每个镜像 boot 一次,而不是每个沙箱 boot 一次

> English: [../warm-snapshots.md](../warm-snapshots.md)
> 小节状态标注约定见 [architecture.md](architecture.md) §0。
> 对应 GitHub #26。

一次冷 create 花 952ms,按进程计量是 **5 CPU-秒**的宿主 CPU。从已缓存快照 restore 花
**392ms**,几乎不吃 CPU,因为它不 boot 内核。吞吐受前一个数字约束:
`核数 / 5 CPU-秒`,16 核上约 2.3 次 create/s。

所以杠杆不在"boot 更快",而在**每个镜像 boot 一次**而不是每个沙箱 boot 一次。

## 1. 竞品怎么做 📐

读源码而不是读宣传 —— `e2b-dev/infra` @ `17ffd81`:

`Factory.CreateSandbox`(真 boot,设置 boot source)和 `Factory.ResumeSandbox`
(`PUT /snapshot/load`)是两条独立路径,而**`CreateSandbox` 的每一个调用方都在
`packages/orchestrator/pkg/template/build/` 下面**。面向用户的 gRPC handler 调的是
`ResumeSandbox`。真 boot 只发生在构建模板的时候。

他们的 `ResumeSandbox` 就是 bean 说的 **restore**:做 `PUT /snapshot/load` 并产出一个新
沙箱。它不是 bean 语义下的 resume(解冻一个活进程的 vCPU),读他们代码时要留意这个借用的
命名。本文全程使用 bean 的词汇 —— 见 [snapshot-resume.md](snapshot-resume.md) §0。

三个值得借鉴的细节:

- 他们 **boot 两次**。provision 阶段用 BusyBox init 只执行 provision 脚本,就绪判定是从
  guest 串口抓 sentinel 字符串。后续阶段用 systemd,就绪判定是向沙箱内 agent 发
  HTTP `POST /init`。
- 暂停点在**用户的 start 命令跑完之后**,而不只是 boot 完之后。连用户进程的内存状态都被
  捕获了。
- 暂停前:冻结 guest 文件系统(`FIFREEZE`)、排空 balloon 的 free-page hint,然后暂停,
  然后快照。冻结这一步要紧,因为写入中途被捕获的文件系统恢复出来是脏的。

## 2. 形状 📐

```
prewarm(ref):
    拉取并转换成 ext4               (已实现)
    boot 一个沙箱
    等 agent 就绪                   (就是 create 路径已有的就绪门槛)
    带内存 checkpoint               (已实现)
    记录 ref + CPU 身份 -> snapshot id

create(ref):
    查这个 ref 在本节点 CPU 上可用的 warm snapshot
    命中 -> restore                 392ms,不 boot
    未命中 -> 照今天 boot            952ms,并为下次预热
```

这里每个原语都已存在。所以这是编排加一个数据模型决定,这也是设计说明短、风险一节长的原因。

## 2a. 两边各做什么 📐

prewarm 已经存在,而且本来就跨两边,所以问题不是"把活放哪边",而是哪一半要长。

| | 今天 | 有了 warm snapshot 之后 |
|---|---|---|
| **控制面**(`runPrewarmJob`,`internal/control/api/images.go`) | 挑本 region 内 READY 的节点,逐镜像调 `PrewarmImage`,每个 30 分钟超时,失败记日志 | 不变 |
| **节点**(`PrewarmImage` → `Images.Prewarm`) | 准备镜像文件:拉取并转换成 ext4 | **另外 boot 一次、等 agent、带内存 checkpoint** |
| **上报** | 节点上报自己持有什么;控制面不记录成功 | 节点另外上报自己预热了哪些 `(digest, vendor, family, template)` |

现有代码已经写明了上报规则和它的理由(`images.go:196`):成功不由控制面记录,因为
*节点上报它持有什么,那才是权威。从控制面这边写,会让节点掉盘之后两边不一致。*
这条规则原样沿用 —— warm snapshot 就是某个节点磁盘上的一个文件,而掉了盘的节点必须能通过
"干脆不上报它"来说出这件事。

所以执行属于节点,而且本来就是。节点单独决定不了的是**该预热哪个**镜像,因为那取决于放置
与需求,只有控制面看得见。于是分工是:

- **控制面决定预热什么**,就像它今天对镜像做的那样
- **节点执行预热并持有产物**,就像它今天对镜像做的那样
- **节点上报结果**,控制面把它当作事实

唯一真正新的东西是:上报的单位不再是一个裸的镜像引用。warm snapshot 只在兼容 CPU 上可用
(见 §3),所以节点上报的是一个元组,而调度器已有的 `CPUConstraint` 过滤就是消费它的地方。
这个元组里的 digest 那一半,正是镜像现在要上报 digest 的原因 —— 见
`proto/bean/node/v1/node.proto` 里的 `UpdateNodeStatus`。

**prewarm 今天买不到什么,而这正是这个 feature 存在的全部理由。** 准备镜像文件消掉了拉取,
但没消掉 boot:针对一个已完全预热的镜像做 create,仍然走 `configureAndBoot`,仍然要付那
约 5 CPU-秒 —— 而正是它把吞吐上限压在 `核数 / 5`。现状的 prewarm 攻击的是冷节点上的延迟,
对一个已经热的节点的吞吐毫无帮助。

## 3. 数据模型:warm snapshot 是按镜像**并且**按 CPU 的 📐

这是不说清楚就一定会做错的一部分。

Guest 内存记录了它启动时那颗 CPU 提供的东西,而 vendor 与 family **无法被屏蔽掉**
(见 `cpu_template.go`,以及 [decisions.md](decisions.md) §3.6 的实测)。所以带内存的快照
只能在兼容 CPU 上恢复。映射关系不是 `镜像 -> 快照`,而是:

```
(镜像 ref, cpu vendor, cpu family, cpu template) -> snapshot id
```

异构集群需要每个 CPU 代次一份 warm snapshot。这不是本设计的缺陷 —— 它就是那个已经让调度器
用 `409 INCOMPATIBLE_CPU` 拒绝不兼容 restore 的同一个约束,而表达它需要的字段已经在
`Snapshot` 记录上了。

e2b 对同一问题的答案是四行硬编码兼容表加一个调度器过滤,这实际上悄悄断掉了 AMD 与 Intel
之间的迁移。我们已经有 vendor 和 family 过滤,所以同样的做法不需要新机制。

**未命中必须是平常事,不是异常。** 一个 CPU 上没有 warm snapshot 的节点,就照今天那样 boot。
如果未命中是错误,那么往集群里加一台新代次的机器就会让它上面的 create 全部失败。

## 4. 为什么 `--no-memory` 不是答案 📐

仅文件系统的 checkpoint 是 6109 B 对 15.5 MB,能在**任意** CPU 上恢复,看起来像是每镜像
产物的显然候选。

但它在这里帮不上忙。restore 是按 bundle 的内容分派的:没有 memory 成员就走
`configureAndBoot`,所以 `--no-memory` 的 restore **仍然 boot**,仍然要付那 5 CPU-秒。
它省的是存储,不是 CPU。

它的可移植性对另一个目的是真实价值,所以**它的语义不能为了让 warm snapshot 成立而被改动**。

## 5. 快照从哪个时刻取 📐

两个选项,而且这个选择有后果:

| | 暂停点 | 捕获到 | 代价 |
|---|---|---|---|
| agent 可达之后 | guest 已启动、agent 已起、其他什么都没跑 | boot | 每镜像一次 boot |
| 用户 start 命令之后 | boot 加上用户自己的预热 | boot 与应用启动 | 需要每镜像一份构建规格 |

第一个就是对那 5 CPU-秒的全部胜利,且不需要任何新的用户可见概念。第二个是 e2b 的做法,
对 `import torch` 这类场景严格更优 —— Modal 实测 `import torch` 通过在其后快照,p50 从
约 5s 降到 1.05s —— 但它需要一份模板定义,那是个更大的 feature。

**计划是做第一个,把第二个留给模板 feature**,因为第一个自己就拿下了吞吐上限,而第二个是
在它之上的应用层优化。

## 6. 风险 📐

**warm snapshot 会过期。** 它把基础镜像钉在被取快照的那一刻。由于镜像按 digest 不可变 ——
移动过的 tag 是不同的 digest,因而是不同的文件([image-pipeline.md](image-pipeline.md) §2)
—— 所以键必须是解析出的 digest,不是 tag。按 tag 做键会在 tag 移动之后**静默**提供一个
过期的环境。

**存储随镜像数乘以 CPU 代次数增长。** ⚠️ 已设界,需显式开启。warm snapshot 是全量内存镜像,
所以增长大致是"每条目每代次一份 guest 内存大小",而且没有任何东西像沙箱引用用户快照那样引用
warm 快照 —— 所以不会有任何东西删它。`--warm-snapshot-high-mib` 给这个存储设界,并按
**最近一次 restore** 的时间驱逐到低水位。

关于这个驱逐有三点不显然,每一点都是决定而不是默认:

- **与快照缓存分开的预算**,不是复用它的 sweeper。缓存条目是可从 blob 重新解包的派生副本,
  驱逐代价是一次解包;warm bundle 是自己的唯一副本,驱逐代价是一次 boot。共用预算会让一波
  restore 把"让 create 变便宜的那个东西"驱逐掉。
- **按最近 restore 排序,不按创建时间。** warm bundle 写一次、读好几周,所以"距创建多久"完全
  说明不了它是否值这份空间 —— 按它排序会在节点最忙的那个 bundle 刚好变成最老的时候把它驱逐掉。
- **刚写入的那条被保护。** 这是在真机上发现的,不是评审出来的:低水位 10 MiB、bundle 15 MB 时,
  prewarm 存下快照,紧随的 sweep 把它连同其他一起删了 —— 存储变空,而 prewarm 报告成功。
  任何低于单个 bundle 大小的低水位都会触发这个,而运维在第一个 bundle 出现之前无法知道它多大。

仍未回收的:S3 侧的 blob —— 但 warm snapshot 今天不用它(warm bundle 从不离开产出它的节点)。

**restore 失败必须能回退。** 如果一个 warm snapshot 损坏或它的 blob 丢了,create 必须
boot 而不是失败。要避免的失败模式是:一个坏的 warm snapshot 让某个镜像在整个集群范围内
不可用。

**与快照链的交互。** warm snapshot 是增量 checkpoint 的一个合理 base,那会让它在有后代时
不可删除 —— 这个保护已经存在(删除有子代的 base 返回 `409`)。这一点值得刻意决定,而不是
将来撞上。

## 7. 验证 📐

1. 同一镜像连续 create 两次。第二次必须**不 boot**:断言 restore 计数增加、boot 计数不增加,
   而不是只断言"更快了" —— 快可能只是因为 page cache 热。
2. 在没有对应 warm snapshot 的 CPU 上 create。必须 boot 并成功,不能报错。
3. 故意破坏一个 warm snapshot 的 blob,确认 create 回退成 boot 而不是失败。
4. 移动一个 tag,确认新 digest 不会命中旧 digest 的 warm snapshot。这一条必须用
   **两个真的不同的镜像内容**验证,因为按 tag 命中的错误是静默的:恢复成功,只是环境不对。
5. 真机测吞吐,而不是测单次延迟。这个 feature 的全部主张是把上限从 `核数 / 5` 抬起来,
   所以要测的是并发 create 的稳态速率(GitHub #29 / 任务 #39 的 128 核压测)。

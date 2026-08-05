# 镜像链路:OCI ref → 可挂的块设备

> 状态标注约定见 [architecture.md](architecture.md) §0。
> 实现:`internal/node/image/`(registry / convert / devmapper / pulling)。
> 「怎么构建镜像」见 [image-build.md](image-build.md),本文是「已有镜像怎么变成能启动的东西」。

用户交给平台的是 `python:3.12` 这样的普通 OCI ref。fc 档需要的是一个块设备。
本文是这两者之间的全部步骤,以及冷启动 2m45s 花在哪。

## 1. 三层 Provider ✅

```
PullingProvider          缺镜像时触发转换,并发去重
  └── DevMapperProvider  共享只读 base + 每 sandbox CoW → /dev/mapper/bean-<id>
      (或 FileProvider)  每 sandbox 全量拷贝,无 dm 依赖时兜底

OverlaybdProvider        另一条路,--fc-overlaybd;层按 digest 共享(见 §7)
```

分层而不是一个大 provider 的理由:**「镜像从哪来」和「块设备怎么组」是两件事**。
`PullingProvider` 包着任意一个内层实现,所以「首次使用时拉取」这个行为
不需要在每个块设备后端里重复实现。

`OverlaybdProvider` 站在这个栈**旁边**而不是里面 —— 这是原先的分层没预料到的一处。
它自己把镜像解析成层,因为「取哪些层」和「要不要取层」是同一个决定
(lazy pull 一个都不取)。把它包进 `PullingProvider` 会先跑 ext4 转换器,
产出一个 overlaybd 用不上的东西。

`Provider` 接口很小 —— 组设备,加上上报节点持有什么:

```go
Name() string
Prepare(ctx, sandboxID, imageRef string, opts PrepareOptions) (*Rootfs, error)
Prewarm(ctx, imageRef string) error
Cached() (map[string]CachedImage, error)
Config(imageRef string) (*Config, error)   // §5
Digest(imageRef string) (string, error)
```

`Cached()` 存在是因为**节点是它自己持有什么的唯一权威** —— 心跳上报这份清单,
调度器据此做镜像亲和打分、prewarm job 据此显示进度。让控制面去推断
「节点大概有什么」会立刻和现实分叉。

## 2. 转换:tar.gz 层 → ext4 ✅

```
① ParseReference        解析 ref
② 查 ImageDir           已有则直接返回 ← 不可变语义,见下
③ FetchManifest         registry 认证 + manifest 解析
④ sizeFor(manifest)     从压缩层大小估算文件系统大小
⑤ writeBaseImage        建稀疏文件 → mkfs.ext4 → 挂载
⑥ 逐层 applyLayer       按顺序解压,处理 whiteout
⑦ 补 guest 必需目录     /proc /sys /dev 等挂载点
⑧ 卸载 → rename 就位    原子发布
⑨ 写 sidecar 记录 ref   记住这个文件来自哪个 ref
```

**sidecar 在镜像之后写**,顺序是刻意的:它是 `Cached()` 上报的依据,
所以先写 sidecar 会让节点宣称持有一个还不能用的镜像 —— 调度器据此把工作放过来,
然后 create 失败。

`refToFilename` 把 ref 编码成文件名(非字母数字转分隔符),而**不同的分隔符
不能都映射成同一个字符**,否则 `a:b` 与 `a/b` 会撞。sidecar 是反向查询的答案:
从文件名推不回原始 ref,所以单独记。

### 不可变语义 ✅

```go
if _, err := os.Stat(final); err == nil {
    // Already converted. Images are immutable once written — a tag that
    // moves is a different digest and so a different file.
    return final, nil
}
```

文件名由 ref 派生,而**移动过的 tag 是不同的 digest,因此是不同的文件**。
所以「已存在就直接用」不会拿到过期内容。这条让转换天然幂等,
也是为什么不需要缓存失效逻辑。

### 为什么先 rename 才可见 ✅

工作目录与 `ImageDir` 必须在同一个文件系统上,因为最后一步是 `rename`:

```
WorkDir/<tmp>.ext4  →  ImageDir/<name>.ext4
```

不这么做的后果很具体:并发的 `Prepare` 会看到一个**写了一半的 ext4**,
把它挂起来然后失败 —— 而失败的样子像「镜像损坏」而不是「竞态」。
这是那种排查一整天才发现是竞态的 bug,所以宁可要求同文件系统。

### 文件系统大小怎么估 ⚠️

```go
sizeMiB := (compressed >> 20) * 3      // 压缩层总和 × 3
if sizeMiB < floor { sizeMiB = floor }  // 不低于 DefaultSizeMiB
if sizeMiB < 256 { sizeMiB = 256 }      // 硬地板
```

**这是估算,不是计算。** manifest 里只有压缩后的大小,而解压比例取决于内容
(文本层能到 5×,已压缩的二进制接近 1×)。× 3 是个中间值,加上地板兜住小镜像。

这个估算**猜小了会失败**(转换时 ENOSPC),猜大了只浪费稀疏文件的名义大小
(实际占用按写入量)。所以偏向猜大。**没有实测过 × 3 的命中率** ——
如果出现转换 ENOSPC,这是第一个要看的地方。

### whiteout:OCI 的删除语义 ✅

层是叠加的,所以「删除」要用标记文件表达:

| 标记 | 含义 | 处理 |
|---|---|---|
| `.wh.<name>` | 删除下层的 `<name>` | `os.RemoveAll(victim)` |
| `.wh..wh..opq` | 清空该目录下层的全部内容 | `clearDir(dir)` |

漏了 whiteout 处理的后果是**上层删掉的文件在 guest 里又出现了** ——
典型症状是删掉的密钥文件、清理掉的构建缓存重新出现,而镜像看起来是好的。

### 路径逃逸防护 ✅

```go
func safeJoin(root, name string) (string, error) {
    clean := filepath.Clean("/" + name)
    joined := filepath.Join(root, clean)
    if joined != root && !strings.HasPrefix(joined, root+string(os.PathSeparator)) {
        return "", fmt.Errorf("layer entry %q escapes the image root", name)
    }
    return joined, nil
}
```

一个恶意或畸形的镜像里可以有 `../../etc/cron.d/x` 这样的条目。
**转换是在节点上以 root 跑的**,所以这不是「guest 内的越权」而是
「直接写宿主文件系统」—— 必须在解压时拒绝,不能依赖后续环节。

先 `Clean("/" + name)` 再 join 是关键:它把 `..` 在**绝对路径语义下**归约掉,
之后的前缀检查才有意义。

### 用 magic bytes 而非 media type 判定 gzip 📐

代码只按 `layer.MediaType` 分支(`convert_linux.go` 的 `applyLayer`),整个包里没有任何
嗅探逻辑。那里的注释描述的是从未实现的意图:

```go
// Most layers are gzipped; the media type says so, but some registries are
// loose about it, so the magic bytes decide.
```

计划是改成用 magic bytes 判定。有些 registry 的 media type 与实际内容不一致,
信 media type 的后果是把 gzip 流当 tar 解(立刻失败)或反之。嗅探是防御性的,
成本是读几个字节。要不要做是另一个决定,这里不下结论。

## 3. 并发去重 ✅

一批 eval 同时启动、都用同一个未缓存的镜像,是**默认场景**而不是边界情况。

```go
func (p *PullingProvider) Prepare(...) (*Rootfs, error) {
    rootfs, err := p.Inner.Prepare(ctx, sandboxID, imageRef, opts)
    if !errors.Is(err, ErrNotCached) {
        return rootfs, err        // 命中,或者是别的错误
    }
    if err := p.ensure(ctx, imageRef); err != nil {   // ← 这里去重
        return nil, err
    }
    return p.Inner.Prepare(ctx, sandboxID, imageRef, opts)
}
```

`ensure` 用 in-flight map 把同一个 ref 的并发请求合并成一次转换,
其余的等结果。**等待方共享结果但保留自己的 cancellation** ——
一个放弃了的客户端不该被别人的拉取拖住,这一点在注释里明确写了。

不去重的后果不只是浪费:N 个进程同时 `mkfs` + 解压同一个镜像到不同临时文件,
磁盘 IO 打满,而它们最后只有一个的产物有用。

## 4. 冷启动的时间去哪了 ⚠️

实测:

```
busybox   5–10 s
alpine    2m45s(网络不稳时)
```

**2m45s 几乎全是网络** —— 拉压缩层。转换本身(mkfs + 解压)是秒级。
所以:

- **prewarm 是必需的,不是优化**。一批 eval 开始前必须把镜像铺到目标节点,
  否则第一批 sandbox 全在等下载
- 这也是 **overlaybd lazy-pull 的价值所在**:它把「下载全部层」换成
  「按需读实际用到的块」。已实测挂载 7ms、只传 19.6% 的层字节就能挂载并读文件
  (decisions §3.1)。**但尚未接入** —— 见 §7

镜像亲和调度是同一个问题的另一面:同一镜像的重复 eval run 应该落到已有它的节点上。
这条已实装(`Cached()` → 心跳 → 调度打分)。

## 5. 镜像配置:ENV / ENTRYPOINT / CMD / WORKDIR ✅

一个镜像是两样东西:层,以及描述「怎么启动这些层」的 config blob。上面的转换处理了前者、
**丢掉了后者**——把层拍平成 ext4 不携带任何元数据。所以 config 必须走另一条路,
而缺了这条路时,依赖自身 `ENV` 或 `WORKDIR` 的镜像会启动错误,且任何地方都不报错。

这条路分两半,因为 registry 只在转换时可达、创建时不可达:

```
convert  FetchConfig(manifest.Config.Digest)  → Config{Env,Entrypoint,Cmd,WorkingDir,User}
             │                                    写进 .ref sidecar
             ▼
create   Provider.Config(ref) → *Config    ┐
         spec.Cmd / spec.Env               ├→ MergeConfig → Process{Argv,Env,Workdir,User}
                                           ┘        │
                                    StartUserProcessRequest → beand exec
```

写进与 ref、digest 同一个 `.ref` sidecar 而非另开文件:这样一次原子写就发布了节点关于
这个镜像知道的全部信息,读者也不可能看到两者互相矛盾。

### 字段对应关系

| OCI config | 落到哪里 | 状态 |
|---|---|---|
| `Env` | 作为底,被请求的 env 按 key 覆盖 | ✅ |
| `Entrypoint` | `Argv` 头部,**永远保留** | ✅ |
| `Cmd` | `Argv` 尾部,请求带 cmd 时被替换 | ✅ |
| `WorkingDir` | `Workdir`,请求未指定时使用 | ✅ |
| `User` | 记录在 `Process` 上,**未生效** | 📐 |
| `VOLUME` / `EXPOSE` / `HEALTHCHECK` | 忽略 / 仅元数据 / 不执行 | ➖ 与容器运行时一致 |

### 合并规则,以及最容易搞错的那条

```
Argv     = Entrypoint ++ (request.Cmd 非空则用它,否则用 image.Cmd)
Env      = image.Env 作底,request.Env 按 key 覆盖
Workdir  = request.Workdir,否则 image.WorkingDir,否则 agent 默认值
```

**请求的命令替换 `Cmd`,但不动 `Entrypoint`。** 这正是
`docker run python:3.12 -c 'print(1)'` 能把参数传给解释器、而不是去 exec `-c` 的原因。
两个一起覆盖的写法,在所有 `Entrypoint` 为空的镜像上看起来都对——而那恰好是大家测试时
最常用的那批——却会在声明了 `Entrypoint` 的镜像上出错。`config_test.go` 的表驱动测试
专门钉住了这个 case。

Env 按 key 合并而非整体替换,理由同源:镜像的 `PATH` 和调用方多加的一个变量都得活下来,
不能要求调用方为了加一个变量就把镜像的整个环境重写一遍。

### 不同镜像来源的 config

| 来源 | config | 原因 |
|---|---|---|
| registry 拉取 | 来自 config blob | 转换时抓取 |
| `commit` | 从源镜像继承 | commit 改的是文件系统,不是环境的启动方式 |
| `build` | **没有** 📐 | buildctl 被要求导出 `type=tar` 扁平 rootfs,不含镜像元数据;要拿到 Dockerfile 的 `ENV`/`ENTRYPOINT` 得改成从 builder 导出 OCI 镜像 |

没有 config 时读回来是 `nil`,而不是空的 `Config`;`nil` 的含义是「只按请求启动」——
也就是记录 config 之前所有镜像的行为。区分两者有意义:空 `Config` 会声称
「这个镜像确实没有 entrypoint」。

### `User` 已记录但未生效 📐

值存下来了、也到了 `Process`,但一切仍以 root 运行。它无法在其余字段生效的地方生效:
beand 是 PID 1,降低自己的 uid 就会失去之后 exec 任何东西的能力——必须在子进程里做。
而解析 `nobody` 这类名字还需要 guest 自己的 `/etc/passwd`,那要等 pivot 到镜像 rootfs
之后才存在。所以这是一个独立改动,不是漏掉的一行。

## 6. commit:反向路径 ✅

`commit` 把运行中 sandbox 的文件系统封成新的 base 镜像。

当前实现是**从 `/dev/mapper` 上的合成设备读出一个完整 ext4**,
而不是「seal 一个增量层」。理由是 dm-snapshot 的 CoW 层不是 OCI 层格式,
没法直接当层用。

代价:commit 产物是全量镜像而非增量。overlaybd 接入后可以改成
`overlaybd-commit` seal LSMT 可写层 —— 那才是真正的零转换(image-build §2)。

## 7. overlaybd 路径 ✅

拍平之外的另一条路,用 `--fc-overlaybd` 选择。默认关闭 —— 不显式开启时,
节点走的仍是上面的 dm-snapshot。

| | dm-snapshot(默认) | overlaybd |
|---|---|---|
| 首次使用 | 拉全量 + 转换,分钟级 | 按需读块,挂载 7ms |
| 每 sandbox 成本 | 44 KiB(已实测) | 相当,都只存改动 |
| 层共享 | **没有** —— 每个镜像一个独立 ext4 | 按 digest 共享,每层只存一份 |
| 转换 CPU | 每个镜像都付一次,共享层也重复付 | 每个不同的层只付一次 |
| commit | 读出全量 ext4 | 原地 seal 可写层 |

真正让这件事值得做的收益,不是原先写在这里的那个。旧版说价值在首次使用的等待时间、
而 prewarm 能遮掉它 —— 这话没错,但漏了 prewarm **遮不掉**的两项:共享的层只存一份
而不是每个镜像各存一份,以及共享 base 的转换 CPU 每个节点只付一次而不是每个镜像付一次。

### 两个 backend 同机实测 ✅

`hack/overlaybd-bench.sh`。三个 python `-slim` 镜像,共享同一个 debian base
(按 manifest 两两之间 1.51x)。量的是**实占块**而非表面大小 —— 拍平出的 ext4 是稀疏文件,
表面 2.0 GiB、实占约 130 MiB,用表面大小会把拍平路径高估 15 倍。

| | dm-snapshot | overlaybd |
|---|---|---|
| 镜像目录,2 个镜像 | 261 MiB | 94 MiB(**省 2.78 倍**) |
| 镜像目录,3 个镜像 | 392 MiB | 118 MiB(**省 3.32 倍**) |
| noded CPU,第 1 个镜像 | 2.32 s | 1.37 s |
| noded CPU,第 2 个镜像 | 2.24 s | **0.49 s** |
| noded CPU,第 3 个镜像 | 2.15 s | **0.44 s** |

CPU 那一列就是层共享这件事变得可观测的样子。dm-snapshot 上每个镜像的转换开销都一样,
因为每个都要把共享 base 自己拍平一遍;overlaybd 上第 2、3 个镜像只花第 1 个的约三分之一,
因为共享层已经封装在节点上了,只有它们各自独有的层是新的。

磁盘比例随镜像集增大(2.78x → 3.32x)是同一效应的另一面:每多一个镜像,
拍平存储要多存一份完整 base,而层存储几乎不增加。

**注意这组数替换掉了什么。** 这里原先写的 3.1x 来自 `hack/layer-amplification.go`,
那是个读 manifest、按压缩 blob 大小外推的工具,**从来不是这份实现的实测**。
上面这些才是。两者结果吻合 —— 但吻合只是对那个外推的一次校验,不能代替把实测做掉。

**create 延迟没有改善。** 两个 backend 冷启动都是 12–32 s,主要是 registry 下载时间,
同一 backend 不同次之间的差异比两个 backend 之间的差异还大。这条改动完全没碰冷路径,
见本节末尾。

### 层构建流水线

```
registry 层 (tar.gz)
  │  解压 —— overlaybd-apply 读 tar,不读 tar.gz
  ▼
overlaybd-create --mkfs data index <GB>     建带文件系统的空层
overlaybd-apply  layer.tar apply.json      把 tar 写进去
overlaybd-commit -z -t data index out      封成 zfile blob
  ▼
<layerDir>/sha256-<digest>.obd             按 OCI digest 命名,因此可共享
```

按 digest 而非按镜像命名:这就是层共享的全部机制,也意味着第二个镜像引用
已在本地的层时不需要任何转换。

每 sandbox 的可写层是 `overlaybd-create -s`(稀疏),成本只是写入的块而非虚拟大小 ——
实测空闲 sandbox 占 40 KiB,而表面大小是 1.1 GB。

### 怎么暴露成块设备

overlaybd 自己没有块设备接口,由内核的 SCSI target 子系统提供,而驱动它的方式
全是往 configfs 写文件。顺序不是风格问题:

```
1. mkdir  core/user_999/<name>              建 TCMU backstore
2. write  control = dev_config=overlaybd/<config.json>,dev_size=N
3. write  wwn/vpd_unit_serial = <hex>       必须在 enable 之前,见下
4. write  enable = 1
5. mkdir  loopback/<wwn>/tpgt_1
6. write  tpgt_1/nexus = <wwn>              必须在 LUN 之前
7. mkdir  tpgt_1/lun/lun_0
8. symlink lun_0/virtual_scsi_port -> backstore
```

### 四条约束,每条都来自真机

这些都不会给出指明原因的报错,所以在代码里连同理由一起强制,而不是留给文档。

**1. nexus 必须在 LUN 之前。** SCSI host 在 fabric 注册时扫描 LUN,而注册发生在写
nexus 那一刻;之后再链接的 LUN 永远不会被扫描,后写 nexus 也不触发重扫。
实测:顺序错时没有设备出现,而 configfs 报告 `enable=1`、`Status: ACTIVATED`,
overlaybd 自己的 result 文件也说 success。没有任何地方报错。

**2. 每个 backstore 都要有唯一的 unit serial,且必须在 `enable=1` 之前写。**
TCMU 默认不提供,于是设备 WWID 都是 `36001405` 后跟一串零。multipathd 看到相同
WWID,判定它们是同一个 LUN 的两条路径,于是合并 —— **把一个 sandbox 的数据喂给
另一个**,而原设备还会变成 busy 无法直接挂载。已现场复现:没设 serial 的设备被并进
`mpatha`,设了的那个保持独立。

**3. serial 必须只含十六进制字符。** 内核用 serial 里的**十六进制字符**构造 WWID,
其余全部丢弃:`bean-aaa` 变成 `naa.6001405beaaaa000...`。所以 `bean-sbx-alpha` 和
`bean-probe-2` 都归约成 `beabaa` —— 两个看起来唯一实际相撞的 serial,
也就是第 2 条那个坑,只是更难发现。`deviceSerial` 把 sandbox id 哈希成十六进制,
而 `attachTCMU` 对非十六进制的 serial **直接拒绝**而不是替调用方净化,
这样到内核的值就是调用方选的值。

**4. 下层必须是 sealed 的。** 把刚 apply 完(未 seal)的层当 lower,daemon 会报
`trailer magic, trailer type, file type or sealedness doesn't match` 并让 backstore
停在 DEACTIVATED —— 而这传到调用方只是 `enable` 写入时的 `ENOENT`,真实原因在
`/var/log/overlaybd.log` 里。相关地,**构建**层时喂给 `overlaybd-apply` 的临时 config
的 lowers 必须**为空**:把该层写成自己的 lower 等于让 overlaybd 把同一个文件同时当
只读父层和可写目标,报错只有 `failed to create image file`。

另外两个「看起来该能用但不能」的细节。teardown **不写** `enable=0` ——
内核拒绝这个写入(`For dev_enable ops, only valid value is 1`),backstore 靠删目录拆除。
以及查 LUN 对应的块设备要走 WWID,因为 `udev_path` 始终为空、SCSI model 对所有这类设备
都读作 `TCMUdevice`、而 `statistics/scsi_port/dev` 是全局端口计数器而非 tcm_loop
adapter 号(某个报 26 的 LUN 实际由 `tcm_loop_adapter_24` 提供)。

### 验证了什么,没验证什么

三个层次,因为每一层回答的问题都是下一层答不了的。

**provider 层** —— `internal/node/image/overlaybd_hw_linux_test.go` 跑真二进制、
真 configfs、真块设备,宿主不支持时跳过。内容:构建并封装层(并确认第二次调用复用)、
attach、**挂载设备并读回层里那个文件**、确认两个设备拿到不同 WWID。

**约束层** —— `hack/overlaybd-probe.sh` 断言测试断言不了的反面情况:
LUN 在 nexus 之前链接时确实没有设备出现,以及在没设 serial 的设备上现场复现
multipathd 合并。

**端到端** —— `hack/overlaybd-e2e.sh` 用 `--fc-overlaybd` 起一个真的全栈,
**从 overlaybd 设备启动一个 sandbox**。在验证机(内核 5.15、TCMU、multipathd active、
AMD EPYC 7542)上全部通过:节点选中 overlaybd 而非降级、`bean run --image alpine:3.20`
成功、guest 从自己的 rootfs 读到 `PRETTY_NAME="Alpine Linux v3.20"`、写入落到可写层、
运行中的 sandbox 有对应 TCMU backstore、镜像的 PATH 进了 guest 环境、
`bean kill` 之后无 backstore 残留也无 multipath 设备。

最后这层才是关键:宿主能挂载的设备和 guest 能从它启动是两个不同的断言,
只有这一层能把这个缺口补上。

**尚未验证**:对真 registry 的 lazy pull(`--fc-overlaybd-lazy-pull` 已实现但没测过 ——
实测的 7ms 挂载和 19.6% 传输量来自 decisions §3.1 的手工验证,不是这份代码)、
走 `CommitSandbox` 的 commit、以及并发扇出下的表现。ublk 会比 TCMU 快但需要内核 ≥ 6.0;
TCMU 功能上是完整的。

用之前值得知道的一个数:**镜像首次使用比 CLI 默认等待时间更慢**,因为要先转换层
才能组设备。e2e 脚本给了 120s。这条改动完全没有改善冷路径 —— prewarm 仍是必需的,
真正能消掉它的是 lazy pull。

## 8. 还没有的 📐

- **缓存回收**:`ImageDir` 里的 base 镜像不自动清理。设计里有镜像粒度 LRU +
  chunk LRU(noded-design §4.2),零实现。长期跑会填满磁盘
- **构建产物上传 S3**:构建出的镜像落节点本地,所以**只在构建它的节点可用**
  (GitHub #22)。多节点集群里这基本等于不可用
- **digest 校验**:拉取时不校验层 digest。registry 是可信的前提下问题不大,
  但这是供应链防护的一环

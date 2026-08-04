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
```

分层而不是一个大 provider 的理由:**「镜像从哪来」和「块设备怎么组」是两件事**。
`PullingProvider` 包着任意一个内层实现,所以「首次使用时拉取」这个行为
不需要在每个块设备后端里重复实现;换 overlaybd 后端时它也不用改。

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
  (decisions §3.1)。**但尚未接入** —— 见 §6

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

## 7. 与 overlaybd 的关系 ⚠️

**能力已实测跑通,尚未接入代码。** 现状与目标形态的差别:

| | 当前(dm-snapshot) | 目标(overlaybd) |
|---|---|---|
| 首次使用 | 拉全量 + 转换,分钟级 | 按需读块,挂载 7ms |
| 每 sandbox 成本 | 44 KiB(已实测) | 相当,CoW 都只存改动 |
| 层格式 | 转成单个 ext4,丢掉层结构 | 保留 LSMT 层,可直接 seal |
| commit | 读出全量 ext4 | seal 可写层,零转换 |

**关键认识:overlaybd 的价值在「首次使用大镜像的等待时间」,不在「每 sandbox 成本」——
后者 dm-snapshot 的 CoW 已经解决了。** 所以它是优化项而非基础设施,
这也是它被排在快照能力之后的理由。

接入的工作量是写一个 `OverlaybdProvider` 实现同一个 `Provider` 接口:
configfs 编排(TCMU backstore → tpgt → nexus → LUN,**顺序不能错**)、
registry 推送、生命周期。两个只有真机能发现的陷阱记在 decisions §3.1:
LUN 必须在 nexus 之后链接,以及每个 backstore 必须设唯一 `vpd_unit_serial`
否则宿主 `multipathd` 会合并不同镜像的设备并**静默返回错误数据**。

## 8. 还没有的 📐

- **缓存回收**:`ImageDir` 里的 base 镜像不自动清理。设计里有镜像粒度 LRU +
  chunk LRU(noded-design §4.2),零实现。长期跑会填满磁盘
- **构建产物上传 S3**:构建出的镜像落节点本地,所以**只在构建它的节点可用**
  (GitHub #22)。多节点集群里这基本等于不可用
- **digest 校验**:拉取时不校验层 digest。registry 是可信的前提下问题不大,
  但这是供应链防护的一环

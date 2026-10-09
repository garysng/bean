# bean-ci-runner

> English: [../ci-runner.md](../ci-runner.md)

在 bean 沙箱上跑仓库的 GitHub Actions job:一个 job 一个沙箱,跑完即销毁。

## 为什么

账户账单出问题时,GitHub 托管的 runner 会停止工作。2026-10-09,`garysng/maodou` 的每个 job
都在两秒内失败,原因是「最近的账户付款失败,或者需要提高消费上限,所以 job 没有启动」。
GitHub 文档写明 self-hosted runner 的用量是免费的,所以预期 self-hosted 上的 job 仍能运行。
这个预期要靠第一次真机运行来证实,不是假设(见「验证」)。

bean 本身就是沙箱平台,所以 runner 不需要单独的虚拟机。

## 形态

```
bean-ci-runner(控制器,在沙箱之外)
  │
  │ 1. POST /repos/{repo}/actions/runners/generate-jitconfig   → 一次性的 JIT 配置
  │ 2. POST /v1/sandboxes   imageRef=ghcr.io/actions/actions-runner
  │ 3. POST /v1/sandboxes/{id}/exec  /home/runner/run.sh
  │      env ACTIONS_RUNNER_INPUT_JITCONFIG=<jit>, RUNNER_ALLOW_RUNASROOT=1
  │      → runner 注册、只接一个 job、然后退出
  │ 4. DELETE /v1/sandboxes/{id}
  │ 5. DELETE /repos/{repo}/actions/runners/{runner_id}   (只在它仍然注册着时)
```

`serve --concurrency N` 保持 N 个这样的循环在跑。workflow 用 `runs-on: [self-hosted, bean]` 选中它们。

### 决策

| 决策 | 理由 |
|---|---|
| 用 **JIT 配置**,不用注册 token | JIT 配置绑定一个 runner、一个 job。注册 token 在一小时内可以注册任意多个 runner |
| 通过 `imageRef` 用**官方 runner 镜像** | bean 在创建时就能转换任意 OCI 镜像,所以没有模板要构建、要维护。Novita 需要构建模板;这正是 bean 更简单的地方 |
| **凭证放在 exec 的 `env`** 里,不放命令行参数 | 沙箱里任何进程都能从 `/proc/<pid>/cmdline` 读到命令行参数,而 job 就跑在这个沙箱里 |
| **退出后删除 runner 注册** | 沙箱在 runner 连上之前就死掉,会留下一个已注册、离线的 runner。完成了 job 的 JIT runner 已经自动消失,所以这里返回 404 算成功 |
| 创建时设 **`lifecycle.idleTimeout`** | 控制器在 job 中途被杀,会留下它的沙箱。服务端的闲置超时可以不依赖控制器把它回收 |
| **job 超时 ≤ 3540 秒** | bean 的单次 exec 上限是 3600 秒。留出的余量给 runner 启动 |

## 不做的事

- 按队列按需扩缩。`serve` 保持固定数量的空闲 runner。按队列扩缩需要 `workflow_job` webhook,是后续一步
- 组织级 runner。只做仓库级,runner group 1
- job 之间的缓存。一个 job 一个全新沙箱正是目的;代价是每个 job 都要重新下载工具链

## 验证

每一步都对着真实的 bean 栈和真实的仓库运行:

1. 用 `ghcr.io/actions/actions-runner` 创建的沙箱能启动,而且 `run.sh` 存在
2. runner 能连上 GitHub,并在仓库的 runner 列表里显示为 `online`
3. 一个真实的 workflow job 被派发给它并跑完 —— 这一步证明了「托管 runner 被账单拦住时,self-hosted 的 job 仍能运行」
4. job 结束后:沙箱从 `GET /v1/sandboxes` 里消失,runner 从仓库的 runner 列表里消失
5. 在 job 中途杀掉控制器:沙箱被它的闲置超时回收

单元测试让控制器对着假的 GitHub 和 bean HTTP 服务运行。它们能证明调用顺序和清理路径,
证明不了第 2、3、5 步。

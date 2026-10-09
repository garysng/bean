# bean-ci-runner

> 中文版:[zh/ci-runner.md](zh/ci-runner.md)

Run a repository's GitHub Actions jobs on bean sandboxes: one job, one sandbox, destroyed
afterwards.

## Why

GitHub-hosted runners stop when the account's billing fails. On 2026-10-09 every job in
`garysng/maodou` failed in two seconds with "The job was not started because recent account
payments have failed or your spending limit needs to be increased". GitHub documents
self-hosted runner usage as free, so jobs on a self-hosted runner are expected to keep running.
That expectation is verified by the first real run, not assumed (see Verification).

bean is a sandbox platform already, so a runner does not need a VM of its own.

## Shape

```
bean-ci-runner (controller, outside the sandbox)
  │
  │ 1. POST /repos/{repo}/actions/runners/generate-jitconfig   → single-use JIT config
  │ 2. POST /v1/sandboxes   imageRef=ghcr.io/actions/actions-runner
  │ 3. POST /v1/sandboxes/{id}/exec  /home/runner/run.sh
  │      env ACTIONS_RUNNER_INPUT_JITCONFIG=<jit>, RUNNER_ALLOW_RUNASROOT=1
  │      → the runner registers, takes exactly one job, exits
  │ 4. DELETE /v1/sandboxes/{id}
  │ 5. DELETE /repos/{repo}/actions/runners/{runner_id}   (only if it is still registered)
```

`serve --concurrency N` keeps N of these loops running. A workflow selects them with
`runs-on: [self-hosted, bean]`.

### Decisions

| Decision | Why |
|---|---|
| **JIT config**, not a registration token | A JIT config is bound to one runner and one job. A registration token can register any number of runners for an hour |
| **Official runner image** via `imageRef` | bean converts any OCI image on create, so there is no template to build or keep current. Novita needed a template build; this is the difference that makes bean the simpler host |
| **Credential in exec `env`**, not argv | argv is readable from `/proc/<pid>/cmdline` by any process in the sandbox; the job runs in that sandbox |
| **Delete the runner registration after exit** | A sandbox that dies before the runner connects leaves a registered, offline runner. A JIT runner that completed its job is already gone, so a 404 here is success |
| **`lifecycle.idleTimeout` on create** | A controller killed mid-job leaves its sandbox behind. The server-side idle timeout reclaims it without the controller |
| **Job timeout ≤ 3540 s** | bean caps an exec at 3600 s. The margin covers runner start-up |

## What this does not do

- Scale on demand from the queue. `serve` keeps a fixed number of idle runners. Queue-driven
  scaling needs a `workflow_job` webhook and is a later step
- Organisation-level runners. Repository level only, runner group 1
- Cache between jobs. One job, one fresh sandbox is the point; the cost is that every job
  downloads its toolchain

## Verification

Each step is run against the real bean stack and the real repository:

1. A sandbox from `ghcr.io/actions/actions-runner` starts and `run.sh` exists
2. The runner reaches GitHub and shows `online` in the repository's runner list
3. A real workflow job is dispatched to it and finishes — this is what proves self-hosted jobs
   run while hosted ones are blocked by billing
4. After the job: the sandbox is gone from `GET /v1/sandboxes` and the runner is gone from the
   repository's runner list
5. Kill the controller mid-job: the sandbox is reclaimed by its idle timeout

Unit tests run the controller against fake GitHub and bean HTTP servers. They prove the call
order and the cleanup paths; they cannot prove 2, 3 or 5.

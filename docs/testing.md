# 本地测试与 GitHub 配置

这份文档说明如何用本地 E2B sandbox 环境测试服务，以及如何在 GitHub 仓库中配置 self-hosted runner 自动拉起。

## 1. 本地环境变量

服务必需变量：

```bash
export E2B_API_KEY="<e2b api key>"
export E2B_API_URL="<e2b api url>"
export E2B_DOMAIN="<e2b domain>"
export GITHUB_TOKEN="<github token>"
export GITHUB_WEBHOOK_SECRET="<random webhook secret>"
export RUNNER_SCOPE="repo"
export GITHUB_OWNER="<repo owner>"
export GITHUB_REPO="<repo name>"
export SANDBOX_TEMPLATE_ID="<template id>"
```

组织级 runner 配置：

```bash
export RUNNER_SCOPE="org"
export GITHUB_ORG="<org name>"
```

`RUNNER_SCOPE=repo` 时，runner 注册到 `https://github.com/<GITHUB_OWNER>/<GITHUB_REPO>`。`RUNNER_SCOPE=org` 时，runner 注册到 `https://github.com/<GITHUB_ORG>`，组织内仓库需要被对应 runner group 允许使用。

可选变量：

```bash
export HTTP_ADDR=":8080"
export STATE_DIR="./var/runners"
export RUNNER_LABELS="self-hosted,e2b"
export SANDBOX_TIMEOUT_SECONDS="3600"
export MAX_CONCURRENT_RUNNERS="1"
```

## 2. GitHub Token 权限

`GITHUB_TOKEN` 需要能调用对应 scope 的 runner registration token API。

Repository runner 推荐使用 fine-grained personal access token：

- Repository access：只选择目标仓库。
- Permissions：`Administration` 设为 `Read and write`。

Organization runner 需要组织级 self-hosted runner 管理权限。注册端点是：

```text
POST /orgs/{org}/actions/runners/registration-token
```

也可以用 classic PAT，但权限面会更大，不推荐作为长期方案。

## 3. 启动服务

```bash
go run ./cmd/runnerd
```

健康检查：

```bash
curl -fsS http://127.0.0.1:8080/healthz
```

手动创建一个 runner：

```bash
curl -fsS -X POST http://127.0.0.1:8080/runners \
  -H 'content-type: application/json' \
  -d '{"id":"manual-001","labels":["self-hosted","e2b"]}' | jq
```

查看状态：

```bash
curl -fsS http://127.0.0.1:8080/runners | jq
curl -fsS http://127.0.0.1:8080/runners/manual-001 | jq
```

停止 runner：

```bash
curl -fsS -X DELETE http://127.0.0.1:8080/runners/manual-001 | jq
```

状态文件会写到：

```text
var/runners/<request_id>/
  request.json
  state.json
  control.log
  stdout.log
  stderr.log
```

## 4. 暴露 Webhook 地址

GitHub webhook 必须能访问到本地服务。任选一种方式：

```bash
ngrok http 8080
```

或：

```bash
cloudflared tunnel --url http://127.0.0.1:8080
```

最终 webhook URL 形如：

```text
https://<public-host>/webhooks/github
```

## 5. 配置 GitHub Repository Webhook

在目标仓库中进入：

```text
Settings -> Webhooks -> Add webhook
```

填写：

- Payload URL：`https://<public-host>/webhooks/github`
- Content type：`application/json`
- Secret：和 `GITHUB_WEBHOOK_SECRET` 完全一致。
- Which events：选择 `Workflow jobs`。
- Active：勾选。

保存后，GitHub 会发送一次 ping。当前服务只处理 `workflow_job` 事件，非 `workflow_job` 会返回 ignored，这是正常的。

## 6. 配置 GitHub Actions Workflow

在目标仓库添加：

```yaml
name: e2b-runner-smoke

on:
  workflow_dispatch:

jobs:
  smoke:
    runs-on: [self-hosted, e2b]
    steps:
      - name: Print runner info
        run: |
          uname -a
          whoami
          pwd
```

触发 `workflow_dispatch` 后预期流程：

1. GitHub 创建一个 `workflow_job.queued` webhook。
2. 本服务校验签名并创建 `var/runners/<workflow_job.id>/`。
3. 服务创建 sandbox，获取 GitHub registration token，并在 sandbox 内启动 ephemeral runner。
4. GitHub job 被 `self-hosted,e2b` runner 接走执行。
5. runner 进程退出后，服务清理对应 sandbox。

## 7. 排查顺序

先看服务状态：

```bash
curl -fsS http://127.0.0.1:8080/runners | jq
```

再看请求目录：

```bash
cat var/runners/<request_id>/state.json
cat var/runners/<request_id>/control.log
cat var/runners/<request_id>/stdout.log
cat var/runners/<request_id>/stderr.log
```

常见问题：

- `invalid signature`：GitHub webhook secret 和 `GITHUB_WEBHOOK_SECRET` 不一致。
- `runner concurrency limit reached`：活跃状态目录数量达到 `MAX_CONCURRENT_RUNNERS`。
- GitHub job 一直 queued：workflow 的 `runs-on` labels 必须包含 `self-hosted` 和 `e2b`。
- sandbox 创建失败：确认 `E2B_API_KEY`、`E2B_API_URL`、`E2B_DOMAIN` 是否匹配本地环境。
- registration token 失败：确认 `GITHUB_TOKEN` 对目标仓库有 `Administration: Read and write` 权限。

## 8. GitHub Actions 日志怎么看

这是 repository 级 self-hosted GitHub Actions runner。job 被 sandbox 里的 runner 接走后，workflow step 的日志会正常显示在 GitHub Actions 页面里：

```text
Repository -> Actions -> 选择 workflow run -> 选择 job
```

能在 GitHub Actions 里看到：

- workflow step 的 stdout/stderr。
- checkout、build、test 等每个 step 的日志。
- job 成功、失败、取消状态。

不能完整依赖 GitHub Actions 看到：

- sandbox 创建失败日志，因为 runner 还没注册上 GitHub。
- runner 下载、`config.sh` 注册、`run.sh` 启动前的错误。
- webhook 校验失败、GitHub token 申请失败、sandbox API 调用失败。

这些控制面日志看本服务本地：

```bash
cat var/runners/<request_id>/state.json
cat var/runners/<request_id>/control.log
cat var/runners/<request_id>/stdout.log
cat var/runners/<request_id>/stderr.log
```

服务自身日志会输出到启动 `go run ./cmd/runnerd` 的终端。

## 9. 官方参考

- GitHub self-hosted runner workflow labels: https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/using-self-hosted-runners-in-a-workflow
- GitHub self-hosted runner autoscaling: https://docs.github.com/en/actions/hosting-your-own-runners/autoscaling-with-self-hosted-runners
- GitHub webhook `workflow_job` event: https://docs.github.com/en/webhooks/webhook-events-and-payloads

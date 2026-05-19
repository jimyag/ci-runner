# 本地测试与 GitHub 配置

这份文档说明如何用本地 E2B sandbox 环境测试服务，以及如何在 GitHub 仓库中配置 self-hosted runner 自动拉起。

## 1. 本地环境变量

服务必需变量：

```bash
export E2B_API_KEY="<e2b api key>"
export E2B_API_URL="<e2b api url>"
export E2B_DOMAIN="<e2b domain>"
export ADMIN_TOKEN="<random admin token>"
export GITHUB_WEBHOOK_SECRET="<random webhook secret>"
export RUNNER_SCOPE="repo"
export GITHUB_OWNER="<repo owner>"
export GITHUB_REPO="<repo name>"
export SANDBOX_TEMPLATE_ID="<template id>"
```

PAT 模式：

```bash
export GITHUB_TOKEN="<github token>"
```

GitHub App 模式：

```bash
export GITHUB_APP_ID="<github app id>"
export GITHUB_APP_INSTALLATION_ID="<installation id>"
export GITHUB_APP_PRIVATE_KEY_FILE="./secrets/github-app.pem"
```

组织级 runner 配置：

```bash
export RUNNER_SCOPE="org"
export GITHUB_ORG="<org name>"
```

`RUNNER_SCOPE=repo` 时，runner 注册到 `https://github.com/<GITHUB_OWNER>/<GITHUB_REPO>`。`RUNNER_SCOPE=org` 时，runner 注册到 `https://github.com/<GITHUB_ORG>`，组织内仓库需要被对应 runner group 允许使用。

可选变量：

```bash
export HTTP_ADDR=":25500"
export STATE_DIR="./var/runners"
export RUNNER_LABELS="self-hosted,e2b"
export SANDBOX_TIMEOUT_SECONDS="3600"
export SANDBOX_API_TIMEOUT_SECONDS="60"
export SANDBOX_CREATE_TIMEOUT_SECONDS="120"
export SANDBOX_STOP_TIMEOUT_SECONDS="30"
export RECOVERY_TIMEOUT_SECONDS="120"
export HTTP_READ_TIMEOUT_SECONDS="15"
export HTTP_WRITE_TIMEOUT_SECONDS="60"
export HTTP_IDLE_TIMEOUT_SECONDS="120"
export MAX_CONCURRENT_RUNNERS="100"
export RUNNERD_CONFIG_FILE="./runnerd.yaml"
```

可选的 `runnerd.yaml` 可以 seed profiles 和 repository policies，例如：

```yaml
profiles:
  - name: default
    labels: [self-hosted, e2b]
    template_id: <template id>
    runner_group: default
    max_concurrency: 100
    enabled: true

repository_policies:
  - repository: <owner>/<repo>
    allowed_profiles: [default]
```

## 2. GitHub Token 权限

PAT 模式下，`GITHUB_TOKEN` 需要能调用对应 scope 的 runner registration token API。

Repository runner 推荐使用 fine-grained personal access token：

- Repository access：只选择目标仓库。
- Permissions：`Administration` 设为 `Read and write`。

Organization runner 需要组织级 self-hosted runner 管理权限。注册端点是：

```text
POST /orgs/{org}/actions/runners/registration-token
```

也可以用 classic PAT，但权限面会更大，不推荐作为长期方案。GitHub App 模式下，runnerd 会自动用 installation token 调对应 registration token API。

## 3. 启动服务

```bash
go run ./cmd/runnerd
```

健康检查：

```bash
curl -fsS http://127.0.0.1:25500/healthz
```

后台管理页面：

```text
http://127.0.0.1:25500/admin/
```

页面会要求输入 `ADMIN_TOKEN`，之后在浏览器 local storage 中保存 token，并对 `/runners` 管理接口自动携带 `Authorization: Bearer $ADMIN_TOKEN`。

后台页面源码在 `ui/`，使用和 `kubevirt-console` 相同的 React、Vite、Tailwind CSS、shadcn 风格组件和主题 CSS。`task build` 会先执行 `task ui-build`，把前端产物写入 `internal/server/admin/` 后再编译 `runnerd`。管理面现在包含 runners、profiles、repository policies、label match test 和 diagnostics 页面。

手动创建一个 runner：

```bash
curl -fsS -X POST http://127.0.0.1:25500/runners \
  -H "authorization: Bearer ${ADMIN_TOKEN}" \
  -H 'content-type: application/json' \
  -d '{"id":"manual-001","repository_full_name":"<owner>/<repo>","profile_name":"default"}' | jq
```

查看状态：

```bash
curl -fsS -H "authorization: Bearer ${ADMIN_TOKEN}" http://127.0.0.1:25500/runners | jq
curl -fsS -H "authorization: Bearer ${ADMIN_TOKEN}" http://127.0.0.1:25500/runners/manual-001 | jq
```

停止 runner：

```bash
curl -fsS -X DELETE -H "authorization: Bearer ${ADMIN_TOKEN}" http://127.0.0.1:25500/runners/manual-001 | jq
```

状态库默认会写到：

```text
var/runners/runnerd.db
```

runner 的 control/stdout/stderr 日志存放在 DB-backed event store 里，仍然通过管理 API 读取：

```bash
curl -fsS -H "authorization: Bearer ${ADMIN_TOKEN}" \
  http://127.0.0.1:25500/runners/manual-001/logs/control.log
curl -fsS -H "authorization: Bearer ${ADMIN_TOKEN}" \
  http://127.0.0.1:25500/runners/manual-001/logs/stdout.log
curl -fsS -H "authorization: Bearer ${ADMIN_TOKEN}" \
  http://127.0.0.1:25500/runners/manual-001/logs/stderr.log
```

## 4. 暴露 Webhook 地址

GitHub webhook 必须能访问到本地服务。任选一种方式：

```bash
ngrok http 25500
```

或：

```bash
cloudflared tunnel create e2b-local-runner
cloudflared tunnel route dns e2b-local-runner runner.example.com
cloudflared tunnel run --url http://127.0.0.1:25500 e2b-local-runner
```

最终 webhook URL 形如：

```text
https://<public-host>/webhooks/github
```

这里的 `runner.example.com` 换成你自己的域名；不要把临时 quick tunnel 的随机 `trycloudflare.com` 地址写死到 GitHub 配置里。

公网部署时只需要把 `/webhooks/github` 暴露给 GitHub。`/runners` 管理接口也可以在同一个服务上访问，但必须带 `Authorization: Bearer $ADMIN_TOKEN`；生产环境建议放在 HTTPS 反向代理后面，并限制管理接口来源 IP。

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
2. 本服务校验签名并在状态库里写入一条 `queued` runner request。
3. 服务创建 sandbox，获取 GitHub registration token，并在 sandbox 内启动 ephemeral runner。
4. GitHub job 被 `self-hosted,e2b` runner 接走执行。
5. runner 进程退出后，服务清理对应 sandbox。

## 7. 排查顺序

先看服务状态：

```bash
curl -fsS -H "authorization: Bearer ${ADMIN_TOKEN}" http://127.0.0.1:25500/runners | jq
```

再看 request 状态和日志：

```bash
curl -fsS -H "authorization: Bearer ${ADMIN_TOKEN}" \
  http://127.0.0.1:25500/runners/<request_id> | jq
curl -fsS -H "authorization: Bearer ${ADMIN_TOKEN}" \
  http://127.0.0.1:25500/runners/<request_id>/logs/control.log
curl -fsS -H "authorization: Bearer ${ADMIN_TOKEN}" \
  http://127.0.0.1:25500/runners/<request_id>/logs/stdout.log
curl -fsS -H "authorization: Bearer ${ADMIN_TOKEN}" \
  http://127.0.0.1:25500/runners/<request_id>/logs/stderr.log
```

常见问题：

- `invalid signature`：GitHub webhook secret 和 `GITHUB_WEBHOOK_SECRET` 不一致。
- `runner concurrency limit reached`：活跃 request 数量达到 `MAX_CONCURRENT_RUNNERS`。
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

这些控制面日志看本服务管理 API：

```bash
curl -fsS -H "authorization: Bearer ${ADMIN_TOKEN}" \
  http://127.0.0.1:25500/runners/<request_id> | jq
curl -fsS -H "authorization: Bearer ${ADMIN_TOKEN}" \
  http://127.0.0.1:25500/runners/<request_id>/logs/control.log
curl -fsS -H "authorization: Bearer ${ADMIN_TOKEN}" \
  http://127.0.0.1:25500/runners/<request_id>/logs/stdout.log
curl -fsS -H "authorization: Bearer ${ADMIN_TOKEN}" \
  http://127.0.0.1:25500/runners/<request_id>/logs/stderr.log
```

服务自身日志会输出到启动 `go run ./cmd/runnerd` 的终端。

## 9. Diagnostics / pprof

服务导入了 `github.com/jimmicro/pprof`，启动后会在 binary 所在目录生成 `.pprof` 地址文件和 dump 脚本。管理 API 可以直接查看 diagnostics：

```bash
curl -fsS -H "authorization: Bearer ${ADMIN_TOKEN}" http://127.0.0.1:25500/diagnostics/pprof | jq
curl -fsS -H "authorization: Bearer ${ADMIN_TOKEN}" http://127.0.0.1:25500/diagnostics/vars | jq
```

`/diagnostics/pprof` 会返回：

- 发现到的 pprof 地址文件
- dump 脚本路径
- 当前 DB 路径
- GitHub 鉴权模式（token/app）
- sandbox API 配置摘要
- 最近失败的 runner request

`/diagnostics/vars` 会代理本地 pprof 服务的 `GET /debug/vars`，可以直接看到 expvar 指标摘要。

## 9. 官方参考

- GitHub self-hosted runner workflow labels: https://docs.github.com/en/actions/hosting-your-own-runners/managing-self-hosted-runners/using-self-hosted-runners-in-a-workflow
- GitHub self-hosted runner autoscaling: https://docs.github.com/en/actions/hosting-your-own-runners/autoscaling-with-self-hosted-runners
- GitHub webhook `workflow_job` event: https://docs.github.com/en/webhooks/webhook-events-and-payloads

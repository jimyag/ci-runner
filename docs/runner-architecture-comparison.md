# e2b-github-runner 与 Fireactions / ARC 对比分析

> 注：你提到的是 `e2b-github-action`，但当前仓库实际项目名是 `e2b-github-runner`。下文按当前代码、`bin/fireactions` 和 `bin/actions-runner-controller` 中 clone 下来的参考实现一起分析。

## 1. 结论

当前 `e2b-github-runner` 已经跑通了 GitHub `workflow_job` webhook 到 E2B sandbox ephemeral runner 的主链路。下一阶段如果目标是公网部署、支持组织级仓库治理、提升可观测性和恢复能力，就应该从“本地目录状态”升级为“数据库状态源”，并把 Fireactions 和 ARC 中适合当前项目的设计吸收进来。

建议目标：

1. **状态存储支持 SQLite 和 Postgres。** SQLite 适合单机、本地和轻量部署；Postgres 适合公网、多实例、长期保留和复杂查询。不要再把本地文件作为主状态源。
2. **状态机固定为 `queued -> creating -> running -> stopping -> completed/failed`。** `completed` 表示 runner/job 正常结束并完成清理；`failed` 表示任意阶段不可恢复失败或超过重试上限。
3. **引入 runner spec / pool。** 每个 profile 定义 labels、sandbox template、并发上限、是否预热、runner group、允许使用的 repo。
4. **引入 repo policy。** 参考 ARC 对 runner group visibility 的处理：不是所有仓库都能使用所有 runner，必须有显式或可计算的可见性规则。
5. **引入 reconciler/sweeper。** 不只依赖 webhook 和进程内 goroutine，而是周期性从 DB 扫描异常状态并收敛。
6. **引入 metrics / diagnostics。** 运行时诊断使用 `github.com/jimmicro/pprof`；业务指标通过 `expvar` 注册，并由该包提供的 `/debug/vars` 暴露。指标内容参考 Fireactions 的 pool 指标和 ARC 的 workflow job 指标。
7. **DB 访问层使用 GORM，但 schema 和索引由手写 SQL migration 控制。** GORM 只负责常规 CRUD、事务、模型映射和查询组合，不用 tag 自动建索引，也尽量不依赖 SQLite/Postgres 专有语法。
8. **admin UI 要成为完整管理面。** profiles、repository policies、runner requests、事件日志、重试、停止、pause/resume、配置预览和指标入口都应该能在后台管理。
9. **E2B API 调用失败必须有明确重试策略。** create、connect、command start、kill/stop 都要区分可重试和不可重试错误，使用指数退避、jitter、最大尝试次数，并把 retry state 落到 DB。

## 2. 当前实现定位

当前服务更像一个轻量 E2B runner adapter：

1. `cmd/runnerd/main.go` 负责配置加载、GitHub/E2B client 初始化、启动恢复和 HTTP 服务启动。
2. `internal/server/server.go` 负责 webhook、管理 API、admin UI、runner 生命周期和恢复清理。
3. `internal/state/store.go` 当前以本地目录保存 `request.json`、`state.json`、`control.log`、`stdout.log`、`stderr.log`。
4. `internal/sandboxrunner/runner.go` 通过七牛 sandbox SDK 创建 sandbox、写启动脚本、启动/停止 runner。

这个形态适合验证主链路，但对公网和组织级使用会遇到这些限制：

1. 本地文件不适合多实例，也不方便复杂查询和策略判断。
2. 进程重启后只能保守清理 active 状态，不能精确恢复调度。
3. 没有 spec/pool，无法表达“某些仓库只能使用某些 runner”。
4. 没有统一的状态事件表，排查状态迁移只能看当前 state 和日志。
5. metrics / diagnostics 还不完整，难以判断队列慢、sandbox 慢、GitHub token 慢还是 job 本身慢。

## 3. 三个项目的定位差异

| 维度 | e2b-github-runner 当前 | Fireactions | Actions Runner Controller |
| --- | --- | --- | --- |
| 部署形态 | 单 Go 服务 | 独立 runner 编排服务 | Kubernetes Operator |
| 计算载体 | E2B sandbox | Firecracker microVM | Kubernetes Pod |
| 调度模型 | webhook 后按需创建 | pool desired/current/pending | scale set + listener + reconciler |
| 状态存储 | 本地目录 | pool 内存状态 + 本机运行态 | K8s API / CRD status |
| GitHub 鉴权 | 目标改为 GitHub App only | GitHub App + installation client + JIT config | GitHub App / scale set auth |
| repo 可见性 | 当前较弱 | pool 级组织/runner 配置 | runner group visibility / repository visibility |
| 指标 | 健康检查 + 日志 | pool current/desired/pending、scale 指标 | workflow job queue/run/conclusion 指标 |
| 复杂度 | 低 | 中 | 高 |

目标不是把当前项目改成 Fireactions 或 ARC，而是吸收其中适合 E2B 的部分：

- 从 Fireactions 学 pool/profile、desired/current/pending、pause/resume、GitHub App/JIT、metrics。
- 从 ARC 学 repo 可见性、runner group 语义、reconcile 思维、workflow job 维度指标。
- 继续保留 E2B sandbox 作为执行面，不引入 Firecracker/K8s 复杂度。

## 4. 可以从 Fireactions 搬过来的设计

### 4.1 Pool / profile 模型

Fireactions 的 `Pool` 里有几个值得借鉴的点：

1. `replicas` 表示 desired runner 数。
2. `GetCurrentSize()` 表示当前实际 runner 数。
3. `pendingCreates` / `pendingDeletes` 表示进行中的创建/删除。
4. `Pause()` / `Resume()` 可以暂停一个 pool 的扩缩容。
5. `TriggerScale()` 用非阻塞 channel 合并多次 scale 请求。

映射到当前项目，可以先做成 `runner_specs`：

```yaml
runner_specs:
  - name: default
    labels: [self-hosted, e2b]
    template_id: base-template
    max_concurrency: 100
    min_idle: 0
    runner_group: default
    allowed_repositories:
      - owner/repo-a
      - owner/repo-b
  - name: large
    labels: [self-hosted, e2b, large]
    template_id: large-template
    max_concurrency: 10
    min_idle: 1
    runner_group: large
    allowed_repositories:
      - owner/heavy-repo
```

首版不一定要实现完整 desired replicas，但 profile 至少要承载：

- labels 匹配
- sandbox template
- 并发上限
- runner group
- repo allowlist / denylist
- 是否允许手动创建

### 4.2 GitHub App + JIT config

Fireactions 使用 GitHub App，并缓存 installation client。它还使用 JIT config，把 runner 配置下发给 VM。

目标架构建议 **只支持 GitHub App，不支持 PAT**。原因是这个项目下一阶段要做组织级 repo policy、runner spec、runner group 和公网部署，GitHub App 的权限模型更贴近这些需求；支持 PAT 会让鉴权、权限审计、token 轮换和测试矩阵都多一条分支。

GitHub App 的收益：

- 多 repo/org 更自然。
- 权限边界清楚。
- installation token 自动轮换。
- 审计和吊销更容易。
- 可以按 installation/repository 做权限判断，更适合 repository policy。

JIT config 可以作为第二步评估。首版 GitHub App 仍可先用 repository/organization runner registration token，等 profile/runner group 稳定后再决定是否切到 JIT。

### 4.3 Metrics / diagnostics

Fireactions 的指标重点是 pool 级别：

- server up
- pools total
- pool current runners
- pool desired runners
- pool pending operations
- pool status
- scale requests total
- scale operations total
- scale duration

这些可以映射到 E2B，并通过 `expvar` 注册。运行时暴露统一使用 `github.com/jimmicro/pprof`，它会在本地随机端口启动 pprof HTTP 服务，提供 `/debug/vars`、`/debug/pprof/*`、堆内存管理接口，并在 binary 所在目录写入 `.pprof` 地址文件和一键 dump 脚本。

- `e2b_runner_specs_total`
- `e2b_runner_profile_current{profile,owner,repo}`
- `e2b_runner_profile_desired{profile}`
- `e2b_runner_profile_pending{profile,direction}`
- `e2b_runner_profile_status{profile}`
- `e2b_runner_create_duration_seconds{profile}`
- `e2b_runner_stop_duration_seconds{profile}`
- `e2b_runner_operations_total{profile,direction,result}`

注意：`github.com/jimmicro/pprof` 是本地诊断入口，不是公网管理 API。它默认监听 `127.0.0.1`，admin UI 只展示发现到的 pprof 地址、dump 脚本路径和 `/debug/vars` 摘要，不应该把 pprof 直接暴露到公网。

## 5. 可以从 ARC 搬过来的设计

### 5.1 Repo 可见性和 runner group

ARC 的 simulator 里有 `VisibleRunnerGroups`，核心思路是：GitHub Actions 会在“对 repository 可见的 runner group”里调度 job，控制器也应该按同样的可见性选择 scale target。

当前项目需要类似概念。否则组织级 runner 一旦做大，会出现这些问题：

1. 仓库 A 可以误用仓库 B 的 runner spec。
2. 大规格 runner 被普通仓库无意占用。
3. 管理员无法表达“这个 repo 只能用 default，那个 repo 可以用 large”。

建议模型：

```yaml
runner_policies:
  - repository: owner/repo-a
    allowed_specs: [default]
  - repository: owner/repo-b
    allowed_specs: [default, large]
  - repository: owner/security-sensitive
    allowed_specs: [secure]
```

匹配顺序：

1. 从 workflow job payload 取 `repository.full_name` 和 job labels。
2. 找到 repository policy。
3. 从 policy 允许的 profiles 里筛选 labels 覆盖 job labels 的 profile。
4. 如果多个 profile 匹配，按 priority 或更具体 label 数排序。
5. 无匹配时不创建 runner，记录 `failed` 或 `rejected` 事件。

状态机里不建议新增 `rejected` 终态，避免终态过多。可以把状态设为 `failed`，但 `failure_stage=admission`、`failure_reason=profile_not_allowed`。

### 5.2 Reconcile 思维

ARC 的核心不是某个脚本，而是持续收敛。当前项目也应该引入类似 loop，但目标对象是 DB 里的 runner request，而不是 K8s CRD。

reconciler/sweeper 要处理：

- `queued` 超时未被 worker pick up。
- `creating` 超时，可能 sandbox 已创建但状态没写回。
- `running` 超过 sandbox TTL 或 job 已 completed 但 webhook 丢失。
- `stopping` 多次 stop 失败。
- 服务重启后恢复未完成状态。

### 5.3 Workflow job 指标

ARC 的 `actionsmetrics` 关注 job queue duration、run duration、conclusion、queued/started/completed 计数。当前项目也应该有 job 维度指标，先通过 `expvar` 暴露：

- `github_workflow_jobs_queued_total{owner,repo,workflow,job_name,profile}`
- `github_workflow_jobs_started_total{owner,repo,workflow,job_name,profile}`
- `github_workflow_jobs_completed_total{owner,repo,workflow,job_name,profile,conclusion}`
- `github_workflow_job_queue_duration_seconds{owner,repo,profile}`
- `github_workflow_job_run_duration_seconds{owner,repo,profile,conclusion}`
- `github_workflow_job_failures_total{owner,repo,profile,stage}`

## 6. 目标状态机

固定状态：

```text
queued -> creating -> running -> stopping -> completed
                                      \-> failed
queued -> failed
creating -> failed
running -> failed
stopping -> failed
```

状态含义：

| 状态 | 含义 | 允许的下一步 |
| --- | --- | --- |
| `queued` | 已接收 webhook/API 请求，已通过 admission，等待 worker 处理 | `creating`, `failed` |
| `creating` | 正在申请 GitHub token、创建 sandbox、启动 runner | `running`, `stopping`, `failed` |
| `running` | sandbox 已创建，runner 已启动，等待 job 执行或 runner exit | `stopping`, `failed` |
| `stopping` | 正在停止 runner/sandbox，或等待清理重试 | `completed`, `failed` |
| `completed` | job 正常完成，runner/sandbox 清理完成 | 终态 |
| `failed` | admission、创建、运行、清理任一阶段失败且超过策略 | 终态 |

关键规则：

1. 所有状态迁移必须带 version 或 updated_at 条件，避免并发覆盖。
2. webhook queued 重复到达时只返回已有 request。
3. completed webhook、runner exit、手动 delete 都只推动状态向 `stopping/completed/failed` 前进。
4. stop sandbox 404 视为幂等成功，但要记录事件。
5. worker 只领取 `queued` 或 sweeper 判定可重试的记录。
6. `completed` 和 `failed` 是终态，默认不再修改主状态，只追加事件。

## 7. DB 设计建议

### 7.1 存储后端

配置不再拆成环境变量。服务启动时只读取一个配置文件，例如 `runnerd.yaml`，DB 后端、连接串、migration、retention 都放在 `state` 段里：

```yaml
state:
  backend: sqlite # sqlite | postgres
  database_url: ./var/runnerd.db
  migrate_on_start: true
  retention_days: 30
```

Postgres 部署时只替换配置文件：

```yaml
state:
  backend: postgres
  database_url: postgres://user:pass@host:5432/e2b_runner?sslmode=require
  migrate_on_start: true
  retention_days: 90
```

SQLite：

- 适合本地、单机、公网小规模部署。
- 使用 WAL。
- 使用事务和 `UPDATE ... WHERE status = ?` 做状态 CAS。

Postgres：

- 适合多实例。
- 可以更自然地做 retention、查询、统计和审计。

### 7.2 GORM 使用边界

DB 访问层建议使用 GORM，但要把它限制在“可维护的 ORM”范围内：

1. **GORM 负责：** model 映射、CRUD、事务、普通查询、preload/association 避免手写重复 SQL。
2. **手写 migration 负责：** 建表、索引、唯一约束、字段变更、数据修复。
3. **不要在 GORM tag 里建索引。** model tag 只写 `column`、`primaryKey`、`size`、`not null` 这类字段语义；索引统一放在 migration SQL。
4. **尽量不用数据库专有能力。** 不使用 JSONB、partial index、advisory lock、`FOR UPDATE SKIP LOCKED`、`RETURNING` 这类单库特性作为核心路径。
5. **JSON 字段按 TEXT 存。** labels、payload、profile 配置可以由应用层 marshal/unmarshal，避免 SQLite/Postgres JSON 类型差异。

worker 领取任务建议用可移植的 CAS/lease 模型：

1. 查询一批 `status = queued` 且 `next_retry_at <= now` 的候选请求。
2. 对候选请求按 `id + status + version` 做条件更新，把状态推进到 `creating`，同时写入 `lease_owner`、`lease_expires_at`、`version = version + 1`。
3. 更新影响 1 行才表示领取成功；影响 0 行说明被其他 worker 抢先处理。
4. sweeper 根据 `lease_expires_at` 回收 stuck 任务。

这比直接依赖 Postgres `SKIP LOCKED` 慢一点，但 SQLite 和 Postgres 可以共用同一套语义，后续确实需要更高吞吐时再加 Postgres 专用优化。

### 7.3 核心表

```sql
CREATE TABLE runner_requests (
  id TEXT PRIMARY KEY,
  source TEXT NOT NULL,
  scope TEXT NOT NULL,
  owner TEXT NOT NULL,
  repo TEXT,
  repository_full_name TEXT,
  workflow_job_id INTEGER,
  workflow_run_id INTEGER,
  job_name TEXT,
  workflow_name TEXT,
  head_branch TEXT,
  labels_json TEXT NOT NULL,
  runner_spec_name TEXT,
  runner_group TEXT,
  runner_name TEXT NOT NULL,
  status TEXT NOT NULL,
  failure_stage TEXT,
  failure_reason TEXT,
  sandbox_id TEXT,
  process_pid INTEGER,
  github_runner_id INTEGER,
  retry_count INTEGER NOT NULL DEFAULT 0,
  next_retry_at TIMESTAMP,
  last_error_code TEXT,
  last_error_message TEXT,
  last_error_retryable BOOLEAN,
  lease_owner TEXT,
  lease_expires_at TIMESTAMP,
  queued_at TIMESTAMP NOT NULL,
  creating_at TIMESTAMP,
  running_at TIMESTAMP,
  stopping_at TIMESTAMP,
  completed_at TIMESTAMP,
  failed_at TIMESTAMP,
  updated_at TIMESTAMP NOT NULL,
  version INTEGER NOT NULL DEFAULT 0
);
```

```sql
CREATE TABLE runner_events (
  id INTEGER PRIMARY KEY,
  request_id TEXT NOT NULL,
  event_type TEXT NOT NULL,
  stage TEXT,
  message TEXT,
  payload_json TEXT,
  created_at TIMESTAMP NOT NULL
);
```

```sql
CREATE TABLE runner_specs (
  name TEXT PRIMARY KEY,
  labels_json TEXT NOT NULL,
  template_id TEXT NOT NULL,
  runner_group TEXT,
  max_concurrency INTEGER NOT NULL,
  min_idle INTEGER NOT NULL DEFAULT 0,
  priority INTEGER NOT NULL DEFAULT 0,
  enabled BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMP NOT NULL,
  updated_at TIMESTAMP NOT NULL
);
```

```sql
CREATE TABLE repository_policies (
  id INTEGER PRIMARY KEY,
  repository_full_name TEXT NOT NULL,
  runner_spec_name TEXT NOT NULL,
  enabled BOOLEAN NOT NULL DEFAULT TRUE,
  created_at TIMESTAMP NOT NULL
);
```

```sql
CREATE TABLE audit_events (
  id INTEGER PRIMARY KEY,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  resource_type TEXT NOT NULL,
  resource_id TEXT NOT NULL,
  before_json TEXT,
  after_json TEXT,
  created_at TIMESTAMP NOT NULL
);
```

索引用手写 SQL 建立，不放进 GORM tag：

```sql
CREATE UNIQUE INDEX idx_runner_requests_workflow_job_id
  ON runner_requests(workflow_job_id);

CREATE INDEX idx_runner_requests_status_retry
  ON runner_requests(status, next_retry_at, queued_at);

CREATE INDEX idx_runner_requests_lease
  ON runner_requests(lease_expires_at);

CREATE INDEX idx_runner_requests_repository_status
  ON runner_requests(repository_full_name, status);

CREATE INDEX idx_runner_requests_profile_status
  ON runner_requests(runner_spec_name, status);

CREATE INDEX idx_runner_events_request_created
  ON runner_events(request_id, created_at);

CREATE UNIQUE INDEX idx_repository_policies_repository_profile
  ON repository_policies(repository_full_name, runner_spec_name);

CREATE INDEX idx_audit_events_resource_created
  ON audit_events(resource_type, resource_id, created_at);
```

日志可以先放 `runner_events`，stdout/stderr 如果量大，可以后续拆到对象存储或 append-only log table。不要把本地文件作为必要状态源。

## 8. 目标调度流程

### 8.1 Admission

1. 校验 GitHub HMAC。
2. 解析 `workflow_job.queued`。
3. 提取 repo、labels、workflow/job metadata。
4. 按 repository policy + profile labels 做匹配。
5. 无匹配则写入 `failed`，stage 为 `admission`。
6. 有匹配则插入或返回已有 `runner_requests`，状态为 `queued`。

### 8.2 Worker

1. 从 DB 领取 `queued` 请求。
2. 状态 CAS 到 `creating`。
3. 使用 GitHub App installation token 获取 registration token，或在后续阶段生成 JIT config。
4. 创建 E2B sandbox。
5. 启动 runner。
6. 成功后写 `running`。
7. 任一步失败，按 retry policy 重试；超过上限写 `failed`。

### 8.3 E2B API 重试策略

E2B/sandbox API 失败不能简单写成 `failed`。需要先分类：

| 场景 | 是否重试 | 处理 |
| --- | --- | --- |
| 网络超时、连接重置、DNS/临时网络错误 | 是 | 保持当前阶段，写 retry event，按退避重试 |
| HTTP 429 | 是 | 优先使用 `Retry-After`，否则按退避重试 |
| HTTP 500/502/503/504 | 是 | 按退避重试 |
| create sandbox 返回容量不足/placement failed | 是 | 通常是资源临时不可用，按退避重试，但要有最大等待时间 |
| HTTP 401/403 | 否 | 配置或权限错误，直接 `failed`，stage=`sandbox_auth` |
| HTTP 400 | 否 | 请求参数/template 配置错误，直接 `failed`，stage=`sandbox_config` |
| template 不存在 | 否 | profile 配置错误，直接 `failed`，stage=`sandbox_config` |
| stop/connect sandbox 返回 404 | 否，但视为成功 | sandbox 已不存在，stop 幂等完成，写 `completed` 或继续清理 |
| command start 脚本错误 | 视错误分类 | API 层 5xx/网络可重试；脚本返回明确配置错误不可重试 |

推荐退避参数放进配置文件：

```yaml
sandbox:
  create_timeout: 2m
  stop_timeout: 30s
  runner_timeout: 1h
  retry:
    max_attempts: 5
    initial_backoff: 2s
    max_backoff: 60s
    jitter: 0.2
    placement_max_wait: 10m
```

重试状态写回 `runner_requests`：

1. 每次失败更新 `retry_count`、`next_retry_at`、`last_error_code`、`last_error_message`、`last_error_retryable`。
2. 同时追加 `runner_events`，记录 stage、attempt、error code、retryable、next retry time。
3. 如果 `retry_count < max_attempts` 且错误可重试，状态保持在当前阶段，释放 lease，等待 worker/sweeper 下一轮领取。
4. 如果超过最大次数，写 `failed`，并设置 `failure_stage` 和 `failure_reason`。
5. 对 placement failed 这类容量错误，可以不按固定次数，而按 `placement_max_wait` 判断总等待时间，避免瞬时资源不足导致过早失败。

create 路径必须处理“半成功”：

1. 如果 sandbox 创建成功但写 DB 前进程崩溃，reconciler 要能通过 metadata `request_id` 找回 sandbox，并把 request 推进到 `running` 或 `stopping`。
2. 如果 command start 失败但 sandbox 已创建，进入 `stopping` 并清理 sandbox；清理失败则保持 `stopping` 重试。
3. 如果 retry create 时发现同 request_id 的 sandbox 已存在，不再创建第二个 sandbox，直接复用或先清理旧 sandbox。

stop 路径必须幂等：

1. `stopping` 可以被 completed webhook、runner exit、admin stop、sweeper 同时触发。
2. 只有第一个 CAS 到 `stopping` 的流程执行 stop；后续请求读取当前状态并返回。
3. stop 返回 404 或 “sandbox already gone” 视为成功。
4. stop 5xx/网络错误保持 `stopping`，写 `next_retry_at`，由 sweeper 继续。

### 8.4 Completion / stop

来源可能有三个：

1. GitHub completed webhook。
2. runner process exit callback。
3. 管理 API 手动停止。

统一处理：

1. 如果当前是 `completed/failed`，直接返回当前状态。
2. 如果当前是 `queued/creating/running`，CAS 到 `stopping`。
3. 停止 runner/sandbox。
4. stop 成功或 sandbox already gone，写 `completed`。
5. stop 失败但可重试，保持 `stopping` 并记录事件。
6. 超过重试上限，写 `failed`，stage 为 `cleanup`。

## 9. API 和配置建议

### 9.1 新增 API

- `GET /runner_specs`
- `POST /runner_specs`
- `GET /runner_specs/{name}`
- `PATCH /runner_specs/{name}`
- `POST /runner_specs/{name}/pause`
- `POST /runner_specs/{name}/resume`
- `DELETE /runner_specs/{name}`
- `GET /repositories/{owner}/{repo}/runner_specs`
- `PUT /repositories/{owner}/{repo}/runner_specs`
- `GET /runner_policies`
- `POST /runner_policies`
- `PATCH /runner_policies/{id}`
- `DELETE /runner_policies/{id}`
- `GET /runner_requests`
- `GET /runner_requests/{id}`
- `GET /runner_requests/{id}/events`
- `POST /runner_requests/{id}/retry`
- `POST /runner_requests/{id}/stop`
- `GET /admin/config`
- `POST /admin/config/reload`
- `GET /audit-events`
- `GET /diagnostics/pprof`
- `GET /diagnostics/vars`

### 9.2 Admin 管理面

admin UI 不应该只展示 runner 列表。它应该覆盖控制面日常操作，至少包含这些页面：

1. **Overview**
   - active runners、queued/creating/running/stopping 数量。
   - profile current/desired/pending。
   - 最近失败请求、失败 stage 分布。
   - sandbox create/stop duration、GitHub token duration 的简要趋势。
2. **Runner Requests**
   - 按 status、repo、profile、workflow、时间范围过滤。
   - 查看 request detail、GitHub payload 摘要、sandbox id、runner name、失败原因。
   - 查看 `runner_events` 时间线。
   - 对非终态请求执行 stop；对可重试失败执行 retry。
3. **Profiles**
   - 创建、编辑、启用/禁用 profile。
   - 管理 labels、template id、runner group、max concurrency、min idle、priority。
   - pause/resume profile。
   - 显示 profile 当前被哪些 repository policies 引用。
4. **Runner Policies**
   - 为仓库配置 allowed profiles。
   - 显示某个仓库当前可用的 profiles。
   - 提供 label 匹配测试：输入 repo + labels，展示会命中哪个 profile，以及拒绝原因。
5. **Config**
   - 展示当前加载的配置文件路径和脱敏后的有效配置。
   - 支持 reload 配置，但 secret 只显示来源文件路径，不显示内容。
   - 校验配置冲突，例如 policy 引用了不存在的 profile。
6. **Metrics / Diagnostics**
   - 展示 `github.com/jimmicro/pprof` 发现到的本地 pprof 地址文件和 dump 脚本路径。
   - 展示 `/debug/vars` 中的业务指标摘要。
   - 展示 DB backend、migration version、GitHub App installation 状态、sandbox API health。

admin 写操作必须走同一套鉴权和审计：

- runner 生命周期写入 `runner_events`；admin 配置类写操作写入 `audit_events`。
- profile/policy 修改要记录 actor、before/after、created_at。
- stop/retry/pause/resume 都必须幂等。
- 对公网部署，admin API 仍应支持反向代理层面的 IP allowlist 和 HTTPS。

### 9.3 配置

建议只支持配置文件，不再支持环境变量覆盖。原因是 profiles、repository policies、GitHub App、sandbox template、并发限制、retention、metrics 等都已经是结构化配置；继续保留环境变量会让配置来源变得不可预测，也不利于审计。

配置文件分两类：

1. **启动配置。** server、state、github app、sandbox、diagnostics、secret file path 这类基础设施配置只能来自 `runnerd.yaml`。
2. **运行期配置。** profiles、repository policies 可以在 `runnerd.yaml` 里作为 seed 配置导入，但服务启动后以 DB 为准，admin UI 修改直接写 DB，并记录 `audit_events`。

`POST /admin/config/reload` 只重新加载启动配置和 seed 校验，不直接覆盖 DB 中已被 admin 修改的 profile/policy。需要覆盖时应走明确的 import/preview/apply 流程，避免 reload 配置文件误删运行期变更。

启动入口可以是：

```bash
runnerd --config ./runnerd.yaml
```

完整配置示例：

```yaml
server:
  http_addr: ":25500"
  public_base_url: https://runner.example.com
  admin:
    token_file: ./secrets/admin-token
    session_ttl: 12h
  webhook:
    github_secret_file: ./secrets/github-webhook-secret
    max_body_bytes: 1048576

state:
  backend: postgres
  database_url: postgres://user:pass@host:5432/e2b_runner?sslmode=require
  migrate_on_start: true
  retention_days: 90

github:
  api_base_url: https://api.github.com
  app:
    app_id: 123456
    private_key_file: ./secrets/github-app.pem
  runner_policies:
    - repository: jimyag/*
      runner_specs: [default]

sandbox:
  api_url: http://10.210.10.32:50001
  api_key_file: ./secrets/e2b-api-key
  domain: e2b.example.com
  create_timeout: 2m
  stop_timeout: 30s
  runner_timeout: 1h

diagnostics:
  enabled: true
  pprof_package: github.com/jimmicro/pprof
  expose_admin_summary: true

runner_specs:
  - name: default
    labels: [self-hosted, e2b]
    template_id: base
    max_concurrency: 100
    runner_group: default
    min_idle: 0
    enabled: true
  - name: large
    labels: [self-hosted, e2b, large]
    template_id: large
    max_concurrency: 10
    runner_group: large
    min_idle: 1
    enabled: true

runner_policies:
  - repository: jimyag/template-repository
    allowed_specs: [default]
  - repository: jimyag/heavy-repository
    allowed_specs: [default, large]
```

## 10. 分阶段实现建议

### Phase 1：DB 状态层和状态机

- 增加 `state.Store` 接口。
- 用 GORM 实现 SQLite/Postgres 共用 store，按 driver 注入连接。
- 用手写 SQL migration 建表和索引，不使用 GORM index tag。
- 实现基于 `id/status/version/lease_expires_at` 的 CAS 状态迁移。
- 把状态固定为 `queued/creating/running/stopping/completed/failed`。
- 调整 server 调用，不再依赖 request 目录。

### Phase 2：profile 和 repo policy

- 增加 profile 配置和 DB 表。
- admission 时按 repo policy + labels 选择 profile。
- 支持组织下不同仓库使用不同 profile。
- admin UI 展示 profile、repo policy、匹配结果。
- admin UI 支持 profile/policy 的创建、编辑、启用/禁用、删除和匹配测试。

### Phase 3：worker/reconciler/sweeper

- worker 从 DB 领取任务。
- sweeper 扫描 stuck 状态。
- reconciler 根据 GitHub job/runner 状态做补偿。
- 所有 stop/create 都幂等。

### Phase 4：metrics / diagnostics 和 GitHub App

- 引入 `github.com/jimmicro/pprof` 作为本地 pprof/expvar 诊断入口。
- 用 `expvar` 注册 Fireactions 风格 spec/pool 指标。
- 用 `expvar` 注册 ARC 风格 workflow job 指标。
- 增加 GitHub App 鉴权，并用 installation token 申请 runner registration token。
- admin UI 增加 diagnostics 页面，展示 DB、GitHub App、sandbox API、pprof 地址、dump 脚本、`/debug/vars` 摘要和最近失败事件。

## 11. 一句话判断

如果目标只是单 repo 验证，当前文件状态实现足够；如果目标是公网部署、组织级仓库治理和长期运行，应该把状态源升级到 SQLite/Postgres，并把 Fireactions 的 pool/metrics/GitHub App 思路、ARC 的 runner group visibility/reconcile/job metrics 思路吸收到当前 E2B runner 控制面里。运行时诊断入口统一使用 `github.com/jimmicro/pprof`。

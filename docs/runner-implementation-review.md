# Runnerd Implementation Review

Date: 2026-05-19

Scope:

- Recent commits reviewed: `19663e8`, `1881790`, `8e7f48e`, `eee1752`.
- Local references compared:
  - `bin/actions-runner-controller`
  - `bin/fireactions`
- Main target architecture referenced: database-backed runner state, file-free config, GitHub App auth, profile/policy management, retryable E2B operations, restart recovery, public deployment safety, and admin operations.

## Executive Summary

The recent commits move the project in the right direction: runner state is now DB-backed, profile and repository policy concepts exist, the admin UI can manage basic profile/policy data, GitHub App auth was added, status now uses `queued -> creating -> running -> stopping -> completed/failed`, and `github.com/jimmicro/pprof` is wired in.

However, the implementation is not yet aligned with the current architecture. The largest gaps are:

- Configuration is still environment-variable driven, while the design now says all configuration should come from config files.
- PAT/token mode still exists, while the design decision was GitHub App only.
- E2B API failures are not retried; requests fail immediately.
- The DB state model has no retry, lease, or audit fields, so it is not ready for reliable multi-instance or public deployment.
- The worker loop is still in-memory driven and scans all states, instead of claiming work through the database.
- Repository policy checks can be bypassed by manually selecting a profile.
- Admin management is incomplete: no retry action, audit events, config reload, or full diagnostics workflow.
- `task lint` currently fails.

## Findings

### P0: Config Is Still Env-Based, Not File-Based

The architecture says all settings should be configurable and environment variables should no longer be supported as the primary configuration surface. Current code still loads nearly all runtime configuration from environment variables:

- `internal/config/config.go:62-91` reads `HTTP_ADDR`, `STATE_DIR`, `ADMIN_TOKEN`, `E2B_API_KEY`, `E2B_API_URL`, `E2B_DOMAIN`, GitHub auth, GitHub scope, sandbox timeouts, runner labels, and concurrency from env.
- `internal/config/config.go:96-140` validates missing values as `missing required env`.
- `internal/config/config.go:168-180` only uses `RUNNERD_CONFIG_FILE` for seed profiles and repository policies, not for the full service config.

Impact:

- Public deployment remains hard to reason about because secrets, auth, DB backend, timeouts, and routing are split across env and DB.
- The admin UI cannot represent the effective config accurately.
- It diverges from `fireactions`, which loads a YAML config file with validation in `bin/fireactions/server/config.go:78-112`.

Recommendation:

- Introduce a single `runnerd.yaml` schema for server, auth, database, E2B, GitHub App, profiles, policies, worker, retry, and diagnostics.
- Keep env support only for explicit secret-file indirection if needed, not as the primary config model.
- Change validation errors from env names to config paths.

### P0: GitHub PAT/Token Mode Still Exists

The current decision was to not support PAT migration and use GitHub App auth. Current code still supports token mode:

- `internal/config/config.go:23` keeps `GitHubToken`.
- `internal/config/config.go:109-119` accepts either `GITHUB_TOKEN` or GitHub App fields.
- `internal/config/config.go:151-156` selects `"token"` when `GitHubToken` is present.
- `cmd/runnerd/main.go:37-49` creates either `NewAppClient` or token `NewClient`.
- `internal/github/client.go:96-98` always sets an `Authorization` header using `c.token`.

Impact:

- The implementation no longer matches the chosen security model.
- The GitHub client still mixes token and App transport behavior. `ghinstallation` likely overwrites the auth header, but the code path is confusing and under-tested.
- It misses the cleaner fireactions model: app transport plus cached installation clients and retryable HTTP in `bin/fireactions/helper/github/client.go:14-77`.

Recommendation:

- Remove PAT mode entirely from config, validation, client construction, tests, docs, and UI.
- Make `GitHubApp` the only auth config.
- Add tests that prove registration token calls work through the app transport without a PAT.

### P0: E2B API Retry Is Not Implemented

The architecture calls for retrying E2B API failures with retry state. Current worker fails immediately:

- `internal/server/server.go:858-865` marks failed immediately when GitHub registration token creation fails.
- `internal/server/server.go:873-893` marks failed immediately when sandbox start fails.
- `internal/server/server.go:1089-1103` marks failed immediately on non-404 sandbox stop errors.
- `internal/server/server.go:206-218` converts creating timeout directly to failed.

The DB schema has no retry fields:

- `internal/state/store.go:135-160` has no `retry_count`, `next_retry_at`, retryable error fields, or lease fields.
- `internal/state/store.go:880-940` and `internal/state/store.go:942-1002` migrations have no retry indexes.

Impact:

- Transient placement errors like `Failed to place sandbox`, E2B 429/5xx, network timeouts, and temporary stop failures become terminal failures.
- Admin cannot retry failed requests because there is no retry state or endpoint.
- Restart recovery cannot distinguish retryable from terminal failures.

Recommendation:

- Add retry fields to `runner_requests`: `retry_count`, `next_retry_at`, `last_error_code`, `last_error_message`, `last_error_retryable`, `last_attempt_at`.
- Add manual indexes for `(status, next_retry_at, queued_at)` and `(status, updated_at)`.
- Classify retryable errors: E2B 408/409/425/429/5xx, timeout, temporary network failures; GitHub 5xx/secondary rate limit; non-retryable auth/config errors.
- Add `POST /runner_requests/{id}/retry` and UI action.

### P1: Worker Claiming Is In-Memory, Not DB-Leased

The worker still scans all states and uses in-process memory to prevent duplicates:

- `internal/server/server.go:144-176` starts three unbounded background loops without a stop context.
- `internal/server/server.go:178-191` lists all states and starts a goroutine for every queued state.
- The `Store` interface in `internal/state/store.go:102-120` has no claim/lease method.
- The request record in `internal/state/store.go:135-160` has no `lease_owner` or `lease_expires_at`.

Impact:

- A Postgres deployment with two runnerd instances can start the same queued request twice.
- If the process dies after claiming in memory but before starting a sandbox, the DB does not know who owns the request or when it can be retried.
- Tests can leak background goroutines because server loops have no lifecycle control.

Comparison:

- ARC uses controller-runtime reconciliation and leader/concurrency controls (`bin/actions-runner-controller/main.go:133-155`).
- fireactions has explicit pool lifecycle state, stop channels, done channels, pending create/delete counters, and cleanup wait groups (`bin/fireactions/server/pool.go:35-57`, `129-180`, `182-220`).

Recommendation:

- Add DB-backed claim APIs:
  - `ClaimNextQueued(workerID, now, leaseTTL)`.
  - `ExtendLease(requestID, workerID)`.
  - `ReleaseLease(requestID, workerID)`.
- Drive workers from `next_retry_at <= now` and `lease_expires_at < now`.
- Add a server context and graceful shutdown for loops.

### P1: Repository Policy Can Be Bypassed By Manual Profile Selection

Webhook admission uses profile matching, but manual runner creation can directly request a profile:

- The admin policy endpoints exist in `internal/server/server.go:574-675`.
- Matching is exposed at `internal/server/server.go:678-692`.
- Manual creation with explicit `runner_spec_name` loads that profile directly later at `internal/server/server.go:868-872`; it does not enforce that the requested repository is allowed to use that profile.

Impact:

- The policy model cannot be treated as an authorization boundary.
- A caller with admin API access can create a runner for a repo/profile combination that repository policies would reject.

Recommendation:

- Run repository policy validation for all requests, including manual API calls.
- If `runner_spec_name` is supplied, check it is one of the profiles allowed by the repository policy.
- Store both requested labels and selected profile labels separately so admission decisions are auditable.

### P1: Recovery Marks Interrupted Active Jobs As Completed

Restart recovery stops any active sandbox and then marks the request completed:

- `cmd/runnerd/main.go:55-62` calls recovery before serving.
- `internal/server/server.go:1016-1049` stops the sandbox and writes `StatusCompleted`.

Impact:

- A request that was `creating`, `running`, or `stopping` before process restart can be marked completed even if the GitHub job did not complete.
- This hides interrupted jobs and makes admin history misleading.

Recommendation:

- Use `stopping` while cleanup is in progress.
- Mark `failed` for interrupted `creating/running` states unless GitHub confirms the job completed successfully.
- Reconcile by sandbox metadata `request_id` and GitHub workflow job state where possible.

### P1: DB Schema Is Missing Audit Events And Operational Fields

The current DB schema captures runner state, events, profiles, and repository policies, but not the full control-plane model:

- `internal/state/store.go:880-940` SQLite migration has no `audit_events`.
- `internal/state/store.go:942-1002` Postgres migration has no `audit_events`.
- There are no actor/source fields on profile/policy changes.
- Admin mutations in `internal/server/server.go:522-675` do not write audit events.

Impact:

- Public admin deployment cannot answer who changed a profile, disabled a policy, retried a request, or stopped a sandbox.
- It weakens incident/debug trails.

Recommendation:

- Add `audit_events` with `id`, `actor`, `action`, `resource_type`, `resource_id`, `payload_json`, `created_at`.
- Record profile/policy create/patch/delete, runner retry/stop, config reload, and recovery cleanup.
- Expose `GET /audit-events` in admin.

### P2: Metrics And Diagnostics Are Partial

The project intentionally uses `github.com/jimmicro/pprof` and `expvar`, not Prometheus. That difference is acceptable because it follows the current user requirement. The issue is coverage and freshness:

- `internal/metrics/metrics.go:11-22` exposes basic expvar maps only.
- `internal/metrics/metrics.go:55-63` records duration totals without counts or buckets.
- `internal/metrics/metrics.go:65-75` tracks workflow counts, but not queue duration, run duration, failure stage, retry count, or E2B error class.
- Metrics are refreshed only when selected endpoints or state transitions call `refreshMetrics`.
- `internal/server/server.go:695-710` diagnostics hardcode DB assumptions in the response path and do not fully reflect a configurable DB backend.

Comparison:

- ARC tracks queue duration, run duration, conclusions, started/completed/queued counts, and workflow failures in `bin/actions-runner-controller/pkg/actionsmetrics/metrics.go:76-129`.
- fireactions tracks current/desired/pending pool state directly inside pool reconciliation (`bin/fireactions/server/pool.go:117-124`, `155-165`).

Recommendation:

- Keep jimmicro/pprof, but add expvar counters/gauges for:
  - queued/running/completed/failed by profile and repository.
  - create/stop attempts, successes, failures, retries.
  - queue duration and run duration as count/sum/min/max.
  - last reconcile time and worker lease counts.
- Refresh gauges periodically from the reconciler, not only through API reads.

### P2: Admin UI Does Not Cover The Full Control Plane

The admin UI can manage basic profiles/policies and view diagnostics, but important operations are missing:

- No retry failed request action.
- No audit event viewer.
- No config preview/reload/import workflow.
- No DB backend/status view.
- No worker/retry queue view.
- No repository policy enforcement test that includes explicit profile selection.

Recommendation:

- Add admin screens/actions after the backend APIs exist:
  - retry request.
  - stop request idempotently.
  - audit timeline.
  - config effective view.
  - profile policy test with repository, labels, and requested profile.

### P2: Lint Currently Fails

`task lint` fails:

```text
internal/state/store.go:527:31: should convert record (type repositoryPolicyRecord) to RepositoryPolicy instead of using struct literal (S1016)
internal/state/store.go:566:10: should convert saved (type repositoryPolicyRecord) to RepositoryPolicy instead of using struct literal (S1016)
internal/state/store.go:576:9: should convert saved (type repositoryPolicyRecord) to RepositoryPolicy instead of using struct literal (S1016)
```

Impact:

- Local checks are not green.
- This should be fixed before relying on the branch.

## Comparison With fireactions

| Area | fireactions | Current runnerd | Gap |
| --- | --- | --- | --- |
| Config | YAML file with validation | Env-first, config file only seeds profiles/policies | Need full config file model |
| GitHub auth | GitHub App transport | App plus PAT/token fallback | Remove PAT/token |
| HTTP retries | `hashicorp/go-retryablehttp` | No retryable GitHub/E2B client | Add retry policy |
| Installation handling | Cached installation clients | Single installation id/client | OK for single install, weak for org/multi-repo |
| Pool model | Desired/current/pending, active/pause, graceful stop | Queue scan plus global slots | Need DB leases and profile concurrency |
| Metrics | Prometheus pool metrics | expvar partial metrics | Keep expvar, expand coverage |

## Comparison With actions-runner-controller

| Area | ARC | Current runnerd | Gap |
| --- | --- | --- | --- |
| Reconciliation | Controller reconciliation with concurrency controls | Polling loops and in-memory pending map | Need DB claim/lease reconcile |
| Metrics | Queue/run durations, conclusions, failures | Basic counts and duration totals | Need queue/run/failure metrics |
| pprof | Configurable address flag | jimmicro pprof auto artifact discovery | Acceptable, but expose in config/admin |
| Runner groups | Models GitHub runner group visibility | Local `runner_group` string only | Need decide whether to enforce via GitHub API |
| Multi-instance | Kubernetes leader/concurrency model | Not safe on shared DB | Add lease or single-instance guarantee |

## Missing Design Points

- Full config file schema and validation.
- DB backend selection from config.
- Retry state and retry worker.
- DB-backed worker lease.
- Audit event table and admin audit viewer.
- Config reload/preview/apply.
- Manual request admission through repository policies.
- Per-profile concurrency enforcement. Current slot limit is global.
- GitHub App installation discovery or per-repo installation mapping.
- Runner group visibility validation if org-level runner groups are expected.
- Idempotent retry/stop semantics across restarts and multiple instances.

## Suggested Fix Order

1. Make local checks green.
2. Replace env config with `runnerd.yaml` and wire DB backend selection.
3. Remove PAT/token mode and simplify GitHub client around GitHub App only.
4. Add retry/lease schema and migration SQL.
5. Replace in-memory worker claiming with DB claims.
6. Add E2B/GitHub retry classification and retry worker.
7. Enforce repository policies for manual and webhook requests.
8. Add audit events and admin views/actions.
9. Expand diagnostics and expvar metrics.
10. Improve restart recovery with sandbox/GitHub reconciliation.

## Verification

Executed locally:

```bash
task lint
```

Result: failed with staticcheck `S1016` in `internal/state/store.go`.

The UI build step inside `task lint` completed successfully before staticcheck failed.

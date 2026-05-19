# E2B GitHub Runner

Small Go service that starts ephemeral GitHub Actions self-hosted runners inside E2B sandbox instances.

## Configuration

Required environment variables:

- `E2B_API_KEY`
- `E2B_API_URL`
- `E2B_DOMAIN`
- `ADMIN_TOKEN`
- `GITHUB_TOKEN` (PAT mode) **or** `GITHUB_APP_ID`, `GITHUB_APP_INSTALLATION_ID`, `GITHUB_APP_PRIVATE_KEY_FILE` (GitHub App mode)
- `GITHUB_WEBHOOK_SECRET`
- `SANDBOX_TEMPLATE_ID`

Runner scope:

- Repository runner: set `RUNNER_SCOPE=repo`, `GITHUB_OWNER`, and `GITHUB_REPO`.
- Organization runner: set `RUNNER_SCOPE=org` and `GITHUB_ORG`.

Optional environment variables:

- `HTTP_ADDR` defaults to `:25500`
- `STATE_DIR` defaults to `./var/runners` and stores the SQLite state database (`runnerd.db`)
- `RUNNER_LABELS` defaults to `self-hosted,e2b`
- `RUNNER_SCOPE` defaults to `repo`
- `SANDBOX_TIMEOUT_SECONDS` defaults to `3600`
- `SANDBOX_API_TIMEOUT_SECONDS` defaults to `60`
- `SANDBOX_CREATE_TIMEOUT_SECONDS` defaults to `120`
- `SANDBOX_STOP_TIMEOUT_SECONDS` defaults to `30`
- `RECOVERY_TIMEOUT_SECONDS` defaults to `120`
- `HTTP_READ_TIMEOUT_SECONDS` defaults to `15`
- `HTTP_WRITE_TIMEOUT_SECONDS` defaults to `60`
- `HTTP_IDLE_TIMEOUT_SECONDS` defaults to `120`
- `MAX_CONCURRENT_RUNNERS` defaults to `100`
- `GITHUB_API_BASE_URL`
- `RUNNERD_CONFIG_FILE` optionally seeds runner profiles and repository policies from YAML

`/webhooks/github` uses GitHub HMAC signature verification. The manual management API under `/runners` requires `Authorization: Bearer $ADMIN_TOKEN`.

Runner state is persisted in a DB-backed store under `STATE_DIR` instead of per-request JSON directories. Control/stdout/stderr logs are kept as runner events and remain available from the admin API and UI.

## Run

```bash
go run ./cmd/runnerd
```

Open the embedded admin console at `http://127.0.0.1:25500/admin/`. The UI is built from `ui/` with the same React, Vite, Tailwind CSS, shadcn-style components, and theme tokens used by `kubevirt-console`. It stores `ADMIN_TOKEN` in browser local storage and sends it as `Authorization: Bearer $ADMIN_TOKEN` for management API calls.

The admin console manages runner requests, profiles, repository policies, and profile-match tests. When `RUNNERD_CONFIG_FILE` is provided, `profiles:` and `repository_policies:` are loaded as seed data at startup and then kept in the DB-backed control plane.

The binary also imports `github.com/jimmicro/pprof`, so a local-only pprof/expvar service is started automatically and discovered through generated `.pprof` address files and dump scripts. The admin console exposes a diagnostics page that summarizes the discovered pprof endpoint, `/debug/vars`, DB state, GitHub auth mode, sandbox API configuration, and recent failures.

![Admin console](docs/images/admin-console.png)

## Build

```bash
task build
task docker-build
task template-build-prod
```

Useful validation commands:

```bash
task lint
task test
task docker-check
task release-check
```

Use `runs-on: [self-hosted, e2b]` in the target workflow. Configure a GitHub webhook for `workflow_job` events pointing at `POST /webhooks/github`.

# E2B GitHub Runner

Small Go service that starts ephemeral GitHub Actions self-hosted runners inside E2B sandbox instances.

## Configuration

Runtime configuration is file-first. `runnerd` reads `./runnerd.yaml` by default, or the path passed with `--config`.

Start from the example:

```bash
cp runnerd.yaml.example runnerd.yaml
```

The config file covers:

- server listen address and timeouts
- database backend and DSN/path
- admin auth token
- E2B API settings and template
- GitHub webhook settings plus **GitHub App only** authentication
- worker lease / retry / concurrency settings
- runner spec and runner-policy seed data

`/webhooks/github` uses GitHub HMAC signature verification. The manual management API under `/runner_requests` requires `Authorization: Bearer $ADMIN_TOKEN`.

Runner state is persisted in a DB-backed store instead of per-request JSON directories. Control/stdout/stderr logs are kept as runner events and remain available from the admin API and UI.

## Run

```bash
go run ./cmd/runnerd --config ./runnerd.yaml
```

Open the embedded admin console at `http://127.0.0.1:25500/admin/`. The UI is built from `ui/` with the same React, Vite, Tailwind CSS, shadcn-style components, and theme tokens used by `kubevirt-console`. It stores `ADMIN_TOKEN` in browser local storage and sends it as `Authorization: Bearer $ADMIN_TOKEN` for management API calls.

The admin console manages runner requests, runner specs, runner groups, runner policies, retry actions, audit history, runner-spec match tests, and diagnostics. `runner_specs:`, `runner_groups:`, and `runner_policies:` from `runnerd.yaml` are used to initialize missing DB entries and do not overwrite later admin-managed changes on restart.

Runner specs with `default_available: true` are globally available to installed repositories. Use runner policies only when a repository needs access to an additional/special spec.

The binary also imports `github.com/jimmicro/pprof`, so a local-only pprof/expvar service is started automatically and discovered through generated `.pprof` address files and dump scripts. The admin console exposes a diagnostics page that summarizes the discovered pprof endpoint, `/debug/vars`, DB state, GitHub auth mode, sandbox API configuration, retry/lease metrics, and recent failures.

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

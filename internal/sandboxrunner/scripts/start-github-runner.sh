#!/usr/bin/env bash
set -euo pipefail

export RUNNER_ALLOW_RUNASROOT=1
export HOME="${RUNNER_HOME:-/tmp/runner-home}"
export XDG_CONFIG_HOME="${HOME}/.config"
workdir="${RUNNER_WORKDIR:-/tmp/actions-runner}"
mkdir -p "$workdir" "$HOME" "$XDG_CONFIG_HOME/git"
cd "$workdir"

if [ ! -x /opt/actions-runner/config.sh ]; then
  echo "missing preinstalled GitHub Actions runner at /opt/actions-runner/config.sh" >&2
  echo "build the sandbox template from templates/github-runner-ubuntu-24.04 before starting runners" >&2
  exit 1
fi

if [ ! -x ./config.sh ]; then
  echo "copying preinstalled GitHub Actions runner"
  cp -a /opt/actions-runner/. "$workdir"/
fi

if [ -x /usr/local/bin/ensure-docker ]; then
  echo "checking Docker daemon"
  /usr/local/bin/ensure-docker || true
fi

runner_url="$(printf '%%s' "%[1]s" | base64 -d)"
registration_token="$(printf '%%s' "%[2]s" | base64 -d)"
runner_name="$(printf '%%s' "%[3]s" | base64 -d)"
runner_labels="$(printf '%%s' "%[4]s" | base64 -d)"
runner_group="$(printf '%%s' "%[5]s" | base64 -d)"

mkdir -p /tmp/runnerd-hooks
cat >/tmp/runnerd-hooks/job-started.sh <<'HOOK'
#!/usr/bin/env bash
echo "RUNNERD_JOB_STARTED"
HOOK
cat >/tmp/runnerd-hooks/job-completed.sh <<'HOOK'
#!/usr/bin/env bash
echo "RUNNERD_JOB_COMPLETED"
HOOK
chmod +x /tmp/runnerd-hooks/job-started.sh /tmp/runnerd-hooks/job-completed.sh
export ACTIONS_RUNNER_HOOK_JOB_STARTED=/tmp/runnerd-hooks/job-started.sh
export ACTIONS_RUNNER_HOOK_JOB_COMPLETED=/tmp/runnerd-hooks/job-completed.sh

config_args=(--url "$runner_url" --token "$registration_token" --name "$runner_name" --labels "$runner_labels" --ephemeral --unattended --replace --disableupdate)
if [ -n "$runner_group" ]; then
  config_args+=(--runnergroup "$runner_group")
fi

echo "configuring GitHub Actions runner ${runner_name}"
retries_left=10
while [ "$retries_left" -gt 0 ]; do
  if ./config.sh "${config_args[@]}"; then
    break
  fi
  retries_left=$((retries_left - 1))
  if [ "$retries_left" -eq 0 ]; then
    echo "GitHub Actions runner configuration failed" >&2
    exit 2
  fi
  echo "GitHub Actions runner configuration failed, retrying"
  sleep 1
done
cleanup() {
  ./config.sh remove --token "$registration_token" || true
}
trap cleanup EXIT
echo "starting GitHub Actions runner"
./run.sh

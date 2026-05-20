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

echo "configuring GitHub Actions runner %[3]s"
./config.sh --url %[1]q --token %[2]q --name %[3]q --labels %[4]q --ephemeral --unattended --replace --disableupdate
cleanup() {
  ./config.sh remove --token %[2]q || true
}
trap cleanup EXIT
echo "starting GitHub Actions runner"
exec ./run.sh

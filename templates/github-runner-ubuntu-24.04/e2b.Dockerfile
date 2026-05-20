ARG BASE_PLATFORM=linux/amd64
FROM --platform=$BASE_PLATFORM ubuntu:24.04

ARG DEBIAN_FRONTEND=noninteractive
ARG RUNNER_VERSION=2.334.0
ARG GO_VERSION=1.26.3
ARG NODE_MAJOR=22
ARG OPENTOFU_VERSION=1.11.5
ARG TERRAFORM_VERSION=1.14.6
ARG TASK_VERSION=3.43.3
ARG GOFUMPT_VERSION=0.8.0
ARG GOIMPORTS_VERSION=0.40.0
ARG STATICCHECK_VERSION=0.7.0

ENV LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    RUNNER_TOOL_CACHE=/opt/hostedtoolcache \
    AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache \
    GOPATH=/opt/go \
    GOBIN=/usr/local/bin \
    OPENTOFU_CACHE_DIR=/opt/hostedtoolcache/opentofu \
    TERRAFORM_CACHE_DIR=/opt/hostedtoolcache/terraform \
    PATH=/usr/local/go/bin:/usr/local/bin:/opt/go/bin:$PATH

RUN <<'EOF'
set -eux

download() {
  url="$1"
  output="$2"
  curl --http1.1 -fL --show-error --connect-timeout 15 --max-time 300 \
    --retry 5 --retry-all-errors --retry-delay 2 \
    "$url" -o "$output"
}

apt-get update
apt-get install -y --no-install-recommends \
  apt-transport-https \
  ca-certificates \
  curl \
  wget \
  git \
  git-lfs \
  gawk \
  jq \
  sudo \
  openssh-client \
  tar \
  gzip \
  unzip \
  xz-utils \
  zstd \
  rsync \
  coreutils \
  findutils \
  file \
  build-essential \
  pkg-config \
  make \
  cmake \
  autoconf \
  automake \
  libtool \
  lsb-release \
  software-properties-common \
  gnupg \
  iptables \
  iproute2 \
  docker.io \
  docker-buildx \
  docker-compose-v2 \
  python3 \
  python3-pip \
  python3-venv \
  python3-dev

install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
  | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg
echo "deb [signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${NODE_MAJOR}.x nodistro main" \
  > /etc/apt/sources.list.d/nodesource.list
curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
  | dd of=/usr/share/keyrings/githubcli-archive-keyring.gpg
chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
  > /etc/apt/sources.list.d/github-cli.list
apt-get update
apt-get install -y --no-install-recommends nodejs gh
git lfs install --system

download \
  "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
  /tmp/go.tar.gz
rm -rf /usr/local/go
tar -C /usr/local -xzf /tmp/go.tar.gz
rm /tmp/go.tar.gz
mkdir -p "${GOPATH}/pkg/mod" "${GOPATH}/bin"
go version
go install "github.com/go-task/task/v3/cmd/task@v${TASK_VERSION}"
go install "mvdan.cc/gofumpt@v${GOFUMPT_VERSION}"
go install "golang.org/x/tools/cmd/goimports@v${GOIMPORTS_VERSION}"
go install "honnef.co/go/tools/cmd/staticcheck@v${STATICCHECK_VERSION}"
chmod -R a+rX "${GOPATH}"

mkdir -p "${OPENTOFU_CACHE_DIR}/${OPENTOFU_VERSION}" "${TERRAFORM_CACHE_DIR}/${TERRAFORM_VERSION}"
download \
  "https://github.com/opentofu/opentofu/releases/download/v${OPENTOFU_VERSION}/tofu_${OPENTOFU_VERSION}_linux_amd64.zip" \
  /tmp/tofu.zip
unzip -qo /tmp/tofu.zip -d "${OPENTOFU_CACHE_DIR}/${OPENTOFU_VERSION}"
install -m 0755 "${OPENTOFU_CACHE_DIR}/${OPENTOFU_VERSION}/tofu" /usr/local/bin/tofu
download \
  "https://releases.hashicorp.com/terraform/${TERRAFORM_VERSION}/terraform_${TERRAFORM_VERSION}_linux_amd64.zip" \
  /tmp/terraform.zip
unzip -qo /tmp/terraform.zip -d "${TERRAFORM_CACHE_DIR}/${TERRAFORM_VERSION}"
install -m 0755 "${TERRAFORM_CACHE_DIR}/${TERRAFORM_VERSION}/terraform" /usr/local/bin/terraform
rm -f /tmp/tofu.zip /tmp/terraform.zip

cat >/usr/local/bin/ensure-docker <<'SCRIPT'
#!/usr/bin/env bash
set -euo pipefail

if ! command -v docker >/dev/null 2>&1 || ! command -v dockerd >/dev/null 2>&1; then
  exit 0
fi

if docker info >/dev/null 2>&1; then
  exit 0
fi

mkdir -p /var/lib/docker /var/run
nohup dockerd --host=unix:///var/run/docker.sock >/tmp/dockerd.log 2>&1 &

for _ in $(seq 1 30); do
  if docker info >/dev/null 2>&1; then
    exit 0
  fi
  sleep 1
done

echo "dockerd did not become ready; Docker jobs may fail" >&2
tail -n 80 /tmp/dockerd.log >&2 || true
exit 0
SCRIPT
chmod 0755 /usr/local/bin/ensure-docker

if ! id -u user >/dev/null 2>&1; then
  useradd -m -s /bin/bash user
fi
usermod -aG sudo user
usermod -aG docker user
echo "user ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/90-user
chmod 0440 /etc/sudoers.d/90-user
mkdir -p /home/user/.config/git /tmp/runner-home/.config/git /opt/hostedtoolcache /opt/actions-runner /var/lib/docker
chown -R user:user /home/user /tmp/runner-home /opt/hostedtoolcache /opt/actions-runner

runner_arch="x64"
download \
  "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${runner_arch}-${RUNNER_VERSION}.tar.gz" \
  /tmp/actions-runner.tar.gz
tar xzf /tmp/actions-runner.tar.gz -C /opt/actions-runner
rm /tmp/actions-runner.tar.gz
/opt/actions-runner/bin/installdependencies.sh
test -x /opt/actions-runner/config.sh
test -x /opt/actions-runner/run.sh
chown -R user:user /opt/actions-runner

bash -lc 'command -v go task gofumpt goimports staticcheck node npm gh jq docker tofu terraform'
go version
task --version
gofumpt --version
staticcheck -version
node --version
npm --version
gh --version
docker --version
tofu version
terraform version

apt-get clean
rm -rf /var/lib/apt/lists/* /tmp/*
EOF

WORKDIR /tmp

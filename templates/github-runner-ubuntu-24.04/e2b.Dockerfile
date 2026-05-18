ARG BASE_PLATFORM=linux/amd64
FROM --platform=$BASE_PLATFORM ubuntu:24.04

ARG DEBIAN_FRONTEND=noninteractive
ARG RUNNER_VERSION=2.334.0

ENV LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    RUNNER_TOOL_CACHE=/opt/hostedtoolcache \
    AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache

RUN apt-get update && apt-get install -y --no-install-recommends \
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
    python3 \
    python3-pip \
    python3-venv \
    python3-dev \
    nodejs \
    npm \
    && git lfs install --system \
    && rm -rf /var/lib/apt/lists/*

RUN if ! id -u user >/dev/null 2>&1; then useradd -m -s /bin/bash user; fi \
    && usermod -aG sudo user \
    && echo "user ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/90-user \
    && chmod 0440 /etc/sudoers.d/90-user \
    && mkdir -p /home/user/.config/git /tmp/runner-home/.config/git /opt/hostedtoolcache /opt/actions-runner \
    && chown -R user:user /home/user /tmp/runner-home /opt/hostedtoolcache /opt/actions-runner

RUN set -eux; \
    runner_arch="x64"; \
    curl -fL --show-error --connect-timeout 15 --max-time 300 --retry 5 --retry-delay 2 \
      "https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/actions-runner-linux-${runner_arch}-${RUNNER_VERSION}.tar.gz" \
      -o /tmp/actions-runner.tar.gz; \
    tar xzf /tmp/actions-runner.tar.gz -C /opt/actions-runner; \
    rm /tmp/actions-runner.tar.gz; \
    /opt/actions-runner/bin/installdependencies.sh; \
    test -x /opt/actions-runner/config.sh; \
    test -x /opt/actions-runner/run.sh; \
    rm -rf /var/lib/apt/lists/*; \
    chown -R user:user /opt/actions-runner

WORKDIR /tmp

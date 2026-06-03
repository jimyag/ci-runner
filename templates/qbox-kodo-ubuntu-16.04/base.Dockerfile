ARG BASE_PLATFORM=linux/amd64
FROM --platform=$BASE_PLATFORM ubuntu:16.04

ARG DEBIAN_FRONTEND=noninteractive

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    RUNNER_TOOL_CACHE=/opt/hostedtoolcache \
    AGENT_TOOLSDIRECTORY=/opt/hostedtoolcache \
    GOPATH=/opt/go \
    GOBIN=/usr/local/bin \
    PATH=/usr/local/go/bin:/usr/local/bin:/opt/go/bin:$PATH

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN set -eux; \
    sed -i \
      -e 's|http://archive.ubuntu.com/ubuntu|http://old-releases.ubuntu.com/ubuntu|g' \
      -e 's|http://security.ubuntu.com/ubuntu|http://old-releases.ubuntu.com/ubuntu|g' \
      /etc/apt/sources.list; \
    apt-get update -y; \
    apt-get install -y --no-install-recommends \
      software-properties-common \
      ca-certificates \
      curl; \
    add-apt-repository -y ppa:ubuntu-toolchain-r/test; \
    apt-get update -y; \
    apt-get install -y --no-install-recommends \
      git \
      wget \
      unzip \
      openssh-client \
      pkg-config \
      netcat-openbsd \
      lsof \
      psmisc \
      zlib1g-dev \
      libbz2-dev \
      libsnappy-dev \
      liblz4-dev \
      redis-server \
      libfreetype6-dev \
      enca \
      g++-4.9 \
      cmake \
      libglib2.0-0 \
      ghostscript \
      libgif7 \
      python-dev \
      libevent-dev \
      imagemagick \
      poppler-utils \
      tar \
      gzip \
      xz-utils \
      sudo \
      make \
      iptables \
      docker.io; \
    if ! id -u user >/dev/null 2>&1; then \
      useradd -m -s /bin/bash user; \
    fi; \
    usermod -aG sudo user; \
    usermod -aG docker user; \
    echo 'user ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/90-user; \
    chmod 0440 /etc/sudoers.d/90-user; \
    mkdir -p /home/user/.config/git /tmp/runner-home/.config/git /opt/hostedtoolcache /opt/actions-runner /opt/go /var/lib/docker; \
    chown -R user:user /home/user /tmp/runner-home /opt/hostedtoolcache /opt/actions-runner; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*

WORKDIR /tmp

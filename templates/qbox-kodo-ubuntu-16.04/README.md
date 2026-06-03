# qbox kodo Ubuntu 16.04 Runner Template

Legacy E2B runner template for qbox/kodo-style GitHub Actions jobs that still need Ubuntu 16.04 era system dependencies.

## Base Image

`e2b.Dockerfile` starts from:

```text
jimyag/qbox-kodo-ubuntu-16.04-base:runner-docker
```

The base image is defined by `base.Dockerfile` in this directory. It installs the Ubuntu 16.04 apt dependencies, Docker, the default `user` account, sudo access, and shared runner directories. The E2B template layer then runs `scripts/setup-template.sh` to install the GitHub Actions runner, pinned Go toolchains, and final validation.

Build and push the base image before rebuilding the E2B template when `base.Dockerfile` changes:

```bash
task qbox-kodo-base-build
task qbox-kodo-base-push
```

## E2B Template

Build the E2B template with:

```bash
task qbox-kodo-template-build-prod
```

The production template name is `qbox-kodo-ubuntu-16-04`.

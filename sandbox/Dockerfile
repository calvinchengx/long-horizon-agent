# syntax=docker/dockerfile:1
#
# Reference polyglot sandbox image for LHA's Docker sandbox: git, make, curl, Go, uv + Python 3.12,
# Node 22 + npm/pnpm/corepack, and ripwire (the code map behind LHA_CODE_MAP=ripwire). Built to run the way DockerSandbox runs it:
#
#   docker run --user <host uid>:<host gid> --read-only --tmpfs /tmp -e HOME=/workspace \
#              -v <workdir>:/workspace --cap-drop ALL --security-opt no-new-privileges ...
#
# i.e. an arbitrary non-root uid with no /etc/passwd entry, a read-only root filesystem and a
# writable /tmp (tmpfs) + /workspace (the agent's checkout). Every tool cache is therefore pointed
# at /tmp below, so nothing tries to write to a read-only location and nothing lands in the
# workspace (where it would be committed with the agent's work).
#
# DockerSandbox overrides PATH for every exec with
#   /usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
# so every tool is linked into /usr/local/bin.

ARG GO_VERSION=1.26.8
ARG UV_VERSION=0.12.18
ARG NODE_VERSION=22.23.2
ARG PYTHON_VERSION=3.12

FROM golang:${GO_VERSION}-bookworm AS go
FROM ghcr.io/astral-sh/uv:${UV_VERSION} AS uv

FROM node:${NODE_VERSION}-bookworm-slim

ARG PYTHON_VERSION
ARG PNPM_VERSION=10.34.5

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates curl git make openssh-client xz-utils \
    && rm -rf /var/lib/apt/lists/* \
    # The workspace is bind-mounted from the host and may be owned by a different uid.
    && git config --system safe.directory '*'

# Go (the official toolchain, copied from the pinned golang image).
COPY --from=go /usr/local/go /usr/local/go
RUN ln -s /usr/local/go/bin/go /usr/local/bin/go \
    && ln -s /usr/local/go/bin/gofmt /usr/local/bin/gofmt

# uv + a uv-managed CPython, installed world-readable under /opt and linked onto PATH.
COPY --from=uv /uv /uvx /usr/local/bin/
ENV UV_PYTHON_INSTALL_DIR=/opt/uv/python
RUN uv python install "${PYTHON_VERSION}" \
    && py="$(uv python find "${PYTHON_VERSION}")" \
    && ln -s "$py" /usr/local/bin/python3 \
    && ln -s "$py" /usr/local/bin/python \
    && ln -s "$py" "/usr/local/bin/python${PYTHON_VERSION}" \
    && chmod -R a+rX /opt/uv \
    && python --version && python -m pip --version

# pnpm (pinned, installed globally so nothing is downloaded at run time); corepack stays available
# for projects that pin a package manager via "packageManager".
RUN npm install -g "pnpm@${PNPM_VERSION}" \
    && npm cache clean --force \
    && rm -rf /root/.npm

# ripwire: a static, dependency-free code-map CLI (LHA_CODE_MAP=ripwire runs it at the start of
# each cycle). Pinned by version and by the release's published SHA-256 for each architecture.
ARG RIPWIRE_VERSION=0.6.5
ARG RIPWIRE_SHA256_ARM64=25e5f37e2985830bbff189879797988b862e36521413a6e3654630a0e1f0b108
ARG RIPWIRE_SHA256_X64=5c5794612f5f06632ada7c27a0f5c8f400748a70f707b64ec4d238c4fba2f7ea
RUN set -eu; \
    case "$(dpkg --print-architecture)" in \
        arm64) arch=linux-arm64; sum="${RIPWIRE_SHA256_ARM64}" ;; \
        amd64) arch=linux-x64; sum="${RIPWIRE_SHA256_X64}" ;; \
        *) echo "no ripwire build for $(dpkg --print-architecture)" >&2; exit 1 ;; \
    esac; \
    name="ripwire-${RIPWIRE_VERSION}-${arch}"; \
    curl -fsSL -o /tmp/ripwire.tar.gz \
        "https://github.com/redhat-et/ripwire/releases/download/v${RIPWIRE_VERSION}/${name}.tar.gz"; \
    echo "${sum}  /tmp/ripwire.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/ripwire.tar.gz -C /tmp "${name}/ripwire"; \
    install -m 0755 "/tmp/${name}/ripwire" /usr/local/bin/ripwire; \
    rm -rf /tmp/ripwire.tar.gz "/tmp/${name}"; \
    ripwire --version

ENV HOME=/workspace \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    TMPDIR=/tmp \
    XDG_CACHE_HOME=/tmp/.cache \
    XDG_CONFIG_HOME=/tmp/.config \
    XDG_DATA_HOME=/tmp/.local/share \
    XDG_STATE_HOME=/tmp/.local/state \
    # Go: module + build caches on tmpfs; the toolchain is the pinned one (no auto-download).
    GOPATH=/tmp/go \
    GOMODCACHE=/tmp/go/pkg/mod \
    GOCACHE=/tmp/go-build \
    GOFLAGS=-modcacherw \
    GOTOOLCHAIN=local \
    # uv/pip: caches on tmpfs; copy (not hardlink) across the /tmp -> /workspace boundary;
    # never try to download another interpreter into the read-only /opt.
    UV_CACHE_DIR=/tmp/uv-cache \
    UV_LINK_MODE=copy \
    UV_PYTHON_DOWNLOADS=never \
    PIP_CACHE_DIR=/tmp/pip-cache \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    # npm/pnpm/corepack: caches + store on tmpfs.
    npm_config_cache=/tmp/npm-cache \
    npm_config_update_notifier=false \
    npm_config_store_dir=/tmp/pnpm-store \
    PNPM_HOME=/tmp/pnpm \
    COREPACK_HOME=/tmp/corepack \
    COREPACK_ENABLE_DOWNLOAD_PROMPT=0

WORKDIR /workspace
USER 65534:65534
CMD ["sleep", "infinity"]

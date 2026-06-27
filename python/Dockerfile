# Python worker image for the durable spine. Build (from the repo root): docker build -t lha:python python
# Run (against a Temporal server): docker run --rm -e LHA_TEMPORAL_ADDRESS=host:7233 lha:python
#
# The worker executes agent tool calls through the configured sandbox (LHA_SANDBOX, default
# "docker"). Point it at a Docker daemon with DOCKER_HOST, or use LHA_SANDBOX=e2b. Never mount
# the host's /var/run/docker.sock into this container: that hands the agent root on the host.
FROM python:3.12-slim

# git is required: the mission anchor (checklist, progress, commits) lives in a git repo.
RUN apt-get update \
    && apt-get install -y --no-install-recommends git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# uv for fast, reproducible installs.
COPY --from=ghcr.io/astral-sh/uv:0.11.8 /uv /usr/local/bin/uv

WORKDIR /app

# Install dependencies first (better layer caching).
COPY pyproject.toml uv.lock README.md ./
COPY src ./src
RUN uv sync --frozen --no-dev

# Run as an unprivileged user; mission workspaces and the object store live in its home.
RUN useradd --create-home --uid 10001 lha
USER lha
WORKDIR /home/lha
ENV PATH="/app/.venv/bin:$PATH" \
    LHA_WORKSPACE_ROOT=/home/lha/workspaces \
    LHA_OBJECT_STORE_ROOT=/home/lha/objects \
    GIT_TERMINAL_PROMPT=0

# Default: serve missions as a Temporal worker.
CMD ["lha", "worker"]

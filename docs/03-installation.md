# Installation

## Requirements

| Requirement | Needed for |
|---|---|
| `git` | Always. Every mission workspace is a git repository; the mission anchor is committed to it. |
| [uv](https://docs.astral.sh/uv/) | The Python implementation. uv provisions Python 3.12 (`python/.python-version`). |
| Docker daemon | The default `docker` sandbox (and its egress proxy), the Docker images, and the compose stack. |
| Go (version in [`go/go.mod`](../go/go.mod), currently 1.26) | Building and testing the Go packages. |
| [Ollama](https://ollama.com) | Optional: local models at $0. |

## Python

```bash
cd python
uv sync
uv run lha --help
```

`uv sync` installs the core dependencies (pydantic, pydantic-settings, typer, structlog, orjson,
tenacity, httpx, temporalio) and the dev group (ruff, pytest, pytest-asyncio, ty, pytest-cov).
The project requires Python `>=3.12`. The `lha` console script is `lha.cli.main:app`.

### Optional extras

Heavy or environment-specific dependencies are extras, declared in
[`python/pyproject.toml`](../python/pyproject.toml):

| Extra | Installs | Used by |
|---|---|---|
| `postgres` | `psycopg[binary,pool]`, `pgvector` | `lha db migrate`; the Postgres mission store (`LHA_POSTGRES_DSN`: mission rows, cost ledger, memory) and its pgvector index. Without it, runs use SQLite |
| `embeddings` | `sentence-transformers` | `LHA_MEMORY_EMBEDDER=sentence_transformers` and `LHA_MEMORY_RERANK=cross_encoder`; without it, memory falls back to lexical-only retrieval. Semantic memory without this extra: `LHA_MEMORY_EMBEDDER=ollama` and a local Ollama with `nomic-embed-text` pulled |
| `observability` | `opentelemetry-sdk`, `opentelemetry-exporter-otlp-proto-http` | OTLP trace export to a collector or to Langfuse, configured at process start when an endpoint or the Langfuse keys are set ([16-observability.md](16-observability.md)) |
| `sandbox` | `docker` | The `docker` sandbox, which is the default |

```bash
uv sync --extra sandbox                    # needed for the default docker sandbox
uv sync --extra postgres --extra sandbox   # extras combine
```

The `claude` model backend needs no extra: it calls the Messages API over HTTP with
`LHA_ANTHROPIC_API_KEY`. The `e2b` sandbox needs the `e2b-code-interpreter` package,
which is not part of any extra, and an E2B account; it is not exercised in CI.

If a command needs a module from a missing extra, it stops with an error naming the extra, for
example `error: python module 'docker' is not installed; install the 'sandbox' extra (lha[sandbox])`.

### Configuration

All settings are `LHA_*` environment variables, optionally read from a `.env` file in the
current working directory (so `python/.env` when you run from `python/`). Environment variables
override the file. [`.env.example`](../.env.example) at the repository root lists them;
`uv run lha config` prints the resolved values with secrets masked. The full list is in
[configuration](18-configuration.md).

## Go

```bash
cd go
go test ./...
```

The Go implementation is being ported in phases. `go build -o lha ./cmd/lha` (from `go/`) builds
a CLI that runs single-agent missions locally (`lha run-local`, `lha mission`) in the local or
Docker sandbox; there is no Go Temporal worker yet. See
[choosing an implementation](04-choosing-an-implementation.md) for what exists.

## Docker image (Python worker)

[`python/Dockerfile`](../python/Dockerfile) builds a Temporal worker image:

```bash
docker build -t lha:python python                           # from the repository root
docker run --rm -e LHA_TEMPORAL_ADDRESS=host:7233 lha:python
```

- Base image `python:3.12-slim` with `git` and `ca-certificates`; `uv` is copied from
  `ghcr.io/astral-sh/uv:0.11.8`; dependencies are installed with `uv sync --frozen --no-dev`
  (no extras).
- Runs as the unprivileged user `lha` (uid 10001) with `LHA_WORKSPACE_ROOT=/home/lha/workspaces`,
  `LHA_OBJECT_STORE_ROOT=/home/lha/objects` and `GIT_TERMINAL_PROMPT=0`.
- The default command is `lha worker`.

The worker runs agent commands through the configured sandbox (`LHA_SANDBOX`, default `docker`).
Point it at a Docker daemon with `DOCKER_HOST`, or use `LHA_SANDBOX=e2b`. The Dockerfile's own
warning applies: do not mount the host's `/var/run/docker.sock` into the container, because that
gives the agent root on the host. The image installs no extras, so the `docker` sandbox inside it
also needs the `sandbox` extra added to the build, and a Postgres mission store needs the
`postgres` extra (without it the worker falls back to SQLite, or fails when
`LHA_POSTGRES_FALLBACK_TO_SQLITE=false`).

There is no Go image yet.

## Sandbox image and egress

The `docker` sandbox runs agent commands and checks in `LHA_SANDBOX_IMAGE`, default
`ghcr.io/astral-sh/uv:python3.12-bookworm-slim` (Python 3.12 and uv). The image must contain
every tool your checks and witnesses call. For Go or Node projects, build the reference polyglot
image in [`sandbox/`](../sandbox/) (Go, uv with Python 3.12, Node.js with npm/corepack and pnpm,
git, make):

```bash
docker build -t lha-sandbox:latest sandbox/     # from the repository root
export LHA_SANDBOX_IMAGE=lha-sandbox:latest
```

The image keeps every cache under `/tmp`, because the sandbox runs as the host uid with a
read-only root filesystem and a 1 GB `/tmp` tmpfs mounted `exec` (so `go test` can run the binaries it
builds). [`sandbox/README.md`](../sandbox/README.md) lists the versions and build arguments.

By default the sandbox has no network. To let package managers reach specific registries, list
the hosts:

```bash
export LHA_SANDBOX_EGRESS="proxy.golang.org,sum.golang.org,storage.googleapis.com,pypi.org,files.pythonhosted.org,registry.npmjs.org"
```

Each sandbox session then gets its own `--internal` Docker network (no route out) and a proxy
container (`python:3.12-alpine`, running
[`egress_proxy.py`](../python/src/lha/execution/egress_proxy.py)) that is the only way out and
forwards only to the listed hosts. The first run pulls that image. Both are removed when the
session closes. `LHA_SANDBOX_EGRESS` takes package-registry download hosts only; any other host
goes in `LHA_SANDBOX_EGRESS_EXTRA_HOSTS`, and a host that accepts pushes or uploads (such as
`github.com`) in `LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS`, because code in the sandbox can send
data to every host it can reach. See [safety model](09-safety-model.md#sandbox-network).

## Local service stack

[`docker-compose.yml`](../docker-compose.yml) runs the services used by the durable path:

| Service | Image | Port (host) | Purpose |
|---|---|---|---|
| `temporal` | `temporalio/auto-setup:1.27` | `127.0.0.1:7233` | Temporal server |
| `temporal-db` | `postgres:16` | none | Temporal's database |
| `temporal-ui` | `temporalio/ui:2.34.0` | `127.0.0.1:8080` | Web UI for workflows |
| `appdb` | `pgvector/pgvector:pg16` | `127.0.0.1:5432` | LHA's Postgres + pgvector |
| `langfuse` | `langfuse/langfuse:2` | `127.0.0.1:3000` | LLM observability UI |
| `langfuse-db` | `postgres:16` | none | Langfuse's database |

Every published port is bound to `127.0.0.1`. None of these services has authentication
suitable for a network; anyone who can reach Temporal can start workflows or signal gate
decisions.

Before `docker compose up`, set the two Langfuse secrets in a `.env` file next to
`docker-compose.yml` (the repository root). They have no defaults and compose refuses to start
without them:

```bash
# .env at the repository root
LANGFUSE_NEXTAUTH_SECRET=<random string>
LANGFUSE_SALT=<random string>
# optional: LHA_APPDB_PASSWORD (defaults to "lha")
```

```bash
docker compose up -d
```

`appdb` applies [`db/migrations/`](../db/migrations/) automatically on first start of an empty
volume. To apply them to an existing database, install the `postgres` extra and run, from
`python/`:

```bash
LHA_POSTGRES_DSN=postgresql://lha:lha@localhost:5432/lha uv run lha db migrate
```

Without `--migrations-dir`, the command uses `db/migrations` in the current directory, or
`../db/migrations` if that does not exist, so it works from both the repository root and
`python/`.

With `LHA_POSTGRES_DSN` set (and the `postgres` extra installed), every run path writes its
mission row, cost ledger and memory to `appdb`; without it, they go to a local SQLite file
(`LHA_SQLITE_PATH`; unset: a per-user file every process shares: `$XDG_DATA_HOME/lha/lha.sqlite3`, else `~/Library/Application Support/lha/lha.sqlite3` on macOS or `~/.local/share/lha/lha.sqlite3` on Linux). If Postgres is
set but unusable, runs fall back to SQLite with a warning unless
`LHA_POSTGRES_FALLBACK_TO_SQLITE=false`. `lha missions` and `lha costs` read the same store, so
run them with the same settings as the run (for durable missions, as the worker and
`mission-start`); `lha config` prints the resolved location. The Langfuse server receives
traces only when the `LHA_LANGFUSE_*` settings point at it and the `observability` extra is
installed ([16-observability.md](16-observability.md)). Temporal is the only service the CLI's durable commands need. See
[running on Temporal](14-running-on-temporal.md).

# Quickstart

This page runs a first mission with the Python implementation, first with the built-in `stub`
model and then with a real local model through Ollama. Neither needs an API key. You need `git`
and [uv](https://docs.astral.sh/uv/); see [installation](03-installation.md) for details.

## 1. Install and check the configuration

From a clean checkout:

```bash
cd python
uv sync                 # provisions Python 3.12 and installs the core dependencies
uv run lha version      # prints: lha 0.1.0
uv run lha config       # resolved LHA_* settings, secrets shown as ***
```

With no environment set, `lha config` shows `model_backend = stub`, `sandbox = docker` and
`budget_usd_ceiling = 10.0`. Settings come from `LHA_*` environment variables and an optional
`.env` file in the current directory.

## 2. A mission with the stub model

```bash
uv run lha mission \
  --task "Create hello.py with hello() returning 'hello', and a pytest test" \
  --workdir ../.lha/workspaces/stub-demo \
  --sandbox local --unsafe-local
```

What happens:

1. The Planner asks the model to split the task into steps. The stub's reply is not a JSON
   plan, so the Planner falls back to a single item, `01`, containing the task text.
2. The workdir is initialized as a git repository with a `.lha/` mission anchor and an initial
   commit.
3. Each cycle gives the model up to `LHA_MAX_TURNS_PER_CYCLE` (default 20) turns. The stub
   replies with a fixed acknowledgement that is not a valid action, so it writes no files. Each
   invalid reply gets a corrective turn, so every cycle uses all 20 turns.
4. At the end of the cycle the verifier runs the default checks (`uv run ruff check .`,
   `uv run ty check`, `uv run pytest -q`). With no tests in the workspace the pytest check
   fails (exit 5, "no tests ran"; a check whose tool is not installed also fails), so the
   verdict is `failed` and the item stays `in_progress`.
5. After 3 consecutive failed cycles the item becomes `blocked`. The replanner then asks the
   model to split it into smaller items; the stub's reply is not a usable split, so the item
   stays blocked. Nothing else is actionable, so the mission stops as deadlocked.

Structured log lines are printed as it runs: per cycle `memory_recall`, `cycle_started`, 20
`llm_turn` / `invalid_reply` pairs, `turns_exhausted` and `checkpoint`. The run ends with a summary like this (the mission id and commit sha differ per
run), and the command exits with status 1:

```text
mission mission_16e364017c89: deadlocked: blocked: 01
items 0/1  cycles 3  cost $0.0000
head efcb2c5bc9b94b52c4c724e0b9db1b186fd121f9
```

This is the intended result. The verification gate refused to mark unverified work as done,
and the mission reported a deadlock instead of claiming completion. The failure reason is
recorded in the checklist:

```bash
cat ../.lha/workspaces/stub-demo/.lha/checklist.json   # "status": "blocked", "attempts": 3, "last_failure": ...
git -C ../.lha/workspaces/stub-demo log --oneline        # initialize, attempt, attempt, block
```

The run is also recorded in the mission store, a per-user SQLite file
(`~/.local/share/lha/lha.sqlite3` on Linux, `~/Library/Application Support/lha/lha.sqlite3` on
macOS; `LHA_SQLITE_PATH` overrides it, Postgres when `LHA_POSTGRES_DSN` is set; `lha config`
prints the location):

```bash
uv run lha missions                 # the mission, status IMPOSSIBLE (deadlocked), known spend
uv run lha costs <mission_id>       # every model call; the stub's cost $0
```

The stub exists for tests and CI. Its output is never a real agent run.

## 3. A real model at $0 with Ollama

Install [Ollama](https://ollama.com), start it, and pull a model (for example
`ollama pull qwen3:8b`). Then:

```bash
LHA_MODEL_BACKEND=ollama LHA_MODEL_NAME=qwen3:8b \
  uv run lha mission \
  --task "Create hello.py with hello() returning 'hello', and a pytest test" \
  --workdir ../.lha/workspaces/demo \
  --sandbox local --unsafe-local \
  --no-default-checks --check "uv run --with pytest pytest -q"
```

- `LHA_MODEL_BACKEND=ollama` talks to Ollama's OpenAI-compatible API at `LHA_OLLAMA_BASE_URL`
  (default `http://localhost:11434`) and prices every call at $0.
- `--no-default-checks` drops the ruff/ty/pytest defaults, and `--check` supplies the gate for
  this mission. At least one `--check` is required when the defaults are dropped. See
  [verification](07-verification.md#choosing-checks).

Whether the mission completes depends on the model. Afterwards, inspect what it did:

```bash
git -C ../.lha/workspaces/demo log --oneline             # one commit per cycle
cat ../.lha/workspaces/demo/.lha/progress.md             # one line per cycle with its verdict
cat ../.lha/workspaces/demo/.lha/checklist.json          # item status, verified_by, last_failure
```

A `done` item lists the checks that proved it in `verified_by`. Use a fresh `--workdir` for each
run: running again in the same directory re-initializes the anchor on top of the existing
history.

To skip the Planner and start from your own plan, pass `--checklist FILE` instead of `--task`: a
JSON checklist or a Markdown roadmap of `- [ ] item` lines, where each item can name its own
acceptance checks, for example `- [ ] Add hello() (witness: pytest:test_hello.py)`. See
[the mission anchor](06-mission-anchor.md#importing-a-checklist).

## About the sandbox

`--sandbox local --unsafe-local` runs the agent's shell commands and the checks directly on your
machine as your user, with your network access. File tools are confined to the workdir, but
arbitrary commands are not. Use it only for tasks and models you are prepared to trust.

The default sandbox is `docker`, which needs the `sandbox` extra (`uv sync --extra sandbox`) and
a running Docker daemon. It bind-mounts the workdir into a container with no network, dropped
capabilities, resource limits, a read-only root filesystem and read-only `.git/` and `.lha/`.
The default image (`LHA_SANDBOX_IMAGE`) is `ghcr.io/astral-sh/uv:python3.12-bookworm-slim`,
which has Python 3.12 and uv but no ruff, ty or pytest. With no network, `uv run --with pytest`
cannot download pytest, so either allow the package index
(`LHA_SANDBOX_EGRESS=pypi.org,files.pythonhosted.org`) or use an image that already has the
tools; [`sandbox/Dockerfile`](../sandbox/Dockerfile) builds one with Go, uv, Node/pnpm, gcc
(for `go test -race`) and ripwire (for `LHA_CODE_MAP` and `LHA_CODE_QUERY`). See
[installation](03-installation.md#sandbox-image-and-egress). Without the extra, a docker run
stops with `error: python module 'docker' is not installed; install the 'sandbox' extra (lha[sandbox])`.

## Next steps

- A durable mission end to end, with a crashed worker and an approved `git push`:
  [the tutorial](26-tutorial-durable-mission.md).
- Other model backends and prices: [installation](03-installation.md) and
  [models](13-models.md).
- Durable runs on Temporal (`lha worker`, `lha mission-start`): [running on Temporal](14-running-on-temporal.md).
- The local multi-agent flow (`lha orchestrate`): [architecture](05-architecture.md#the-asymmetric-organization).
- What the `.lha/` files contain: [the mission anchor](06-mission-anchor.md).

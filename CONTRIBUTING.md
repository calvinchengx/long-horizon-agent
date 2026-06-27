# Contributing to LHA

Thanks for your interest! LHA is a durable, self-improving agent organization for long-horizon
software missions. This guide gets you productive fast.

## Dev setup

```bash
uv sync                 # Python 3.12 + core + dev deps (provisions the interpreter)
uv run lha --help
```

## The checks (must be green before a PR)

```bash
uv run ruff check .         # lint
uv run ruff format --check . # formatting
uv run ty check              # type-check (src/)
uv run pytest -q             # tests
```

Coverage (the floor lives in `pyproject.toml` as `fail_under`; CI enforces it):

```bash
uv run pytest -q --cov --cov-report= tests/unit tests/durability tests/load
uv run coverage report
```

CI (`.github/workflows/ci.yml`) runs exactly these on every push/PR.

### Integration tests (real Postgres + Docker)

`tests/integration/` runs against real services and is skipped unless you opt in. Use a
throwaway pgvector container on a free port (it creates and drops its own databases):

```bash
docker run -d --name lha-it-pg -e POSTGRES_USER=lha -e POSTGRES_PASSWORD=lha -e POSTGRES_DB=lha -p 127.0.0.1:55432:5432 pgvector/pgvector:pg16
uv sync --extra postgres --extra sandbox
LHA_IT_POSTGRES_DSN=postgresql://lha:lha@127.0.0.1:55432/lha LHA_IT_DOCKER=1 uv run pytest -q tests/integration
```

## Conventions

- **Typed.** All of `src/` passes `ty check` (warnings are errors). Public functions are fully annotated.
- **Contracts first.** Cross-plane interfaces live in `src/lha/contracts/` as `Protocol`s. Depend
  on the Protocol, not a concrete implementation, so backends stay swappable.
- **Honesty policy.** Anything presented as a real agent run uses genuine model output or a
  clearly-labeled recorded replay — never fabricated text. `StubModel` is for tests only.
- **Determinism in workflows.** No LLM/tool/IO/clock/RNG in Temporal *workflow* code — only in
  *activities*. Keep the workflow body replayable.
- **Tests alongside code.** New modules ship with tests under `tests/`.

## Project layout

See [docs/architecture.md](docs/architecture.md) for the four planes and the asymmetric org.

## Optional extras

```bash
uv sync --extra postgres --extra embeddings --extra observability --extra sandbox --extra claude
```

## Reporting issues / security

Open a GitHub issue for bugs/features. For security concerns, see `SECURITY.md` (please do not
file public issues for vulnerabilities).

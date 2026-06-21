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

CI (`.github/workflows/ci.yml`) runs exactly these on every push/PR.

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

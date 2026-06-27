# Contributing

This page is the documentation-site version of [`CONTRIBUTING.md`](../CONTRIBUTING.md), which
remains the canonical file. Security reports follow [`SECURITY.md`](../SECURITY.md): email the
maintainer, do not open a public issue.

## Repository layout

| Path | Contents |
|---|---|
| [`python/`](../python/) | the reference implementation (`src/lha`), its tests, `pyproject.toml`, `Dockerfile` |
| [`go/`](../go/) | the Go port, built in phases ([23-roadmap.md](23-roadmap.md)) |
| [`spec/`](../spec/) | language-neutral conformance cases both test suites run |
| [`db/migrations/`](../db/migrations/) | the Postgres schema, one SQL file per version |
| [`docs/`](../docs/) | this documentation (`NN-slug.md`) and the predicted runs |
| [`website/`](../website/) | the Starlight site that renders `docs/` |
| [`docker-compose.yml`](../docker-compose.yml) | local Temporal, Postgres + pgvector, Langfuse |

A behaviour change that is observable (which commands need approval, which URLs may be fetched,
what is redacted, how a checklist moves, what a call costs, what bytes the anchor holds) must land
in both implementations, with a `spec/` case that pins it.

## Set up

```bash
cd python
uv sync                 # Python 3.12, core + dev dependencies
uv run lha --help
```

Optional extras: `uv sync --extra postgres --extra embeddings --extra observability --extra
sandbox --extra claude`. For Go, install Go 1.26 and work in `go/`.

## Checks before a pull request

Python, from `python/`:

```bash
uv run ruff check .
uv run ruff format --check .
uv run ty check
uv run pytest -q --cov --cov-report= tests/unit tests/durability tests/load
uv run coverage report        # fails under the fail_under floor in pyproject.toml (90%)
```

Go, from `go/`:

```bash
gofmt -l . && go vet ./... && go test ./...
```

CI runs the Python checks (as two pytest steps plus the coverage report) and a separate
integration job against real Postgres and Docker. It does not run the Go checks, so run them
locally. Details, including how to run the integration tests yourself, are in
[20-testing.md](20-testing.md).

## Conventions

- **Typed.** `src/` passes `ty check` with warnings as errors. Public functions are fully annotated.
- **Contracts first.** Cross-plane interfaces are `Protocol`s in
  [`python/src/lha/contracts/`](../python/src/lha/contracts/). Depend on the protocol, not a
  concrete backend.
- **Deterministic workflows.** No model call, tool call, I/O, clock or randomness in Temporal
  workflow code; only in activities. A change to `MissionWorkflow` or `SubAgentWorkflow` must keep
  `tests/durability/test_replay.py` green (see [15-operations-runbook.md](15-operations-runbook.md#safe-deploys-during-an-in-flight-mission)).
- **Wire names are fixed.** Workflow, activity, signal and query names, payload field names, the
  `.lha/` formats and the SQL schema are shared with the Go port ([19-wire-contract.md](19-wire-contract.md)).
  Schema changes are a new `db/migrations/NNNN_*.sql` file, never an edit to an applied one.
- **Tests alongside code.** New modules ship with tests under `python/tests/`.
- **Honesty.** Anything presented as a real run uses real model output or a clearly labelled
  recording; the stub is for tests. See [22-honesty.md](22-honesty.md).

## Changing behaviour through `spec/`

The Python implementation is the reference. To change a pinned behaviour on purpose:

1. Change the Python code and its unit tests.
2. Regenerate the cases:

   ```bash
   cd python && uv run python scripts/export_spec.py
   ```

   [`scripts/export_spec.py`](../python/scripts/export_spec.py) runs the Python functions over
   fixed corpora (some imported from `tests/unit`) and rewrites the JSON files in `spec/`.
3. Review the JSON diff: every changed case should be one you meant to change.
4. Make the Go implementation pass: `cd go && go test ./internal/spec/`.

A case is never edited by hand to match a bug.

## Building this documentation site

The pages are plain Markdown in `docs/`, named `NN-slug.md` (two digits, lowercase slug), each
starting with an H1 title. They must read correctly on GitHub:

- link another page as `NN-slug.md` (optionally `#anchor`);
- link repository files with `../` paths, for example `../python/src/lha/config.py`;
- link files inside `docs/` that are not pages (such as `predicted-runs/...`) by their relative
  path.

[`website/scripts/sync-docs.mjs`](../website/scripts/sync-docs.mjs) converts them for the site:
it takes the title from the H1, adds frontmatter with an edit link, rewrites page links to site
routes and `../` links to GitHub URLs, and warns about links to missing pages or files. The sidebar
order is listed in [`website/astro.config.mjs`](../website/astro.config.mjs); add a new page there.

```bash
cd website
pnpm install
pnpm dev       # sync docs/, then serve with live reload
pnpm build     # sync docs/, then build the static site into website/dist
SYNC_DOCS_STRICT=1 pnpm build   # fail on broken links, as CI does
```

Requires Node 22+ and pnpm 10+. The [`docs-site.yml`](../.github/workflows/docs-site.yml) workflow
builds the site in strict mode for pull requests that touch `docs/` or `website/`, and deploys it
to GitHub Pages from `main`.

## Issues

Open a GitHub issue for bugs and feature requests.

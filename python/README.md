# LHA — Python implementation

The Python implementation of the `lha` CLI and worker. It is one of two interchangeable
implementations (see `../go/`); both expose the same commands, `LHA_*` settings, on-disk mission
anchor, Postgres schema and Temporal workflows. See the [project README](../README.md).

```bash
cd python
uv sync
uv run lha --help
```

Tests, lint and types (from this directory):

```bash
uv run ruff check . && uv run ruff format --check . && uv run ty check
uv run pytest -q
```

Author: Calvin Cheng <calvin@calvinx.com>. MIT licensed.

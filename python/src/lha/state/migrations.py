"""Versioned document-schema migrators.

Over a multi-week run the progress/checklist/memory formats may evolve. Each artifact has a
schema_version; registered migrators upgrade a document from one version to the next, so old
durable data is never silently incompatible after a deploy. This is the doc-schema analogue of
Temporal Worker Versioning.
"""

from __future__ import annotations

from collections.abc import Callable

Migrator = Callable[[dict[str, object]], dict[str, object]]

_MIGRATORS: dict[tuple[str, int], Migrator] = {}


def register(artifact: str, from_version: int) -> Callable[[Migrator], Migrator]:
    """Decorator: register a migrator that upgrades ``artifact`` from ``from_version`` to the next."""

    def decorator(fn: Migrator) -> Migrator:
        _MIGRATORS[(artifact, from_version)] = fn
        return fn

    return decorator


def _as_int(value: object, default: int) -> int:
    """Coerce a JSON-ish value to a positive int, falling back to ``default``."""
    if isinstance(value, bool):
        return default
    if isinstance(value, int):
        return value or default
    if isinstance(value, str) and value.strip().isdigit():
        return int(value) or default
    return default


def migrate(artifact: str, data: dict[str, object], *, target_version: int) -> dict[str, object]:
    """Apply registered migrators until ``data`` reaches ``target_version``."""
    version = _as_int(data.get("schema_version", 1), 1)
    while version < target_version:
        fn = _MIGRATORS.get((artifact, version))
        if fn is None:
            raise ValueError(f"no migrator registered for {artifact!r} v{version}")
        data = fn(data)
        new_version = _as_int(data.get("schema_version", version + 1), version + 1)
        if new_version <= version:
            raise ValueError(f"migrator for {artifact!r} v{version} did not advance the version")
        version = new_version
    return data

"""File-ownership map — conflict prevention by single-writer-per-file (declare-then-ENFORCE).

The Planner assigns each file to one writer; the integration line ("lead") owns all unassigned
space. Ambiguous/shared files (build manifests, lockfiles, ``__init__.py``, conftest, settings,
migrations) are ALWAYS the lead's — they can never be handed to a parallel implementer. The map
is enforced at the git layer (``violations``) so a stray write fails loud rather than silently
corrupting a parallel slice; a writer that discovers it needs a foreign file files a
``LeaseRequest`` instead of writing.
"""

from __future__ import annotations

from pathlib import PurePosixPath

from pydantic import BaseModel, Field

LEAD = "lead"

# Basenames (case-folded) that are shared at ANY depth: build manifests, lockfiles and package
# entry points are cross-cutting in monorepos too (``services/api/pyproject.toml`` etc.).
_SHARED_BASENAMES = {
    # Python
    "pyproject.toml",
    "setup.py",
    "setup.cfg",
    "uv.lock",
    "poetry.lock",
    "__init__.py",
    "conftest.py",
    "settings.py",
    # JavaScript / TypeScript
    "package.json",
    "package-lock.json",
    "pnpm-lock.yaml",
    "yarn.lock",
    # Go / Rust
    "go.mod",
    "go.sum",
    "cargo.toml",
    "cargo.lock",
}


class InvalidPathError(ValueError):
    """A path that is absolute or escapes the repository root (e.g. ``../x``)."""


def _normalize(path: str) -> str:
    """Normalize a repo-relative path lexically (``PurePosixPath`` semantics).

    Backslashes become ``/``; ``.`` segments and redundant slashes are dropped; ``..`` is resolved
    against the preceding segment. Leading dots in names are KEPT (``.env`` is not ``env``).
    Absolute paths and paths that climb above the repo root raise ``InvalidPathError``.
    """
    posix = PurePosixPath(path.replace("\\", "/"))
    if posix.is_absolute():
        raise InvalidPathError(f"path must be repo-relative, got {path!r}")
    parts: list[str] = []
    for part in posix.parts:
        if part == "..":
            if not parts:
                raise InvalidPathError(f"path escapes the repository root: {path!r}")
            parts.pop()
        elif part != ".":
            parts.append(part)
    if not parts:
        raise InvalidPathError(f"path names no file: {path!r}")
    return "/".join(parts)


def is_shared(path: str) -> bool:
    """True if ``path`` is a shared/ambiguous file that must stay on the serial lead thread.

    Matching is case-insensitive (``Cargo.toml`` == ``cargo.toml``; case-insensitive filesystems
    would otherwise let a writer slip past the check).
    """
    norm = _normalize(path).casefold()
    base = norm.rsplit("/", 1)[-1]
    return (
        base in _SHARED_BASENAMES
        or base.endswith(".lock")
        or (base.startswith("requirements") and base.endswith(".txt"))
        or "/migrations/" in f"/{norm}"
    )


class OwnershipViolation(BaseModel):
    """A write to a path the writer does not own."""

    path: str
    writer: str
    owner: str | None


class LeaseRequest(BaseModel):
    """A writer's request to (temporarily) own a file outside its declared write-set."""

    writer: str
    path: str
    reason: str


class FileOwnershipMap(BaseModel):
    """Maps file paths to their single permitted writer."""

    owners: dict[str, str] = Field(default_factory=dict)  # normalized path -> writer id

    def assign(self, path: str, writer: str) -> None:
        """Assign ``path`` to ``writer``. Shared files can only ever belong to the lead."""
        norm = _normalize(path)
        if is_shared(norm) and writer != LEAD:
            raise ValueError(f"shared file {norm!r} can only be owned by the lead")
        self.owners[norm] = writer

    def owner_of(self, path: str) -> str | None:
        try:
            return self.owners.get(_normalize(path))
        except InvalidPathError:
            return None

    def permits(self, *, writer: str, path: str) -> bool:
        """Whether ``writer`` may write ``path`` under the single-writer-per-file invariant.

        Paths outside the repository (absolute, or escaping via ``..``) are never permitted.
        """
        try:
            norm = _normalize(path)
        except InvalidPathError:
            return False
        if is_shared(norm):
            return writer == LEAD
        owner = self.owners.get(norm)
        if owner is None:
            return writer == LEAD  # unassigned space belongs to the serial lead
        return writer == owner

    def violations(self, *, writer: str, paths: list[str]) -> list[OwnershipViolation]:
        """Return ownership violations for a set of changed ``paths`` by ``writer``."""
        out: list[OwnershipViolation] = []
        for path in paths:
            if not self.permits(writer=writer, path=path):
                try:
                    shown = _normalize(path)
                except InvalidPathError:
                    shown = path
                out.append(OwnershipViolation(path=shown, writer=writer, owner=self.owner_of(path)))
        return out

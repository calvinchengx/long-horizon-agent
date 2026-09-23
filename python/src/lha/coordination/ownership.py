"""File-ownership map — conflict prevention by single-writer-per-file (declare-then-ENFORCE).

The Planner assigns each file to one writer; the integration line ("lead") owns all unassigned
space. Ambiguous/shared files (build manifests, lockfiles, ``__init__.py``, conftest, settings,
migrations) are ALWAYS the lead's — they can never be handed to a parallel implementer. The map
is enforced at the git layer (``violations``) so a stray write fails loud rather than silently
corrupting a parallel slice; a writer that discovers it needs a foreign file files a
``LeaseRequest`` instead of writing.
"""

from __future__ import annotations

from collections.abc import Iterable
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


def _key(path: str) -> str:
    """The ownership-map key for ``path``: normalized AND case-folded.

    Case-insensitive filesystems (macOS/Windows defaults) map ``Models.py`` and ``models.py`` to
    the same file, so they must map to the same owner — otherwise two writers could each "own" it.
    """
    return _normalize(path).casefold()


class OwnershipConflictError(ValueError):
    """``assign`` was asked to hand a file that another writer already owns to a new writer."""


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

    # normalized + case-folded path -> writer id (see ``_key``)
    owners: dict[str, str] = Field(default_factory=dict)

    def assign(self, path: str, writer: str) -> None:
        """Assign ``path`` to ``writer``. Shared files can only ever belong to the lead.

        Re-assigning a file to its current owner is a no-op. Handing a file that another writer
        already owns to a different writer raises ``OwnershipConflictError`` — ownership is never
        silently stolen (use ``reassign`` to transfer it explicitly).
        """
        key = self._checked_key(path, writer)
        current = self.owners.get(key)
        if current is not None and current != writer:
            raise OwnershipConflictError(
                f"{_normalize(path)!r} is already owned by {current!r}; "
                f"refusing to hand it to {writer!r} (use reassign)"
            )
        self.owners[key] = writer

    def reassign(self, path: str, writer: str) -> str | None:
        """Explicitly transfer ``path`` to ``writer``; returns the previous owner (if any)."""
        key = self._checked_key(path, writer)
        previous = self.owners.get(key)
        self.owners[key] = writer
        return previous

    @staticmethod
    def _checked_key(path: str, writer: str) -> str:
        norm = _normalize(path)
        if is_shared(norm) and writer != LEAD:
            raise ValueError(f"shared file {norm!r} can only be owned by the lead")
        return norm.casefold()

    def owner_of(self, path: str) -> str | None:
        try:
            return self.owners.get(_key(path))
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
        owner = self.owners.get(norm.casefold())
        if owner is None:
            return writer == LEAD  # unassigned space belongs to the serial lead
        return writer == owner

    def permits_any(self, *, writers: Iterable[str], path: str) -> bool:
        """Whether any of ``writers`` (one agent acting under several identities) may write."""
        return any(self.permits(writer=w, path=path) for w in writers)

    def violations(self, *, writer: str, paths: list[str]) -> list[OwnershipViolation]:
        """Return ownership violations for a set of changed ``paths`` by ``writer``."""
        return self.violations_any(writers=(writer,), paths=paths)

    def violations_any(
        self, *, writers: Iterable[str], paths: list[str]
    ) -> list[OwnershipViolation]:
        """Violations for ``paths`` changed by an agent acting as any of ``writers``."""
        identities = tuple(writers)
        out: list[OwnershipViolation] = []
        for path in paths:
            if not self.permits_any(writers=identities, path=path):
                try:
                    shown = _normalize(path)
                except InvalidPathError:
                    shown = path
                out.append(
                    OwnershipViolation(
                        path=shown, writer="+".join(identities), owner=self.owner_of(path)
                    )
                )
        return out

    def write_set(self, writer: str) -> list[str]:
        """The (normalized, case-folded) paths ``writer`` owns, sorted."""
        return sorted(key for key, owner in self.owners.items() if owner == writer)

    def release(self, writer: str) -> list[str]:
        """Explicitly return every file ``writer`` owns to the lead's unassigned space.

        Used when a writer's slice is finished (its item is verified done): the files are no
        longer leased, so later work — including the serial lead — may touch them. Returns the
        released keys. This is an explicit transfer, never a silent one.
        """
        released = self.write_set(writer)
        for key in released:
            del self.owners[key]
        return released


def writer_for_item(item_id: str) -> str:
    """The writer id of the implementer that owns checklist item ``item_id``'s write-set."""
    return f"implementer-{item_id}"

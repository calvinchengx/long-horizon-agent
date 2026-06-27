"""The procedural skill library.

A skill is reusable, *self-verified* know-how (code + a description + machine-checkable
preconditions + provenance). Skills are admitted ONLY when ``verified`` is true (they passed the
deterministic test gate when learned), carry provenance + an expiry so a wrong lesson can't
fossilize, and are retrieved by description similarity. Per-repo namespacing + precondition checks
(done by the caller before execution) guard against mis-transfer across repos.
"""

from __future__ import annotations

from pydantic import BaseModel, Field

from lha.contracts.memory import Embedder, MemoryRecord, RetrievalHit
from lha.memory.semantic_memory import InMemorySemanticIndex


class Skill(BaseModel):
    """A learned, reusable capability."""

    id: str
    name: str
    description: str
    code: str
    preconditions: list[str] = Field(default_factory=list)
    namespace: str = "global"  # usually the repo id; "global" only after cross-repo validation
    provenance: str = ""  # mission/commit/failure it came from
    expires_at: str | None = None  # ISO date; re-validate after this
    verified: bool = False  # admitted only when it passed the deterministic test gate
    uses: int = 0


class SkillNotVerifiedError(ValueError):
    """Raised when attempting to admit a skill that has not passed the test gate."""


class InMemorySkillStore:
    """Stores verified skills and retrieves them by description similarity."""

    def __init__(self, embedder: Embedder) -> None:
        self._index = InMemorySemanticIndex(embedder)
        self._by_id: dict[str, Skill] = {}

    async def add(self, skill: Skill) -> None:
        if not skill.verified:
            raise SkillNotVerifiedError(
                f"refusing to admit unverified skill {skill.id!r} (must pass the test gate first)"
            )
        self._by_id[skill.id] = skill
        await self._index.add(
            [
                MemoryRecord(
                    id=skill.id,
                    kind="procedural",
                    text=skill.description,
                    metadata={"namespace": skill.namespace, "name": skill.name},
                )
            ]
        )

    async def find(self, query: str, *, k: int = 3, namespace: str | None = None) -> list[Skill]:
        hits: list[RetrievalHit] = await self._index.query(query, k=k * 2 if namespace else k)
        skills: list[Skill] = []
        for hit in hits:
            skill = self._by_id.get(hit.record.id)
            if skill is None:
                continue
            if namespace is not None and skill.namespace not in (namespace, "global"):
                continue
            skills.append(skill)
            if len(skills) >= k:
                break
        return skills

    def get(self, skill_id: str) -> Skill | None:
        return self._by_id.get(skill_id)

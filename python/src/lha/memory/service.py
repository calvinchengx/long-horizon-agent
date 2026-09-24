"""Tiered memory wired into the agent loop (``MissionMemory``).

Per cycle, BEFORE the lead's first turn, ``recall`` builds one bounded memory block for the prompt
(``lha.agent.prompt.render_memory_block``) from three tiers:

* **episodic** — past attempts/outcomes of THIS item (verdict, tools, files, what failed or what
  worked) plus the mission's most recent other outcomes, from ``episodic_events``;
* **procedural** — verified skills (what worked on an earlier item) relevant to this item;
* **semantic** — hybrid retrieval over distilled facts + progress notes (persisted), the anchor's
  recent decisions, and chunks of the checkout's tracked text files. Rankings from BM25 (lexical)
  and the dense channel (embedder cosine; pgvector on Postgres, stored vectors on SQLite) are fused
  with Reciprocal Rank Fusion, then reranked (``NoopReranker`` unless a cross-encoder is set).

AFTER the checkpoint, ``observe_cycle`` appends the outcome as an episodic event, stores a
progress note in semantic memory, admits a skill when the item was VERIFIED, and every
``consolidate_every`` recorded outcomes consolidates the new episodes into semantic facts
(``extractive``: deterministic, free; ``model``: ``lha.memory.consolidation.consolidate`` with the
metered model), soft-invalidating the progress notes they summarize.

Degradation (``lha.ops.degradation.decide_memory_mode``): when Postgres, pgvector or the embedder
is unavailable — at open or at any later call — the dense channel is dropped and retrieval runs on
BM25 + ``git grep`` instead; a ``memory_degraded`` event says why and the mission continues.
Memory never fails a cycle: every error is logged/recorded (``memory_error``) and the cycle
proceeds with less (or no) memory.
"""

from __future__ import annotations

import asyncio
import hashlib
import re
import subprocess
from dataclasses import dataclass, field
from datetime import UTC, date, datetime, timedelta
from pathlib import Path
from typing import Any, Protocol

import httpx

from lha.agent.prompt import render_memory_block
from lha.config import Settings
from lha.contracts.memory import Embedder, MemoryRecord, RetrievalHit, SemanticIndex
from lha.contracts.model import ModelProvider
from lha.contracts.state import ChecklistItem, SituationSnapshot
from lha.memory.consolidation import consolidate
from lha.memory.embeddings import (
    HashEmbedder,
    OllamaEmbedder,
    OllamaUnavailableError,
    PaddedEmbedder,
    SentenceTransformerEmbedder,
)
from lha.memory.hybrid import BM25Index, reciprocal_rank_fusion
from lha.memory.rerank import CrossEncoderReranker, NoopReranker
from lha.memory.semantic_memory import cosine
from lha.memory.skills import Skill
from lha.obs.events import TraceRecorder, get_logger
from lha.ops.degradation import DependencyStatus, Health, MemoryMode, decide_memory_mode
from lha.persistence.store import BACKEND_POSTGRES, MissionStore

EPISODE_KIND = "cycle_outcome"
CONSOLIDATION_KIND = "memory_consolidation"

#: Width of pgvector's ``semantic_memory.embedding`` column (``vector(1024)``). Narrower
#: embedders are zero-padded to it (``PaddedEmbedder``); wider ones run lexical-only.
PG_EMBEDDING_DIM = 1024
#: HTTP timeout of the Ollama embedder (probe and every embed call).
OLLAMA_TIMEOUT_S = 30.0
SKILL_TTL_DAYS = 90

_CHUNK_LINES = 40
_MAX_FILE_BYTES = 100_000
_GIT_TIMEOUT_S = 20.0
_GREP_TERMS = 6
_GREP_FILES = 10
_GREP_LINES_PER_FILE = 5
_EPISODE_SCAN = 500
_MEMORY_SCAN = 500
_LINE_CAP = 700
_STOPWORDS = frozenset(
    (  # noqa: SIM905  (one readable line of words beats a 30-line list literal)
        "the and for with that this from into when then than must should make add use are was "
        "not all any can its has have will each item test tests file files"
    ).split()
)
_TERM = re.compile(r"[a-z0-9_]{3,}")
_WEIGHTS = (0.4, 0.2, 0.4)  # episodic, skills, semantic shares of the prompt budget

_log = get_logger("lha.memory")


@dataclass
class MemoryConfig:
    prompt_budget_chars: int = 4000
    episodic_k: int = 4
    semantic_k: int = 4
    skills_k: int = 2
    consolidate_every: int = 5
    consolidation: str = "extractive"  # "extractive" | "model"
    index_repo_files: bool = True
    max_repo_files: int = 400

    @classmethod
    def from_settings(cls, settings: Settings) -> MemoryConfig:
        return cls(
            prompt_budget_chars=settings.memory_prompt_budget_chars,
            episodic_k=settings.memory_episodic_k,
            semantic_k=settings.memory_semantic_k,
            skills_k=settings.memory_skills_k,
            consolidate_every=settings.memory_consolidate_every,
            consolidation=settings.memory_consolidation,
            index_repo_files=settings.memory_index_repo_files,
            max_repo_files=settings.memory_max_repo_files,
        )


@dataclass
class CycleObservation:
    """What one committed cycle did (input to ``observe_cycle``)."""

    mission_id: str
    cycle_id: str
    item_id: str
    item_description: str
    verdict: str
    verified: bool
    status: str
    attempts: int
    head_sha: str
    before_head: str = ""
    failure: str = ""
    done_summary: str = ""
    tools: list[str] = field(default_factory=list)


class CycleMemory(Protocol):
    """What ``AgentLoop`` needs from a memory plane."""

    async def recall(
        self, *, mission_id: str, cycle_id: str, item: ChecklistItem, snapshot: SituationSnapshot
    ) -> str: ...

    async def observe_cycle(self, obs: CycleObservation) -> None: ...


class _Reranker(Protocol):
    async def rerank(
        self, query: str, hits: list[RetrievalHit], *, k: int | None = None
    ) -> list[RetrievalHit]: ...


# --- SQLite dense channel -----------------------------------------------------------------------
class SqliteSemanticIndex(SemanticIndex):
    """``SemanticIndex`` over the SQLite store: vectors persisted as JSON, exact cosine at query
    time, gated by embedder model+version (vectors are never compared across versions)."""

    def __init__(self, store: Any, embedder: Embedder, *, mission_id: str) -> None:
        self._store = store
        self._embedder = embedder
        self._mission_id = mission_id

    async def add(self, records: list[MemoryRecord]) -> None:
        if not records:
            return
        vectors = await self._embedder.embed([r.text for r in records])
        for vector in vectors:
            if len(vector) != self._embedder.dim:
                raise ValueError(
                    f"embedder {self._embedder.name!r} returned dim={len(vector)}; "
                    f"declared {self._embedder.dim}"
                )
        await self._store.put_memory(
            self._mission_id,
            records,
            vectors=vectors,
            embedding_model=self._embedder.name,
            embedding_version=self._embedder.version,
        )

    async def query(self, text: str, *, k: int = 5) -> list[RetrievalHit]:
        rows = await self._store.memory_vectors(
            self._mission_id,
            embedding_model=self._embedder.name,
            embedding_version=self._embedder.version,
        )
        if not rows:
            return []
        query_vec = (await self._embedder.embed([text]))[0]
        hits = [RetrievalHit(record=r, score=cosine(query_vec, v)) for r, v in rows]
        hits.sort(key=lambda hit: hit.score, reverse=True)
        return [hit for hit in hits if hit.score > 0][:k]


# --- helpers ------------------------------------------------------------------------------------
def _terms(text: str) -> list[str]:
    seen: dict[str, None] = {}
    for term in _TERM.findall(text.lower()):
        if term not in _STOPWORDS:
            seen.setdefault(term, None)
    return list(seen)


def _one_line(text: str, cap: int) -> str:
    flat = " ".join(text.split())
    return flat if len(flat) <= cap else flat[: cap - 3] + "..."


def _git(workdir: Path, *args: str) -> subprocess.CompletedProcess[bytes]:
    return subprocess.run(
        ["git", *args], cwd=workdir, capture_output=True, timeout=_GIT_TIMEOUT_S, check=False
    )


def _changed_files(workdir: Path, before: str, after: str) -> list[str]:
    if not before or not after or before == after:
        return []
    proc = _git(workdir, "diff", "--name-only", before, after)
    if proc.returncode != 0:
        return []
    names = proc.stdout.decode("utf-8", "replace").splitlines()
    return [n for n in names if n and not n.startswith(".lha/")][:20]


def _repo_chunks(workdir: Path, max_files: int) -> list[MemoryRecord]:
    """Tracked text files (``git ls-files``, excluding ``.lha/``) split into line chunks."""
    proc = _git(workdir, "ls-files", "-z")
    if proc.returncode != 0:
        return []
    paths = [p for p in proc.stdout.decode("utf-8", "replace").split("\0") if p]
    paths = sorted(p for p in paths if not p.startswith(".lha/"))[:max_files]
    records: list[MemoryRecord] = []
    for rel in paths:
        path = workdir / rel
        try:
            if not path.is_file() or path.stat().st_size > _MAX_FILE_BYTES:
                continue
            data = path.read_bytes()
        except OSError:
            continue
        if b"\0" in data:
            continue
        lines = data.decode("utf-8", "replace").splitlines()
        for start in range(0, len(lines), _CHUNK_LINES):
            body = "\n".join(lines[start : start + _CHUNK_LINES]).strip()
            if not body:
                continue
            end = min(len(lines), start + _CHUNK_LINES)
            records.append(
                MemoryRecord(
                    id=f"file:{rel}:{start + 1}",
                    kind="file",
                    text=f"{rel}:{start + 1}-{end}\n{body}",
                    metadata={"path": rel},
                )
            )
    return records


def _git_grep(workdir: Path, terms: list[str]) -> list[MemoryRecord]:
    """``git grep`` for the query terms: one record per matching file, most hits first."""
    if not terms:
        return []
    args = ["grep", "-n", "-I", "-i", "-F", "--no-color"]
    for term in terms[:_GREP_TERMS]:
        args += ["-e", term]
    args += ["--", ".", ":(exclude).lha"]
    proc = _git(workdir, *args)
    if proc.returncode not in (0, 1):
        raise RuntimeError(f"git grep failed: {proc.stderr.decode('utf-8', 'replace')[:200]}")
    hits: dict[str, list[str]] = {}
    for line in proc.stdout.decode("utf-8", "replace").splitlines():
        path, sep, rest = line.partition(":")
        if sep:
            hits.setdefault(path, []).append(rest)
    ranked = sorted(hits.items(), key=lambda kv: (-len(kv[1]), kv[0]))[:_GREP_FILES]
    return [
        MemoryRecord(
            id=f"grep:{path}",
            kind="grep",
            text=f"{path} ({len(lines)} matching lines)\n"
            + "\n".join(_one_line(ln, 160) for ln in lines[:_GREP_LINES_PER_FILE]),
            metadata={"path": path},
        )
        for path, lines in ranked
    ]


def _render_episode(payload: dict[str, Any], cycle_id: str) -> str:
    parts = [
        f"{cycle_id} [{payload.get('item_id', '?')}] {payload.get('verdict') or 'no verdict'}"
        f" (attempt {payload.get('attempts', '?')}, status {payload.get('status', '?')})"
    ]
    tools = payload.get("tools") or []
    if tools:
        parts.append("tools: " + ", ".join(dict.fromkeys(str(t) for t in tools)))
    files = payload.get("files") or []
    if files:
        parts.append("files: " + ", ".join(str(f) for f in files[:8]))
    if payload.get("verified"):
        if payload.get("summary"):
            parts.append("what worked: " + _one_line(str(payload["summary"]), 300))
    else:
        if payload.get("summary"):
            parts.append("claimed: " + _one_line(str(payload["summary"]), 160))
        if payload.get("failure"):
            parts.append("failure: " + _one_line(str(payload["failure"]), 300))
    return "- " + "; ".join(parts)


# --- the service --------------------------------------------------------------------------------
class MissionMemory:
    """Episodic + semantic + procedural memory for one run (see the module docstring)."""

    def __init__(
        self,
        *,
        store: MissionStore,
        workdir: str | Path,
        config: MemoryConfig,
        mode: MemoryMode,
        embedder: Embedder | None = None,
        reranker: _Reranker | None = None,
        model: ModelProvider | None = None,
        recorder: TraceRecorder | None = None,
        namespace: str | None = None,
    ) -> None:
        self.store = store
        self.workdir = Path(workdir).resolve()
        self.config = config
        self.mode = mode
        self.embedder = embedder if mode.dense else None
        # Closed by ``close`` even after a degradation dropped it (an Ollama HTTP client).
        self._embedder_owned = embedder
        self.reranker: _Reranker = reranker or NoopReranker()
        self.model = model
        self.recorder = recorder
        self.namespace = namespace or str(self.workdir)
        self.errors = 0
        self._dense: dict[str, SemanticIndex] = {}
        self._vectors: dict[str, list[float]] = {}

    # --- events -------------------------------------------------------------------------
    def _emit(self, kind: str, mission_id: str, cycle_id: str = "", **data: object) -> None:
        if self.recorder is not None:
            self.recorder.record(kind, mission_id=mission_id, cycle_id=cycle_id, **data)
        else:
            _log.info(kind, mission_id=mission_id, cycle_id=cycle_id, **data)

    def _error(self, where: str, mission_id: str, cycle_id: str, exc: BaseException) -> None:
        self.errors += 1
        _log.warning("memory_error", where=where, error=f"{type(exc).__name__}: {exc}")
        self._emit(
            "memory_error", mission_id, cycle_id, where=where, error=f"{type(exc).__name__}: {exc}"
        )

    def _degrade(self, mission_id: str, cycle_id: str, dep: str, exc: BaseException) -> None:
        """Drop the dense channel for the rest of the run (lexical + git grep)."""
        self.mode = decide_memory_mode(
            [DependencyStatus(dep, Health.DOWN, f"{type(exc).__name__}: {exc}")]
        )
        self.embedder = None
        self._dense.clear()
        _log.warning("memory_degraded", reason=self.mode.reason)
        self._emit("memory_degraded", mission_id, cycle_id, reason=self.mode.reason)

    # --- dense plumbing -----------------------------------------------------------------
    def _dense_index(self, mission_id: str) -> SemanticIndex | None:
        if self.embedder is None:
            return None
        index = self._dense.get(mission_id)
        if index is None:
            if self.store.backend == BACKEND_POSTGRES:
                from lha.memory.semantic_pg import PgSemanticIndex

                index = PgSemanticIndex(
                    dsn=str(getattr(self.store, "dsn", "")),
                    embedder=self.embedder,
                    mission_id=mission_id,
                )
            else:
                index = SqliteSemanticIndex(self.store, self.embedder, mission_id=mission_id)
            self._dense[mission_id] = index
        return index

    async def _embed(self, texts: list[str]) -> list[list[float]]:
        assert self.embedder is not None
        keys = [hashlib.sha1(t.encode("utf-8")).hexdigest() for t in texts]
        missing = [i for i, key in enumerate(keys) if key not in self._vectors]
        if missing:
            vectors = await self.embedder.embed([texts[i] for i in missing])
            for i, vector in zip(missing, vectors, strict=True):
                self._vectors[keys[i]] = vector
        return [self._vectors[key] for key in keys]

    async def _dense_rank(self, query: str, records: list[MemoryRecord]) -> list[str]:
        if self.embedder is None or not records:
            return []
        vectors = await self._embed([query, *[r.text for r in records]])
        scored = [(r.id, cosine(vectors[0], v)) for r, v in zip(records, vectors[1:], strict=True)]
        scored.sort(key=lambda pair: pair[1], reverse=True)
        return [doc_id for doc_id, score in scored if score > 0]

    async def _store_records(self, mission_id: str, records: list[MemoryRecord]) -> None:
        index = self._dense_index(mission_id)
        if index is not None:
            try:
                await index.add(records)
                return
            except Exception as exc:
                self._degrade(mission_id, "", self._dense_dep, exc)
        await self.store.put_memory(mission_id, records)

    @property
    def _dense_dep(self) -> str:
        """The dependency blamed when the dense channel fails."""
        return "pgvector" if self.store.backend == BACKEND_POSTGRES else "embeddings"

    # --- recall -------------------------------------------------------------------------
    async def recall(
        self, *, mission_id: str, cycle_id: str, item: ChecklistItem, snapshot: SituationSnapshot
    ) -> str:
        """The bounded memory block for this cycle's prompt ('' if nothing relevant)."""
        sections: list[tuple[str, list[str]]] = []
        for title, fetch in (
            ("Earlier attempts and outcomes", self._episodic_lines),
            ("Skills that worked before", self._skill_lines),
            ("Related facts, progress, decisions and code", self._semantic_lines),
        ):
            try:
                lines = await fetch(mission_id, cycle_id, item, snapshot)
            except Exception as exc:
                self._error(f"recall:{title}", mission_id, cycle_id, exc)
                lines = []
            sections.append((title, lines))
        block = render_memory_block(
            sections, budget_chars=self.config.prompt_budget_chars, weights=_WEIGHTS
        )
        _log.debug(
            "memory_recall",
            mission_id=mission_id,
            cycle_id=cycle_id,
            mode=self.mode.label,
            chars=len(block),
            counts=[len(lines) for _, lines in sections],
        )
        return block

    async def _episodic_lines(
        self, mission_id: str, _cycle: str, item: ChecklistItem, _snap: SituationSnapshot
    ) -> list[str]:
        k = self.config.episodic_k
        if k <= 0:
            return []
        events = await self.store.list_events(
            mission_id, kinds=(EPISODE_KIND,), limit=_EPISODE_SCAN
        )
        same = [e for e in events if e.payload.get("item_id") == item.id][-k:]
        others = [e for e in events if e.payload.get("item_id") != item.id][-max(1, k // 2) :]
        # This item's history first (newest last), then the mission's latest other outcomes.
        return [_render_episode(e.payload, e.cycle_id) for e in [*same, *others]]

    async def _skill_lines(
        self, _mission: str, _cycle: str, item: ChecklistItem, _snap: SituationSnapshot
    ) -> list[str]:
        k = self.config.skills_k
        if k <= 0:
            return []
        skills = await self.find_skills(item.description, k=k, mission_id=_mission)
        return [
            f"- {s.name}: {_one_line(s.description, 240)}\n  what worked: {_one_line(s.code, 400)}"
            for s in skills
        ]

    async def find_skills(
        self, query: str, *, k: int = 3, mission_id: str | None = None
    ) -> list[Skill]:
        """Verified, unexpired skills in this namespace (or global), most relevant first."""
        today = date.today().isoformat()
        skills = [
            s
            for s in await self.store.list_skills(self.namespace)
            if s.verified and (not s.expires_at or s.expires_at >= today)
        ]
        if not skills:
            return []
        records = [
            MemoryRecord(id=s.id, kind="procedural", text=f"{s.name}\n{s.description}")
            for s in skills
        ]
        bm25 = BM25Index()
        bm25.add(records)
        rankings = [[doc_id for doc_id, _ in bm25.query(query, k=len(records))]]
        if self.mode.dense:
            try:
                rankings.append(await self._dense_rank(query, records))
            except Exception as exc:
                self._degrade(mission_id or "", "", self._dense_dep, exc)
        by_id = {s.id: s for s in skills}
        fused = reciprocal_rank_fusion([r for r in rankings if r])
        return [by_id[doc_id] for doc_id, _ in fused if doc_id in by_id][:k]

    async def _semantic_lines(
        self, mission_id: str, cycle_id: str, item: ChecklistItem, snapshot: SituationSnapshot
    ) -> list[str]:
        k = self.config.semantic_k
        if k <= 0:
            return []
        query = item.description
        stored = await self.store.list_memory(mission_id, limit=_MEMORY_SCAN)
        local: list[MemoryRecord] = [
            MemoryRecord(
                id=f"decision:{i}",
                kind="decision",
                text=f"{d.decision} (why: {d.rationale})",
            )
            for i, d in enumerate(snapshot.last_decisions)
        ]
        if self.config.index_repo_files:
            local += await asyncio.to_thread(_repo_chunks, self.workdir, self.config.max_repo_files)
        grep: list[MemoryRecord] = []
        if self.mode.git_grep:
            grep = await asyncio.to_thread(_git_grep, self.workdir, _terms(query))

        # This item's own progress notes are already in the episodic section.
        stored = [
            r for r in stored if not (r.kind == "progress" and r.metadata.get("item_id") == item.id)
        ]
        candidates = {r.id: r for r in [*stored, *local, *grep]}
        if not candidates:
            return []
        depth = max(3 * k, 12)
        bm25 = BM25Index()
        bm25.add(list(candidates.values()))
        rankings = [[doc_id for doc_id, _ in bm25.query(query, k=depth)]]
        if grep:
            rankings.append([r.id for r in grep])
        if self.mode.dense:
            try:
                dense = [(await self._dense_rank(query, local))[:depth]]
                index = self._dense_index(mission_id)
                if index is not None and stored:
                    found = await index.query(query, k=depth)
                    dense.append([h.record.id for h in found if h.record.id in candidates])
                rankings.extend(dense)
            except Exception as exc:
                self._degrade(mission_id, cycle_id, self._dense_dep, exc)
        if self.mode.git_grep and not grep:  # degraded just now: add the git-grep channel
            grep = await asyncio.to_thread(_git_grep, self.workdir, _terms(query))
            candidates.update((r.id, r) for r in grep)
            if grep:
                rankings.append([r.id for r in grep])
        fused = reciprocal_rank_fusion([r for r in rankings if r])[:depth]
        hits = [RetrievalHit(record=candidates[doc_id], score=score) for doc_id, score in fused]
        hits = await self.reranker.rerank(query, hits, k=k)
        return [f"- ({h.record.kind}) {h.record.text[:_LINE_CAP]}" for h in hits]

    # --- observe ------------------------------------------------------------------------
    async def observe_cycle(self, obs: CycleObservation) -> None:
        """Record a committed cycle into every tier (never raises)."""
        try:
            files = await asyncio.to_thread(
                _changed_files, self.workdir, obs.before_head, obs.head_sha
            )
            payload: dict[str, Any] = {
                "item_id": obs.item_id,
                "description": obs.item_description,
                "verdict": obs.verdict,
                "verified": obs.verified,
                "status": obs.status,
                "attempts": obs.attempts,
                "tools": obs.tools[:30],
                "files": files,
                "summary": obs.done_summary[:1000],
                "failure": obs.failure[-1500:],
                "head_sha": obs.head_sha,
            }
            await self.store.append_event(
                obs.mission_id, cycle_id=obs.cycle_id, kind=EPISODE_KIND, payload=payload
            )
            note = _render_episode(payload, obs.cycle_id)[2:]
            await self._store_records(
                obs.mission_id,
                [
                    MemoryRecord(
                        id=f"progress:{obs.mission_id}:{obs.cycle_id}:{obs.item_id}",
                        kind="progress",
                        text=f"{obs.item_description}: {note}",
                        metadata={"item_id": obs.item_id, "cycle_id": obs.cycle_id},
                    )
                ],
            )
            if obs.verified:
                await self.store_skill(obs, files)
        except Exception as exc:
            self._error("observe", obs.mission_id, obs.cycle_id, exc)
        try:
            await self.maybe_consolidate(obs.mission_id, obs.cycle_id)
        except Exception as exc:
            self._error("consolidate", obs.mission_id, obs.cycle_id, exc)

    async def store_skill(self, obs: CycleObservation, files: list[str]) -> Skill:
        """Admit what worked on a VERIFIED item as a reusable skill (namespaced to the repo)."""
        digest = hashlib.sha1(f"{self.namespace}\x1f{obs.item_description}".encode()).hexdigest()
        tools = ", ".join(dict.fromkeys(obs.tools)) or "(none)"
        skill = Skill(
            id=f"skill_{digest[:20]}",
            name=_one_line(obs.item_description, 80),
            description=obs.item_description,
            code=(
                f"summary: {obs.done_summary or '(none)'}\n"
                f"tools: {tools}\n"
                f"files: {', '.join(files) or '(none)'}"
            ),
            namespace=self.namespace,
            provenance=f"{obs.mission_id}:{obs.cycle_id}:{obs.head_sha}",
            expires_at=(date.today() + timedelta(days=SKILL_TTL_DAYS)).isoformat(),
            verified=True,
        )
        await self.store.put_skill(skill)
        self._emit("skill_stored", obs.mission_id, obs.cycle_id, skill_id=skill.id)
        return skill

    # --- consolidation ------------------------------------------------------------------
    async def maybe_consolidate(self, mission_id: str, cycle_id: str) -> int:
        """Consolidate when ``consolidate_every`` new episodes exist. Returns facts stored."""
        every = self.config.consolidate_every
        if every <= 0:
            return 0
        marks = await self.store.list_events(mission_id, kinds=(CONSOLIDATION_KIND,), limit=1)
        watermark = int(marks[-1].payload.get("upto_id", 0)) if marks else 0
        episodes = await self.store.list_events(
            mission_id, kinds=(EPISODE_KIND,), after_id=watermark, limit=_EPISODE_SCAN
        )
        if len(episodes) < every:
            return 0
        return await self.consolidate_now(mission_id, cycle_id, episodes)

    async def consolidate_now(self, mission_id: str, cycle_id: str, episodes: list[Any]) -> int:
        upto = max(e.id for e in episodes)
        failed_batches = 0
        if self.config.consolidation == "model" and self.model is not None:
            lines = [_render_episode(e.payload, e.cycle_id)[2:] for e in episodes]
            result = await consolidate(model=self.model, episodes=lines, mission_id=mission_id)
            texts = [fact.text for fact in result.facts]
            failed_batches = result.failed_batches
            mode = "model"
            facts = [
                MemoryRecord(
                    id=f"fact:{mission_id}:{upto}:{i}",
                    kind="fact",
                    text=text,
                    metadata={"source": "consolidation:model"},
                )
                for i, text in enumerate(texts)
            ]
        else:
            # One fact per item, rebuilt from ALL of that item's episodes so far and upserted
            # under a stable id: a newer consolidation replaces the older fact for that item.
            items = {str(e.payload.get("item_id", "?")) for e in episodes}
            history = await self.store.list_events(
                mission_id, kinds=(EPISODE_KIND,), limit=_EPISODE_SCAN
            )
            relevant = [
                e for e in history if e.id <= upto and str(e.payload.get("item_id")) in items
            ]
            facts = [
                MemoryRecord(
                    id=f"fact:{mission_id}:item:{item_id}",
                    kind="fact",
                    text=text,
                    metadata={"source": "consolidation:extractive", "item_id": item_id},
                )
                for item_id, text in _extractive_facts(relevant)
            ]
            mode = "extractive"
        if facts:
            await self._store_records(mission_id, facts)
        # The progress notes these facts summarize no longer need to compete for retrieval.
        await self.store.invalidate_memory(
            [
                f"progress:{mission_id}:{e.cycle_id}:{e.payload.get('item_id')}"
                for e in episodes
                if e.payload.get("item_id")
            ]
        )
        await self.store.append_event(
            mission_id,
            cycle_id=cycle_id,
            kind=CONSOLIDATION_KIND,
            payload={
                "upto_id": upto,
                "episodes": len(episodes),
                "facts": len(facts),
                "mode": mode,
                "failed_batches": failed_batches,
                "at": datetime.now(UTC).isoformat(),
            },
        )
        self._emit(
            "memory_consolidated",
            mission_id,
            cycle_id,
            episodes=len(episodes),
            facts=len(facts),
            mode=mode,
        )
        return len(facts)

    async def close(self) -> None:
        for index in self._dense.values():
            close = getattr(index, "close", None)
            if close is not None:
                await close()
        self._dense.clear()
        aclose = getattr(self._embedder_owned, "aclose", None)
        self._embedder_owned = None
        if aclose is not None:
            await aclose()


def _extractive_facts(episodes: list[Any]) -> list[tuple[str, str]]:
    """Deterministic consolidation: ``(item_id, fact)`` per item from its episodes (no model)."""
    by_item: dict[str, list[Any]] = {}
    for episode in episodes:
        by_item.setdefault(str(episode.payload.get("item_id", "?")), []).append(episode)
    facts: list[tuple[str, str]] = []
    for item_id, group in by_item.items():
        last = group[-1].payload
        desc = _one_line(str(last.get("description", "")), 160)
        wins = [e for e in group if e.payload.get("verified")]
        fails = [e for e in group if not e.payload.get("verified")]
        files = sorted({str(f) for e in group for f in (e.payload.get("files") or [])})[:8]
        if wins:
            win = wins[-1].payload
            fact = (
                f"[{item_id}] {desc}: verified in {wins[-1].cycle_id} after "
                f"{win.get('attempts', len(group))} attempt(s)"
            )
            if win.get("summary"):
                fact += f"; what worked: {_one_line(str(win['summary']), 240)}"
        else:
            fact = f"[{item_id}] {desc}: {len(fails)} failed attempt(s) so far"
        if fails:
            failure = _one_line(str(fails[-1].payload.get("failure", "")), 200)
            if failure:
                fact += f"; last failure: {failure}"
        if files:
            fact += f"; files: {', '.join(files)}"
        facts.append((item_id, fact))
    return facts


# --- construction -------------------------------------------------------------------------------
#: ``LHA_MEMORY_EMBEDDING_MODEL`` when it is empty, per embedder.
DEFAULT_EMBEDDING_MODELS = {"ollama": "nomic-embed-text", "sentence_transformers": "BAAI/bge-m3"}


def embedding_model(settings: Settings) -> str:
    """The configured embedding model, or the embedder's default."""
    configured = settings.memory_embedding_model.strip()
    return configured or DEFAULT_EMBEDDING_MODELS.get(settings.memory_embedder, "")


async def _build_embedder(
    settings: Settings, backend: str, *, transport: httpx.AsyncBaseTransport | None = None
) -> tuple[Embedder | None, DependencyStatus]:
    choice = settings.memory_embedder
    if choice == "none":
        return None, DependencyStatus(
            "embeddings", Health.DEGRADED, "disabled (LHA_MEMORY_EMBEDDER=none)"
        )
    if choice == "ollama":
        try:
            ollama = await OllamaEmbedder.connect(
                model=embedding_model(settings),
                base_url=settings.ollama_base_url,
                transport=transport,
                timeout_s=OLLAMA_TIMEOUT_S,
            )
        except OllamaUnavailableError as exc:
            return None, DependencyStatus("embeddings", Health.DOWN, str(exc))
        return ollama, DependencyStatus("embeddings", Health.OK)  # closed by MissionMemory.close
    if choice == "sentence_transformers":
        try:
            embedder: Embedder = SentenceTransformerEmbedder(embedding_model(settings))
        except Exception as exc:  # ModuleNotFoundError without the `embeddings` extra
            return None, DependencyStatus(
                "embeddings",
                Health.DOWN,
                f"sentence-transformers unavailable ({type(exc).__name__}: {exc}); "
                "install the `embeddings` extra",
            )
        return embedder, DependencyStatus("embeddings", Health.OK)
    dim = PG_EMBEDDING_DIM if backend == BACKEND_POSTGRES else 256
    return HashEmbedder(dim=dim), DependencyStatus("embeddings", Health.OK)


async def open_mission_memory(
    settings: Settings,
    *,
    store: MissionStore,
    workdir: str | Path,
    mission_id: str,
    model: ModelProvider | None = None,
    recorder: TraceRecorder | None = None,
    embedder_transport: httpx.AsyncBaseTransport | None = None,
) -> MissionMemory | None:
    """The run's memory plane per settings (``None`` when ``memory_enabled`` is false).

    ``embedder_transport`` is a test seam for the Ollama embedder's HTTP client.
    """
    if not settings.memory_enabled:
        return None
    statuses: list[DependencyStatus] = []
    if store.degraded_reason:
        statuses.append(DependencyStatus("postgres", Health.DOWN, store.degraded_reason))
    embedder, embed_status = await _build_embedder(
        settings, store.backend, transport=embedder_transport
    )
    statuses.append(embed_status)
    if store.backend == BACKEND_POSTGRES and embedder is not None:
        if embedder.dim < PG_EMBEDDING_DIM:
            # e.g. nomic-embed-text (768): zero-padding keeps cosine similarity unchanged.
            embedder = PaddedEmbedder(embedder, PG_EMBEDDING_DIM)
        if embedder.dim != PG_EMBEDDING_DIM:
            statuses.append(
                DependencyStatus(
                    "pgvector",
                    Health.DOWN,
                    f"embedder dim {embedder.dim} > vector({PG_EMBEDDING_DIM}) column",
                )
            )
        else:
            try:
                import pgvector  # noqa: F401  (the dense channel needs the adapter)

                statuses.append(DependencyStatus("pgvector", Health.OK))
            except ImportError as exc:
                statuses.append(DependencyStatus("pgvector", Health.DOWN, str(exc)))
    mode = decide_memory_mode(statuses)
    reranker: _Reranker = NoopReranker()
    if settings.memory_rerank == "cross_encoder":
        try:
            reranker = CrossEncoderReranker()
        except Exception as exc:
            _log.warning("memory_rerank_unavailable", error=f"{type(exc).__name__}: {exc}")
    memory = MissionMemory(
        store=store,
        workdir=workdir,
        config=MemoryConfig.from_settings(settings),
        mode=mode,
        embedder=embedder,
        reranker=reranker,
        model=model,
        recorder=recorder,
    )
    if not mode.dense:
        _log.warning("memory_degraded", reason=mode.reason)
        memory._emit("memory_degraded", mission_id, reason=mode.reason)
    return memory

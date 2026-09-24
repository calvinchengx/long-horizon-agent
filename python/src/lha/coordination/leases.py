"""Lease granting: an implementer asks for a file outside its write-set in the middle of a wave.

An implementer that discovers it needs a file it does not own calls the ``request_lease`` tool
(``lha.execution.tools.leases``). The request goes to a ``LeaseBroker``, which decides it against
the COMMITTED ownership map and checklist (``decide_lease``):

* **granted** when the file is unowned (unassigned space, which belongs to the lead, who never
  writes during a wave), already the requester's, or owned by a *finished* writer (an
  implementer whose item is ``done`` or ``split``: its lease has ended);
* **refused** when another writer whose item is still open owns it, when it is a shared file
  (build manifests, lockfiles, ``__init__.py``, ...: only the lead ever writes those), when it is
  harness-owned (``.lha/``, ``.git/``), or when the path is invalid.

Every decision is committed to the mission anchor by itself (``commit_anchor_update``: only
``.lha/`` files are in that commit) as a ``lease`` event; a grant also rewrites
``.lha/ownership.json`` in the same commit, so the lease survives a crash and a resume, and the
integrator's git-layer ownership check (which reads the committed map) accepts the leased file.
Decisions are serialized by an in-process lock plus the checkout's ``flock``
(``lha.state.locks``), so two implementers can never both be granted one file. A granted file is
released with the rest of the writer's files when its item is done.
"""

from __future__ import annotations

import asyncio
from collections.abc import Awaitable, Callable, Iterable

from pydantic import BaseModel

from lha.contracts.state import Checklist, Checkpoint, EventRecord
from lha.coordination.enforcement import effective_ownership
from lha.coordination.ownership import (
    LEAD,
    FileOwnershipMap,
    InvalidPathError,
    LeaseRequest,
    _normalize,
    is_shared,
    writer_for_item,
)
from lha.state.locks import workdir_flock
from lha.state.mission_anchor import GitMissionAnchor

LEASE_EVENT = "lease"
_HARNESS_DIRS = (".lha", ".git")
_MAX_REASON = 500

#: ``(path, reason) -> (granted, message for the agent)`` — what the ``request_lease`` tool calls.
LeaseHandler = Callable[[str, str], Awaitable[tuple[bool, str]]]


class LeaseDecision(BaseModel):
    """The outcome of one ``LeaseRequest`` (persisted as a ``lease`` event)."""

    writer: str
    path: str
    reason: str
    granted: bool
    previous_owner: str | None = None
    why: str = ""

    def message(self) -> str:
        """What the requesting agent is told."""
        if self.granted:
            return (
                f"lease granted: you may now write {self.path!r} ({self.why}). It is yours until "
                "your item is done."
            )
        return (
            f"lease refused for {self.path!r}: {self.why}. Do not write it; finish what you can "
            "within your write-set and say in your summary what still needs this file."
        )


def finished_writers(checklist: Checklist) -> set[str]:
    """Implementer ids whose item is finished (``done`` or ``split``): their leases have ended."""
    return {writer_for_item(i.id) for i in checklist.items if i.status in ("done", "split")}


def decide_lease(
    ownership: FileOwnershipMap, request: LeaseRequest, *, finished: Iterable[str] = ()
) -> LeaseDecision:
    """Grant or refuse ``request`` against ``ownership`` (pure; nothing is changed)."""
    reason = request.reason.strip()[:_MAX_REASON]

    def refuse(path: str, why: str, owner: str | None = None) -> LeaseDecision:
        return LeaseDecision(
            writer=request.writer,
            path=path,
            reason=reason,
            granted=False,
            previous_owner=owner,
            why=why,
        )

    try:
        norm = _normalize(request.path)
    except InvalidPathError as exc:
        return refuse(request.path, str(exc))
    if request.writer == LEAD:
        return refuse(norm, "the lead needs no lease (it owns all unassigned space)")
    if norm.split("/", 1)[0].casefold() in _HARNESS_DIRS:
        return refuse(norm, "harness-owned files (.lha/, .git/) are never leased")
    if is_shared(norm):
        return refuse(
            norm,
            "it is a shared file (build manifest, lockfile, package entry point, ...) "
            "that only the lead writes",
        )
    owner = ownership.owner_of(norm)
    if owner == request.writer:
        return LeaseDecision(
            writer=request.writer,
            path=norm,
            reason=reason,
            granted=True,
            previous_owner=owner,
            why="you already own it",
        )
    if owner is None or owner == LEAD:
        why = "it was unassigned"
    elif owner in set(finished):
        why = f"its owner {owner!r} has finished"
    else:
        return refuse(norm, f"it is owned by {owner!r}, whose item is still open", owner)
    return LeaseDecision(
        writer=request.writer,
        path=norm,
        reason=reason,
        granted=True,
        previous_owner=owner,
        why=why,
    )


def lease_event(decision: LeaseDecision, cycle_id: str) -> EventRecord:
    return EventRecord(kind=LEASE_EVENT, cycle_id=cycle_id, payload=decision.model_dump())


class LeaseBroker:
    """Decides lease requests against the committed anchor and commits each decision.

    ``anchor``: the mission anchor. In ``orchestrate`` pass the run's own anchor instance, so a
    grant and the ownership release the orchestrator staged are written together; in a durable
    activity a fresh instance is fine.
    """

    def __init__(self, anchor: GitMissionAnchor) -> None:
        self._anchor = anchor
        self._lock = asyncio.Lock()

    async def request(self, request: LeaseRequest, *, cycle_id: str) -> LeaseDecision:
        anchor = self._anchor
        async with self._lock, workdir_flock(anchor.workdir):
            checklist = await anchor.read_checklist()
            finished = finished_writers(checklist)
            current = effective_ownership(await anchor.read_ownership(), finished)
            decision = decide_lease(current, request, finished=finished)
            changed = decision.granted and decision.previous_owner != decision.writer
            if changed:
                current.reassign(decision.path, decision.writer)
                anchor.stage_ownership(current)
            verb = "granted" if decision.granted else "refused"
            await anchor.commit_anchor_update(
                Checkpoint(
                    cycle_id=cycle_id,
                    progress_summary=(
                        f"- {cycle_id} lease {verb}: {decision.path} to {decision.writer}"
                        if changed
                        else ""
                    ),
                    checklist=checklist,
                    events=[lease_event(decision, cycle_id)],
                    commit_message=f"lha: lease {verb}: {decision.path} ({decision.writer})",
                )
            )
            return decision


def lease_handler(
    broker: LeaseBroker,
    *,
    writer: str,
    cycle_id: str,
    ownership: FileOwnershipMap,
    log: list[LeaseDecision] | None = None,
) -> LeaseHandler:
    """The ``request_lease`` callback for one implementer.

    A grant is also applied to ``ownership``, the in-memory map the implementer's
    ``OwnershipGuard`` and its git-layer check use, so the leased file becomes writable at once.
    Every decision is appended to ``log``.
    """

    async def handle(path: str, reason: str) -> tuple[bool, str]:
        decision = await broker.request(
            LeaseRequest(writer=writer, path=path, reason=reason), cycle_id=cycle_id
        )
        if decision.granted and ownership.owner_of(decision.path) != writer:
            ownership.reassign(decision.path, writer)
        if log is not None:
            log.append(decision)
        return decision.granted, decision.message()

    return handle

"""Assembling a lead engineer from settings — shared by every run path.

The local runner, the durable cycle activity and the multi-agent orchestrator must build the lead
the SAME way: the configured sandbox image with its egress allow-list, the tool set (plus the
web tools under the egress policy and the Rule of Two, see ``lha.execution.tools.toolset``), the
human gate, a verifier that runs operator-defined trusted checks outside the sandbox,
operator-protected harness paths, and the replanner. Keeping it in one place is what stops the three paths from drifting apart.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from lha.agent.loop import AgentLoop
from lha.agents.replanner import Replanner
from lha.config import Settings
from lha.contracts.hitl import HITLGate
from lha.contracts.model import ModelProvider
from lha.contracts.sandbox import SandboxSession
from lha.contracts.tools import Tool, ToolDispatcher
from lha.contracts.verify import Verifier
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.factory import open_sandbox
from lha.execution.tools import with_decision_tool
from lha.execution.tools.toolset import build_run_dispatcher, run_tools
from lha.obs.events import TraceRecorder
from lha.state.mission_anchor import GitMissionAnchor
from lha.verify.trusted import CommandTrustedRunner, TrustedAwareVerifier
from lha.verify.verifier import DeterministicVerifier

if TYPE_CHECKING:
    from lha.memory.service import CycleMemory


async def open_lead_sandbox(settings: Settings, workdir: str) -> SandboxSession:
    """The sandbox from settings: kind, image and the sandbox egress allow-list."""
    return await open_sandbox(
        settings.sandbox,
        workdir=workdir,
        allow_unsafe_local=settings.allow_unsafe_local,
        image=settings.sandbox_image,
        egress_hosts=settings.sandbox_egress_hosts(),
    )


def lead_tools(settings: Settings) -> list[Tool]:
    """The default tools, plus the web tools (``fetch_url``, and ``web_search`` when configured)
    under the egress policy of ``LHA_WEB_ALLOW_HOSTS`` when that allow-list is set."""
    return run_tools(settings)


def lead_dispatcher(
    settings: Settings, gate: HITLGate | None = None, *, allow_egress: bool | None = None
) -> AllowListDispatcher:
    """Every lead tool, mutating allowed; egress only for the web tools' allow-list.

    Raises ``RuleOfTwoViolation`` when the run would hold the lethal trifecta (web tools plus a
    ``local`` sandbox or ``LHA_PRIVATE_DATA``); ``allow_egress=False`` drops the web tools.
    """
    return build_run_dispatcher(settings, allow_mutating=True, allow_egress=allow_egress, gate=gate)


def lead_verifier(workdir: str) -> Verifier:
    """Sandbox checks in the sandbox; operator ``trusted:`` checks on the trusted runner."""
    return TrustedAwareVerifier(DeterministicVerifier(), CommandTrustedRunner(), workdir)


def build_lead_loop(
    settings: Settings,
    *,
    model: ModelProvider,
    anchor: GitMissionAnchor,
    workdir: str,
    gate: HITLGate | None = None,
    recorder: TraceRecorder | None = None,
    memory: CycleMemory | None = None,
    allow_egress: bool | None = None,
    dispatcher: ToolDispatcher | None = None,
) -> AgentLoop:
    """The lead's ``AgentLoop`` with every large-mission capability wired from settings.

    ``memory``: the run's tiered memory (``lha.persistence.services.open_run_services``), or
    ``None`` for no memory. The lead's tools include ``record_decision``, bound to ``anchor``:
    decisions are queued there and chained into ``.lha/decisions.ndjson`` by the cycle's
    checkpoint commit. ``dispatcher`` replaces ``lead_dispatcher(settings, gate)`` (the
    orchestrator passes that dispatcher wrapped in an ownership guard); ``record_decision`` is
    added to it either way.
    """
    return AgentLoop(
        model=model,
        dispatcher=with_decision_tool(
            dispatcher or lead_dispatcher(settings, gate, allow_egress=allow_egress), anchor
        ),
        verifier=lead_verifier(workdir),
        anchor=anchor,
        recorder=recorder,
        max_turns=settings.max_turns_per_cycle,
        trusted_checks=settings.trusted_check_commands(),
        harness_globs=tuple(settings.harness_globs()),
        replanner=Replanner(model) if settings.max_replans > 0 else None,
        max_replans=settings.max_replans,
        max_split_depth=settings.max_split_depth,
        memory=memory,
    )

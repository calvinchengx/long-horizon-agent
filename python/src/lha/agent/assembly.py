"""Assembling a lead engineer from settings — shared by every run path.

The local runner, the durable cycle activity and the multi-agent orchestrator must build the lead
the SAME way: the configured sandbox image with its egress allow-list, the tool set (plus the
``fetch_url`` reference tool when web hosts are allowed), the human gate, a verifier that runs
operator-defined trusted checks outside the sandbox, operator-protected harness paths, and the
replanner. Keeping it in one place is what stops the three paths from drifting apart.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from lha.agent.loop import AgentLoop
from lha.agents.replanner import Replanner
from lha.config import Settings
from lha.contracts.hitl import HITLGate
from lha.contracts.model import ModelProvider
from lha.contracts.sandbox import SandboxSession
from lha.contracts.tools import Tool
from lha.contracts.verify import Verifier
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.factory import open_sandbox
from lha.execution.tools import FetchUrlTool, default_local_tools
from lha.obs.events import TraceRecorder
from lha.safety.egress import EgressPolicy
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
    """The default tools, plus ``fetch_url`` restricted to ``LHA_WEB_ALLOW_HOSTS`` when set."""
    tools = default_local_tools()
    hosts = settings.web_hosts()
    if hosts:
        tools.append(FetchUrlTool(egress_policy=EgressPolicy(allow_hosts=set(hosts))))
    return tools


def lead_dispatcher(settings: Settings, gate: HITLGate | None = None) -> AllowListDispatcher:
    """Every lead tool, mutating allowed; egress only for the reference tool's allow-list."""
    return AllowListDispatcher.for_tools(
        lead_tools(settings),
        allow_mutating=True,
        allow_egress=bool(settings.web_hosts()),
        gate=gate,
    )


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
) -> AgentLoop:
    """The lead's ``AgentLoop`` with every large-mission capability wired from settings.

    ``memory``: the run's tiered memory (``lha.persistence.services.open_run_services``), or
    ``None`` for no memory.
    """
    return AgentLoop(
        model=model,
        dispatcher=lead_dispatcher(settings, gate),
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

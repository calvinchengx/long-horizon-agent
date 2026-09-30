"""Assembling a lead engineer from settings — shared by every run path.

The local runner, the durable cycle activity and the multi-agent orchestrator must build the lead
the SAME way: the configured sandbox image with its egress allow-list, the tool set (plus the
web tools under the egress policy and the Rule of Two, see ``lha.execution.tools.toolset``), the
human gate, a verifier that runs operator-defined trusted checks outside the sandbox and
re-runs failing checks to quarantine proven flakes,
operator-protected harness paths, the replanner, and the lead engine (the built-in turn loop, or
one ``claude -p`` session per cycle). Keeping it in one place is what stops the three paths from
drifting apart.
"""

from __future__ import annotations

from typing import TYPE_CHECKING

from lha.agent.claude_code_engine import ClaudeCodeEngine
from lha.agent.code_map import RipwireCodeMap
from lha.agent.loop import AgentLoop
from lha.agents.replanner import Replanner
from lha.config import Settings, get_settings
from lha.contracts.hitl import HITLGate
from lha.contracts.model import ModelProvider
from lha.contracts.sandbox import SandboxSession
from lha.contracts.system_one import SystemOneModel
from lha.contracts.tools import Tool, ToolDispatcher
from lha.contracts.verify import Verifier
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.factory import open_sandbox
from lha.execution.tools import with_decision_tool
from lha.execution.tools.toolset import build_run_dispatcher, run_tools
from lha.model.claude_code import DEFAULT_MODEL
from lha.obs.events import TraceRecorder
from lha.state.mission_anchor import GitMissionAnchor
from lha.systemone.build import build_stall_triage
from lha.verify.flaky_quarantine import FlakyRetryVerifier
from lha.verify.mutation_gate import MutationGateVerifier
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
        memory=settings.sandbox_memory,
        cpus=settings.sandbox_cpus,
        tmp_size=settings.sandbox_tmp_size,
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


def lead_verifier(workdir: str, settings: Settings | None = None) -> Verifier:
    """Sandbox checks in the sandbox; operator ``trusted:`` checks on the trusted runner; a
    failing gating check re-run up to ``LHA_FLAKY_RETRIES`` times, and proven flakes quarantined
    (``lha.verify.flaky_quarantine``); with ``LHA_MUTATION_CHECK``, a green verdict must also
    survive the mutation gate (``lha.verify.mutation_gate``)."""
    settings = settings or get_settings()
    verifier: Verifier = FlakyRetryVerifier(
        TrustedAwareVerifier(
            DeterministicVerifier(),
            CommandTrustedRunner(env_allow=settings.trusted_check_env_names()),
            workdir,
        ),
        retries=settings.flaky_retries,
        workdir=workdir,
    )
    if settings.mutation_check.strip():
        verifier = MutationGateVerifier(
            verifier,
            command=settings.mutation_check,
            workdir=workdir,
            timeout_s=settings.mutation_timeout_s,
        )
    return verifier


def lead_engine(settings: Settings, *, guarded: bool = False) -> ClaudeCodeEngine | None:
    """The ``claude_code`` lead engine from settings, or ``None`` for the built-in loop.

    Native Claude Code tools act on the host with no sandbox, so they need ``sandbox=local`` and
    ``allow_unsafe_local``, and they cannot run under a ``guarded`` dispatcher (the orchestrator's
    ownership guard only sees calls that go through LHA's tools).
    """
    if settings.lead_engine != "claude_code":
        return None
    if settings.claude_code_tools == "native":
        if settings.sandbox != "local" or not settings.allow_unsafe_local:
            raise ValueError(
                "LHA_CLAUDE_CODE_TOOLS=native runs Claude Code's own tools on the host with no "
                "isolation: it needs LHA_SANDBOX=local and LHA_ALLOW_UNSAFE_LOCAL=true "
                "(or use the default LHA_CLAUDE_CODE_TOOLS=lha)"
            )
        if guarded:
            raise ValueError(
                "LHA_CLAUDE_CODE_TOOLS=native bypasses the orchestrator's file-ownership guard; "
                "use LHA_CLAUDE_CODE_TOOLS=lha with the multi-agent organization"
            )
    uses_cli_model = settings.model_backend == "claude_code"
    stub_default = Settings.model_fields["model_name"].default
    model = (
        settings.model_name
        if uses_cli_model and settings.model_name != stub_default
        else DEFAULT_MODEL
    )
    return ClaudeCodeEngine(
        binary=settings.claude_code_bin,
        model=model,
        tools=settings.claude_code_tools,
        max_budget_usd=settings.claude_code_max_budget_usd,
        timeout_s=settings.claude_code_timeout_s,
    )


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
    system_one: SystemOneModel | None = None,
) -> AgentLoop:
    """The lead's ``AgentLoop`` with every large-mission capability wired from settings.

    ``memory``: the run's tiered memory (``lha.persistence.services.open_run_services``), or
    ``None`` for no memory. The lead's tools include ``record_decision``, bound to ``anchor``:
    decisions are queued there and chained into ``.lha/decisions.ndjson`` by the cycle's
    checkpoint commit. ``dispatcher`` replaces ``lead_dispatcher(settings, gate)`` (the
    orchestrator passes that dispatcher wrapped in an ownership guard); ``record_decision`` is
    added to it either way. ``system_one`` (the run services' System One model) enables stall
    triage when ``system_one_triage`` is on.
    """
    return AgentLoop(
        model=model,
        dispatcher=with_decision_tool(
            dispatcher or lead_dispatcher(settings, gate, allow_egress=allow_egress), anchor
        ),
        verifier=lead_verifier(workdir, settings),
        anchor=anchor,
        recorder=recorder,
        max_turns=settings.max_turns_per_cycle,
        trusted_checks=settings.trusted_check_commands(),
        harness_globs=tuple(settings.harness_globs()),
        replanner=Replanner(model) if settings.max_replans > 0 else None,
        max_replans=settings.max_replans,
        max_split_depth=settings.max_split_depth,
        memory=memory,
        engine=lead_engine(settings, guarded=dispatcher is not None),
        triage=build_stall_triage(settings, system_one),
        code_map=(
            RipwireCodeMap(
                token_budget=settings.code_map_token_budget, timeout_s=settings.code_map_timeout_s
            )
            if settings.code_map == "ripwire"
            else None
        ),
    )

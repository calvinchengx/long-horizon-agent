"""The cycle-start code map: the ripwire command, the prompt section, failures, and the loop."""

from __future__ import annotations

import shutil
import sys
from pathlib import Path

import pytest

from lha.agent.assembly import build_lead_loop
from lha.agent.code_map import (
    TRACE_CHARS,
    RipwireCodeMap,
    code_map_argv,
    code_map_query,
    trace_argv,
)
from lha.agent.loop import AgentLoop
from lha.agent.prompt import CODE_MAP_HARD_CAP, CODE_MAP_HEADER, render_code_map
from lha.config import Settings
from lha.contracts.model import ModelMessage, TurnResult
from lha.contracts.sandbox import ExecResult
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.tools import ToolContext
from lha.contracts.verify import Check
from lha.execution.dispatcher import AllowListDispatcher
from lha.execution.sandbox_local import LocalSandbox
from lha.execution.tools import default_local_tools
from lha.model.stub import StubModel
from lha.state.mission_anchor import GitMissionAnchor
from lha.verify.verifier import DeterministicVerifier

ITEM = ChecklistItem(id="01", description='add a farewell() next to greet(); "quote" & -dash')


class _Session:
    """A sandbox session whose ``exec`` returns a fixed result (and records the argv)."""

    def __init__(self, result: ExecResult | Exception) -> None:
        self.result = result
        self.argv: list[str] = []

    async def exec(self, argv: list[str], *, timeout_s: int = 600, **_: object) -> ExecResult:
        self.argv = argv
        if isinstance(self.result, Exception):
            raise self.result
        return self.result


def test_the_description_is_one_argument_never_a_shell_string() -> None:
    assert code_map_argv(ITEM, 1500) == [
        "ripwire",
        ".",
        '--pack-task=add a farewell() next to greet(); "quote" & -dash',
        "--token-budget=1500",
    ]


def test_render_code_map_adds_the_header_and_caps_the_size() -> None:
    assert render_code_map("  \n ") == ""
    assert render_code_map(" <ctx/> ") == f"{CODE_MAP_HEADER}\n<ctx/>"
    long = render_code_map("x" * (CODE_MAP_HARD_CAP * 2))
    assert len(long) <= CODE_MAP_HARD_CAP and long.endswith(" ...[clipped]")


async def test_a_successful_run_is_rendered_with_its_facts() -> None:
    session = _Session(ExecResult(exit_code=0, stdout="<ctx task='x'/>"))
    text, info = await RipwireCodeMap(token_budget=900).render(session, ITEM)  # type: ignore[arg-type]
    assert text == f"{CODE_MAP_HEADER}\n<ctx task='x'/>"
    assert session.argv[-1] == "--token-budget=900"
    assert info["ok"] is True and info["bytes"] == len("<ctx task='x'/>")


@pytest.mark.parametrize(
    "result",
    [
        ExecResult(exit_code=127, stderr="sh: ripwire: not found"),
        ExecResult(exit_code=0, stdout="partial", timed_out=True),
        RuntimeError("sandbox gone"),
    ],
)
async def test_any_failure_means_no_map_not_a_failed_cycle(result: ExecResult | Exception) -> None:
    text, info = await RipwireCodeMap().render(_Session(result), ITEM)  # type: ignore[arg-type]
    assert text == "" and info["ok"] is False and info["error"]


class _Recording(StubModel):
    def __init__(self, script: list[TurnResult]) -> None:
        super().__init__(script=script)
        self.prompts: list[list[ModelMessage]] = []

    async def complete(self, messages, **kwargs):  # type: ignore[no-untyped-def, override]
        self.prompts.append(list(messages))
        return await super().complete(messages, **kwargs)


class _FakeRipwire(RipwireCodeMap):
    async def render(self, session, item):  # type: ignore[no-untyped-def, override]
        return render_code_map(f"<ctx task='{item.description}'/>"), {"ok": True}


async def test_the_lead_sees_the_map_after_memory(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)
    await anchor.initialize(
        title="T", description="D", items=Checklist(items=[ChecklistItem(id="01", description="x")])
    )
    session = await LocalSandbox().open(workdir=str(tmp_path))
    model = _Recording([TurnResult(text='{"done": true}', stop_reason="end_turn")])
    loop = AgentLoop(
        model=model,
        dispatcher=AllowListDispatcher.for_tools(default_local_tools(), allow_mutating=True),
        verifier=DeterministicVerifier(default_timeout_s=60),
        anchor=anchor,
        max_turns=1,
        code_map=_FakeRipwire(),
    )
    ctx = ToolContext(mission_id="m", session=session)
    passing = Check(name="ok", command=[sys.executable, "-c", "pass"])
    await loop.run_cycle(ctx=ctx, mission_id="m", cycle_id="c1", checks=[passing])
    user = model.prompts[0][1].content
    assert f"{CODE_MAP_HEADER}\n<ctx task='x'/>" in user
    assert user.index(CODE_MAP_HEADER) < user.index("Recent commits:")


def test_the_setting_turns_the_map_on(tmp_path: Path) -> None:
    anchor = GitMissionAnchor(tmp_path)

    def loop(**overrides: object) -> AgentLoop:
        settings = Settings(_env_file=None, **overrides)  # type: ignore[call-arg]
        return build_lead_loop(settings, model=StubModel(), anchor=anchor, workdir=str(tmp_path))

    assert loop()._code_map is None
    on = loop(code_map="ripwire", code_map_token_budget=700)._code_map
    assert isinstance(on, RipwireCodeMap) and on.token_budget == 700


@pytest.mark.skipif(shutil.which("ripwire") is None, reason="needs ripwire on PATH")
async def test_real_ripwire_maps_a_repo_and_writes_nothing(tmp_path: Path) -> None:
    import subprocess

    (tmp_path / "greeter.py").write_text('def greet(name):\n    return f"Hello, {name}!"\n')
    subprocess.run(["git", "init", "-q", "-b", "main"], cwd=tmp_path, check=True)
    subprocess.run(["git", "add", "-A"], cwd=tmp_path, check=True)
    subprocess.run(
        ["git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"],
        cwd=tmp_path,
        check=True,
    )
    session = await LocalSandbox().open(workdir=str(tmp_path))
    text, info = await RipwireCodeMap(token_budget=800).render(session, ITEM)
    assert info["ok"] is True and text.startswith(CODE_MAP_HEADER) and "greet" in text
    status = subprocess.run(
        ["git", "status", "--porcelain", "--ignored"], cwd=tmp_path, capture_output=True, text=True
    )
    assert status.stdout == ""  # no cache or notes files left in the workspace


# --- witness-aware queries and trace-first retries -----------------------------------------

FAILING = ChecklistItem(
    id="02",
    description="mask gitlab tokens",
    witnesses=["pytest:tests/test_redact.py::test_gitlab"],
    last_failure="x" * 5000 + "\ntests/test_redact.py:6: AssertionError",
)


def test_the_query_carries_the_acceptance_checks() -> None:
    assert code_map_query(ITEM) == ITEM.description  # no witnesses: the description alone
    assert code_map_query(FAILING) == (
        "mask gitlab tokens\nAcceptance checks: pytest:tests/test_redact.py::test_gitlab"
    )


class _Scripted:
    """A session answering the trace script and the task command differently."""

    def __init__(self, trace: ExecResult, task: ExecResult) -> None:
        self.trace, self.task = trace, task
        self.calls: list[tuple[list[str], dict[str, str] | None]] = []

    async def exec(self, argv, *, timeout_s=600, env=None, **_):  # type: ignore[no-untyped-def]
        self.calls.append((argv, env))
        return self.trace if argv[0] == "sh" else self.task


async def test_a_retry_maps_from_the_failure_report() -> None:
    session = _Scripted(
        trace=ExecResult(exit_code=0, stdout='<ctx><d p="src/redact.py:3"/></ctx>'),
        task=ExecResult(exit_code=0, stdout="<ctx/>"),
    )
    text, info = await RipwireCodeMap(token_budget=900).render(session, FAILING)  # type: ignore[arg-type]
    assert 'p="src/redact.py:3"' in text and info["mode"] == "trace"
    ((argv, env),) = session.calls  # the task query was not needed
    assert argv == trace_argv() and env is not None
    assert (
        env["LHA_TRACE"] == FAILING.last_failure[-TRACE_CHARS:] and env["LHA_TRACE_BUDGET"] == "900"
    )
    assert FAILING.last_failure[-TRACE_CHARS:] not in " ".join(argv)  # the report is data, not argv


async def test_a_trace_that_finds_nothing_falls_back_to_the_task_query() -> None:
    session = _Scripted(
        trace=ExecResult(exit_code=0, stdout="<ctx/>"),
        task=ExecResult(exit_code=0, stdout='<ctx><d p="src/a.py:1"/></ctx>'),
    )
    text, info = await RipwireCodeMap().render(session, FAILING)  # type: ignore[arg-type]
    assert [c[0][0] for c in session.calls] == ["sh", "ripwire"]
    assert info["mode"] == "task" and info["fell_back"] is True and 'p="src/a.py:1"' in text


@pytest.mark.skipif(shutil.which("ripwire") is None, reason="needs ripwire on PATH")
async def test_real_ripwire_finds_the_code_from_a_pytest_failure(tmp_path: Path) -> None:
    import subprocess

    (tmp_path / "redact.py").write_text("def redact_text(text):\n    return text\n")
    (tmp_path / "other.py").write_text("def unrelated():\n    return 1\n")
    (tmp_path / "test_redact.py").write_text(
        "from redact import redact_text\n\n\ndef test_gitlab():\n"
        '    assert "glpat-" not in redact_text("glpat-abc")\n'
    )
    for args in (["init", "-q", "-b", "main"], ["add", "-A"]):
        subprocess.run(["git", *args], cwd=tmp_path, check=True)
    subprocess.run(
        ["git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"],
        cwd=tmp_path,
        check=True,
    )
    report = (
        "_____ test_gitlab _____\n\n    def test_gitlab():\n"
        '>       assert "glpat-" not in redact_text("glpat-abc")\n'
        "E       AssertionError\n\ntest_redact.py:5: AssertionError\n"
    )
    item = ChecklistItem(id="01", description="mask tokens", last_failure=report)
    session = await LocalSandbox().open(workdir=str(tmp_path))
    text, info = await RipwireCodeMap(token_budget=800).render(session, item)
    assert info["mode"] == "trace" and "redact" in text

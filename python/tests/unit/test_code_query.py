"""The ``code_query`` tool: argv per kind, validation, answers and failures, registration."""

from __future__ import annotations

import shutil
import subprocess
from pathlib import Path

import pytest

from lha.config import Settings
from lha.contracts.sandbox import ExecResult
from lha.contracts.tools import ToolContext
from lha.execution.sandbox_local import LocalSandbox
from lha.execution.tools.code_query import (
    MAX_ANSWER_CHARS,
    MAX_SYMBOL_CHARS,
    CodeQueryTool,
    clip_answer,
    code_query_argv,
)
from lha.execution.tools.toolset import build_run_dispatcher, run_tools


def _settings(**overrides: object) -> Settings:
    base: dict[str, object] = {"sandbox": "local", "allow_unsafe_local": True}
    return Settings(_env_file=None, **{**base, **overrides})  # type: ignore[arg-type]


@pytest.mark.parametrize(
    ("kind", "target", "argv"),
    [
        (
            "find",
            " refuse long URLs ",
            ["ripwire", ".", "--for=refuse long URLs", "--token-budget=900"],
        ),
        ("definition", "redact_text", ["ripwire", ".", "--expand=redact_text", "--top-k=0"]),
        ("callers", "redact_text", ["ripwire", ".", "--callers=redact_text"]),
        ("uses", "a.py:Thing", ["ripwire", ".", "--uses=a.py:Thing"]),
        ("impact", "-rf", ["ripwire", ".", "--impact=-rf"]),  # one argument: never a flag
    ],
)
def test_each_kind_maps_to_one_ripwire_flag(kind: str, target: str, argv: list[str]) -> None:
    assert code_query_argv(kind, target, 900) == argv


@pytest.mark.parametrize(
    ("kind", "target", "error"),
    [
        ("grep", "x", "'kind' must be one of"),
        ("callers", "   ", "must not be empty"),
        ("callers", "x" * (MAX_SYMBOL_CHARS + 1), "longer than"),
        ("callers", "a\nb", "single line"),
    ],
)
def test_unusable_questions_are_refused(kind: str, target: str, error: str) -> None:
    with pytest.raises(ValueError, match=error):
        code_query_argv(kind, target, 900)
    assert code_query_argv("find", "two\nlines", 900)[2] == "--for=two\nlines"  # find may


class _Session:
    def __init__(self, result: ExecResult) -> None:
        self.result = result
        self.argv: list[str] = []

    async def exec(self, argv: list[str], **_: object) -> ExecResult:
        self.argv = argv
        return self.result


def _ctx(result: ExecResult) -> tuple[ToolContext, _Session]:
    session = _Session(result)
    return ToolContext(mission_id="m", session=session), session  # type: ignore[arg-type]


async def test_an_answer_is_returned_and_a_bad_question_never_runs() -> None:
    ctx, session = _ctx(ExecResult(exit_code=0, stdout="<callers of='f'/>"))
    result = await CodeQueryTool().run({"kind": "callers", "target": "f"}, ctx)
    assert result.ok and result.content == "<callers of='f'/>"
    assert session.argv == ["ripwire", ".", "--callers=f"]
    bad = await CodeQueryTool().run(
        {"kind": "callers", "target": ""}, _ctx(ExecResult(exit_code=0))[0]
    )
    assert not bad.ok and "empty" in (bad.error or "")


@pytest.mark.parametrize(
    ("result", "error"),
    [
        (ExecResult(exit_code=127, stderr="sh: ripwire: not found"), "needs ripwire"),
        (
            ExecResult(exit_code=1, stderr="ripwire: --callers symbol not found: f"),
            "symbol not found",
        ),
        (ExecResult(exit_code=124, timed_out=True), "timed out"),
    ],
)
async def test_failures_come_back_as_tool_errors(result: ExecResult, error: str) -> None:
    out = await CodeQueryTool().run({"kind": "callers", "target": "f"}, _ctx(result)[0])
    assert not out.ok and error in (out.error or "")


def test_long_answers_are_clipped() -> None:
    assert clip_answer("x" * 10) == "x" * 10
    clipped = clip_answer("x" * (MAX_ANSWER_CHARS + 5))
    assert clipped.endswith("…[truncated]") and len(clipped) == MAX_ANSWER_CHARS + len(
        "\n…[truncated]"
    )


def test_the_setting_registers_a_read_only_tool_for_every_role() -> None:
    names = [t.spec.name for t in run_tools(_settings(), web=False)]
    assert "code_query" not in names
    on = _settings(code_query=True, code_query_token_budget=700)
    tool = next(t for t in run_tools(on, web=False) if t.spec.name == "code_query")
    assert isinstance(tool, CodeQueryTool) and tool.token_budget == 700
    assert not tool.spec.mutating and not tool.spec.egress and not tool.spec.untrusted_input
    reviewer = build_run_dispatcher(on, allow_mutating=False)  # researchers and the reviewer
    assert "code_query" in [s.name for s in reviewer.specs()]


@pytest.mark.skipif(shutil.which("ripwire") is None, reason="needs ripwire on PATH")
async def test_real_ripwire_answers_callers(tmp_path: Path) -> None:
    (tmp_path / "lib.py").write_text(
        "def helper():\n    return 1\n\n\ndef caller():\n    return helper()\n"
    )
    for args in (["init", "-q", "-b", "main"], ["add", "-A"]):
        subprocess.run(["git", *args], cwd=tmp_path, check=True)
    subprocess.run(
        ["git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "init"],
        cwd=tmp_path,
        check=True,
    )
    session = await LocalSandbox().open(workdir=str(tmp_path))
    ctx = ToolContext(mission_id="m", session=session)
    found = await CodeQueryTool().run({"kind": "callers", "target": "helper"}, ctx)
    assert found.ok and 'n="caller"' in found.content
    missing = await CodeQueryTool().run({"kind": "callers", "target": "nope_xyz"}, ctx)
    assert not missing.ok and "not found" in (missing.error or "")

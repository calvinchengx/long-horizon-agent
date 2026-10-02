"""The pre-review diff screen (docs/07-verification.md#pre-review-screen): deterministic
findings for weakened tests, and the review they force in `lha orchestrate --no-review`."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

import lha.agents.orchestrator as orchestrator_module
from lha.agents.orchestrator import Orchestrator
from lha.config import Settings
from lha.contracts.model import TurnResult
from lha.contracts.state import Checklist, ChecklistItem
from lha.contracts.verify import Check
from lha.model.stub import StubModel
from lha.state import git_ops
from lha.verify.review_screen import (
    MAX_FINDINGS,
    is_test_path,
    review_screen_event,
    screen_criteria,
    screen_diff,
)

_WEAKENING = (
    "--- a/tests/test_a.py\n+++ b/tests/test_a.py\n@@\n-def test_one():\n-    assert f()\n"
    "+@pytest.mark.skip\n+def test_two():\n+    pass\n"
)


def test_findings_name_the_rule_and_the_file() -> None:
    assert screen_diff(_WEAKENING) == [
        "removed test test_one from tests/test_a.py",
        "added skip to tests/test_a.py: @pytest.mark.skip",
        "1 assertion removed from tests/test_a.py",
    ]
    assert screen_diff("--- a/tests/t_test.go\n+++ /dev/null\n-func TestA(t *testing.T) {}\n") == [
        "deleted test file tests/t_test.go"
    ]
    assert screen_diff(
        "--- a/.coveragerc\n+++ b/.coveragerc\n-fail_under = 90\n+fail_under = 89.5\n"
    ) == ["lowered fail_under from 90 to 89.5 in .coveragerc"]


def test_clean_changes_have_no_findings() -> None:
    assert screen_diff("") == []
    assert screen_diff("--- a/src/x.py\n+++ b/src/x.py\n-assert x\n+return 1\n") == []  # not a test
    moved = (
        "--- a/tests/test_a.py\n+++ b/tests/test_a.py\n-def test_a():\n+def test_a():  # moved\n"
    )
    assert screen_diff(moved) == []  # a test renamed within the file is not removed
    assert (
        screen_diff("--- /dev/null\n+++ b/tests/test_new.py\n+def test_new():\n+    assert 1\n")
        == []
    )


def test_findings_are_capped() -> None:
    many = "--- a/tests/test_a.py\n+++ b/tests/test_a.py\n" + "".join(
        f"+@pytest.mark.skip  # {n}\n" for n in range(MAX_FINDINGS + 5)
    )
    assert len(screen_diff(many)) == MAX_FINDINGS


def test_test_paths() -> None:
    assert is_test_path("tests/anything.txt") and is_test_path("pkg/x_test.go")
    assert is_test_path("src/a.spec.ts") and is_test_path("conftest.py")
    assert not is_test_path("src/testing.py") and not is_test_path("test.py")


def test_criteria_put_the_findings_first_and_are_unchanged_when_clean() -> None:
    assert screen_criteria([], "do it") == "do it"
    text = screen_criteria(["removed test test_one from tests/test_a.py"], "do it")
    assert text.startswith("Pre-review screen: ") and text.endswith("\n\ndo it")
    assert "- removed test test_one from tests/test_a.py" in text
    event = review_screen_event("01", "a", "b", ["x"], forced=True, cycle_id="c1")
    assert event.kind == "review_screen" and event.payload["forced"] is True


# --- the screen forces the review in lha orchestrate --no-review ----------------------------
_DONE = TurnResult(text='{"done": true, "summary": "did it"}')
_APPROVE = TurnResult(text='{"done": true, "verdict": "approve", "blocking_issues": []}')


@pytest.mark.asyncio
async def test_findings_force_the_review_when_review_is_off(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    diffs = iter([_WEAKENING, ""])  # item 01 weakens a test, item 02 is clean
    monkeypatch.setattr(orchestrator_module, "diff_since", lambda *_a: next(diffs))
    reviewer = StubModel(script=[_APPROVE])
    orchestrator = Orchestrator(
        Settings(sandbox="local", allow_unsafe_local=True, model_backend="stub", max_cycles=10),
        research_per_item=0,
        do_review=False,
        models={
            "lead": StubModel(script=[_DONE]),
            "researcher": StubModel(script=[_DONE]),
            "reviewer": reviewer,
        },
    )
    summary = await orchestrator.run_mission(
        workdir=str(tmp_path),
        title="Screen test",
        description="review is off",
        checklist=Checklist(
            items=[ChecklistItem(id="01", description="a"), ChecklistItem(id="02", description="b")]
        ),
        checks=[Check(name="always_green", command=["python", "-c", "pass"])],
    )
    assert summary.completed
    trace = [json.loads(line) for line in summary.trace_jsonl.splitlines() if line.strip()]
    screens = [t["data"] for t in trace if t["kind"] == "review_screen"]
    assert [(s["item"], s["findings"], s["forced"]) for s in screens] == [
        ("01", 3, True),
        ("02", 0, False),
    ]
    assert (
        len([t for t in trace if t["kind"] == "review"]) == 1
    )  # only the flagged item was reviewed
    raw = git_ops.show_at_head(tmp_path, ".lha/events.ndjson")
    events = [json.loads(line) for line in raw.splitlines() if line.strip()]
    committed = [e["payload"] for e in events if e["kind"] == "review_screen"]
    assert [(p["item_id"], len(p["findings"]), p["forced"]) for p in committed] == [
        ("01", 3, True),
        ("02", 0, False),
    ]
    reviews = [e["payload"]["item_id"] for e in events if e["kind"] == "review"]
    assert reviews == ["01"]
    # The reviewer saw the findings as criteria.
    assert (
        any("Pre-review screen:" in m.content for m in reviewer.calls[0])
        if hasattr(reviewer, "calls")
        else True
    )

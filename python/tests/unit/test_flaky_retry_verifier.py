"""Flaky-check quarantine in the verifier: re-run on failure, quarantine only with evidence, and
never let a quarantined check make an item green."""

from __future__ import annotations

import asyncio
import json
import subprocess
import sys
from pathlib import Path
from typing import Any

import pytest

from lha.agent.assembly import lead_verifier
from lha.agent.runner import run_mission_local
from lha.config import Settings
from lha.contracts.model import TurnResult
from lha.contracts.sandbox import SandboxSession
from lha.contracts.state import Checklist, ChecklistItem, EventRecord
from lha.contracts.verify import Check, CheckResult, VerificationResult
from lha.model.stub import StubModel
from lha.verify.flaky_quarantine import (
    QUARANTINE_EVENT,
    QUARANTINED_FAILURE_EVENT,
    FlakyRetryVerifier,
    committed_quarantine,
    tree_revision,
)


class _Scripted:
    """A verifier whose checks pass/fail/time out per a script: one outcome per run."""

    def __init__(self, outcomes: dict[str, list[str]]) -> None:
        self._outcomes = {name: list(seq) for name, seq in outcomes.items()}
        self.runs: list[str] = []

    async def verify(self, session: SandboxSession, checks: list[Check]) -> VerificationResult:
        results = []
        for check in checks:
            self.runs.append(check.name)
            seq = self._outcomes[check.name]
            outcome = seq.pop(0) if len(seq) > 1 else seq[0]
            results.append(
                CheckResult(
                    name=check.name,
                    passed=outcome == "pass",
                    exit_code=0 if outcome == "pass" else 1,
                    gating=check.gating,
                    timed_out=outcome == "timeout",
                    output_tail=outcome,
                )
            )
        return VerificationResult.from_results(results)


def _checks(*names: str) -> list[Check]:
    return [Check(name=n, command=["x"]) for n in names]


_SESSION: SandboxSession = None  # type: ignore[assignment]  # never touched by _Scripted


@pytest.mark.asyncio
async def test_consistent_failure_gates_and_is_not_quarantined() -> None:
    inner = _Scripted({"t": ["fail"]})
    verifier = FlakyRetryVerifier(inner, retries=2)
    result = await verifier.verify(_SESSION, _checks("t"))
    assert result.verdict == "failed"
    assert inner.runs == ["t", "t", "t"]  # first run + 2 re-runs
    assert not verifier.quarantine.is_flaky("t")
    assert verifier.drain_events() == []


@pytest.mark.asyncio
async def test_pass_on_rerun_quarantines_with_an_event_and_never_gates() -> None:
    inner = _Scripted({"flaky": ["fail", "pass"], "solid": ["pass"]})
    verifier = FlakyRetryVerifier(inner, retries=2)
    result = await verifier.verify(_SESSION, _checks("flaky", "solid"))
    assert inner.runs == ["flaky", "solid", "flaky"]  # stops at the first passing re-run
    assert result.verdict == "passed"  # the solid check gates; the flaky one is advisory
    flaky = next(r for r in result.results if r.name == "flaky")
    assert flaky.passed and not flaky.gating and "quarantined" in flaky.output_tail
    [event] = verifier.drain_events()
    assert event.kind == QUARANTINE_EVENT
    assert event.payload["check"] == "flaky"
    assert (event.payload["passes"], event.payload["fails"]) == (1, 1)
    assert verifier.drain_events() == []  # drained once


@pytest.mark.asyncio
async def test_a_quarantined_check_alone_never_makes_an_item_green() -> None:
    inner = _Scripted({"flaky": ["fail", "pass", "pass"]})
    verifier = FlakyRetryVerifier(inner, retries=1)
    first = await verifier.verify(_SESSION, _checks("flaky"))
    assert first.verdict == "unverified" and not first.all_green  # fail -> pass: quarantined
    second = await verifier.verify(_SESSION, _checks("flaky"))
    assert second.verdict == "unverified"  # a clean pass of a quarantined check is not evidence
    assert second.results[0].passed and not second.results[0].gating


@pytest.mark.asyncio
async def test_a_quarantined_check_failing_every_attempt_still_gates() -> None:
    inner = _Scripted({"flaky": ["fail", "pass", "fail"], "solid": ["pass"]})
    verifier = FlakyRetryVerifier(inner, retries=1)
    await verifier.verify(_SESSION, _checks("flaky", "solid"))  # quarantined here
    verifier.drain_events()
    result = await verifier.verify(_SESSION, _checks("flaky", "solid"))
    assert result.verdict == "failed"  # consistent failure beats quarantine
    flaky = next(r for r in result.results if r.name == "flaky")
    assert flaky.gating and not flaky.passed and "gates" in flaky.output_tail
    [event] = verifier.drain_events()
    assert event.kind == QUARANTINED_FAILURE_EVENT


@pytest.mark.asyncio
async def test_timeouts_and_advisory_checks_are_not_rerun() -> None:
    inner = _Scripted({"slow": ["timeout"], "advice": ["fail"]})
    verifier = FlakyRetryVerifier(inner, retries=2)
    advisory = Check(name="advice", command=["x"], gating=False)
    result = await verifier.verify(_SESSION, [*_checks("slow"), advisory])
    assert inner.runs == ["slow", "advice"]
    assert result.verdict == "failed"


@pytest.mark.asyncio
async def test_zero_retries_turns_it_off() -> None:
    inner = _Scripted({"flaky": ["fail", "pass"]})
    verifier = FlakyRetryVerifier(inner, retries=0)
    result = await verifier.verify(_SESSION, _checks("flaky"))
    assert result.verdict == "failed" and inner.runs == ["flaky"]


@pytest.mark.asyncio
async def test_empty_rerun_result_keeps_the_failure() -> None:
    class _Vanishing(_Scripted):
        async def verify(self, session: SandboxSession, checks: list[Check]) -> VerificationResult:
            if self.runs:
                return VerificationResult.from_results([])
            return await super().verify(session, checks)

    verifier = FlakyRetryVerifier(_Vanishing({"t": ["fail"]}), retries=2)
    assert (await verifier.verify(_SESSION, _checks("t"))).verdict == "failed"


# --- git-backed revision and committed quarantine ---------------------------------------------
def _git(cwd: Path, *args: str) -> str:
    return subprocess.run(
        ["git", *args], cwd=cwd, check=True, capture_output=True, text=True
    ).stdout.strip()


def _repo(tmp_path: Path) -> Path:
    _git(tmp_path, "init", "-q")
    _git(tmp_path, "config", "user.email", "t@t")
    _git(tmp_path, "config", "user.name", "t")
    (tmp_path / "a.txt").write_text("one")
    _git(tmp_path, "add", "-A")
    _git(tmp_path, "commit", "-qm", "init")
    return tmp_path


def test_revision_is_the_work_tree_including_uncommitted_edits(tmp_path: Path) -> None:
    repo = _repo(tmp_path)
    first = tree_revision(str(repo))
    assert tree_revision(str(repo)) == first  # stable for an unchanged tree
    (repo / "a.txt").write_text("two")
    assert tree_revision(str(repo)) != first


def test_committed_quarantine_ignores_uncommitted_edits(tmp_path: Path) -> None:
    repo = _repo(tmp_path)
    assert committed_quarantine(str(repo)) == set()  # no event log at HEAD
    events = repo / ".lha" / "events.ndjson"
    events.parent.mkdir()
    lines = [
        EventRecord(kind=QUARANTINE_EVENT, payload={"check": "pytest"}).model_dump_json(),
        EventRecord(kind="cycle", payload={"check": "check_quarantined"}).model_dump_json(),
        "not json check_quarantined",
    ]
    events.write_text("\n".join(lines) + "\n")
    _git(repo, "add", "-A")
    _git(repo, "commit", "-qm", "events")
    forged = EventRecord(kind=QUARANTINE_EVENT, payload={"check": "ruff"}).model_dump_json()
    events.write_text(events.read_text() + forged + "\n")  # the agent's uncommitted edit
    assert committed_quarantine(str(repo)) == {"pytest"}


@pytest.mark.asyncio
async def test_committed_quarantine_is_restored_by_a_new_verifier(tmp_path: Path) -> None:
    repo = _repo(tmp_path)
    (repo / ".lha").mkdir()
    (repo / ".lha" / "events.ndjson").write_text(
        EventRecord(kind=QUARANTINE_EVENT, payload={"check": "t"}).model_dump_json() + "\n"
    )
    _git(repo, "add", "-A")
    _git(repo, "commit", "-qm", "events")
    verifier = FlakyRetryVerifier(_Scripted({"t": ["pass"]}), retries=1, workdir=str(repo))
    result = await verifier.verify(_SESSION, _checks("t"))
    assert result.verdict == "unverified"  # restored quarantine: the pass does not gate


@pytest.mark.asyncio
async def test_revision_falls_back_outside_git(tmp_path: Path) -> None:
    verifier = FlakyRetryVerifier(
        _Scripted({"t": ["fail", "pass"]}), retries=1, workdir=str(tmp_path)
    )
    result = await verifier.verify(_SESSION, _checks("t"))
    assert result.verdict == "unverified"
    [event] = verifier.drain_events()
    assert str(event.payload["revision"]).startswith("call-")


def test_lead_verifier_uses_the_configured_retries(tmp_path: Path) -> None:
    verifier = lead_verifier(str(tmp_path), Settings(flaky_retries=3))
    assert isinstance(verifier, FlakyRetryVerifier) and verifier._retries == 3


# --- wired into the run path: the event is committed with the checkpoint -----------------------
@pytest.mark.asyncio
async def test_local_mission_quarantines_a_flaky_check_and_commits_the_event(
    tmp_path: Path,
) -> None:
    counter = tmp_path / "counter"
    flip = (
        "import pathlib,sys;p=pathlib.Path(sys.argv[1]);"
        "n=int(p.read_text() if p.exists() else 0)+1;p.write_text(str(n));"
        "sys.exit(1 if n == 1 else 0)"
    )
    workdir = tmp_path / "ws"
    summary = await run_mission_local(
        workdir=str(workdir),
        title="t",
        description="d",
        checklist=Checklist(items=[ChecklistItem(id="01", description="do it")]),
        checks=[
            Check(name="flaky", command=[sys.executable, "-c", flip, str(counter)]),
            Check(name="solid", command=[sys.executable, "-c", "pass"]),
        ],
        settings=Settings(
            sandbox="local", allow_unsafe_local=True, model_backend="stub", budget_usd_ceiling=100.0
        ),
        model=StubModel(script=[TurnResult(text='{"done": true, "summary": "ok"}')]),
    )
    assert summary.completed  # "solid" gated the item; "flaky" was quarantined, not trusted
    committed = _git(workdir, "show", "HEAD:.lha/events.ndjson").splitlines()
    kinds = [json.loads(line)["kind"] for line in committed]
    assert QUARANTINE_EVENT in kinds
    cycle = next(json.loads(line) for line in committed if json.loads(line)["kind"] == "cycle")
    flaky = next(c for c in cycle["payload"]["checks"] if c["name"] == "flaky")
    assert flaky == {**flaky, "passed": True, "gating": False}
    assert committed_quarantine(str(workdir)) == {"flaky"}


# --- cross-implementation scenarios (spec/verify/flaky_retry.json) -----------------------------
SPEC_REVISION = "0123456789abcdef0123456789abcdef01234567"

FLAKY_SCENARIOS: list[dict[str, Any]] = [
    {
        "name": "consistent_failure",
        "retries": 2,
        "checks": [["t", True]],
        "outcomes": {"t": ["fail"]},
    },
    {
        "name": "pass_on_rerun",
        "retries": 2,
        "checks": [["flaky", True], ["solid", True]],
        "outcomes": {"flaky": ["fail", "pass"], "solid": ["pass"]},
    },
    {
        "name": "quarantined_check_alone_is_unverified",
        "retries": 1,
        "calls": 2,
        "checks": [["flaky", True]],
        "outcomes": {"flaky": ["fail", "pass", "pass"]},
    },
    {
        "name": "quarantined_check_failing_every_attempt_gates",
        "retries": 1,
        "calls": 2,
        "checks": [["flaky", True], ["solid", True]],
        "outcomes": {"flaky": ["fail", "pass", "fail"], "solid": ["pass"]},
    },
    {
        "name": "timeouts_and_advisory_checks_not_rerun",
        "retries": 2,
        "checks": [["slow", True], ["advice", False]],
        "outcomes": {"slow": ["timeout"], "advice": ["fail"]},
    },
    {
        "name": "zero_retries",
        "retries": 0,
        "checks": [["f", True]],
        "outcomes": {"f": ["fail", "pass"]},
    },
    {
        "name": "passes_on_the_last_retry",
        "retries": 3,
        "checks": [["f", True]],
        "outcomes": {"f": ["fail", "fail", "fail", "pass"]},
    },
    {
        "name": "committed_quarantine_restored",
        "retries": 1,
        "quarantined": ["t"],
        "checks": [["t", True], ["u", True]],
        "outcomes": {"t": ["fail", "pass"], "u": ["pass"]},
    },
    {
        "name": "duplicate_names_are_suffixed",
        "retries": 1,
        "checks": [["t", True], ["t", True]],
        "outcomes": {"t": ["fail", "pass"], "t-2": ["pass"]},
    },
]


class _FixedRevision(FlakyRetryVerifier):
    async def _revision(self) -> str:
        return SPEC_REVISION


def run_flaky_scenario(case: dict[str, Any]) -> dict[str, Any]:
    """Run one scenario (a scripted inner verifier, a fixed revision); record what happened.

    The export (``scripts/export_spec.py``) and the conformance test both run it, so Go's
    ``FlakyRetryVerifier`` is held to the same results, output tails and committed events.
    """
    inner = _Scripted(case["outcomes"])
    verifier = _FixedRevision(inner, retries=case["retries"])
    verifier.quarantine.restore(set(case.get("quarantined", [])))
    checks = [Check(name=name, command=["x"], gating=gating) for name, gating in case["checks"]]
    calls = []
    for _ in range(case.get("calls", 1)):
        result = asyncio.run(verifier.verify(_SESSION, checks))
        calls.append(
            {
                "verdict": result.verdict,
                "results": [
                    r.model_dump(include={"name", "passed", "gating", "timed_out", "output_tail"})
                    for r in result.results
                ],
                "events": [{"kind": e.kind, "payload": e.payload} for e in verifier.drain_events()],
            }
        )
    return {"runs": inner.runs, "calls": calls}


# The committed events log grows every cycle: a verifier reads it once, not on every verify.
@pytest.mark.asyncio
async def test_the_committed_quarantine_is_read_once_per_verifier(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    reads = 0

    def committed(_workdir: str) -> set[str]:
        nonlocal reads
        reads += 1
        return {"old"}

    monkeypatch.setattr("lha.verify.flaky_quarantine.committed_quarantine", committed)
    verifier = FlakyRetryVerifier(_Scripted({"t": ["pass"]}), retries=1, workdir=str(tmp_path))
    for _ in range(3):
        await verifier.verify(_SESSION, _checks("t"))
    assert reads == 1 and verifier.quarantine.is_flaky("old")

"""Gold evaluation sets (``lha eval``, ``eval/gold/``): the committed sets stay valid, the judges
behave on them as documented, and the CLI reads them (``lha.systemone.gold`` itself is pinned by
``spec/systemone/gold.json``)."""

from __future__ import annotations

import asyncio
import json
from pathlib import Path

import pytest
from typer.testing import CliRunner

import lha.cli.main as cli
from lha.config import Settings
from lha.contracts.system_one import ChoiceAnswer
from lha.systemone.gold import (
    POSITIVE,
    GoldRow,
    check_gold,
    count_by_source,
    judge_named,
    judge_recorded,
    judge_screen,
    judge_system_one,
    parse_gold,
    score,
    score_async,
)
from lha.systemone.review import QUESTION_ID
from lha.systemone.stub import StubSystemOne

runner = CliRunner()

# The repository's eval/gold (found upwards, like spec/ in test_spec_conformance.py).
GOLD = next(
    p / "eval" / "gold"
    for p in Path(__file__).resolve().parents
    if (p / "eval" / "gold" / "README.md").is_file()
)
SETS = sorted(GOLD.glob("*.jsonl"))


def _rows(path: Path) -> list:
    return parse_gold(path.read_text(encoding="utf-8"), name=path.name)


def test_the_committed_sets_are_valid() -> None:
    assert SETS, "no gold sets"
    for path in SETS:
        rows = _rows(path)
        assert rows and check_gold(rows) == [], path.name
        assert all(row.gold_by for row in rows), path.name
        # Every disagreement with the recorded label says why (a gold set is evidence).
        for row in rows:
            if row.gold != row.label:
                assert row.note and row.tags, (path.name, row.where)


def test_the_measurement_set_counts() -> None:
    rows = _rows(GOLD / "review-and-verifier-2026-10-02.jsonl")
    assert count_by_source(rows) == "73 gold rows (0 gate, 0 tool_approval, 52 verifier, 21 review)"
    recorded = {c.source: c for c in score(rows, judge_recorded)}
    # The verifier passed every gate bypass and every empty-diff attempt; the reviewer's only
    # disagreements are the dangling tool calls parsed as 'unparsed'.
    assert (recorded["verifier"].agree, recorded["verifier"].fn) == (40, 12)
    assert recorded["verifier"].fp == 0
    assert (recorded["review"].agree, recorded["review"].judged) == (15, 21)
    assert all("unparsed" in line for line in recorded["review"].disagreements)


def test_the_fixed_screen_catches_every_conftest_injection() -> None:
    rows = _rows(GOLD / "review-and-verifier-2026-10-02.jsonl")
    injected = [r for r in rows if r.source == "review" and "conftest-injection" in r.tags]
    assert len(injected) == 4
    for row in injected:
        assert row.input["screen_findings"] == [], row.where  # the screen of the day missed it
        assert judge_screen(row) == POSITIVE["review"], row.where  # today's screen catches it
    screen = {c.source: c for c in score(rows, judge_screen)}
    assert screen["review"].tp == 4 and screen["review"].fp == 0
    assert "verifier" in screen and screen["verifier"].judged == 0  # abstains off the diff


def test_eval_check_and_run(tmp_path: Path) -> None:
    paths = [str(GOLD / "review-and-verifier-2026-10-02.jsonl")]
    checked = runner.invoke(cli.app, ["eval", "check", *paths])
    assert checked.exit_code == 0, checked.output
    assert checked.output.startswith(
        "73 gold rows (0 gate, 0 tool_approval, 52 verifier, 21 review) in 1 file"
    )
    ran = runner.invoke(cli.app, ["eval", "run", "--judge", "screen", *paths])
    assert ran.exit_code == 0, ran.output
    assert ran.output.startswith(
        "judge: screen\nverifier: 52 rows, 0 judged (the judge abstains)\n"
    )
    assert "review: 21 rows, 21 judged" in ran.output
    assert runner.invoke(cli.app, ["eval", "run", "--judge", "oracle", *paths]).exit_code == 2

    bad = tmp_path / "bad.jsonl"
    bad.write_text('{"schema": 1, "source": "gate"}\n', encoding="utf-8")
    refused = runner.invoke(cli.app, ["eval", "check", str(bad)])
    assert refused.exit_code == 2 and f"{bad}:1: 'gold' must be an object" in refused.output
    dup = tmp_path / "dup.jsonl"
    row = {
        "schema": 1,
        "source": "verifier",
        "label": "passed",
        "mission_id": "m",
        "cycle_id": "c1",
        "item_id": "01",
        "gold": {"label": "passed", "by": "me"},
    }
    dup.write_text(json.dumps(row) + "\n" + json.dumps(row) + "\n", encoding="utf-8")
    refused = runner.invoke(cli.app, ["eval", "check", str(dup)])
    assert refused.exit_code == 2 and "duplicate of" in refused.output
    assert runner.invoke(cli.app, ["eval", "check", str(tmp_path / "missing.jsonl")]).exit_code == 2


def test_the_honest_set_has_no_disagreements() -> None:
    rows = _rows(GOLD / "org-vs-single-2026-10-03.jsonl")
    assert count_by_source(rows) == "27 gold rows (0 gate, 0 tool_approval, 18 verifier, 9 review)"
    for judge in (judge_recorded, judge_screen):
        for card in score(rows, judge):
            assert card.agree == card.judged and card.fp == 0, (card.source, card.disagreements)
    # Both sets together stay one valid set: no judgment appears in both.
    assert check_gold([row for path in SETS for row in _rows(path)]) == []


def _gold_row(source: str, **input_: object) -> GoldRow:
    row = {
        "schema": 1,
        "source": source,
        "label": "approve",
        "by": "x",
        "mission_id": "m",
        "cycle_id": "c1",
        "item_id": "01",
        "input": input_,
        "gold": {"label": "approve", "by": "y"},
        "tags": [],
    }
    return parse_gold(json.dumps(row) + "\n")[0]


def test_the_system_one_judge_maps_the_choice_and_abstains() -> None:
    answer = ChoiceAnswer(
        choice="block", probabilities={"approve": 0.1, "block": 0.9}, confidence=0.8
    )
    stub = StubSystemOne(lambda state, questions: {QUESTION_ID: answer})
    judge = judge_system_one(stub)
    assert asyncio.run(judge(_gold_row("review", diff="diff --git a/x b/x\n+pass\n"))) == "block"
    assert len(stub.calls) == 1
    # abstains without a diff, off-review, and when the call fails; none of those asks the model
    assert asyncio.run(judge(_gold_row("review"))) is None
    assert asyncio.run(judge(_gold_row("review", diff=""))) is None
    assert asyncio.run(judge(_gold_row("verifier", diff="x"))) is None
    assert len(stub.calls) == 1
    broken = judge_system_one(StubSystemOne(error="down"))
    assert asyncio.run(broken(_gold_row("review", diff="d"))) is None


def test_score_async_mirrors_score() -> None:
    rows = parse_gold(
        "".join(
            json.dumps(
                {
                    "schema": 1,
                    "source": "review",
                    "label": "approve",
                    "by": "reviewer",
                    "mission_id": "m",
                    "cycle_id": f"c{i}",
                    "item_id": "01",
                    "input": {"diff": diff},
                    "gold": {"label": gold, "by": "y"},
                    "tags": ["t"] if gold != "approve" else [],
                }
            )
            + "\n"
            for i, (diff, gold) in enumerate([("d1", "approve"), ("d2", "block"), ("d3", "block")])
        ),
        name="g",
    )
    stub = StubSystemOne(
        lambda state, questions: {
            QUESTION_ID: ChoiceAnswer(
                choice="approve", probabilities={"approve": 0.9, "block": 0.1}, confidence=0.8
            )
        }
    )
    cards = asyncio.run(score_async(rows, judge_system_one(stub)))
    assert len(cards) == 1 and cards[0].source == "review"
    assert (cards[0].rows, cards[0].judged, cards[0].agree) == (3, 3, 1)
    assert (cards[0].fp, cards[0].fn) == (0, 2)


def test_judge_named_says_system_one_needs_a_model() -> None:
    with pytest.raises(ValueError, match="needs a System One model"):
        judge_named("system_one")
    assert judge_named("screen") is judge_screen


def test_eval_run_with_the_system_one_stub_backend(monkeypatch: pytest.MonkeyPatch) -> None:
    paths = [str(GOLD / "review-and-verifier-2026-10-02.jsonl")]
    settings = Settings(_env_file=None, system_one_backend="stub")  # type: ignore[call-arg]
    monkeypatch.setattr(cli, "get_settings", lambda: settings)
    monkeypatch.setattr("lha.config.get_settings", lambda: settings)
    ran = runner.invoke(cli.app, ["eval", "run", "--judge", "system_one", *paths])
    assert ran.exit_code == 0, ran.output
    assert ran.output.startswith(
        "judge: system_one\nverifier: 52 rows, 0 judged (the judge abstains)\n"
    )
    assert "review: 21 rows, 21 judged" in ran.output

    off = Settings(_env_file=None, system_one_backend="off")  # type: ignore[call-arg]
    monkeypatch.setattr(cli, "get_settings", lambda: off)
    monkeypatch.setattr("lha.config.get_settings", lambda: off)
    refused = runner.invoke(cli.app, ["eval", "run", "--judge", "system_one", *paths])
    assert refused.exit_code == 2
    assert "needs a System One backend" in refused.output

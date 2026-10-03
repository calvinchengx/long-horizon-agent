"""Gold evaluation sets (``lha eval``, ``eval/gold/``): the committed sets stay valid, the judges
behave on them as documented, and the CLI reads them (``lha.systemone.gold`` itself is pinned by
``spec/systemone/gold.json``)."""

from __future__ import annotations

import json
from pathlib import Path

from typer.testing import CliRunner

import lha.cli.main as cli
from lha.systemone.gold import (
    POSITIVE,
    check_gold,
    count_by_source,
    judge_recorded,
    judge_screen,
    parse_gold,
    score,
)

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
    paths = [str(p) for p in SETS]
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

"""Tests for the Planner (mission intake → checklist) and its robust parsing."""

from __future__ import annotations

import pytest

from lha.agents.planner import Planner, parse_checklist
from lha.contracts.model import TurnResult
from lha.contracts.state import Checklist
from lha.model.stub import StubModel


def test_parse_checklist_from_json_array() -> None:
    text = '[{"description": "add model", "depends_on": []}, {"description": "add tests", "depends_on": ["01"]}]'
    items = parse_checklist(text)
    assert [i.id for i in items] == ["01", "02"]
    assert items[1].depends_on == ["01"]


def test_parse_checklist_tolerates_prose_wrapping() -> None:
    text = 'Sure! Here is the plan:\n[{"description": "do x"}]\nHope that helps.'
    items = parse_checklist(text)
    assert len(items) == 1
    assert items[0].description == "do x"


def test_parse_checklist_empty_on_garbage() -> None:
    assert parse_checklist("no json here") == []


@pytest.mark.asyncio
async def test_planner_uses_model_output() -> None:
    model = StubModel(
        script=[TurnResult(text='[{"description": "step one"}, {"description": "step two"}]')]
    )
    checklist = await Planner(model).plan(title="T", description="D")
    assert [i.description for i in checklist.items] == ["step one", "step two"]


@pytest.mark.asyncio
async def test_planner_falls_back_to_single_item() -> None:
    model = StubModel(script=[TurnResult(text="I cannot plan this.")])
    checklist = await Planner(model).plan(title="T", description="build the thing")
    assert len(checklist.items) == 1
    assert checklist.items[0].description == "build the thing"


def test_parse_checklist_normalizes_dependency_ids() -> None:
    text = (
        '[{"description": "a"}, {"description": "b", "depends_on": ["1"]},'
        ' {"description": "c", "depends_on": [1, "step 2", "#2"]}]'
    )
    items = parse_checklist(text)
    assert [i.depends_on for i in items] == [[], ["01"], ["01", "02"]]
    assert all(not i.notes for i in items)


def test_parse_checklist_drops_forward_self_and_unknown_deps() -> None:
    text = (
        '[{"description": "a", "depends_on": ["2"]}, {"description": "b", "depends_on": ["2"]},'
        ' {"description": "c", "depends_on": ["99", "setup"]}]'
    )
    items = parse_checklist(text)
    assert [i.depends_on for i in items] == [[], [], []]
    assert "planner dropped invalid depends_on" in items[2].notes
    assert Checklist(items=items).dependency_errors() == []


def test_parse_checklist_maps_indices_across_skipped_entries() -> None:
    text = '[{"description": "a"}, {"description": ""}, {"description": "c", "depends_on": ["1", "2"]}]'
    items = parse_checklist(text)
    assert [i.id for i in items] == ["01", "02"]
    assert items[1].depends_on == ["01"]  # dep on the skipped (empty) step is dropped


def test_parse_checklist_reads_harness_edit_opt_in() -> None:
    items = parse_checklist('[{"description": "fix the flaky test", "allow_harness_edits": true}]')
    assert items[0].allow_harness_edits

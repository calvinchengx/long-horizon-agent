"""Tests for failure reflection."""

from __future__ import annotations

import pytest

from lha.agents.reflection import reflect_on_failure
from lha.contracts.model import TurnResult
from lha.model.stub import StubModel


@pytest.mark.asyncio
async def test_reflection_returns_postmortem() -> None:
    model = StubModel(script=[TurnResult(text="Root cause: missing import. Try adding it.")])
    out = await reflect_on_failure(
        model=model, item_description="add a function", failure_summary="ImportError: x"
    )
    assert "Root cause" in out

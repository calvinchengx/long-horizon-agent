"""Tests for the deterministic stub model."""

from __future__ import annotations

import pytest

from lha.contracts.model import ModelMessage, ModelProvider, TurnResult, Usage
from lha.model import build_provider
from lha.model.stub import StubModel


def _msgs() -> list[ModelMessage]:
    return [
        ModelMessage(role="system", content="you are a coding agent"),
        ModelMessage(role="user", content="add a function"),
    ]


def test_stub_satisfies_protocol() -> None:
    assert isinstance(StubModel(), ModelProvider)


def test_stub_name_is_unmistakable() -> None:
    # Honesty: a stub must never look like a real model in logs/traces.
    assert StubModel("foo").name.startswith("stub:")


@pytest.mark.asyncio
async def test_stub_is_deterministic() -> None:
    a = await StubModel().complete(_msgs())
    b = await StubModel().complete(_msgs())
    assert a.text == b.text
    assert a.usage.input_tokens == b.usage.input_tokens > 0


@pytest.mark.asyncio
async def test_stub_cost_is_zero() -> None:
    model = StubModel()
    result = await model.complete(_msgs())
    assert model.estimate_cost_usd(result.usage) == 0.0


@pytest.mark.asyncio
async def test_stub_script_drives_turns_in_order() -> None:
    script = [
        TurnResult(text="first", usage=Usage(output_tokens=1)),
        TurnResult(text="second", usage=Usage(output_tokens=1)),
    ]
    model = StubModel(script=script)
    assert (await model.complete(_msgs())).text == "first"
    assert (await model.complete(_msgs())).text == "second"
    # Cycles on the last entry rather than raising.
    assert (await model.complete(_msgs())).text == "second"


def test_build_provider_returns_stub_by_default() -> None:
    provider = build_provider()
    assert isinstance(provider, StubModel)

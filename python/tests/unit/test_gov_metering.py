"""The metered provider: every call recorded, budget hard-stopped per call, unknown cost handled."""

from __future__ import annotations

import asyncio

import pytest

from lha.contracts.model import ModelMessage, TurnResult, UnknownPriceError, Usage
from lha.governor import BudgetExceeded, BudgetGovernor, CostLedger, CostMeter
from lha.model.stub import StubModel


class _Priced:
    """A fake provider at $1 per 1K tokens (input or output), with a fixed usage per call."""

    def __init__(self, *, usage: Usage, default_max_tokens: int = 100, priced: bool = True) -> None:
        self.name = "fake:priced"
        self.default_max_tokens = default_max_tokens
        self._usage = usage
        self._priced = priced
        self.calls = 0
        self.gate: asyncio.Event | None = None

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        self.calls += 1
        if self.gate is not None:
            await self.gate.wait()
        return TurnResult(text="ok", usage=self._usage.model_copy())

    def estimate_cost_usd(self, usage: Usage) -> float:
        if not self._priced:
            raise UnknownPriceError("unpriced")
        return (usage.input_tokens + usage.output_tokens) / 1000.0


def _meter(ceiling: float, *, allow_unknown: bool = False) -> CostMeter:
    return CostMeter(
        ledger=CostLedger(),
        governor=BudgetGovernor(
            ceiling_usd=ceiling, max_cycles=100, allow_unknown_cost=allow_unknown
        ),
    )


_HI = [ModelMessage(role="user", content="hi")]


@pytest.mark.asyncio
async def test_every_call_is_recorded_with_role_and_cycle() -> None:
    meter = _meter(100.0)
    reviewer = meter.wrap(
        _Priced(usage=Usage(input_tokens=500, output_tokens=500)), role="reviewer"
    )
    planner = meter.wrap(StubModel(), role="planner")
    meter.cycle_id = "c3"
    await reviewer.complete(_HI)
    await planner.complete(_HI)
    entries = meter.ledger.entries
    assert [(e.role, e.cycle_id) for e in entries] == [("reviewer", "c3"), ("planner", "c3")]
    assert meter.ledger.total_usd == pytest.approx(1.0)


@pytest.mark.asyncio
async def test_hard_stop_refuses_call_whose_worst_case_breaches_budget() -> None:
    provider = _Priced(usage=Usage(input_tokens=10, output_tokens=10), default_max_tokens=5000)
    meter = _meter(1.0)
    model = meter.wrap(provider)
    # Worst case = ~5000 output tokens => ~$5 > $1 ceiling: refused BEFORE running.
    with pytest.raises(BudgetExceeded) as info:
        await model.complete(_HI)
    assert provider.calls == 0
    assert "worst-case" in info.value.decision.reason
    # A call with a small explicit max_tokens fits.
    await model.complete(_HI, max_tokens=50)
    assert provider.calls == 1


@pytest.mark.asyncio
async def test_hard_stop_accounts_for_spend_so_far() -> None:
    provider = _Priced(usage=Usage(input_tokens=450, output_tokens=450), default_max_tokens=100)
    meter = _meter(1.0)
    model = meter.wrap(provider)
    await model.complete(_HI)  # $0.90 spent
    with pytest.raises(BudgetExceeded):
        await model.complete(_HI)  # 0.90 + worst case (> $0.10) > $1.00
    assert provider.calls == 1


@pytest.mark.asyncio
async def test_parallel_calls_reserve_their_worst_case() -> None:
    provider = _Priced(usage=Usage(input_tokens=1, output_tokens=1), default_max_tokens=600)
    provider.gate = asyncio.Event()
    meter = _meter(1.0)
    model = meter.wrap(provider)
    first = asyncio.create_task(model.complete(_HI))
    await asyncio.sleep(0)  # first call is now in flight with ~$0.6 reserved
    with pytest.raises(BudgetExceeded):
        await model.complete(_HI)
    provider.gate.set()
    await first
    assert meter.reserved_usd == 0.0


@pytest.mark.asyncio
async def test_unknown_cost_is_refused_by_default() -> None:
    provider = _Priced(usage=Usage(input_tokens=1, output_tokens=1), priced=False)
    model = _meter(100.0).wrap(provider)
    with pytest.raises(BudgetExceeded) as info:
        await model.complete(_HI)
    assert "price" in info.value.decision.reason
    assert provider.calls == 0


@pytest.mark.asyncio
async def test_unknown_cost_recorded_when_explicitly_allowed() -> None:
    provider = _Priced(usage=Usage(input_tokens=7, output_tokens=3, model="m"), priced=False)
    meter = _meter(100.0, allow_unknown=True)
    await meter.wrap(provider).complete(_HI)
    entry = meter.ledger.entries[0]
    assert not entry.cost_known
    assert entry.input_tokens == 7
    assert meter.ledger.unknown_cost_entries == 1


def test_governor_denies_next_cycle_when_spend_is_unverifiable() -> None:
    ledger = CostLedger()
    ledger.record(cycle_id="c1", usage=Usage(input_tokens=5, model="m"), usd=None)
    decision = BudgetGovernor(ceiling_usd=100.0, max_cycles=10).authorize_next(
        ledger, cycles_done=1
    )
    assert not decision.allow
    assert "unknown price" in decision.reason


def test_governor_projects_from_most_expensive_cycle() -> None:
    ledger = CostLedger()
    ledger.record(cycle_id="c1", usage=Usage(model="m"), usd=0.1)
    ledger.record(cycle_id="c2", usage=Usage(model="m"), usd=5.0)
    gov = BudgetGovernor(ceiling_usd=10.0, max_cycles=10)
    # spent 5.1; mean 2.55 would allow, the max cycle (5.0) projects 10.1 > 10.
    assert not gov.authorize_next(ledger, cycles_done=2).allow

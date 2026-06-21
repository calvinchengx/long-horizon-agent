"""The cost ledger — real spend, accumulated from real token usage.

Every model turn appends an entry computed from the provider's reported ``Usage`` via the
provider's own ``estimate_cost_usd`` (so local/free backends record genuine ``$0`` while still
tracking tokens). When a provider cannot price a turn (no configured price for the responding
model) the entry is recorded with ``cost_known=False``: tokens are kept, and the governor treats
the unknown spend conservatively instead of assuming ``$0``.
"""

from __future__ import annotations

from pydantic import BaseModel, Field

from lha.contracts.model import Usage


class CostEntry(BaseModel):
    """One recorded model turn."""

    cycle_id: str
    model: str
    input_tokens: int
    output_tokens: int
    usd: float  # 0.0 when ``cost_known`` is False (the real cost is unknown, NOT zero)
    cache_read_input_tokens: int = 0
    cache_creation_input_tokens: int = 0
    cost_known: bool = True
    role: str = ""


class CostLedger(BaseModel):
    """Append-only ledger of spend for one mission."""

    entries: list[CostEntry] = Field(default_factory=list)

    def record(
        self, *, cycle_id: str, usage: Usage, usd: float | None, role: str = ""
    ) -> CostEntry:
        """Record one turn; ``usd=None`` marks the cost as unknown (tokens are still kept)."""
        entry = CostEntry(
            cycle_id=cycle_id,
            model=usage.model,
            input_tokens=usage.input_tokens,
            output_tokens=usage.output_tokens,
            usd=usd if usd is not None else 0.0,
            cache_read_input_tokens=usage.cache_read_input_tokens,
            cache_creation_input_tokens=usage.cache_creation_input_tokens,
            cost_known=usd is not None,
            role=role,
        )
        self.entries.append(entry)
        return entry

    @property
    def total_usd(self) -> float:
        """Known spend only; check ``unknown_cost_entries`` before trusting it as complete."""
        return sum(e.usd for e in self.entries)

    @property
    def unknown_cost_entries(self) -> int:
        return sum(1 for e in self.entries if not e.cost_known)

    @property
    def total_input_tokens(self) -> int:
        return sum(e.input_tokens for e in self.entries)

    @property
    def total_output_tokens(self) -> int:
        return sum(e.output_tokens for e in self.entries)

    def mean_usd_per_cycle(self) -> float:
        cycles = {e.cycle_id for e in self.entries}
        return self.total_usd / len(cycles) if cycles else 0.0

    def max_usd_per_cycle(self) -> float:
        """The most expensive cycle so far (a conservative next-cycle projection)."""
        per_cycle: dict[str, float] = {}
        for e in self.entries:
            per_cycle[e.cycle_id] = per_cycle.get(e.cycle_id, 0.0) + e.usd
        return max(per_cycle.values(), default=0.0)

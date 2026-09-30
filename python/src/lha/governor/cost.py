"""The cost ledger — real spend, accumulated from real token usage.

Every model turn appends an entry computed from the provider's reported ``Usage`` via the
provider's own ``estimate_cost_usd`` (so local/free backends record genuine ``$0`` while still
tracking tokens). When a provider cannot price a turn (no configured price for the responding
model) the entry is recorded with ``cost_known=False``: tokens are kept, and the governor treats
the unknown spend conservatively instead of assuming ``$0``.
"""

from __future__ import annotations

from pydantic import BaseModel, Field, PrivateAttr

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


#: Entries a ``CostLedger`` keeps (the newest). The totals cover every entry ever added; the rows
#: themselves are in the mission store's ``cost_ledger`` (``lha costs``).
MAX_LEDGER_ENTRIES = 5_000


class CostLedger(BaseModel):
    """Ledger of spend for one mission: running totals over every entry, and the newest
    ``MAX_LEDGER_ENTRIES`` entries. Add entries with ``record`` / ``add``, never to ``entries``."""

    entries: list[CostEntry] = Field(default_factory=list)
    _usd: float = PrivateAttr(default=0.0)
    _unknown: int = PrivateAttr(default=0)
    _input_tokens: int = PrivateAttr(default=0)
    _output_tokens: int = PrivateAttr(default=0)
    _per_cycle: dict[str, float] = PrivateAttr(default_factory=dict)

    def model_post_init(self, context: object, /) -> None:
        entries, self.entries = self.entries, []
        for entry in entries:
            self.add(entry)

    def add(self, entry: CostEntry) -> CostEntry:
        """Add one entry to the totals and the window of newest entries."""
        self._usd += entry.usd
        self._unknown += 0 if entry.cost_known else 1
        self._input_tokens += entry.input_tokens
        self._output_tokens += entry.output_tokens
        self._per_cycle[entry.cycle_id] = self._per_cycle.get(entry.cycle_id, 0.0) + entry.usd
        self.entries.append(entry)
        if len(self.entries) > MAX_LEDGER_ENTRIES + MAX_LEDGER_ENTRIES // 10:
            del self.entries[: len(self.entries) - MAX_LEDGER_ENTRIES]
        return entry

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
        return self.add(entry)

    @property
    def total_usd(self) -> float:
        """Known spend only; check ``unknown_cost_entries`` before trusting it as complete."""
        return self._usd

    @property
    def unknown_cost_entries(self) -> int:
        return self._unknown

    @property
    def total_input_tokens(self) -> int:
        return self._input_tokens

    @property
    def total_output_tokens(self) -> int:
        return self._output_tokens

    def mean_usd_per_cycle(self) -> float:
        return self._usd / len(self._per_cycle) if self._per_cycle else 0.0

    def max_usd_per_cycle(self) -> float:
        """The most expensive cycle so far (a conservative next-cycle projection)."""
        return max(self._per_cycle.values(), default=0.0)

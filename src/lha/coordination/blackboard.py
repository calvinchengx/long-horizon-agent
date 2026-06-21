"""Shared blackboard with response-board separation.

Agents read/write a shared board instead of chatting directly. Writes within a round go to a
SEPARATE response board and are only promoted into the main board between rounds — so one agent's
output can't silently bias another mid-round (a known multi-agent failure mode).
"""

from __future__ import annotations

from dataclasses import dataclass, field


@dataclass
class BoardEntry:
    author: str
    content: str


@dataclass
class Blackboard:
    _main: list[BoardEntry] = field(default_factory=list)
    _responses: list[BoardEntry] = field(default_factory=list)

    def post(self, author: str, content: str) -> None:
        """Append to the durable main board."""
        self._main.append(BoardEntry(author=author, content=content))

    def respond(self, author: str, content: str) -> None:
        """Write to the per-round response board (not yet visible on the main board)."""
        self._responses.append(BoardEntry(author=author, content=content))

    def read(self) -> list[BoardEntry]:
        return list(self._main)

    def read_responses(self) -> list[BoardEntry]:
        return list(self._responses)

    def commit_round(self) -> None:
        """Promote this round's responses into the main board and clear the response board."""
        self._main.extend(self._responses)
        self._responses.clear()

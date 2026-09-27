"""The cycle-start code map (``LHA_CODE_MAP=ripwire``): orient the lead before its first turn.

Each cycle starts from a mostly fresh context, and the lead would otherwise spend turns finding its
way around the code (``list_files``, ``grep``, ``read_file``). With a code map, the harness runs
`ripwire <https://github.com/redhat-et/ripwire>`_ once per cycle, inside the sandbox, as
``ripwire . --pack-task="<the item>" --token-budget=N`` and puts the task bundle (ranked
definitions, callers, tests) into the first message, after the memory block.

ripwire is deterministic, offline and writes nothing into the workspace. It runs in the sandbox
because it parses agent-written code. The map is advisory: it can miss things, and the verifier
still decides what is done. A missing ``ripwire``, a failure or a timeout means no map for that
cycle, never a failed cycle.
"""

from __future__ import annotations

import time

from lha.agent.prompt import render_code_map
from lha.contracts.sandbox import SandboxSession
from lha.contracts.state import ChecklistItem


def code_map_argv(item: ChecklistItem, token_budget: int) -> list[str]:
    """The ripwire command for ``item`` (no shell: the description is one argument)."""
    return ["ripwire", ".", f"--pack-task={item.description}", f"--token-budget={token_budget}"]


class RipwireCodeMap:
    """Renders ripwire's task bundle for an item, in the cycle's sandbox session."""

    def __init__(self, *, token_budget: int = 2000, timeout_s: float = 60.0) -> None:
        self.token_budget = token_budget
        self.timeout_s = timeout_s

    async def render(
        self, session: SandboxSession, item: ChecklistItem
    ) -> tuple[str, dict[str, object]]:
        """The rendered section (``""`` when unavailable) and the ``code_map`` event fields."""
        started = time.monotonic()
        try:
            result = await session.exec(
                code_map_argv(item, self.token_budget), timeout_s=max(1, int(self.timeout_s))
            )
        except Exception as exc:  # a broken sandbox must not fail the cycle over an optional map
            return "", {"ok": False, "error": f"{type(exc).__name__}: {exc}"[:300]}
        info: dict[str, object] = {
            "ok": result.ok,
            "exit_code": result.exit_code,
            "timed_out": result.timed_out,
            "bytes": len(result.stdout),
            "duration_s": round(time.monotonic() - started, 3),
        }
        if not result.ok:
            info["error"] = (result.stderr or result.stdout)[-300:]
            return "", info
        return render_code_map(result.stdout), info

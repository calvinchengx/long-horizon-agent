"""The cycle-start code map (``LHA_CODE_MAP=ripwire``): orient the lead before its first turn.

Each cycle starts from a mostly fresh context, and the lead would otherwise spend turns finding its
way around the code (``list_files``, ``grep``, ``read_file``). With a code map, the harness runs
`ripwire <https://github.com/redhat-et/ripwire>`_ once per cycle, inside the sandbox, as
``ripwire . --pack-task="<the item>" --token-budget=N`` and puts the task bundle (ranked
definitions, callers, tests) into the first message, after the memory block.

Two things make the map find the right code more often (both measured on real LHA runs):

- the task query includes the item's acceptance checks (witnesses), whose test names often use the
  code's own words when the description does not;
- on a retry, the map comes from the last failure report (``ripwire . --from-trace=-``), which
  points straight at the code the failing test exercised. If that finds nothing, the task query
  is used instead. The report reaches ripwire through an environment variable and stdin, never
  through the command line or a file in the workspace.

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

#: Characters of the failure report (its end, where the errors are) given to ``--from-trace``.
TRACE_CHARS = 4000
#: The fixed script that feeds the failure report to ripwire (the report is only ever data).
TRACE_SCRIPT = (
    'printf %s "$LHA_TRACE" | ripwire . --from-trace=- --token-budget="$LHA_TRACE_BUDGET"'
)


def code_map_query(item: ChecklistItem) -> str:
    """The task query: the description, plus the acceptance checks when the item has any."""
    if not item.witnesses:
        return item.description
    return f"{item.description}\nAcceptance checks: {', '.join(item.witnesses)}"


def code_map_argv(item: ChecklistItem, token_budget: int) -> list[str]:
    """The ripwire task command for ``item`` (no shell: the query is one argument)."""
    return [
        "ripwire",
        ".",
        f"--pack-task={code_map_query(item)}",
        f"--token-budget={token_budget}",
    ]


def trace_argv() -> list[str]:
    return ["sh", "-c", TRACE_SCRIPT]


def trace_env(item: ChecklistItem, token_budget: int) -> dict[str, str]:
    """The environment of the trace command: the end of the last failure report, and the budget."""
    return {
        "LHA_TRACE": item.last_failure[-TRACE_CHARS:],
        "LHA_TRACE_BUDGET": str(token_budget),
    }


def found_code(output: str) -> bool:
    """Whether a ripwire answer names any code location (a row with a ``p=`` path)."""
    return ' p="' in output


class RipwireCodeMap:
    """Renders ripwire's task bundle for an item, in the cycle's sandbox session."""

    def __init__(self, *, token_budget: int = 2000, timeout_s: float = 60.0) -> None:
        self.token_budget = token_budget
        self.timeout_s = timeout_s

    async def render(
        self, session: SandboxSession, item: ChecklistItem
    ) -> tuple[str, dict[str, object]]:
        """The rendered section (``""`` when unavailable) and the ``code_map`` event fields.

        A retry (the item has a failure report) tries the trace first; the task query is used
        when there is no report or the trace finds no code.
        """
        started = time.monotonic()
        fell_back = False
        if item.last_failure.strip():
            output, info = await self._run(
                session, trace_argv(), env=trace_env(item, self.token_budget)
            )
            if info["ok"] and found_code(output):
                info.update(mode="trace", duration_s=round(time.monotonic() - started, 3))
                return render_code_map(output), info
            fell_back = True
        output, info = await self._run(session, code_map_argv(item, self.token_budget))
        info.update(mode="task", fell_back=fell_back)
        info["duration_s"] = round(time.monotonic() - started, 3)
        return (render_code_map(output) if info["ok"] else ""), info

    async def _run(
        self, session: SandboxSession, argv: list[str], *, env: dict[str, str] | None = None
    ) -> tuple[str, dict[str, object]]:
        try:
            result = await session.exec(argv, timeout_s=max(1, int(self.timeout_s)), env=env)
        except Exception as exc:  # a broken sandbox must not fail the cycle over an optional map
            return "", {"ok": False, "error": f"{type(exc).__name__}: {exc}"[:300]}
        info: dict[str, object] = {
            "ok": result.ok,
            "exit_code": result.exit_code,
            "timed_out": result.timed_out,
            "bytes": len(result.stdout),
        }
        if not result.ok:
            info["error"] = (result.stderr or result.stdout)[-300:]
        return result.stdout, info

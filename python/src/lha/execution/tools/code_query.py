"""``code_query``: ask ripwire structural questions about the code instead of reading files.

Enabled by ``LHA_CODE_QUERY=true`` (off by default; needs ripwire in the sandbox, which the
reference image in ``sandbox/`` has). One read-only tool, five kinds of question:

- ``find``: rank the code relevant to a task in words (``ripwire --for``);
- ``definition``: a symbol's full body (``--expand``);
- ``callers``: what calls a symbol (``--callers``);
- ``uses``: where a symbol is used (``--uses``);
- ``impact``: what reaches a symbol, the blast radius of changing it (``--impact``).

Each answer is small next to reading whole files, and the model pays for it only when it asks
(the cycle-start code map, by contrast, is re-sent with every turn). ripwire runs in the sandbox,
offline, and writes nothing into the workspace. The target is one argument (no shell). Answers
are ranked and can miss things; the verifier still decides what is done.
"""

from __future__ import annotations

from lha.contracts.tools import ToolContext, ToolResult, ToolSpec

#: The question kinds and the ripwire flag each maps to.
KINDS: dict[str, str] = {
    "find": "--for",
    "definition": "--expand",
    "callers": "--callers",
    "uses": "--uses",
    "impact": "--impact",
}
#: The longest target accepted: a symbol, or for ``find`` a task in words.
MAX_SYMBOL_CHARS = 300
MAX_FIND_CHARS = 1000
#: The longest answer returned to the model.
MAX_ANSWER_CHARS = 16_000


def code_query_argv(kind: str, target: str, token_budget: int) -> list[str]:
    """The ripwire command for one question (raises ``ValueError`` for an unusable one)."""
    flag = KINDS.get(kind)
    if flag is None:
        raise ValueError(f"'kind' must be one of {', '.join(KINDS)}")
    target = target.strip()
    if not target:
        raise ValueError("'target' must not be empty")
    limit = MAX_FIND_CHARS if kind == "find" else MAX_SYMBOL_CHARS
    if len(target) > limit:
        raise ValueError(f"'target' is longer than {limit} characters")
    if kind != "find" and any(ch in target for ch in "\n\r\x00"):
        raise ValueError("a symbol 'target' must be a single line")
    argv = ["ripwire", ".", f"{flag}={target}"]
    if kind == "find":
        argv.append(f"--token-budget={token_budget}")
    elif kind == "definition":
        argv.append("--top-k=0")  # the body only, not the ranked map that otherwise rides along
    return argv


def clip_answer(text: str) -> str:
    """The answer as returned to the model, cut to ``MAX_ANSWER_CHARS`` with a marker."""
    if len(text) <= MAX_ANSWER_CHARS:
        return text
    return text[:MAX_ANSWER_CHARS] + "\n…[truncated]"


class CodeQueryTool:
    spec = ToolSpec(
        name="code_query",
        description=(
            "Ask the code map (ripwire) about this repository instead of reading whole files. "
            "kind 'find': the code relevant to a task, target = the task in words; "
            "'definition': a symbol's full body; 'callers': what calls a symbol; "
            "'uses': where a symbol is used; 'impact': what reaches a symbol (the blast radius "
            "of changing it). Answers are ranked and can miss things: read a file before you "
            "change it."
        ),
        parameters={
            "type": "object",
            "properties": {
                "kind": {"type": "string", "enum": list(KINDS)},
                "target": {"type": "string"},
            },
            "required": ["kind", "target"],
        },
    )

    def __init__(self, *, token_budget: int = 1500, timeout_s: float = 60.0) -> None:
        self.token_budget = token_budget
        self.timeout_s = timeout_s

    async def run(self, arguments: dict[str, object], ctx: ToolContext) -> ToolResult:
        try:
            argv = code_query_argv(
                str(arguments.get("kind", "")), str(arguments.get("target", "")), self.token_budget
            )
        except ValueError as exc:
            return ToolResult.failure(str(exc))
        result = await ctx.session.exec(argv, timeout_s=max(1, int(self.timeout_s)))
        if result.exit_code == 127:
            return ToolResult.failure(
                "code_query needs ripwire in the sandbox; it is not installed"
            )
        if not result.ok:
            detail = (result.stderr or result.stdout).strip()[-500:]
            reason = "timed out" if result.timed_out else detail or f"exit {result.exit_code}"
            return ToolResult.failure(f"code_query: {reason}")
        return ToolResult.success(clip_answer(result.stdout))

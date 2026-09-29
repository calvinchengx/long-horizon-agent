"""Claude Code (``claude -p``) as a model backend.

``claude -p`` runs one headless Claude Code session and prints a JSON result: the final text,
token usage per model, the session id and ``total_cost_usd``. Running LHA through it means a
Claude Pro/Max login works without an API key, with Claude Code's own model defaults.

Two uses, sharing the helpers here:

- ``ClaudeCodeModel`` (``LHA_MODEL_BACKEND=claude_code``): one ``complete()`` is one ``claude -p``
  call with every built-in tool switched off (``--tools ""``), so it is a plain text turn. The
  conversation is flattened into the prompt and LHA's tools are described in the system prompt,
  so the lead replies with the JSON actions ``lha.agent.prompt`` asks for and LHA executes them.
  Planner, replanner, reviewer and every other role work the same way.
- ``lha.agent.claude_code_engine`` (``LHA_LEAD_ENGINE=claude_code``): a whole lead cycle is one
  ``claude -p`` session with LHA's tools served over MCP.

Cost: the ledger records the ``total_cost_usd`` Claude Code reports (``Usage.reported_cost_usd``).
On a subscription that is the API-equivalent cost, not a bill, but the budget ceiling still
applies to it. Before a call runs, its worst case is ``LHA_CLAUDE_CODE_MAX_BUDGET_USD``, which is
also passed to the CLI as ``--max-budget-usd``.

Failures: a result with ``is_error`` raises ``ClaudeCodeError``; rate limits, overload and 5xx
are marked retryable (``lha.model.retry.is_retryable``), authentication and usage errors are not.
"""

from __future__ import annotations

import asyncio
import json
import os
import shutil
from dataclasses import dataclass, field

from lha.contracts.model import ModelMessage, ModelProvider, TurnResult, Usage
from lha.model.health import ModelHealth
from lha.model.pricing import ModelPrice, lookup_claude_price
from lha.model.retry import Sleep, with_retries

# ``claude --model`` value meaning "whatever Claude Code would pick" (no flag passed).
DEFAULT_MODEL = "default"
# Transient API failures worth another try (rate limit, overload, server errors, timeouts).
_TRANSIENT_MARKERS = ("rate limit", "rate_limit", "overloaded", "529", "timeout", "timed out")
_STDERR_TAIL = 2000
# Environment variables that make a child ``claude`` think it runs nested inside Claude Code.
_NESTED_ENV = ("CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT")


class ClaudeCodeError(RuntimeError):
    """``claude -p`` failed: it could not start, exited non-zero, or reported ``is_error``."""

    def __init__(
        self,
        message: str,
        *,
        retryable: bool = False,
        subtype: str | None = None,
        usage: Usage | None = None,
    ) -> None:
        super().__init__(message)
        self.retryable = retryable
        # The result's ``subtype`` (``error_max_budget_usd``, ``error_max_turns``, ...), if any.
        self.subtype = subtype
        # What the failed run still spent, when it got far enough to report it.
        self.usage = usage


@dataclass
class ClaudeCodeResult:
    """The parsed ``--output-format json`` result of one ``claude -p`` run."""

    text: str
    usage: Usage
    session_id: str | None = None
    num_turns: int = 0
    stop_reason: str | None = None
    raw: dict[str, object] = field(default_factory=dict)


def _int(value: object) -> int:
    return int(value) if isinstance(value, int | float) else 0


def _serving_model(model_usage: object, fallback: str) -> str:
    """The model that produced most of the output (Claude Code may also use a small model)."""
    if not isinstance(model_usage, dict) or not model_usage:
        return fallback
    best = max(
        model_usage.items(),
        key=lambda kv: _int(kv[1].get("outputTokens")) if isinstance(kv[1], dict) else 0,
    )
    return str(best[0])


def _is_transient(data: dict[str, object], text: str) -> bool:
    status = data.get("api_error_status")
    if isinstance(status, int) and (status in (408, 409, 429) or status >= 500):
        return True
    lowered = text.lower()
    return any(marker in lowered for marker in _TRANSIENT_MARKERS)


def parse_result(stdout: str, *, provider: str, fallback_model: str) -> ClaudeCodeResult:
    """Parse the JSON ``claude -p --output-format json`` printed; raise on an error result."""
    try:
        data = json.loads(stdout)
    except json.JSONDecodeError as exc:
        raise ClaudeCodeError(f"claude -p printed no JSON result: {stdout[-500:]!r}") from exc
    if not isinstance(data, dict):
        raise ClaudeCodeError(f"claude -p printed an unexpected result: {stdout[-500:]!r}")
    text = data.get("result")
    text = text if isinstance(text, str) else ""
    usage = _usage(data, provider=provider, fallback_model=fallback_model)
    subtype = data.get("subtype")
    if data.get("is_error") or subtype not in (None, "success"):
        reason = text or str(subtype or "error")
        raise ClaudeCodeError(
            f"claude -p failed: {reason[:500]}",
            retryable=_is_transient(data, reason),
            subtype=subtype if isinstance(subtype, str) else None,
            usage=usage,
        )
    session = data.get("session_id")
    stop = data.get("stop_reason")
    return ClaudeCodeResult(
        text=text,
        usage=usage,
        session_id=session if isinstance(session, str) else None,
        num_turns=_int(data.get("num_turns")),
        stop_reason=stop if isinstance(stop, str) else None,
        raw=data,
    )


def _usage(data: dict[str, object], *, provider: str, fallback_model: str) -> Usage:
    raw_usage = data.get("usage")
    usage = raw_usage if isinstance(raw_usage, dict) else {}
    breakdown = usage.get("cache_creation")
    cost = data.get("total_cost_usd")
    return Usage(
        input_tokens=_int(usage.get("input_tokens")),
        output_tokens=_int(usage.get("output_tokens")),
        cache_read_input_tokens=_int(usage.get("cache_read_input_tokens")),
        cache_creation_input_tokens=_int(usage.get("cache_creation_input_tokens")),
        cache_creation_1h_input_tokens=_int(breakdown.get("ephemeral_1h_input_tokens"))
        if isinstance(breakdown, dict)
        else 0,
        model=_serving_model(data.get("modelUsage"), fallback_model),
        provider=provider,
        reported_cost_usd=float(cost) if isinstance(cost, int | float) else None,
    )


def child_env(extra: dict[str, str] | None = None) -> dict[str, str]:
    """The environment for a child ``claude``: ours, minus the nested-session markers."""
    env = {k: v for k, v in os.environ.items() if k not in _NESTED_ENV}
    env.update(extra or {})
    return env


def base_args(*, model: str, max_budget_usd: float) -> list[str]:
    """Flags every LHA ``claude -p`` call shares: JSON out, no saved session, a spend cap."""
    args = [
        "-p",
        "--output-format",
        "json",
        "--no-session-persistence",
        "--disable-slash-commands",
        "--max-budget-usd",
        f"{max_budget_usd:.4f}",
    ]
    if model and model != DEFAULT_MODEL:
        args += ["--model", model]
    return args


async def run_claude(
    args: list[str],
    *,
    prompt: str,
    binary: str,
    cwd: str | None,
    timeout_s: float,
    provider: str,
    fallback_model: str,
) -> ClaudeCodeResult:
    """Run ``binary *args`` with ``prompt`` on stdin and parse its JSON result.

    The prompt goes on stdin, never argv, so its size is not limited by the OS. A run past
    ``timeout_s`` is killed and raises ``TimeoutError`` (retryable).
    """
    try:
        proc = await asyncio.create_subprocess_exec(
            binary,
            *args,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            cwd=cwd,
            env=child_env(),
        )
    except OSError as exc:
        raise ClaudeCodeError(
            f"cannot run {binary!r}: {exc}. Install Claude Code or set LHA_CLAUDE_CODE_BIN."
        ) from exc
    try:
        stdout, stderr = await asyncio.wait_for(
            proc.communicate(prompt.encode()), timeout=timeout_s
        )
    except TimeoutError:
        proc.kill()
        await proc.wait()
        raise TimeoutError(f"claude -p did not finish within {timeout_s:.0f}s") from None
    out = stdout.decode(errors="replace").strip()
    if proc.returncode != 0 and not out.startswith("{"):
        err = stderr.decode(errors="replace")[-_STDERR_TAIL:]
        raise ClaudeCodeError(f"claude -p exited {proc.returncode}: {err or out[-500:]}")
    return parse_result(out, provider=provider, fallback_model=fallback_model)


def render_transcript(messages: list[ModelMessage]) -> str:
    """The non-system messages as one prompt, ending with a request for the next reply."""
    parts: list[str] = []
    for m in messages:
        if m.role == "system":
            continue
        if m.role == "assistant":
            body = m.content
            for call in m.tool_calls:
                action = json.dumps({"tool": call.name, "arguments": call.arguments})
                body = f"{body}\n{action}".strip()
            parts.append(f"<assistant>\n{body}\n</assistant>")
        elif m.role == "tool":
            parts.append(f"<tool_result id={m.tool_call_id or ''!r}>\n{m.content}\n</tool_result>")
        else:
            parts.append(f"<user>\n{m.content}\n</user>")
    if len(parts) == 1 and parts[0].startswith("<user>"):
        return messages[-1].content  # a single user turn needs no transcript framing
    parts.append("Write the assistant's next reply only.")
    return "\n\n".join(parts)


class ClaudeCodeModel(ModelProvider):
    """A ``ModelProvider`` that runs each turn as one tool-less ``claude -p`` call."""

    def __init__(
        self,
        *,
        model_name: str = DEFAULT_MODEL,
        binary: str = "claude",
        cwd: str | None = None,
        max_budget_usd: float = 5.0,
        timeout_s: float = 3600.0,
        price: ModelPrice | None = None,
        max_retries: int = 3,
        retry_base_delay_s: float = 2.0,
        sleep: Sleep = asyncio.sleep,
    ) -> None:
        self.name = f"claude_code:{model_name}"
        self._model = model_name
        self._binary = binary
        self._cwd = cwd
        self._max_budget_usd = max_budget_usd
        self._timeout_s = timeout_s
        self._price = price
        self._max_retries = max_retries
        self._retry_base_delay_s = retry_base_delay_s
        self._sleep = sleep

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        # ``tools`` is ignored: native tool calling needs MCP (the claude_code lead engine); here
        # the tools are described in the system prompt and the reply is a JSON action.
        system = "\n\n".join(m.content for m in messages if m.role == "system")
        args = [
            *base_args(model=self._model, max_budget_usd=self._max_budget_usd),
            "--tools",
            "",
            "--strict-mcp-config",
        ]
        if system:
            args += ["--system-prompt", system]

        async def _run() -> ClaudeCodeResult:
            return await run_claude(
                args,
                prompt=render_transcript(messages),
                binary=self._binary,
                cwd=self._cwd,
                timeout_s=self._timeout_s,
                provider=self.name,
                fallback_model=self._model,
            )

        result = await with_retries(
            _run,
            max_retries=self._max_retries,
            base_delay_s=self._retry_base_delay_s,
            sleep=self._sleep,
        )
        return TurnResult(
            text=result.text,
            usage=result.usage,
            stop_reason=result.stop_reason,
            session_id=result.session_id,
        )

    def estimate_cost_usd(self, usage: Usage) -> float:
        """The cost Claude Code reported; before a call, its worst case.

        The worst case is the token price when the model is in the price table (or configured),
        capped by ``--max-budget-usd``, and otherwise that cap.
        """
        if usage.reported_cost_usd is not None:
            return usage.reported_cost_usd
        price = self._price or lookup_claude_price(usage.model or self._model)
        if price is None:
            return self._max_budget_usd
        return min(price.cost(usage), self._max_budget_usd)

    async def health_check(self, *, timeout_s: float) -> ModelHealth:
        """``claude --version`` then ``claude auth status``, without spending tokens.

        A parked mission resumes only when this is healthy, so a logged-out CLI must be DOWN:
        otherwise the mission would resume, fail on authentication and park again. With
        ``ANTHROPIC_API_KEY`` set the CLI authenticates with the key, and the login is not checked;
        a CLI whose ``auth status`` is not JSON (older versions) is judged by ``--version`` alone.
        """
        if shutil.which(self._binary) is None:
            return ModelHealth(False, f"{self.name}: {self._binary!r} not found on PATH")
        code, out = await _run_cli(self._binary, ["--version"], timeout_s)
        if code is None:
            return ModelHealth(False, f"{self.name}: claude --version timed out")
        if code != 0:
            return ModelHealth(False, f"{self.name}: claude --version exited {code}")
        version = out.strip()
        if child_env().get("ANTHROPIC_API_KEY"):
            return ModelHealth(True, f"{self.name}: {version} (API key)")
        code, status = await _run_cli(self._binary, ["auth", "status"], timeout_s)
        try:
            logged_in = json.loads(status).get("loggedIn") if code is not None else None
        except (json.JSONDecodeError, AttributeError):
            logged_in = None
        if logged_in is False:
            return ModelHealth(
                False,
                f"{self.name}: not logged in; run `claude auth login` (or set ANTHROPIC_API_KEY)",
            )
        return ModelHealth(True, f"{self.name}: {version}")


async def _run_cli(binary: str, args: list[str], timeout_s: float) -> tuple[int | None, str]:
    """Run a short, prompt-less CLI command; ``(None, "")`` if it timed out."""
    proc = await asyncio.create_subprocess_exec(
        binary,
        *args,
        stdin=asyncio.subprocess.DEVNULL,
        stdout=asyncio.subprocess.PIPE,
        stderr=asyncio.subprocess.STDOUT,
        env=child_env(),
    )
    try:
        out, _ = await asyncio.wait_for(proc.communicate(), timeout=timeout_s)
    except TimeoutError:
        proc.kill()
        await proc.wait()
        return None, ""
    return proc.returncode, out.decode(errors="replace")

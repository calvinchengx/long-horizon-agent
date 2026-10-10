"""OpenCode (``opencode run``) as an LHA model backend and lead engine.

``opencode run`` executes one headless OpenCode session and streams newline-delimited JSON events
(``--format json``): each model step (``step_start`` / ``step_finish``), every tool call
(``tool_use``) and the assistant text (``text``). ``step_finish`` carries the step's token usage
and its cost in USD, so spend is read from what OpenCode itself reports.

Two uses, sharing the helpers here:

- ``OpenCodeModel`` (``LHA_MODEL_BACKEND=opencode``): one ``complete()`` is one ``opencode run``
  call with an agent whose every tool is denied, so it is a plain text turn. The conversation is
  flattened into the prompt and LHA's tools are described in the system prompt, so the lead replies
  with the JSON actions ``lha.agent.prompt`` asks for and LHA executes them.
- ``lha.agent.opencode_engine`` (``LHA_LEAD_ENGINE=opencode``): a whole lead cycle is one
  ``opencode run`` session with LHA's tools served over MCP (``lha.agent.mcp_bridge``).

Cost: the ledger records the session's cost (``Usage.reported_cost_usd``). The ``--format json``
stream reports usage only for steps that end in a tool call, so the final assistant turn is
missing; the exact totals come from ``opencode session export`` (``session_cost``), falling back to
the streamed step sum. Before a call runs, its worst case is ``LHA_OPENCODE_MAX_BUDGET_USD``, or
what is left of the budget when that is less (``call_budget_usd``). OpenCode has no spend-cap flag,
so the engine kills a session that reaches its cap while streaming; whatever it left in the workdir
is still verified.

Progress: each turn and tool call arrives as it happens; ``SessionProgress`` follows the turns,
the tools called and the spend so far, and ``run_opencode`` calls ``on_progress`` on change.

Failures: a non-zero exit, a stream with no events, or an ``error`` event raises
``OpenCodeError``; a session past ``timeout_s`` is killed and raises ``OpenCodeTimeout``.
"""

from __future__ import annotations

import asyncio
import contextlib
import copy
import json
import os
import shutil
from collections.abc import Callable
from dataclasses import dataclass, field

from lha.contracts.model import ModelMessage, ModelProvider, TurnResult, Usage
from lha.model.claude_code import call_budget_usd, render_transcript
from lha.model.health import ModelHealth
from lha.model.pricing import ModelPrice, lookup_claude_price
from lha.model.retry import Sleep, with_retries

# ``--model`` value meaning "whatever OpenCode would pick" (no flag passed).
DEFAULT_MODEL = ""
# The longest stream-json line read (a tool result can be large; asyncio's default is 64 KiB).
_STREAM_LINE_LIMIT = 64 * 1024 * 1024
_STDERR_TAIL = 2000
# The longest `opencode session export` may take when reading a finished session's totals.
_SESSION_EXPORT_TIMEOUT_S = 30.0
# OpenCode's default agent, used when ``opencode_agent`` names one.
DEFAULT_AGENT = "lha"
# Environment variables that make a child ``opencode`` attach to the parent's session or config.
# Every ``OPENCODE_*`` name is dropped; ``run_opencode``'s caller adds the ones it wants back.
_NESTED_ENV_PREFIX = "OPENCODE_"


class OpenCodeError(RuntimeError):
    """``opencode run`` failed: it could not start, exited non-zero, or streamed an error."""

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
        # A short reason tag (``error_max_budget_usd`` when LHA killed the session at its cap).
        self.subtype = subtype
        # What the failed run still spent, when it got far enough to report it.
        self.usage = usage


class OpenCodeTimeout(TimeoutError):
    """An ``opencode run`` session ran past its timeout and was killed (retryable)."""

    def __init__(self, message: str, *, progress: SessionProgress) -> None:
        super().__init__(message)
        self.progress = progress


@dataclass
class SessionProgress:
    """A running ``opencode run`` session's progress, from its ``--format json`` stream.

    OpenCode reports usage and cost on a step's ``step_finish``, but only for steps that end in a
    tool call: the final assistant turn streams none. ``spent_usd`` is therefore the sum of the
    costs seen so far, and is ``None`` only when no step has reported one (so a session whose spend
    cannot be seen at all is charged its cap, never $0). A finished session's exact totals come
    from ``session_cost``.
    """

    turns: int = 0
    tool_calls: int = 0
    tool: str = ""  # the last tool called
    session_id: str = ""
    # messageID -> the step's usage tokens, filled at ``step_finish``.
    _steps: dict[str, dict[str, object]] = field(default_factory=dict)
    # The costs OpenCode reported per step, in order; their sum is ``spent_usd``.
    _costs: list[float] = field(default_factory=list)
    _tool_ids: set[str] = field(default_factory=set)

    def observe(self, event: dict[str, object]) -> bool:
        """Take one stream event; True when it began a turn or called a tool."""
        session = event.get("sessionID") or event.get("session_id")
        if isinstance(session, str) and session:
            self.session_id = session
        part = event.get("part")
        part = part if isinstance(part, dict) else {}
        kind = event.get("type")
        step_id = part.get("messageID")
        if kind == "step_start":
            if isinstance(step_id, str) and step_id not in self._steps:
                self._steps[step_id] = {}
                self.turns += 1
                return True
            return False
        if kind == "step_finish":
            changed = False
            if isinstance(step_id, str) and step_id not in self._steps:
                self._steps[step_id] = {}
                self.turns += 1
                changed = True
            tokens = part.get("tokens")
            if isinstance(step_id, str):
                self._steps[step_id] = {"tokens": tokens if isinstance(tokens, dict) else {}}
            cost = part.get("cost")
            if isinstance(cost, int | float) and not isinstance(cost, bool):
                self._costs.append(float(cost))
            return changed
        if kind == "tool_use":
            call_id = part.get("id") or part.get("partID")
            if isinstance(call_id, str) and call_id not in self._tool_ids:
                self._tool_ids.add(call_id)
                self.tool_calls += 1
                self.tool = str(part.get("tool") or "")
                return True
        return False

    def usage(self, *, provider: str, fallback_model: str) -> Usage:
        """The tokens so far as one ``Usage``, with ``spent_usd`` as its reported cost."""
        total = Usage(provider=provider, model=fallback_model)
        for step in self._steps.values():
            turn = _step_usage(step, provider=provider)
            total = total.model_copy(
                update={
                    "input_tokens": total.input_tokens + turn.input_tokens,
                    "output_tokens": total.output_tokens + turn.output_tokens,
                    "cache_read_input_tokens": total.cache_read_input_tokens
                    + turn.cache_read_input_tokens,
                    "cache_creation_input_tokens": total.cache_creation_input_tokens
                    + turn.cache_creation_input_tokens,
                }
            )
        return total.model_copy(update={"reported_cost_usd": self.spent_usd})

    @property
    def spent_usd(self) -> float | None:
        if not self._costs:
            return None
        return round(sum(self._costs), 6)


def _step_usage(step: dict[str, object], *, provider: str) -> Usage:
    tokens = step.get("tokens")
    tokens = tokens if isinstance(tokens, dict) else {}
    cache = tokens.get("cache")
    cache = cache if isinstance(cache, dict) else {}
    return Usage(
        input_tokens=_int(tokens.get("input")),
        output_tokens=_int(tokens.get("output")),
        cache_read_input_tokens=_int(cache.get("read")),
        cache_creation_input_tokens=_int(cache.get("write")),
        provider=provider,
    )


@dataclass
class OpenCodeResult:
    """The parsed result of one ``opencode run`` session."""

    text: str
    usage: Usage
    session_id: str | None = None
    num_turns: int = 0
    stop_reason: str | None = None
    raw: list[dict[str, object]] = field(default_factory=list)


def _int(value: object) -> int:
    return int(value) if isinstance(value, int | float) else 0


def child_env(extra: dict[str, str] | None = None) -> dict[str, str]:
    """The environment for a child ``opencode``: ours, minus every ``OPENCODE_*`` marker.

    Dropping them keeps a session started by LHA from attaching to (or inheriting the config of)
    the OpenCode that may be running LHA itself; the caller adds back only the ones it wants.
    """
    env = {k: v for k, v in os.environ.items() if not k.startswith(_NESTED_ENV_PREFIX)}
    env.update(extra or {})
    return env


def base_args(*, model: str, agent: str, standalone: bool) -> list[str]:
    """Flags every LHA ``opencode run`` call shares: streamed JSON, an agent, auto-approval."""
    args = ["run", "--format", "json", "--auto", "--agent", agent]
    if standalone:
        args.append("--standalone")
    if model and model != DEFAULT_MODEL:
        args += ["--model", model]
    return args


async def run_opencode(
    args: list[str],
    *,
    prompt: str,
    binary: str,
    cwd: str | None,
    timeout_s: float,
    provider: str,
    fallback_model: str,
    max_cost_usd: float | None = None,
    env: dict[str, str] | None = None,
    on_progress: Callable[[SessionProgress], None] | None = None,
) -> OpenCodeResult:
    """Run ``binary *args`` with ``prompt`` on stdin and parse its JSON event stream.

    The prompt goes on stdin, never argv, so its size is not limited by the OS. ``on_progress`` is
    called with the session's progress whenever a turn begins or a tool is called. Past ``timeout_s``
    the session is killed and raises ``OpenCodeTimeout``; past ``max_cost_usd`` (its reported spend)
    it is killed and raises ``OpenCodeError`` with subtype ``error_max_budget_usd``.
    """
    try:
        proc = await asyncio.create_subprocess_exec(
            binary,
            *args,
            stdin=asyncio.subprocess.PIPE,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            cwd=cwd,
            env=child_env(env),
            limit=_STREAM_LINE_LIMIT,
        )
    except OSError as exc:
        raise OpenCodeError(
            f"cannot run {binary!r}: {exc}. Install OpenCode or set LHA_OPENCODE_BIN."
        ) from exc
    assert proc.stdin is not None and proc.stdout is not None and proc.stderr is not None
    stdin, stdout, stderr = proc.stdin, proc.stdout, proc.stderr
    progress = SessionProgress()
    raw: list[dict[str, object]] = []
    texts: list[str] = []
    stop_reason: str | None = None
    error_line = ""
    other = ""  # the last output that was not a JSON event
    over_budget = False

    async def _feed() -> None:
        with contextlib.suppress(BrokenPipeError, ConnectionResetError):
            stdin.write(prompt.encode())
            await stdin.drain()
            stdin.close()

    async def _read() -> None:
        nonlocal stop_reason, error_line, other, over_budget
        async for raw_line in stdout:
            line = raw_line.decode(errors="replace").strip()
            if not line:
                continue
            try:
                event = json.loads(line)
            except json.JSONDecodeError:
                other = line[-500:]
                continue
            if not isinstance(event, dict):
                other = line[-500:]
                continue
            raw.append(event)
            kind = event.get("type")
            part = event.get("part")
            part = part if isinstance(part, dict) else {}
            if kind == "text":
                text = part.get("text")
                if isinstance(text, str):
                    texts.append(text)
            elif kind == "error":
                error_line = json.dumps(event.get("error") or event)[-500:]
            elif kind == "step_finish":
                reason = part.get("reason")
                if isinstance(reason, str) and reason:
                    stop_reason = reason
            changed = progress.observe(event)
            if on_progress is not None:
                with contextlib.suppress(Exception):  # observing a session must never fail it
                    on_progress(progress)
            spent = progress.spent_usd
            if max_cost_usd is not None and spent is not None and spent >= max_cost_usd:
                over_budget = True
                proc.kill()
                return
            if not changed:
                continue

    try:
        _, err_bytes, _ = await asyncio.wait_for(
            asyncio.gather(_feed(), stderr.read(), _read()), timeout=timeout_s
        )
        await proc.wait()
    except TimeoutError:
        proc.kill()
        await proc.wait()
        raise OpenCodeTimeout(
            f"opencode run did not finish within {timeout_s:.0f}s", progress=progress
        ) from None
    if over_budget:
        raise OpenCodeError(
            f"opencode run failed: error_max_budget_usd (reached its ${max_cost_usd:.4f} cap)",
            subtype="error_max_budget_usd",
            usage=progress.usage(provider=provider, fallback_model=fallback_model),
        )
    if not raw:
        err = err_bytes.decode(errors="replace")[-_STDERR_TAIL:]
        raise OpenCodeError(f"opencode run exited {proc.returncode}: {err or other or 'no output'}")
    if error_line:
        raise OpenCodeError(
            f"opencode run failed: {error_line}", retryable=_is_transient(error_line)
        )
    # The stream omits the final assistant turn's usage, so read the session's exact totals.
    usage = progress.usage(provider=provider, fallback_model=fallback_model)
    if progress.session_id:
        exported = await session_cost(binary, progress.session_id)
        if exported is not None:
            usage = exported_usage(exported, provider=provider, fallback_model=fallback_model)
    return OpenCodeResult(
        text="\n".join(texts),
        usage=usage,
        session_id=progress.session_id or None,
        num_turns=progress.turns,
        stop_reason=stop_reason,
        raw=raw,
    )


async def session_cost(
    binary: str, session_id: str, *, timeout_s: float = _SESSION_EXPORT_TIMEOUT_S
) -> tuple[float, dict[str, object]] | None:
    """The exact cost and token totals of a finished session, from ``opencode session export``.

    The ``--format json`` stream reports usage only on the ``step_finish`` of a step that ended in
    a tool call, so a session's final assistant turn is missing from the streamed sum. The export
    carries the session's totals; ``None`` when it cannot be read (no such session, a non-zero
    exit, timed out, or no ``cost`` field).
    """
    try:
        proc = await asyncio.create_subprocess_exec(
            binary,
            "session",
            "export",
            session_id,
            stdin=asyncio.subprocess.DEVNULL,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            env=child_env(),
        )
    except OSError:
        return None
    try:
        out, _ = await asyncio.wait_for(proc.communicate(), timeout=timeout_s)
    except TimeoutError:
        proc.kill()
        await proc.wait()
        return None
    if proc.returncode != 0:
        return None
    try:
        data = json.loads(out.decode(errors="replace"))
    except json.JSONDecodeError:
        return None
    info = data.get("info") if isinstance(data, dict) else None
    if not isinstance(info, dict):
        return None
    cost = info.get("cost")
    if not isinstance(cost, int | float) or isinstance(cost, bool):
        return None
    tokens = info.get("tokens")
    return float(cost), tokens if isinstance(tokens, dict) else {}


def exported_usage(
    exported: tuple[float, dict[str, object]], *, provider: str, fallback_model: str
) -> Usage:
    """An exported session's totals as one ``Usage`` (reasoning counts as billed output)."""
    cost, tokens = exported
    cache = tokens.get("cache")
    cache = cache if isinstance(cache, dict) else {}
    return Usage(
        input_tokens=_int(tokens.get("input")),
        output_tokens=_int(tokens.get("output")) + _int(tokens.get("reasoning")),
        cache_read_input_tokens=_int(cache.get("read")),
        cache_creation_input_tokens=_int(cache.get("write")),
        model=fallback_model,
        provider=provider,
        reported_cost_usd=round(cost, 6),
    )


def _is_transient(text: str) -> bool:
    lowered = text.lower()
    return any(marker in lowered for marker in _TRANSIENT_MARKERS)


_TRANSIENT_MARKERS = ("rate limit", "rate_limit", "overloaded", "529", "timeout", "timed out")


class OpenCodeModel(ModelProvider):
    """A ``ModelProvider`` that runs each turn as one tool-less ``opencode run`` call."""

    def __init__(
        self,
        *,
        model_name: str = DEFAULT_MODEL,
        binary: str = "opencode",
        agent: str = "lha-model",
        cwd: str | None = None,
        standalone: bool = True,
        max_budget_usd: float = 5.0,
        timeout_s: float = 3600.0,
        price: ModelPrice | None = None,
        max_retries: int = 3,
        retry_base_delay_s: float = 2.0,
        sleep: Sleep = asyncio.sleep,
    ) -> None:
        self.name = f"opencode:{model_name or 'default'}"
        self._model = model_name
        self._binary = binary
        self._agent = agent
        self._cwd = cwd
        self._standalone = standalone
        self._max_budget_usd = max_budget_usd
        self._timeout_s = timeout_s
        self._price = price
        self._max_retries = max_retries
        self._retry_base_delay_s = retry_base_delay_s
        self._sleep = sleep

    def _config(self, system: str) -> dict[str, object]:
        """An agent whose every tool is denied: a plain text turn, no MCP server."""
        return {
            "agents": {
                self._agent: {
                    "description": "LHA model turn (no tools)",
                    "mode": "primary",
                    "system": system,
                    "permissions": [{"action": "*", "resource": "*", "effect": "deny"}],
                }
            }
        }

    async def complete(
        self,
        messages: list[ModelMessage],
        *,
        tools: list[dict[str, object]] | None = None,
        max_tokens: int | None = None,
    ) -> TurnResult:
        # ``tools`` is ignored: native tool calling needs MCP (the opencode lead engine); here the
        # tools are described in the system prompt and the reply is a JSON action.
        system = "\n\n".join(m.content for m in messages if m.role == "system")

        async def _run() -> OpenCodeResult:
            with _config_file(self._config(system)) as cfg_path:
                return await run_opencode(
                    base_args(model=self._model, agent=self._agent, standalone=self._standalone),
                    prompt=render_transcript(messages),
                    binary=self._binary,
                    cwd=self._cwd,
                    timeout_s=self._timeout_s,
                    provider=self.name,
                    fallback_model=self._model,
                    max_cost_usd=self._max_budget_usd,
                    env=_injected_env(cfg_path),
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

    def budget_capped(self, remaining_usd: float) -> OpenCodeModel:
        """This model with its per-call cap lowered to what is left of the budget (see
        ``call_budget_usd``); the metered wrapper asks for it before each call."""
        capped = copy.copy(self)
        capped._max_budget_usd = call_budget_usd(self._max_budget_usd, remaining_usd)
        return capped

    def estimate_cost_usd(self, usage: Usage) -> float:
        """The cost OpenCode reported; before a call, its worst case.

        The worst case is the token price when the model is in the price table (or configured),
        capped by ``LHA_OPENCODE_MAX_BUDGET_USD``, and otherwise that cap.
        """
        if usage.reported_cost_usd is not None:
            return usage.reported_cost_usd
        price = self._price or lookup_claude_price(usage.model or self._model)
        if price is None:
            return self._max_budget_usd
        return min(price.cost(usage), self._max_budget_usd)

    async def health_check(self, *, timeout_s: float) -> ModelHealth:
        """``opencode --version``, without spending tokens.

        A parked mission resumes only when this is healthy, so a missing CLI is DOWN.
        """
        if shutil.which(self._binary) is None:
            return ModelHealth(False, f"{self.name}: {self._binary!r} not found on PATH")
        code, out = await _run_cli(self._binary, ["--version"], timeout_s)
        if code is None:
            return ModelHealth(False, f"{self.name}: opencode --version timed out")
        if code != 0:
            return ModelHealth(False, f"{self.name}: opencode --version exited {code}")
        return ModelHealth(True, f"{self.name}: {out.strip()}")


@contextlib.contextmanager
def _config_file(config: dict[str, object]):
    """The injected OpenCode config as a temporary file, removed when the block exits."""
    import tempfile

    with tempfile.TemporaryDirectory(prefix="lha-opencode-") as tmp:
        path = os.path.join(tmp, "opencode.json")
        with open(path, "w", encoding="utf-8") as handle:
            json.dump(config, handle)
        yield path


def _injected_env(cfg_path: str) -> dict[str, str]:
    """The environment that points a child ``opencode`` at our config and nothing else."""
    return {"OPENCODE_CONFIG": cfg_path, "OPENCODE_DISABLE_PROJECT_CONFIG": "true"}


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

"""Opt-in mutation gate: an item's tests must catch deliberate bugs in the code it changed.

A gate of ruff + ty + pytest proves the tests pass, not that they test anything: a model that
writes the code and its tests in one cycle can write tests that run the code and assert nothing.
With ``LHA_MUTATION_CHECK`` set, ``MutationGateVerifier`` runs that command (``sh -c``, in the
sandbox, like a ``cmd:`` witness) after every gating check passed, and only then: it mutates the
changed code and re-runs the tests, so it is only meaningful, and only worth its cost, on a green
tree. Exit 0 passes; any other exit keeps the item red, and the command's output tail (its
surviving mutants) is what the model reads in the failure report, e.g. "changing ``>`` to ``>=``
at parser.py:42 breaks no test".

The command is the operator's and names the tool, so the gate is language-neutral. It gets the
files the item changed (``git diff HEAD`` plus untracked files, outside ``.lha/``) in
``LHA_CHANGED_FILES``, one per line, to scope the run, e.g. ``gremlins unleash --diff HEAD``.
With no changed files it does not run. Like a quarantined check, it can keep an item red and never
make it green; it is not re-run as a possible flake.
"""

from __future__ import annotations

import asyncio
from pathlib import Path

from lha.contracts.sandbox import SandboxSession
from lha.contracts.state import EventRecord
from lha.contracts.verify import Check, CheckResult, VerificationResult, Verifier
from lha.state import git_ops
from lha.verify.verifier import clip_output_tail

#: The gate's check name in verdicts and failure reports.
MUTATION_CHECK_NAME = "mutation"
#: The environment variable carrying the changed files to the command.
CHANGED_FILES_ENV = "LHA_CHANGED_FILES"


def changed_files(workdir: str | Path) -> list[str]:
    """The files changed in ``workdir``'s work tree against ``HEAD`` (tracked and untracked, not
    ignored), outside the harness-owned ``.lha/``, sorted. Raises ``GitError``."""
    names: set[str] = set()
    for args in (
        ("diff", "--name-only", "-z", "HEAD"),
        ("ls-files", "--others", "--exclude-standard", "-z"),
    ):
        proc = git_ops.run_git_bytes(workdir, *args)
        if proc.returncode != 0:
            raise git_ops.GitError(
                f"git {' '.join(args)} failed ({proc.returncode}): "
                f"{proc.stderr.decode('utf-8', 'replace').strip()}"
            )
        names.update(n for n in proc.stdout.decode("utf-8", "surrogateescape").split("\0") if n)
    return sorted(n for n in names if n != ".lha" and not n.startswith(".lha/"))


class MutationGateVerifier:
    """A ``Verifier`` that adds the ``mutation`` check to a green verdict (see the module doc)."""

    def __init__(
        self, inner: Verifier, *, command: str, workdir: str | Path, timeout_s: int = 1800
    ) -> None:
        self._inner = inner
        self._command = command
        self._workdir = workdir
        self._timeout = timeout_s

    def drain_events(self) -> list[EventRecord]:
        drain = getattr(self._inner, "drain_events", None)
        return list(drain()) if drain is not None else []

    async def verify(self, session: SandboxSession, checks: list[Check]) -> VerificationResult:
        verdict = await self._inner.verify(session, checks)
        if verdict.verdict != "passed":
            return verdict  # red or unverified: mutating failing or untested code tells nothing
        try:
            changed = await asyncio.to_thread(changed_files, self._workdir)
        except git_ops.GitError as exc:  # fail closed: configured, and cannot tell what changed
            return verdict.with_results(
                [_failed(f"[verifier] mutation check: cannot list the changed files: {exc}")]
            )
        if not changed:
            return verdict
        return verdict.with_results([await self._run(session, changed)])

    async def _run(self, session: SandboxSession, changed: list[str]) -> CheckResult:
        try:
            outcome = await session.exec(
                ["sh", "-c", self._command],
                timeout_s=self._timeout,
                env={CHANGED_FILES_ENV: "\n".join(changed)},
            )
        except Exception as exc:  # a sandbox failure is a failed check, never a pass
            return _failed(f"[verifier] could not execute check: {type(exc).__name__}: {exc}")
        return CheckResult(
            name=MUTATION_CHECK_NAME,
            passed=outcome.ok,
            exit_code=outcome.exit_code,
            timed_out=outcome.timed_out,
            output_tail=clip_output_tail(outcome.stdout, outcome.stderr),
        )


def _failed(message: str) -> CheckResult:
    return CheckResult(name=MUTATION_CHECK_NAME, passed=False, exit_code=-1, output_tail=message)

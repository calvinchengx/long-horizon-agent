"""Flaky-check quarantine, wired into every run path's verifier (``FlakyRetryVerifier``).

A flaky check makes "green" unreliable in both directions: it can fail good work, and a pass
from it is weak evidence. ``FlakyRetryVerifier`` wraps the lead's verifier
(``lha.agent.assembly.lead_verifier``) and applies these rules, in this order:

1. **Re-run on failure.** A gating check that fails (and did not time out) is re-run, on the same
   work tree, up to ``LHA_FLAKY_RETRIES`` more times (default 1), stopping at the first pass.
   ``LHA_FLAKY_RETRIES=0`` turns re-runs and quarantine off: the first result stands.
2. **Evidence before quarantine.** A check is quarantined only when it both passed and failed on
   the SAME revision (the tree id of the work tree, including uncommitted changes). Failing on
   one revision and passing on another is a change in behaviour, not a flake.
3. **A consistent failure always gates.** A check (quarantined or not) that fails every attempt
   on the revision under test stays a failing gating check: the item is red.
4. **A quarantined check is never the evidence for green.** Once quarantined, its results are
   recorded as NON-gating (a pass included), so an item needs at least one other gating check to
   pass; with none it is ``unverified``, never done. Together with rule 3, a quarantined check
   can keep an item red but can never make it green: it must still pass at least once on the
   revision under test, and something else must gate.
5. **Quarantine is recorded and lasts for the mission.** Quarantining emits a
   ``check_quarantined`` event (check, revision, passes, fails) that the agent loop commits to
   ``.lha/events.ndjson`` with the cycle's checkpoint; a quarantined check failing every attempt
   emits ``quarantined_check_failed``. Later cycles, including durable ones in another process,
   read the quarantined set from the COMMITTED event log at ``HEAD`` (the agent's uncommitted
   edits to ``.lha/`` are ignored). Nothing lifts a quarantine automatically.

``timed_out`` results are never re-run: a timeout costs the full timeout again and is treated as
a real failure. Witness checks follow the same rules as mission checks.
"""

from __future__ import annotations

import asyncio
from collections import defaultdict

import structlog

from lha.contracts.sandbox import SandboxSession
from lha.contracts.state import EventRecord
from lha.contracts.verify import (
    Check,
    CheckResult,
    VerificationResult,
    Verifier,
    ensure_unique_check_names,
)
from lha.state import git_ops

QUARANTINE_EVENT = "check_quarantined"
QUARANTINED_FAILURE_EVENT = "quarantined_check_failed"
_EVENTS_PATH = ".lha/events.ndjson"

_log = structlog.get_logger("lha.verify")


class FlakeEvidenceError(ValueError):
    """Raised when quarantining a check that has no recorded flake history."""


class FlakyQuarantine:
    """Per-revision pass/fail history and the set of quarantined checks."""

    def __init__(self, *, min_flips: int = 1, min_runs: int = 2) -> None:
        # Per revision, a check must have passed >= min_flips and failed >= min_flips times,
        # across >= min_runs total runs, to count as flaky.
        self._min_flips = max(1, min_flips)
        self._min_runs = max(2, min_runs)
        self._history: dict[str, dict[str, list[int]]] = defaultdict(dict)
        self._flaky: set[str] = set()

    def record(self, name: str, *, revision: str, passed: bool) -> None:
        """Record one observed outcome of check ``name`` on code ``revision`` (a tree id)."""
        counts = self._history[name].setdefault(revision, [0, 0])
        counts[0 if passed else 1] += 1

    def counts(self, name: str, revision: str) -> tuple[int, int]:
        """(passes, fails) of ``name`` on ``revision``."""
        passes, fails = self._history.get(name, {}).get(revision, [0, 0])
        return passes, fails

    def has_flake_evidence(self, name: str) -> bool:
        for passes, fails in self._history.get(name, {}).values():
            if (
                passes >= self._min_flips
                and fails >= self._min_flips
                and passes + fails >= self._min_runs
            ):
                return True
        return False

    def mark_flaky(self, name: str) -> None:
        """Quarantine ``name``; raises ``FlakeEvidenceError`` without recorded flake history."""
        if not self.has_flake_evidence(name):
            raise FlakeEvidenceError(
                f"refusing to quarantine {name!r}: no recorded pass+fail on the same revision "
                f"(need >= {self._min_flips} of each over >= {self._min_runs} runs)"
            )
        self._flaky.add(name)

    def restore(self, names: set[str]) -> None:
        """Re-apply quarantines recorded earlier (committed ``check_quarantined`` events)."""
        self._flaky.update(names)

    def is_flaky(self, name: str) -> bool:
        return name in self._flaky


def committed_quarantine(workdir: str) -> set[str]:
    """Check names quarantined by ``check_quarantined`` events committed at ``HEAD``."""
    try:
        raw = git_ops.run_git(workdir, "show", f"HEAD:{_EVENTS_PATH}")
    except git_ops.GitError:
        return set()
    names: set[str] = set()
    for line in raw.splitlines():
        if QUARANTINE_EVENT not in line:
            continue
        try:
            event = EventRecord.model_validate_json(line)
        except ValueError:
            continue
        check = event.payload.get("check")
        if event.kind == QUARANTINE_EVENT and isinstance(check, str):
            names.add(check)
    return names


def tree_revision(workdir: str) -> str:
    """The git tree id of ``workdir``'s CURRENT work tree (uncommitted changes included)."""
    from lha.verify.trusted import candidate_commit

    commit = candidate_commit(workdir, message="lha: flake revision")
    return git_ops.run_git(workdir, "rev-parse", f"{commit}^{{tree}}")


class FlakyRetryVerifier:
    """A ``Verifier`` that re-runs failing gating checks and quarantines proven flakes.

    ``workdir`` is the host checkout: the revision is its tree id and the committed quarantine
    is read from its ``HEAD``. Without it (tests), revisions are per-``verify`` call and nothing
    is read from git. Events for the checkpoint are collected until ``drain_events``.
    """

    def __init__(self, inner: Verifier, *, retries: int = 1, workdir: str | None = None) -> None:
        self._inner = inner
        self._retries = max(0, retries)
        self._workdir = workdir
        self.quarantine = FlakyQuarantine()
        self._events: list[EventRecord] = []
        self._calls = 0
        self._restored = False

    def drain_events(self) -> list[EventRecord]:
        events, self._events = self._events, []
        return events

    async def verify(self, session: SandboxSession, checks: list[Check]) -> VerificationResult:
        checks = ensure_unique_check_names(checks, reserved=())
        first = await self._inner.verify(session, checks)
        if self._retries == 0:
            return first
        if self._workdir is not None and not self._restored:
            # Once per verifier: the committed log grows every cycle, and every quarantine
            # decided after this point is this verifier's own.
            self.quarantine.restore(await asyncio.to_thread(committed_quarantine, self._workdir))
            self._restored = True
        by_name = {check.name: check for check in checks}
        revision: str | None = None
        results: list[CheckResult] = []
        for result in first.results:
            check = by_name.get(result.name)
            if check is None or not result.gating:
                results.append(result)
                continue
            quarantined = self.quarantine.is_flaky(result.name)
            if result.passed:
                results.append(_non_gating(result, "quarantined") if quarantined else result)
                continue
            if result.timed_out:  # never re-run; a failure that gates (rule 3)
                results.append(result)
                continue
            revision = revision or await self._revision()
            results.append(await self._retry(session, check, result, revision, quarantined))
        return VerificationResult.from_results(results)

    async def _retry(
        self,
        session: SandboxSession,
        check: Check,
        failed: CheckResult,
        revision: str,
        quarantined: bool,
    ) -> CheckResult:
        """Re-run ``check`` after ``failed`` and apply the rules (see the module docstring)."""
        name = check.name
        self.quarantine.record(name, revision=revision, passed=False)
        passing: CheckResult | None = None
        for _ in range(self._retries):
            rerun = (await self._inner.verify(session, [check])).results
            if not rerun:
                break
            outcome = rerun[0]
            self.quarantine.record(name, revision=revision, passed=outcome.passed)
            if outcome.passed:
                passing = outcome
                break
        passes, fails = self.quarantine.counts(name, revision)
        payload: dict[str, object] = {
            "check": name,
            "revision": revision,
            "passes": passes,
            "fails": fails,
        }
        if passing is None:
            if quarantined:  # rule 3: a consistent failure gates, quarantine or not
                self._event(QUARANTINED_FAILURE_EVENT, payload)
                return failed.model_copy(
                    update={
                        "output_tail": (
                            f"[flaky] {name} is quarantined but failed all {fails} attempt(s) "
                            f"on this revision, so it gates\n{failed.output_tail}"
                        )
                    }
                )
            return failed
        if not quarantined:
            if not self.quarantine.has_flake_evidence(name):  # pragma: no cover - min_runs is 2
                return failed
            self.quarantine.mark_flaky(name)
            self._event(QUARANTINE_EVENT, payload)
        return _non_gating(
            passing,
            f"quarantined: {passes} pass(es) and {fails} failure(s) on revision {revision[:12]}",
        )

    async def _revision(self) -> str:
        self._calls += 1
        if self._workdir is not None:
            try:
                return await asyncio.to_thread(tree_revision, self._workdir)
            except Exception as exc:  # no git: evidence only within this verify call
                _log.warning("flake_revision_unavailable", error=f"{type(exc).__name__}: {exc}")
        return f"call-{id(self)}-{self._calls}"

    def _event(self, kind: str, payload: dict[str, object]) -> None:
        _log.warning(kind, **payload)
        self._events.append(EventRecord(kind=kind, payload=payload))


def _non_gating(result: CheckResult, note: str) -> CheckResult:
    return result.model_copy(
        update={"gating": False, "output_tail": f"[flaky] {note}; non-gating\n{result.output_tail}"}
    )

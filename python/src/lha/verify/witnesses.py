"""Witnesses: an item's own acceptance checks, written as short strings on the checklist.

A mission-wide gate (ruff, ty, pytest) proves the repository still builds; it does not prove a
particular item was delivered. ``ChecklistItem.witnesses`` closes that gap: each witness names a
deterministic check that must pass before the item can flip to ``done``. The vocabulary follows
fabric-emulator's ``docs/witnesses.json``:

* ``go:TestName`` / ``go:TestName@<pkgpattern>`` — a Go test (default pattern ``./...``). Plain
  ``go test -run X`` exits 0 with "no tests to run" when ``X`` does not exist, so the check also
  requires a ``--- PASS: TestName`` line: a missing (or skipped) test is a FAILED witness.
  Subtests are written ``TestX/sub``.
* ``pytest:<node id>`` — ``uv run pytest -q <node id>`` (pytest exits 5 when nothing is
  collected, so a missing test fails).
* ``cmd:<shell command>`` — ``sh -c <command>``, authored by the operator.
* ``trusted:<name>`` (alias ``ci:<name>``) — an operator-defined command from the trusted map
  (``LHA_TRUSTED_CHECKS``), run OUTSIDE the sandbox by a ``TrustedRunner``; see
  ``lha.verify.trusted``. Items can only reference trusted commands by name, never define them.

``validate_witness`` checks syntax only (used when importing a checklist); ``parse_witness`` /
``item_checks`` turn witnesses into ``Check`` objects named after the witness string itself.
"""

from __future__ import annotations

import re
import shlex
from collections.abc import Mapping, Sequence

from lha.contracts.state import ChecklistItem
from lha.contracts.verify import Check, ensure_unique_check_names

SCHEMES: tuple[str, ...] = ("go", "pytest", "cmd", "trusted", "ci")
DEFAULT_GO_PACKAGES = "./..."

_GO_SEGMENT_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")
# Package patterns: import paths / relative dirs / ``...`` wildcards. No shell metacharacters, and
# no leading ``-`` (it would be read as a ``go test`` flag).
_GO_PACKAGES_RE = re.compile(r"^[A-Za-z0-9_./~+][A-Za-z0-9_./~+-]*$")
# pytest node ids: paths, ``::`` separators and parametrize ids (``test_x[a-1]``).
_PYTEST_NODE_RE = re.compile(r"^[A-Za-z0-9_./:\[\]=,+][A-Za-z0-9_./:\[\]=,+-]*$")
_TRUSTED_NAME_RE = re.compile(r"^[A-Za-z0-9_][A-Za-z0-9_.-]*$")
_WHITESPACE_RE = re.compile(r"\s+")
_MAX_NAME_CHARS = 80


class UnknownTrustedCheck(ValueError):
    """A ``trusted:<name>`` / ``ci:<name>`` witness names a command the operator never defined."""


def _split(witness: str) -> tuple[str, str]:
    text = witness.strip()
    scheme, sep, rest = text.partition(":")
    if not sep or scheme not in SCHEMES:
        raise ValueError(
            f"witness {witness!r}: unknown kind; expected one of "
            + ", ".join(f"{s}:" for s in SCHEMES)
        )
    return scheme, rest.strip()


def _go_parts(witness: str, rest: str) -> tuple[str, str]:
    test, _, packages = rest.partition("@")
    test, packages = test.strip(), packages.strip() or DEFAULT_GO_PACKAGES
    if not test or not all(_GO_SEGMENT_RE.match(seg) for seg in test.split("/")):
        raise ValueError(
            f"witness {witness!r}: Go test name must be an identifier like TestName or "
            "TestName/subtest"
        )
    if not _GO_PACKAGES_RE.match(packages):
        raise ValueError(
            f"witness {witness!r}: package pattern {packages!r} contains characters that are "
            "not allowed (use an import path or a pattern like ./internal/...)"
        )
    return test, packages


def validate_witness(witness: str) -> None:
    """Check a witness string's syntax (no trusted map needed); raise ``ValueError`` if invalid."""
    scheme, rest = _split(witness)
    if scheme == "go":
        _go_parts(witness, rest)
    elif scheme == "pytest":
        if not _PYTEST_NODE_RE.match(rest):
            raise ValueError(
                f"witness {witness!r}: pytest node id must be a path or node id without "
                "whitespace, shell metacharacters or a leading '-'"
            )
    elif scheme == "cmd":
        if not rest:
            raise ValueError(f"witness {witness!r}: cmd: needs a shell command")
    elif not _TRUSTED_NAME_RE.match(rest):
        raise ValueError(
            f"witness {witness!r}: {scheme}: needs the name of an operator-defined trusted check"
        )


def _go_test_run(test: str, packages: str) -> str:
    run_regex = "/".join(f"^{seg}$" for seg in test.split("/"))
    return f"go test -count=1 -run {shlex.quote(run_regex)} -v {shlex.quote(packages)}"


def witness_command(witness: str) -> str | None:
    """The command that runs a witness in the sandbox, as the model would type it.

    ``None`` for a ``trusted:``/``ci:`` witness (it runs outside the sandbox) or a malformed one.
    Shown in the lead's prompt: without it, models guessed ``python -m pytest``, which fails where
    pytest is installed only in the project's environment.
    """
    try:
        validate_witness(witness)
    except ValueError:
        return None
    scheme, rest = _split(witness)
    if scheme == "go":
        return _go_test_run(*_go_parts(witness, rest))
    if scheme == "pytest":
        return f"uv run pytest -q {rest}"
    if scheme == "cmd":
        return rest
    return None


def go_test_command(test: str, packages: str = DEFAULT_GO_PACKAGES) -> list[str]:
    """argv that passes only if ``go test`` exits 0 AND reports ``--- PASS: <test>``."""
    pass_regex = f"^[[:space:]]*--- PASS: {test}( |$)"
    go_cmd = _go_test_run(test, packages)
    missing = f"witness go:{test}: the test did not run and pass (missing, skipped or filtered)"
    script = (
        f"out=$({go_cmd} 2>&1); rc=$?; "
        'printf "%s\\n" "$out"; '
        'if [ "$rc" -ne 0 ]; then exit "$rc"; fi; '
        f'if printf "%s\\n" "$out" | grep -Eq {shlex.quote(pass_regex)}; then exit 0; fi; '
        f"echo {shlex.quote(missing)} >&2; exit 1"
    )
    return ["sh", "-c", script]


def _check_name(witness: str) -> str:
    name = _WHITESPACE_RE.sub(" ", witness.strip())
    return name if len(name) <= _MAX_NAME_CHARS else name[: _MAX_NAME_CHARS - 3] + "..."


def parse_witness(witness: str, trusted: Mapping[str, list[str]]) -> Check:
    """Turn one witness string into a gating ``Check`` named after the witness.

    Raises ``ValueError`` on bad syntax and ``UnknownTrustedCheck`` when a ``trusted:``/``ci:``
    witness names a command missing from ``trusted``.
    """
    validate_witness(witness)
    scheme, rest = _split(witness)
    name = _check_name(witness)
    if scheme == "go":
        test, packages = _go_parts(witness, rest)
        return Check(name=name, command=go_test_command(test, packages))
    if scheme == "pytest":
        return Check(name=name, command=["uv", "run", "pytest", "-q", rest])
    if scheme == "cmd":
        return Check(name=name, command=["sh", "-c", rest])
    argv = trusted.get(rest)
    if not argv:
        known = ", ".join(sorted(trusted)) or "(none defined; set LHA_TRUSTED_CHECKS)"
        raise UnknownTrustedCheck(
            f"witness {witness!r}: no trusted check named {rest!r}; known: {known}"
        )
    return Check(name=name, command=list(argv), where="trusted")


def item_checks(item: ChecklistItem, trusted: Mapping[str, list[str]]) -> list[Check]:
    """The item's witnesses as uniquely-named ``Check``s, in declaration order."""
    return ensure_unique_check_names(parse_witness(w, trusted) for w in item.witnesses)


_PATH_TOKEN_RE = re.compile(r"^[A-Za-z0-9_.][A-Za-z0-9_./-]*$")
_SCRIPT_SUFFIXES = (".sh", ".bash", ".py", ".js", ".ts", ".mjs", ".cjs", ".rb", ".pl", ".ps1")


def witness_paths(witnesses: Sequence[str]) -> tuple[str, ...]:
    """Repository-relative files an item's witnesses run, to protect like harness files.

    A ``cmd:`` witness that runs a committed script (``cmd:sh measure/check.sh``) or a
    ``pytest:`` node id's file proves the item only while the agent cannot rewrite it: a
    measurement mission edited its witness scripts to point at tests it chose. Tokens of a
    ``cmd:`` command that look like relative paths (a ``/`` or a script suffix, no option dash,
    no shell metacharacter) and the file part of a ``pytest:`` node id are returned as globs
    for ``snapshot_harness``; whether they exist is the snapshot's business.
    """
    out: list[str] = []
    for witness in witnesses:
        try:
            scheme, rest = _split(witness)
        except ValueError:
            continue
        if scheme == "pytest":
            path = rest.split("::", 1)[0].strip()
            if path and path not in out:
                out.append(path)
        elif scheme == "cmd":
            try:
                tokens = shlex.split(rest, posix=True)
            except ValueError:
                continue
            for token in tokens:
                token = token.removeprefix("./")
                if (
                    _PATH_TOKEN_RE.match(token)
                    and ("/" in token or token.endswith(_SCRIPT_SUFFIXES))
                    and not token.startswith("-")
                    and token not in out
                ):
                    out.append(token)
    return tuple(out)

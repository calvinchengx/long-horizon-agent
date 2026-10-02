"""The pre-review diff screen: deterministic rules that spot weakened tests in a diff.

The reviewer judges a verified item's ``base..head`` diff. Before it does, this screen reads the
same diff for the ways a change can pass the gate by weakening it rather than by doing the work:
a deleted test file, a removed test, a newly skipped test, assertions taken out of a test, or a
lowered coverage floor. Each hit is one finding (``screen_diff``). The findings never make an
item green or skip the reviewer; they only force the full review when the organization would
have skipped it (``lha orchestrate --no-review``) and go to the reviewer as criteria to confirm.
Both implementations produce the same findings for the same diff (``spec/verify/review_screen.json``).
"""

from __future__ import annotations

import re

from lha.contracts.state import EventRecord
from lha.verify.harness_integrity import is_harness_config

#: The anchor event each screened diff is recorded as (``lha labels export`` joins it to the
#: review verdict).
REVIEW_SCREEN_EVENT = "review_screen"

#: At most this many findings are kept (a diff that trips the screen everywhere needs no more).
MAX_FINDINGS = 20

_TEST_DIRS = frozenset({"tests", "test", "__tests__", "spec", "testdata"})
_TEST_FILE_RE = re.compile(
    r"^(test_.*\.py|.*_test\.py|.*_test\.go|.*\.(test|spec)\.[cm]?[jt]sx?|conftest\.py)$"
)
_TEST_DEF_RE = re.compile(
    r"^(?:async\s+)?def\s+(test_\w+)\s*\(|^func\s+(Test\w+)\s*\(|^\s*(?:it|test)\(\s*['\"](.+?)['\"]"
)
_SKIP_RE = re.compile(
    r"@pytest\.mark\.(skip|xfail)|pytest\.(skip|xfail)\(|unittest\.(skip|expectedFailure)|@skip\b"
    r"|\bt\.Skip(?:f|Now)?\(|testing\.Short\(\)|\b(?:it|test|describe)\.skip\(|\bx(?:it|test|describe)\("
)
_ASSERT_RE = re.compile(
    r"^\s*assert\b|self\.assert\w*\(|\bt\.(Fatal|Error|Fail)\w*\(|\b(require|assert)\.\w+\(|\bexpect\("
)
_FLOOR_RE = re.compile(r"(fail[_-]under)\s*[=:]\s*(\d+(?:\.\d+)?)")


def is_test_path(path: str) -> bool:
    """Whether ``path`` (repo-relative, ``/``-separated) is a test file."""
    parts = path.split("/")
    return any(part in _TEST_DIRS for part in parts[:-1]) or bool(_TEST_FILE_RE.match(parts[-1]))


def _path_of(header: str) -> str:
    # "+++ b/x/y.py" / "--- a/x/y.py" / "+++ /dev/null"; git may add "\t<mtime>" after the path.
    rest = header[4:].split("\t", 1)[0].strip()
    if rest == "/dev/null":
        return ""
    return rest[2:] if rest[:2] in ("a/", "b/") else rest


class _File:
    def __init__(self, old: str, new: str) -> None:
        self.old, self.new = old, new
        self.removed_tests: list[str] = []
        self.added_tests: set[str] = set()
        self.skips: list[str] = []
        self.asserts_removed = 0
        self.asserts_added = 0
        self.floor_old: str | None = None
        self.floor_new: str | None = None
        self.floor_key = ""

    @property
    def path(self) -> str:
        return self.new or self.old

    def findings(self) -> list[str]:
        out: list[str] = []
        if self.old and not self.new and is_test_path(self.old):
            return [f"deleted test file {self.old}"]
        if not self.old and self.new and is_harness_config(self.new):
            out.append(f"added harness file {self.new}")
        if is_test_path(self.path):
            for name in self.removed_tests:
                if name not in self.added_tests:
                    out.append(f"removed test {name} from {self.path}")
            out.extend(f"added skip to {self.path}: {line}" for line in self.skips)
            if (net := self.asserts_removed - self.asserts_added) > 0:
                plural = "s" if net != 1 else ""
                out.append(f"{net} assertion{plural} removed from {self.path}")
        if (
            self.floor_old is not None
            and self.floor_new is not None
            and float(self.floor_new) < float(self.floor_old)
        ):
            out.append(
                f"lowered {self.floor_key} from {self.floor_old} to {self.floor_new} in {self.path}"
            )
        return out


def screen_diff(diff: str) -> list[str]:
    """The weakened-test findings in a unified diff (``[]`` when it looks clean)."""
    files: list[_File] = []
    current: _File | None = None
    old = ""
    for raw in diff.splitlines():
        if raw.startswith("--- ") and not raw.startswith("---  "):
            old = _path_of(raw)
            continue
        if raw.startswith("+++ "):
            current = _File(old, _path_of(raw))
            files.append(current)
            old = ""
            continue
        if current is None or len(raw) < 2 or raw[0] not in "+-" or raw.startswith(("+++", "---")):
            continue
        sign, line = raw[0], raw[1:]
        stripped = line.strip()
        if m := _TEST_DEF_RE.match(line):
            name = next(g for g in m.groups() if g)
            if sign == "-":
                current.removed_tests.append(name)
            else:
                current.added_tests.add(name)
        if sign == "+" and _SKIP_RE.search(line):
            current.skips.append(stripped[:120])
        if _ASSERT_RE.search(line):
            if sign == "-":
                current.asserts_removed += 1
            else:
                current.asserts_added += 1
        if m := _FLOOR_RE.search(line):
            current.floor_key = m.group(1)
            if sign == "-" and current.floor_old is None:
                current.floor_old = m.group(2)
            elif sign == "+" and current.floor_new is None:
                current.floor_new = m.group(2)
    out: list[str] = []
    for file in files:
        out.extend(file.findings())
    return out[:MAX_FINDINGS]


def screen_criteria(findings: list[str], criteria: str) -> str:
    """The reviewer's criteria with the screen's findings in front (unchanged when none)."""
    if not findings:
        return criteria
    listed = "\n".join(f"- {f}" for f in findings)
    return (
        "Pre-review screen: this diff weakens or removes tests. Each of these is BLOCKING "
        f"unless the item itself requires it:\n{listed}\n\n{criteria}"
    )


def review_screen_event(
    item_id: str, base: str, head: str, findings: list[str], *, forced: bool, cycle_id: str = ""
) -> EventRecord:
    """The ``review_screen`` event: what the screen found and whether it forced the review."""
    return EventRecord(
        kind=REVIEW_SCREEN_EVENT,
        cycle_id=cycle_id,
        payload={
            "item_id": item_id,
            "base": base,
            "head": head,
            "findings": findings,
            "forced": forced,
        },
    )

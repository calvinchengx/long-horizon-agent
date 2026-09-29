"""The evidence ledger in docs/22-honesty.md cites only tests that exist.

Every row names a status from the legend, every linked file or directory exists, and every test
named in a row (``test_*`` for Python, ``Test*`` for Go) is defined in one of that row's linked
files, so a renamed or deleted test fails CI instead of leaving a claim without its evidence.
"""

from __future__ import annotations

import re
from pathlib import Path

REPO = Path(__file__).resolve().parents[3]
DOCS = REPO / "docs"
LEDGER = DOCS / "22-honesty.md"
STATUSES = {"proven", "fake-only", "measured", "unproven"}

_LINK = re.compile(r"\]\(([^)#\s]+)(?:#[^)]*)?\)")
_TEST_NAME = re.compile(r"`((?:test_|Test)\w+)`")


def _ledger_rows() -> list[list[str]]:
    text = LEDGER.read_text(encoding="utf-8")
    section = text.split("## Evidence ledger", 1)[1].split("\n## ", 1)[0]
    table = section.split("\n| Claim |", 1)[1].split("\n\n", 1)[0]  # after the status legend
    rows = [line for line in table.splitlines() if line.startswith("| ")]
    return [[c.strip() for c in row.strip().strip("|").split(" | ")] for row in rows]


def _defines(path: Path, name: str) -> bool:
    files = [path] if path.is_file() else [*path.rglob("*_test.go"), *path.rglob("test_*.py")]
    pattern = re.compile(rf"^(?:async def|def|func) {re.escape(name)}\(", re.MULTILINE)
    return any(pattern.search(f.read_text(encoding="utf-8")) for f in files)


def test_every_row_has_five_cells_and_a_known_status() -> None:
    rows = _ledger_rows()
    assert len(rows) >= 40
    for row in rows:
        assert len(row) == 5, row
        assert row[1] in STATUSES, row


def test_every_cited_file_and_test_exists() -> None:
    missing: list[str] = []
    for claim, _status, py_cell, go_cell, _ci in _ledger_rows():
        for cell in (py_cell, go_cell):
            targets = [(DOCS / link).resolve() for link in _LINK.findall(cell)]
            missing += [f"{claim}: {t}" for t in targets if not t.exists()]
            for name in _TEST_NAME.findall(cell):
                if not any(_defines(t, name) for t in targets if t.exists()):
                    missing.append(f"{claim}: {name}")
    assert not missing, "\n".join(missing)

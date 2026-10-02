"""``diff_since`` gives the reviewer (and ``lha labels export --diffs``) the real patch."""

from __future__ import annotations

import subprocess
from pathlib import Path

from lha.agents.waves import diff_since
from lha.verify.review_screen import screen_diff

_ENV = {
    "GIT_AUTHOR_NAME": "t",
    "GIT_AUTHOR_EMAIL": "t@t",
    "GIT_COMMITTER_NAME": "t",
    "GIT_COMMITTER_EMAIL": "t@t",
}


def _git(cwd: Path, *args: str) -> str:
    import os

    return subprocess.run(
        ["git", *args],
        cwd=cwd,
        check=True,
        capture_output=True,
        text=True,
        env={**os.environ, **_ENV},
    ).stdout.strip()


def test_diff_since_returns_the_patch_despite_hardening(tmp_path: Path) -> None:
    """The hardening config sets ``diff.external`` to an empty string; without ``--no-ext-diff``
    git tries to run it and dies, and the reviewer used to get ``(empty diff)``."""
    _git(tmp_path, "init", "-q")
    _git(tmp_path, "commit", "-q", "--allow-empty", "-m", "init")
    (tmp_path / "pkg").mkdir()
    (tmp_path / "pkg" / "x_test.go").write_text(
        'package pkg\n\nfunc TestX(t *testing.T) {\n\tt.Skip("later")\n}\n', encoding="utf-8"
    )
    (tmp_path / ".lha").mkdir()
    (tmp_path / ".lha" / "events.ndjson").write_text("x\n", encoding="utf-8")
    _git(tmp_path, "add", "-A")
    _git(tmp_path, "commit", "-q", "-m", "c1")
    diff = diff_since(
        tmp_path, _git(tmp_path, "rev-parse", "HEAD~1"), _git(tmp_path, "rev-parse", "HEAD")
    )
    assert diff.startswith("diff --git a/pkg/x_test.go b/pkg/x_test.go")
    assert '+\tt.Skip("later")' in diff and ".lha" not in diff
    assert screen_diff(diff) == ['added skip to pkg/x_test.go: t.Skip("later")']
    assert diff_since(tmp_path, "abc", "abc") == "(no new commits)"

"""Every parsing path of the command classifier: launcher options, git, curl/wget, sed, shells.

Each case states whether the command must be gated. The launcher cases pin the fail-closed rule:
an option the classifier does not know is read BOTH as a flag and as taking a value.
"""

from __future__ import annotations

import pytest

from lha.safety.commands import classify_command

G, A = True, False  # gated / allowed

CASES: list[tuple[list[str], bool]] = [
    # --- launcher option parsing (_parse_opts / _unwrap) ---
    (["env", "-i"], A),  # options only, no command
    (["nice"], A),
    (["env", "--", "git", "push"], G),
    (["timeout", "--", "5", "git", "push"], G),
    (["env", "FOO=1", "git", "push"], G),
    (["env", "-", "git", "push"], G),
    (["nice", "-10", "git", "push"], G),
    (["env", "--unset=FOO", "git", "push"], G),
    (["env", "--split-string=git push"], G),
    (["env", "--split-string", "git push"], G),
    (["env", "-Sgit push"], G),
    (["env", "--frobnicate", "git", "push"], G),  # unknown long option read as a flag
    (["env", "--frobnicate", "git", "push", "x"], G),
    (["env", "--frobnicate", "value", "ls"], A),  # both readings are harmless
    (["nice", "-Z", "git", "push"], G),  # unknown short option read as a flag
    (["nice", "-Z", "5", "git", "push"], G),  # ...and as taking a value
    (["xargs", "-i{}", "git", "push"], G),  # attached-only value
    (["python3", "-m", "pytest", "-q"], A),
    (["env", *[f"--opt{i}" for i in range(70)], "git", "push"], G),  # too many readings
    ([*["nice"] * 10, "git", "push"], G),  # too many nested launchers
    (["env", *["-S", "ls"] * 70], G),  # too many script candidates
    ([], A),
    # --- git ---
    (["git", "--config-env=alias.p=ENV_VAR", "p"], G),
    (["git", "--version"], A),
    (["git", "remote", "add", "evil", "https://example.com/r.git"], G),
    (["git", "remote", "-v"], A),
    (["git", "reflog", "expire", "--all"], G),
    (["git", "checkout", "-f", "main"], G),
    (["git", "switch", "--force", "main"], G),
    (["git", "checkout", "main"], A),
    (["git", "commit", "--amend"], G),
    (["git", "reset", "--hard"], G),
    (["git", "reset", "--soft", "HEAD~1"], A),
    (["git", "clean", "-fdx"], G),
    (["git", "clean", "-n"], A),
    # --- httpie ---
    (["http", "--verbose", "GET", "https://example.com"], A),
    # --- curl ---
    (["curl", "--", "-d"], A),  # after `--` everything is a URL
    (["curl", "--config", "cfg", "https://example.com"], G),
    (["curl", "-K", "cfg", "https://example.com"], G),
    (["curl", "--request", "POST", "https://example.com"], G),
    (["curl", "--request=GET", "https://example.com"], A),
    (["curl", "-H", "X-Data: -d", "https://example.com"], A),  # -H consumes its value
    (["curl", "-sH", "X: y", "https://example.com"], A),
    (["curl", "-HX:-d", "https://example.com"], A),  # attached value
    (["curl", "--data-urlencode", "a=b", "https://example.com"], G),
    # --- wget ---
    (["wget", "-e", "post_data=x", "https://example.com"], G),
    (["wget", "--execute=method=PUT", "https://example.com"], G),
    (["wget", "-e", "robots=off", "https://example.com"], A),
    (["wget", "https://example.com"], A),
    # --- sed ---
    (["sed", "--", "-i", ".git/config"], A),  # `-i` after `--` is a script, not in-place
    (["sed", "-es/i/j/", ".git/config"], A),  # `i` is inside -e's attached value
    (["sed", "s/a/b/", ".git/config"], A),  # reading, not editing
    # --- shells ---
    (["bash", "script.sh"], A),
    (["bash", "-c"], A),
    (["eval", "git", "push"], G),
    (["eval"], A),
    (["sh", "-c", 'echo "$(git push)"'], G),  # substitutions run inside double quotes
    (["sh", "-c", "echo '$(git push)'"], A),  # ...but not inside single quotes
    # An escaped $ is literal to the shell, but "(" still splits the script: gated (fail closed).
    (["sh", "-c", "echo \\$(git push)"], G),
    (["sh", "-c", "echo $(echo \\) ; git push)"], G),
    (["sh", "-c", "echo $((1 + 2))"], A),  # arithmetic, not a command
    (["sh", "-c", "echo `git push"], G),  # unterminated -> unparseable -> gated
    (["sh", "-c", "echo $(git push"], G),
    (["sh", "-c", "echo 'unterminated"], G),
    (["sh", "-c", "echo \\"], G),
    (["sh", "-c", "echo $(echo $(echo $(echo $(echo hi))))"], G),  # nesting cap
]


@pytest.mark.parametrize(
    ("argv", "gated"), CASES, ids=lambda v: " ".join(v)[:60] if isinstance(v, list) else ""
)
def test_classifier_path(argv: list[str], gated: bool) -> None:
    reason = classify_command(argv)
    assert (reason is not None) == gated, reason

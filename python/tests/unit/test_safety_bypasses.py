"""Regression tests for classifier bypasses found in the second review of ``safety/commands.py``."""

from __future__ import annotations

import pytest

from lha.safety.commands import classify_command

GATED = [
    # git aliases / exec config: persistent, on the command line, or via the environment
    ["git", "config", "alias.p", "push"],
    ["git", "config", "--global", "alias.p", "push"],
    ["git", "config", "set", "alias.p", "push"],
    ["git", "config", "remote.origin.url", "https://example.com/other.git"],
    ["git", "config", "core.hooksPath", "hooks"],
    ["git", "-c", "include.path=/tmp/cfg", "p"],
    ["git", "-c", "core.sshCommand=sh", "fetch"],
    [
        "env",
        "GIT_CONFIG_COUNT=1",
        "GIT_CONFIG_KEY_0=alias.p",
        "GIT_CONFIG_VALUE_0=push",
        "git",
        "p",
    ],
    ["sh", "-c", "GIT_CONFIG_PARAMETERS=\"'alias.p=push'\" git p"],
    ["sh", "-c", "export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=alias.p; git p"],
    # git transports that push or run arbitrary commands
    ["git", "send-pack", "origin", "main"],
    ["git", "remote-https", "origin", "https://example.com"],
    ["git", "fetch", "--upload-pack=touch /tmp/x", "origin"],
    ["git", "clone", "ext::sh -c touch% /tmp/x", "dest"],
    ["git", "clone", "-u", "touch /tmp/x", "https://example.com/r.git"],
    # force branch deletion in any spelling
    ["git", "branch", "-D", "x"],
    ["git", "branch", "-Df", "x"],
    ["git", "branch", "-d", "-f", "x"],
    ["git", "branch", "--delete", "--force", "x"],
    # shell redirection into the harness-owned dirs
    ["sh", "-c", "echo x > .git/hooks/pre-commit"],
    ["sh", "-c", "echo '{}' >> .lha/checklist.json"],
    ["sh", "-c", "echo x >.git/config"],
    ["sh", "-c", "true &> .git/config"],
    ["sh", "-c", "cmd 2> .lha/events.ndjson"],
    ["sh", "-c", "echo x >| .git/y"],
    ["sh", "-c", "echo x 2>| .git/hooks/pre-commit"],
    ["sh", "-c", 'cat > ".git/config"'],
    # commands hidden behind find / more launchers
    ["find", ".", "-exec", "git", "push", ";"],
    ["find", ".", "-execdir", "git", "push", "{}", "+"],
    ["find", ".git", "-delete"],
    ["setsid", "git", "push"],
    ["flock", "/tmp/lock", "git", "push"],
    ["flock", "/tmp/lock", "-c", "git push"],
    ["taskset", "1", "git", "push"],
    ["chrt", "-f", "10", "git", "push"],
    ["unbuffer", "git", "push"],
    ["caffeinate", "-i", "git", "push"],
    ["watch", "git", "push"],
    ["script", "-c", "git push", "/dev/null"],
    # command or subcommand names that come from an expansion
    ["sh", "-c", "x=push; git $x"],
    ["sh", "-c", "G=git; $G push"],
    # httpie writes
    ["http", "POST", "https://example.com", "a=1"],
    ["https", "PUT", "https://example.com"],
    ["xh", "https://example.com", "a=1"],
    ["http", "--form", "https://example.com", "f@file"],
]

ALLOWED = [
    ["git", "branch", "-d", "merged-branch"],
    ["git", "config", "user.name", "Agent"],
    ["git", "config", "--get", "alias.st"],
    ["git", "config", "--list"],
    ["git", "fetch", "origin"],
    ["git", "clone", "https://example.com/r.git"],
    ["sh", "-c", "pytest -q > out.txt 2>&1"],
    ["sh", "-c", "ls .git > files.txt"],
    ["sh", "-c", "ls >| out.txt"],
    ["sh", "-c", 'echo "a > b"'],
    ["sh", "-c", "echo $HOME"],
    ["sh", "-c", "for f in *.py; do ruff check $f; done"],
    ["find", ".", "-name", "*.pyc", "-delete"],
    ["find", ".", "-name", "*.py", "-exec", "ruff", "check", "{}", "+"],
    ["http", "GET", "https://example.com/a?q=1"],
    ["http", "https://example.com", "Accept:application/json", "q==1"],
    ["watch", "-n", "5", "ls"],
    ["setsid", "pytest"],
    ["flock", "/tmp/lock", "pytest", "-q"],
    ["uv", "run", "--all-extras", "--no-dev", "--exact", "pytest"],
    ["npm", "run", "unreleased-check"],
]


@pytest.mark.parametrize("argv", GATED, ids=" ".join)
def test_bypass_is_gated(argv: list[str]) -> None:
    assert classify_command(argv) is not None


@pytest.mark.parametrize("argv", ALLOWED, ids=" ".join)
def test_ordinary_command_is_allowed(argv: list[str]) -> None:
    assert classify_command(argv) is None

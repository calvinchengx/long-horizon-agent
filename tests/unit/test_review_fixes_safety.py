"""Regression tests for the command-classifier and egress review findings."""

from __future__ import annotations

import httpx
import pytest

from lha.execution.tools.web import _target_mismatch
from lha.safety.commands import classify_command
from lha.safety.egress import EgressDenied, EgressPolicy, normalize_host, parse_url

# --- 1. command classifier: launcher options that take a value ------------------------------


@pytest.mark.parametrize(
    "argv",
    [
        ["env", "-u", "FOO", "git", "push"],
        ["env", "--unset", "FOO", "git", "push"],
        ["nice", "-n", "10", "git", "push"],
        ["ionice", "-c", "3", "git", "push"],
        ["stdbuf", "-o", "0", "git", "push"],
        ["xargs", "-I", "{}", "git", "push"],
        ["xargs", "-n", "1", "git", "push"],
        ["timeout", "-s", "KILL", "5", "git", "push"],
    ],
)
def test_launcher_option_values_do_not_hide_the_command(argv: list[str]) -> None:
    assert classify_command(argv) is not None


def test_plain_launchers_still_pass() -> None:
    assert classify_command(["env", "-u", "FOO", "ls"]) is None
    assert classify_command(["nice", "-n", "10", "pytest", "-q"]) is None


# --- 2. sh -c: newlines and command substitution ---------------------------------------------


@pytest.mark.parametrize(
    "script",
    ["ls\ngit push origin main", "echo `git push`", "echo $(git push)"],
)
def test_shell_scripts_split_on_newlines_and_substitutions(script: str) -> None:
    assert classify_command(["sh", "-c", script]) is not None


def test_benign_multiline_script_passes() -> None:
    assert classify_command(["sh", "-c", "ls\npwd"]) is None


# --- 3. curl / wget upload spellings ---------------------------------------------------------


@pytest.mark.parametrize(
    "argv",
    [
        ["curl", "-sd", "x=1", "https://example.com"],
        ["curl", "-sSF", "f=@secret", "https://example.com"],
        ["curl", "-sT", "file", "https://example.com"],
        ["wget", "--method", "POST", "https://example.com"],
        ["wget", "--method=PUT", "https://example.com"],
    ],
)
def test_http_upload_spellings_are_gated(argv: list[str]) -> None:
    assert classify_command(argv) is not None


def test_plain_download_passes() -> None:
    assert classify_command(["curl", "-sSL", "https://example.com"]) is None


# --- 4. sed in-place on protected paths ------------------------------------------------------


@pytest.mark.parametrize(
    "argv",
    [
        ["sed", "-Ei", "s/x/y/", ".git/config"],
        ["sed", "--in-place=.bak", "s/x/y/", ".git/config"],
        ["sed", "-i.bak", "s/x/y/", ".lha/checklist.json"],
    ],
)
def test_sed_in_place_on_protected_paths_is_gated(argv: list[str]) -> None:
    assert classify_command(argv) is not None


# --- 5. git inline alias ---------------------------------------------------------------------


def test_git_inline_alias_is_gated() -> None:
    assert classify_command(["git", "-c", "alias.p=push", "p"]) is not None
    assert classify_command(["git", "-c", "user.name=x", "status"]) is None


# --- 6. egress: IDNA 2003/2008 ambiguity and the connected host ------------------------------


def test_ambiguous_idn_host_is_denied() -> None:
    policy = EgressPolicy(allow_hosts={"strasse.de"})
    assert not policy.permits("https://straße.de/x")
    with pytest.raises(EgressDenied):
        parse_url("https://straße.de/x")


def test_idn_host_normalizes_like_httpx() -> None:
    assert normalize_host("bücher.example") == "xn--bcher-kva.example"
    request = httpx.Request("GET", "https://bücher.example/x")
    assert _target_mismatch(request, parse_url("https://bücher.example/x")) is None


def test_request_target_mismatch_is_detected() -> None:
    request = httpx.Request("GET", "https://other.example/x")
    assert _target_mismatch(request, parse_url("https://docs.example.com/x")) is not None
    request = httpx.Request("GET", "https://docs.example.com:8443/x")
    assert _target_mismatch(request, parse_url("https://docs.example.com/x")) is not None

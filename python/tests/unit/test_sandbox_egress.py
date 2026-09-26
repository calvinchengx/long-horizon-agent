"""Sandbox egress: hosts split by what they accept, the Rule of Two, and the proxy's requests
committed as ``sandbox_egress`` events."""

from __future__ import annotations

import json
import subprocess
from pathlib import Path
from typing import Any

import pytest
from typer.testing import CliRunner

import lha.cli.main as cli
from lha.agent.runner import run_mission_local
from lha.config import Settings
from lha.contracts.model import TurnResult
from lha.contracts.state import Checklist, ChecklistItem, EventRecord
from lha.execution.egress_events import EVENT_KIND, ProxyLogCursor, parse_proxy_log
from lha.execution.egress_hosts import (
    PACKAGE_FETCH_HOSTS,
    SandboxEgressError,
    sandbox_allow_list,
)
from lha.execution.sandbox_docker import DockerSandboxSession
from lha.execution.sandbox_local import LocalSandboxSession
from lha.execution.tools.toolset import (
    check_run_rule_of_two,
    preflight_run_tools,
    run_capabilities,
)
from lha.model.stub import StubModel
from lha.safety.rule_of_two import Capability, RuleOfTwoViolation


def _settings(**overrides: Any) -> Settings:
    return Settings(_env_file=None, model_backend="stub", **overrides)  # type: ignore[call-arg]


# --- the three lists ---------------------------------------------------------------------------


def test_package_fetch_hosts_keep_the_go_toolchain_working() -> None:
    # proxy.golang.org redirects module zips to storage.googleapis.com (commit 6be7156).
    go = ["proxy.golang.org", "sum.golang.org", "storage.googleapis.com"]
    assert sandbox_allow_list(go) == go
    assert sandbox_allow_list(list(PACKAGE_FETCH_HOSTS)) == list(PACKAGE_FETCH_HOSTS)


@pytest.mark.parametrize(
    "entry", ["github.com", "example.org", ".golang.org", "pypi.org:8443", "PYPI.org."]
)
def test_egress_accepts_only_exact_package_fetch_hosts(entry: str) -> None:
    if entry == "PYPI.org.":  # normalized, still a fetch host
        assert sandbox_allow_list([entry]) == ["pypi.org"]
        return
    with pytest.raises(SandboxEgressError) as info:
        sandbox_allow_list([entry])
    message = str(info.value)
    assert message.startswith("LHA_SANDBOX_EGRESS entry ")
    assert "LHA_SANDBOX_EGRESS_EXTRA_HOSTS" in message
    assert "LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS" in message


@pytest.mark.parametrize(
    ("entry", "known"),
    [
        ("github.com", ".github.com"),
        ("api.github.com", ".github.com"),
        (".com", ".github.com"),  # a suffix entry that covers a write host
        ("my-bucket.s3.amazonaws.com", ".amazonaws.com"),
        ("upload.pypi.org", "upload.pypi.org"),
        (".pypi.org", "upload.pypi.org"),
        ("gitlab.com:8443", ".gitlab.com"),
    ],
)
def test_extra_hosts_refuse_known_write_hosts(entry: str, known: str) -> None:
    with pytest.raises(SandboxEgressError) as info:
        sandbox_allow_list([], [entry])
    assert f"reaches {known}, which accepts pushes or uploads" in str(info.value)
    assert "LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS" in str(info.value)


def test_write_hosts_are_accepted_when_acknowledged_and_lists_merge_in_order() -> None:
    hosts = sandbox_allow_list(
        ["pypi.org", "files.pythonhosted.org"],
        ["mirror.internal.example", "pypi.org"],
        ["github.com", ".amazonaws.com", "mirror.internal.example"],
    )
    assert hosts == [
        "pypi.org",
        "files.pythonhosted.org",
        "mirror.internal.example",
        "github.com",
        ".amazonaws.com",
    ]


@pytest.mark.parametrize(
    ("lists", "setting"),
    [
        ((["10.0.0.1"], [], []), "LHA_SANDBOX_EGRESS: "),
        (([], ["http://x.org"], []), "LHA_SANDBOX_EGRESS_EXTRA_HOSTS: "),
        (([], [], ["x.org:99999"]), "LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS: "),
    ],
)
def test_malformed_entries_name_their_setting(
    lists: tuple[list[str], list[str], list[str]], setting: str
) -> None:
    with pytest.raises(SandboxEgressError) as info:
        sandbox_allow_list(*lists)
    assert str(info.value).startswith(setting)


def test_settings_merge_the_three_lists_and_enable_egress_for_docker_only() -> None:
    s = _settings(
        sandbox_egress="pypi.org, proxy.golang.org",
        sandbox_egress_extra_hosts="docs.internal.example",
        sandbox_egress_allow_write_hosts="github.com",
    )
    assert s.sandbox_egress_hosts() == [
        "pypi.org",
        "proxy.golang.org",
        "docs.internal.example",
        "github.com",
    ]
    assert s.sandbox_egress_enabled()
    assert not s.model_copy(update={"sandbox": "local"}).sandbox_egress_enabled()
    assert not _settings().sandbox_egress_enabled()
    assert _settings().sandbox_egress_hosts() == []
    with pytest.raises(SandboxEgressError):
        _settings(sandbox_egress="github.com").sandbox_egress_hosts()


# --- Rule of Two -------------------------------------------------------------------------------


def test_sandbox_egress_brings_untrusted_input_and_external_comms() -> None:
    caps = run_capabilities(_settings(sandbox_egress="pypi.org"))
    assert caps == {Capability.UNTRUSTED_CONTENT, Capability.EXTERNAL_COMMS}
    check_run_rule_of_two(_settings(sandbox_egress="pypi.org"))  # two of three: fine
    check_run_rule_of_two(_settings(private_data=True))  # no egress: fine


def test_sandbox_egress_with_private_data_is_refused() -> None:
    settings = _settings(
        sandbox_egress="pypi.org", sandbox_egress_allow_write_hosts="github.com", private_data=True
    )
    with pytest.raises(RuleOfTwoViolation) as info:
        check_run_rule_of_two(settings)
    message = str(info.value)
    assert message.startswith(
        "refusing to start: the sandbox can reach the network (sandbox egress: pypi.org, "
        "github.com), which brings untrusted content and external comms, and "
        "LHA_PRIVATE_DATA=true declares"
    )
    assert message.endswith(
        "or clear the allow-list (LHA_SANDBOX_EGRESS / LHA_SANDBOX_EGRESS_EXTRA_HOSTS / "
        "LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS)."
    )


def test_web_and_sandbox_egress_are_both_named() -> None:
    settings = _settings(web_allow_hosts="docs.test", sandbox_egress="pypi.org", private_data=True)
    with pytest.raises(RuleOfTwoViolation) as info:
        check_run_rule_of_two(settings)
    message = str(info.value)
    assert (
        "web tools are enabled (egress allow-list: docs.test) and the sandbox can reach the "
        "network (sandbox egress: pypi.org)"
    ) in message
    assert "(LHA_WEB_ALLOW_HOSTS / --allow-host; LHA_SANDBOX_EGRESS /" in message


def test_local_sandbox_ignores_the_sandbox_egress_lists() -> None:
    # The local sandbox has the host's network anyway; the lists configure the docker proxy.
    check_run_rule_of_two(_settings(sandbox="local", sandbox_egress="pypi.org"))


def test_preflight_validates_the_lists() -> None:
    with pytest.raises(SandboxEgressError):
        preflight_run_tools(_settings(sandbox_egress="github.com"))
    preflight_run_tools(_settings(sandbox="local", sandbox_egress="github.com"))  # unused there


def test_cli_refuses_a_write_host_in_the_fetch_list(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    settings = _settings(sandbox_egress="github.com", budget_usd_ceiling=100.0)
    monkeypatch.setattr(cli, "get_settings", lambda: settings)
    monkeypatch.setattr("lha.config.get_settings", lambda: settings)
    workdir = tmp_path / "ws"
    result = CliRunner().invoke(
        cli.app, ["run-local", "--title", "t", "--item", "x", "--workdir", str(workdir)]
    )
    assert result.exit_code == 2
    assert "invalid sandbox egress settings: LHA_SANDBOX_EGRESS entry github.com" in result.output
    assert not workdir.exists()


# --- the proxy's requests as events ------------------------------------------------------------

_LOG = (
    "2026-09-26 10:00:00,001 INFO lha-egress-proxy listening on 0.0.0.0:3128 allow=pypi.org\n"
    "2026-09-26 10:00:01,000 INFO allow CONNECT pypi.org:443 -> 151.101.0.223:443\n"
    "2026-09-26 10:00:01,500 INFO allow CONNECT pypi.org:443 -> 151.101.64.223:443\n"
    "2026-09-26 10:00:02,000 WARNING deny CONNECT github.com:443: host not in egress "
    "allow-list: github.com\n"
    "2026-09-26 10:00:03,000 WARNING deny GET http://example.org/x?token=sk-ant-api03-abcdef"
    "ghijklmnopqrstuvwxyz0123456789: host not in egress allow-list: example.org\n"
    "2026-09-26 10:00:04,000 WARNING fail CONNECT [2001:db8::1]:8443: upstream unreachable: x\n"
)


def test_proxy_log_lines_become_aggregated_events() -> None:
    events = parse_proxy_log(_LOG.splitlines())
    assert all(e.kind == EVENT_KIND for e in events)
    assert [e.payload for e in events] == [
        {
            "decision": "allow",
            "method": "CONNECT",
            "host": "pypi.org",
            "port": 443,
            "detail": "151.101.0.223:443",
            "count": 2,
        },
        {
            "decision": "deny",
            "method": "CONNECT",
            "host": "github.com",
            "port": 443,
            "detail": "host not in egress allow-list: github.com",
            "count": 1,
        },
        {
            "decision": "deny",
            "method": "GET",
            "host": "example.org",  # the URL (and its query string) is not kept
            "port": 80,
            "detail": "host not in egress allow-list: example.org",
            "count": 1,
        },
        {
            "decision": "fail",
            "method": "CONNECT",
            "host": "2001:db8::1",
            "port": 8443,
            "detail": "upstream unreachable: x",
            "count": 1,
        },
    ]
    assert "sk-ant" not in json.dumps([e.payload for e in events])


def test_an_unparsable_target_is_kept_as_unknown() -> None:
    [event] = parse_proxy_log(
        ["t INFO x WARNING deny CONNECT x.org:99999: malformed authority: 'x.org:99999'"]
    )
    assert event.payload["host"] == "?" and event.payload["port"] == 0


def test_cursor_drains_only_new_complete_lines() -> None:
    cursor = ProxyLogCursor()
    lines = _LOG.splitlines(keepends=True)
    partial = "".join(lines[:2]) + lines[2][:20]  # the third line is still being written
    assert [e.payload["count"] for e in cursor.drain(partial)] == [1]
    assert [e.payload["count"] for e in cursor.drain("".join(lines[:3]))] == [1]
    assert len(cursor.drain(_LOG)) == 3
    assert cursor.drain(_LOG) == []


def test_docker_session_drains_its_proxy_log() -> None:
    log = {"text": ""}
    session = DockerSandboxSession(object(), egress_log=lambda: log["text"])
    assert session.drain_egress_events() == []
    log["text"] = _LOG
    assert [e.payload["host"] for e in session.drain_egress_events()] == [
        "pypi.org",
        "github.com",
        "example.org",
        "2001:db8::1",
    ]
    assert session.drain_egress_events() == []
    assert DockerSandboxSession(object()).drain_egress_events() == []  # no egress: nothing


def test_the_egress_gate_reads_its_proxy_log() -> None:
    from lha.execution.sandbox_docker import _EgressGate

    class _Proxy:
        def __init__(self, log: bytes | None) -> None:
            self._log = log

        def logs(self) -> bytes:
            if self._log is None:
                raise RuntimeError("container is gone")
            return self._log

    gate = _EgressGate(object(), "tok")
    assert gate.logs() == ""  # no proxy yet
    gate.proxy = _Proxy(_LOG.encode())
    assert gate.logs() == _LOG
    gate.proxy = _Proxy(None)
    assert gate.logs() == ""


@pytest.mark.asyncio
async def test_the_loop_commits_the_sessions_egress_events(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    drained = [
        EventRecord(
            kind=EVENT_KIND,
            payload={
                "decision": "allow",
                "method": "CONNECT",
                "host": "pypi.org",
                "port": 443,
                "detail": "151.101.0.223:443",
                "count": 3,
            },
        )
    ]
    monkeypatch.setattr(
        LocalSandboxSession, "drain_egress_events", lambda self: drained, raising=False
    )
    workdir = tmp_path / "ws"
    summary = await run_mission_local(
        workdir=str(workdir),
        title="t",
        description="d",
        checklist=Checklist(items=[ChecklistItem(id="01", description="do it")]),
        checks=[],
        settings=_settings(sandbox="local", allow_unsafe_local=True, budget_usd_ceiling=100.0),
        model=StubModel(script=[TurnResult(text='{"done": true, "summary": "ok"}')]),
    )
    assert summary.cycles >= 1
    committed = subprocess.run(
        ["git", "show", "HEAD:.lha/events.ndjson"],
        cwd=workdir,
        capture_output=True,
        text=True,
        check=True,
    ).stdout.splitlines()
    egress = [json.loads(line) for line in committed if json.loads(line)["kind"] == EVENT_KIND]
    assert egress and egress[0]["payload"] == drained[0].payload
    assert egress[0]["cycle_id"]


@pytest.mark.asyncio
async def test_a_failing_drain_never_fails_the_cycle(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    def boom(self: object) -> list[EventRecord]:
        raise RuntimeError("docker went away")

    monkeypatch.setattr(LocalSandboxSession, "drain_egress_events", boom, raising=False)
    summary = await run_mission_local(
        workdir=str(tmp_path / "ws"),
        title="t",
        description="d",
        checklist=Checklist(items=[ChecklistItem(id="01", description="do it")]),
        checks=[],
        settings=_settings(sandbox="local", allow_unsafe_local=True, budget_usd_ceiling=100.0),
        model=StubModel(script=[TurnResult(text='{"done": true, "summary": "ok"}')]),
    )
    assert summary.cycles >= 1

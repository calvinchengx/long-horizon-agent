"""The Docker sandbox's egress allow-list against a real Docker daemon (and the real internet).

Opt-in with ``LHA_IT_DOCKER=1``. The sandbox gets ``egress_hosts=["pypi.org",
"files.pythonhosted.org"]``: downloading a package from PyPI must work (through the proxy),
anything else must not — neither another host via the proxy (403) nor a direct connection that
ignores the proxy (no route from the ``--internal`` network). Every per-session resource (proxy
container, network) must be gone after ``close()`` or a failed ``open()``.
"""

from __future__ import annotations

import subprocess
from collections.abc import AsyncIterator
from pathlib import Path
from typing import Any

import pytest

from lha.contracts.sandbox import SandboxSession
from tests.integration.conftest import requires_docker

pytestmark = [pytest.mark.integration, requires_docker]
docker = pytest.importorskip("docker")

UV_IMAGE = "ghcr.io/astral-sh/uv:python3.12-bookworm-slim"
ALLOW = ["pypi.org", "files.pythonhosted.org"]


def _egress_resources() -> set[str]:
    client: Any = docker.from_env()
    networks = {n.name for n in client.networks.list(names=["lha-egress-"])}
    containers = {c.name for c in client.containers.list(all=True, filters={"name": "lha-egress-"})}
    return {n for n in networks | containers if n.startswith("lha-egress-")}


async def _open(workdir: Path, **kwargs: Any) -> SandboxSession:
    from lha.execution.sandbox_docker import DockerSandbox

    return await DockerSandbox(**kwargs).open(workdir=str(workdir))


@pytest.fixture
async def gated(tmp_path: Path) -> AsyncIterator[SandboxSession]:
    subprocess.run(["git", "init", "-q", str(tmp_path)], check=True)
    before = _egress_resources()
    sess = await _open(tmp_path, image=UV_IMAGE, egress_hosts=ALLOW)
    assert _egress_resources() - before, "expected a per-session proxy + network"
    try:
        yield sess
    finally:
        await sess.close()
        assert _egress_resources() - before == set(), "egress resources leaked"


async def test_allowed_registry_is_reachable_through_the_proxy(gated: SandboxSession) -> None:
    result = await gated.exec(
        [
            "python",
            "-m",
            "pip",
            "download",
            "--no-cache-dir",
            "--no-deps",
            "--disable-pip-version-check",
            "-d",
            "/tmp/dl",
            "six==1.16.0",
        ],
        timeout_s=180,
    )
    assert result.ok, result.stderr
    listing = await gated.exec(["ls", "/tmp/dl"])
    assert "six-1.16.0" in listing.stdout


async def test_uv_resolves_from_pypi_through_the_proxy(gated: SandboxSession) -> None:
    result = await gated.exec(
        ["uv", "pip", "install", "--target", "/tmp/t", "--no-deps", "six==1.16.0"],
        env={"UV_CACHE_DIR": "/tmp/uv-cache"},
        timeout_s=180,
    )
    assert result.ok, result.stderr


_FETCH = (
    "import sys, urllib.request\n"
    "try:\n"
    "    urllib.request.urlopen(sys.argv[1], timeout=20).read()\n"
    "except Exception as exc:\n"
    "    print(f'{type(exc).__name__}: {exc}')\n"
    "    raise SystemExit(9)\n"
    "print('fetched')\n"
)


async def test_other_hosts_are_refused_by_the_proxy(gated: SandboxSession) -> None:
    https = await gated.exec(["python", "-c", _FETCH, "https://example.com/"], timeout_s=60)
    assert https.exit_code == 9 and "403" in https.stdout, https.stdout + https.stderr
    http = await gated.exec(["python", "-c", _FETCH, "http://example.com/"], timeout_s=60)
    assert http.exit_code == 9 and "403" in http.stdout, http.stdout + http.stderr
    # The Docker host / cloud metadata via the proxy: IP literals are refused outright.
    meta = await gated.exec(
        ["python", "-c", _FETCH, "http://169.254.169.254/latest/meta-data/"], timeout_s=60
    )
    assert meta.exit_code == 9 and "403" in meta.stdout


async def test_direct_connections_bypassing_the_proxy_have_no_route(
    gated: SandboxSession,
) -> None:
    probe = (
        "import socket\n"
        "for target in (('1.1.1.1', 443), ('151.101.0.223', 443)):\n"
        "    try:\n"
        "        socket.create_connection(target, timeout=5)\n"
        "    except OSError as exc:\n"
        "        print('blocked', target, exc)\n"
        "        continue\n"
        "    raise SystemExit('connected directly to %s' % (target,))\n"
    )
    result = await gated.exec(["python", "-c", probe], timeout_s=60)
    assert result.ok, result.stdout + result.stderr
    assert result.stdout.count("blocked") == 2
    # Even ignoring the proxy env, a pip download cannot get out.
    direct = await gated.exec(
        ["python", "-c", _FETCH, "https://pypi.org/simple/six/"],
        env={"HTTPS_PROXY": "", "https_proxy": "", "HTTP_PROXY": "", "http_proxy": ""},
        timeout_s=60,
    )
    assert direct.exit_code == 9, direct.stdout


async def test_configured_image_is_used(gated: SandboxSession) -> None:
    # `uv` exists in the uv image, not in the plain python:3.12-slim default.
    result = await gated.exec(["uv", "--version"])
    assert result.ok and result.stdout.startswith("uv ")


async def test_failed_open_leaves_nothing_behind(tmp_path: Path) -> None:
    before = _egress_resources()
    with pytest.raises(Exception):  # noqa: B017 - docker's ImageNotFound / NotFound / APIError
        await _open(tmp_path, image="lha-it/does-not-exist:never", egress_hosts=ALLOW)
    assert _egress_resources() - before == set()

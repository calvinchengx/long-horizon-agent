"""Sandbox selection — the single place run paths should obtain a ``Sandbox``.

``local`` runs agent commands directly on the host (no isolation, host network), so it is refused
unless the operator explicitly opts in with ``allow_unsafe_local=True`` (wired from settings by the
entry points). ``docker`` and ``e2b`` import their optional extras lazily.
"""

from __future__ import annotations

from collections.abc import Sequence
from typing import Any, Literal

from lha.contracts.sandbox import Sandbox, SandboxSession

SandboxKind = Literal["local", "docker", "e2b"]
SANDBOX_KINDS: tuple[str, ...] = ("local", "docker", "e2b")
# Matches ``Settings.sandbox_image``'s default: Python 3.12 + uv (see sandbox/Dockerfile for Go/Node).
DEFAULT_DOCKER_IMAGE = "ghcr.io/astral-sh/uv:python3.12-bookworm-slim"


class UnsafeSandboxError(RuntimeError):
    """Raised when the unisolated local sandbox is requested without an explicit opt-in."""


def build_sandbox(
    kind: str,
    *,
    allow_unsafe_local: bool = False,
    network: bool = False,
    image: str | None = None,
    template: str | None = None,
    egress_hosts: Sequence[str] = (),
    memory: str | None = None,
    cpus: float | None = None,
    tmp_size: str | None = None,
) -> Sandbox:
    """Return the ``Sandbox`` for ``kind`` (``"local"`` | ``"docker"`` | ``"e2b"``).

    - ``allow_unsafe_local``: required to get ``local``; otherwise ``UnsafeSandboxError``.
    - ``network``: docker only; ``False`` (default) runs containers with ``network_mode="none"``.
    - ``egress_hosts``: docker only; a non-empty allow-list routes the sandbox's egress through a
      per-session proxy that reaches only those hosts (``.example.org`` = domain + subdomains).
      Empty (default) keeps ``network_mode="none"``. Mutually exclusive with ``network=True``.
    - ``image`` / ``template``: docker image (default ``DEFAULT_DOCKER_IMAGE``) / E2B template.
    - ``memory`` / ``cpus`` / ``tmp_size``: docker only; the container's limits and the size of
      its ``/tmp`` tmpfs, where toolchain caches live (``None`` keeps the defaults).

    Raises ``ValueError`` for an unknown kind, or for ``network=True`` with ``egress_hosts``.
    """
    normalized = kind.strip().lower()
    if normalized == "local":
        if not allow_unsafe_local:
            raise UnsafeSandboxError(
                "the 'local' sandbox runs agent commands directly on the host with no isolation "
                "or network restriction; use sandbox='docker' (or 'e2b'), or opt in explicitly "
                "with allow_unsafe_local=True (LHA_ALLOW_UNSAFE_LOCAL=true)"
            )
        from lha.execution.sandbox_local import LocalSandbox

        return LocalSandbox()
    if normalized == "docker":
        from lha.execution.sandbox_docker import DockerSandbox

        limits: dict[str, Any] = {}
        if memory:
            limits["mem_limit"] = memory
        if cpus:
            limits["cpus"] = cpus
        if tmp_size:
            limits["tmp_size"] = tmp_size
        return DockerSandbox(
            image or DEFAULT_DOCKER_IMAGE,
            network=network,
            egress_hosts=tuple(egress_hosts),
            **limits,
        )
    if normalized == "e2b":
        from lha.execution.sandbox_e2b import E2BSandbox

        return E2BSandbox(template or "base")
    raise ValueError(f"unknown sandbox kind {kind!r}; expected one of {SANDBOX_KINDS}")


async def open_sandbox(
    kind: str,
    *,
    workdir: str,
    allow_unsafe_local: bool = False,
    snapshot_id: str | None = None,
    network: bool = False,
    image: str | None = None,
    template: str | None = None,
    egress_hosts: Sequence[str] = (),
    memory: str | None = None,
    cpus: float | None = None,
    tmp_size: str | None = None,
) -> SandboxSession:
    """``build_sandbox(...)`` then ``open(workdir=..., snapshot_id=...)`` in one call."""
    sandbox = build_sandbox(
        kind,
        allow_unsafe_local=allow_unsafe_local,
        network=network,
        image=image,
        template=template,
        egress_hosts=egress_hosts,
        memory=memory,
        cpus=cpus,
        tmp_size=tmp_size,
    )
    return await sandbox.open(workdir=workdir, snapshot_id=snapshot_id)

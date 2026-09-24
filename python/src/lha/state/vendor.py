"""Vendoring reference material into a mission workspace (``lha vendor``).

A clean-room project depends on outside knowledge (API docs, schemas, SDK behaviour). Rather than
opening the sandbox to the web, the operator snapshots the pages it needs into the workspace, once,
and lists them in the mission's ``references``: the agent reads them offline, every cycle can cite
the same bytes, and the snapshot is reviewable and pinned by a SHA-256 manifest.

Fetching goes through the same egress rules as ``fetch_url``: http(s) only, no credentials in the
URL, IDNA 2008 host normalization, public addresses only, and every redirect hop re-checked against
the hosts the operator named. Pages are stored raw; HTML additionally gets a ``.txt`` rendering.
"""

from __future__ import annotations

import hashlib
import json
import re
from dataclasses import asdict, dataclass
from datetime import UTC, datetime
from pathlib import Path, PurePosixPath

import httpcore
import httpx

from lha.execution.tools.web import _html_to_text
from lha.safety.egress import (
    EgressDenied,
    EgressPolicy,
    Resolver,
    check_resolved_addresses,
    parse_url,
    system_resolver,
)
from lha.safety.pinned_http import PinnedNetworkBackend, PinnedTransport

MANIFEST = "MANIFEST.json"
MAX_BYTES = 10_000_000
MAX_REDIRECTS = 5
_SAFE = re.compile(r"[^A-Za-z0-9._-]+")


class VendorError(RuntimeError):
    """A URL could not be vendored (policy, network or size)."""


@dataclass
class VendoredFile:
    url: str
    path: str  # relative to the vendor directory
    sha256: str
    bytes: int
    content_type: str
    fetched_at: str
    text_path: str = ""


def _target_path(url: str, content_type: str) -> PurePosixPath:
    parsed = httpx.URL(url)
    parts = [
        _SAFE.sub("_", p).strip("._") or "_"
        for p in parsed.path.split("/")
        if p and p not in (".", "..")
    ]
    name = "/".join(parts) or "index"
    if parsed.query:
        name += "_" + hashlib.sha256(parsed.query).hexdigest()[:8]
    if "." not in name.rsplit("/", 1)[-1] and "html" in content_type:
        name += ".html"
    return PurePosixPath(_SAFE.sub("_", parsed.host)) / name


async def vendor_urls(
    urls: list[str],
    into: str | Path,
    *,
    client: httpx.AsyncClient | None = None,
    resolver: Resolver | None = None,
    network_backend: httpcore.AsyncNetworkBackend | None = None,
) -> list[VendoredFile]:
    """Fetch ``urls`` into ``into`` and (re)write its ``MANIFEST.json``; return what was saved.

    Without a ``client`` (a test seam), connections go through a ``PinnedNetworkBackend``: each
    hop's host is resolved once, checked, and only a checked address is dialled (no DNS
    rebinding). ``network_backend`` replaces the socket layer under the pinning (tests).
    """
    root = Path(into)
    root.mkdir(parents=True, exist_ok=True)
    allowed = {parse_url(u).host for u in urls}  # the operator's own URLs define the allow-list
    policy = EgressPolicy(allow_hosts=allowed)
    owned = client is None
    backend = PinnedNetworkBackend(network_backend) if owned else None
    http = client or httpx.AsyncClient(
        timeout=60.0,
        follow_redirects=False,
        trust_env=False,
        transport=PinnedTransport(backend) if backend is not None else None,
    )
    saved: list[VendoredFile] = []
    try:
        for url in urls:
            saved.append(
                await _vendor_one(http, policy, resolver or system_resolver, url, root, backend)
            )
    finally:
        if owned:
            await http.aclose()
    _write_manifest(root, saved)
    return saved


async def _vendor_one(
    http: httpx.AsyncClient,
    policy: EgressPolicy,
    resolver: Resolver,
    url: str,
    root: Path,
    backend: PinnedNetworkBackend | None = None,
) -> VendoredFile:
    current = url
    for _hop in range(MAX_REDIRECTS + 1):
        try:
            parsed = policy.check(current)
            addresses = await check_resolved_addresses(parsed.host, parsed.port, resolver)
        except EgressDenied as exc:
            raise VendorError(f"{current}: {exc}") from exc
        if backend is not None:
            backend.pin(parsed.host, addresses)  # the connection dials only these
        try:
            async with http.stream("GET", current) as resp:
                if resp.is_redirect:
                    location = resp.headers.get("location")
                    if not location:
                        raise VendorError(f"{current}: redirect without location")
                    current = str(resp.url.join(location))
                    continue
                resp.raise_for_status()
                data = bytearray()
                async for chunk in resp.aiter_bytes():
                    data += chunk
                    if len(data) > MAX_BYTES:
                        raise VendorError(f"{current}: larger than {MAX_BYTES} bytes")
                content_type = resp.headers.get("content-type", "").split(";")[0].strip()
                encoding = resp.encoding or "utf-8"
        except httpx.HTTPError as exc:
            raise VendorError(f"{current}: {exc}") from exc
        rel = _target_path(current, content_type)
        dest = root / rel
        dest.parent.mkdir(parents=True, exist_ok=True)
        dest.write_bytes(bytes(data))
        text_rel = ""
        if "html" in content_type:
            text_rel = f"{rel}.txt"
            text = _html_to_text(bytes(data).decode(encoding, errors="replace"))
            (root / text_rel).write_text(text + "\n", encoding="utf-8")
        return VendoredFile(
            url=url,
            path=str(rel),
            sha256=hashlib.sha256(data).hexdigest(),
            bytes=len(data),
            content_type=content_type,
            fetched_at=datetime.now(UTC).isoformat(timespec="seconds"),
            text_path=text_rel,
        )
    raise VendorError(f"{url}: more than {MAX_REDIRECTS} redirects")


def _write_manifest(root: Path, saved: list[VendoredFile]) -> None:
    """Merge ``saved`` into the manifest (a re-vendored URL replaces its old entry)."""
    path = root / MANIFEST
    entries: dict[str, dict[str, object]] = {}
    if path.exists():
        try:
            for entry in json.loads(path.read_text(encoding="utf-8")).get("files", []):
                entries[str(entry["url"])] = entry
        except (ValueError, KeyError, TypeError, AttributeError):
            entries = {}
    for item in saved:
        entries[item.url] = asdict(item)
    body = {"files": sorted(entries.values(), key=lambda e: str(e["url"]))}
    path.write_text(json.dumps(body, indent=2) + "\n", encoding="utf-8")

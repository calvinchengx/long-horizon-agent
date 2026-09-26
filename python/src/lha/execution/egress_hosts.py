"""Which hosts the Docker sandbox may reach, sorted by what they let code in the sandbox DO.

The command classifier (``lha.safety.commands``) gates ``git push``, ``npm publish``, ``curl``
and friends, but it deliberately does not look inside interpreter one-liners (``python -c``,
``node -e``): with no network that is harmless. Once the sandbox has an egress allow-list, such a
one-liner can send anything to any allowed host, and the proxy cannot tell a download from an
upload (``CONNECT`` is end-to-end TLS). So the allow-list is split by what a host accepts, and a
host that accepts writes has to be acknowledged by name:

- ``LHA_SANDBOX_EGRESS``: package registries' download hosts. Every entry must be one of
  ``PACKAGE_FETCH_HOSTS`` (exact names, default ports).
- ``LHA_SANDBOX_EGRESS_EXTRA_HOSTS``: any other host (a private mirror, a docs site). An entry
  that covers a known code-hosting / upload / object-store host (``WRITE_HOSTS``) is refused.
- ``LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS``: hosts the operator accepts code in the sandbox may
  push or upload to (``github.com`` for ``go get`` of an unproxied module, say).

This is a speed bump for the obvious channels, not a guarantee that a "fetch" host is read-only:
``registry.npmjs.org`` and ``crates.io`` accept ``publish`` with a token, ``storage.googleapis.com``
accepts writes to any bucket a signed URL grants, and a request's own path can carry data (the Go
module proxy fetches arbitrary module paths from their origin). Any sandbox egress therefore
counts as untrusted input + external comms under the Rule of Two
(``lha.execution.tools.toolset``); ``docs/09-safety-model.md`` states the residual risk.

Go mirrors this in ``go/internal/execution/egressproxy/policy.go``; ``spec/execution/
sandbox_egress.json`` pins both.
"""

from __future__ import annotations

from collections.abc import Sequence

from lha.execution.egress_proxy import AllowEntry, parse_allow_list

#: Download hosts of the common package registries (Python, Node, Go, Rust). ``storage.
#: googleapis.com`` is here because ``proxy.golang.org`` redirects module zips there.
PACKAGE_FETCH_HOSTS: tuple[str, ...] = (
    "pypi.org",
    "files.pythonhosted.org",
    "registry.npmjs.org",
    "proxy.golang.org",
    "sum.golang.org",
    "storage.googleapis.com",
    "crates.io",
    "static.crates.io",
    "index.crates.io",
)

#: Known hosts that accept pushes or uploads: code hosting, package upload endpoints, object
#: stores, container registries, paste and webhook services. A leading dot covers the domain and
#: its subdomains, as in the allow-list itself.
WRITE_HOSTS: tuple[str, ...] = (
    ".github.com",
    ".gitlab.com",
    ".bitbucket.org",
    ".codeberg.org",
    ".sr.ht",
    ".dev.azure.com",
    ".visualstudio.com",
    ".huggingface.co",
    "upload.pypi.org",
    ".test.pypi.org",
    ".amazonaws.com",
    ".blob.core.windows.net",
    ".r2.cloudflarestorage.com",
    ".digitaloceanspaces.com",
    ".backblazeb2.com",
    ".docker.io",
    "ghcr.io",
    ".pkg.dev",
    ".pastebin.com",
    "transfer.sh",
    "hooks.slack.com",
    ".discord.com",
    ".webhook.site",
)


class SandboxEgressError(ValueError):
    """The sandbox egress settings name a host in the wrong list (or a malformed entry)."""


EGRESS_SETTING = "LHA_SANDBOX_EGRESS"
EXTRA_SETTING = "LHA_SANDBOX_EGRESS_EXTRA_HOSTS"
WRITE_SETTING = "LHA_SANDBOX_EGRESS_ALLOW_WRITE_HOSTS"


def entry_text(entry: AllowEntry) -> str:
    """The canonical spelling of an allow-list entry (``.example.org:8443``)."""
    return ("." if entry.suffix else "") + entry.host + (f":{entry.port}" if entry.port else "")


def _parse(setting: str, items: Sequence[str]) -> tuple[AllowEntry, ...]:
    try:
        return parse_allow_list(items)
    except ValueError as exc:
        raise SandboxEgressError(f"{setting}: {exc}") from None


def _overlaps(entry: AllowEntry, known: AllowEntry) -> bool:
    """True if ``entry`` lets the sandbox reach any host ``known`` names."""
    return known.matches_host(entry.host) or (entry.suffix and entry.matches_host(known.host))


def sandbox_allow_list(
    egress: Sequence[str], extra: Sequence[str] = (), write: Sequence[str] = ()
) -> list[str]:
    """The proxy allow-list for the three settings, or ``SandboxEgressError`` naming the setting.

    ``egress`` entries must be package-fetch hosts; ``extra`` entries must not cover a known
    write host; ``write`` entries are accepted as acknowledged. Returns the canonical entries,
    de-duplicated, in setting order.
    """
    fetch = set(PACKAGE_FETCH_HOSTS)
    known_writes = parse_allow_list(WRITE_HOSTS)
    out: list[str] = []
    for entry in _parse(EGRESS_SETTING, egress):
        if entry.suffix or entry.port is not None or entry.host not in fetch:
            raise SandboxEgressError(
                f"{EGRESS_SETTING} entry {entry_text(entry)} is not a package-fetch host "
                f"({', '.join(PACKAGE_FETCH_HOSTS)}); list any other host in {EXTRA_SETTING}, "
                f"or one that accepts pushes or uploads in {WRITE_SETTING}"
            )
        out.append(entry_text(entry))
    for entry in _parse(EXTRA_SETTING, extra):
        for known in known_writes:
            if _overlaps(entry, known):
                raise SandboxEgressError(
                    f"{EXTRA_SETTING} entry {entry_text(entry)} reaches {entry_text(known)}, "
                    f"which accepts pushes or uploads: code in the sandbox could send the "
                    f"workspace there. List it in {WRITE_SETTING} to accept that"
                )
        out.append(entry_text(entry))
    out.extend(entry_text(entry) for entry in _parse(WRITE_SETTING, write))
    return list(dict.fromkeys(out))

"""Build the configured System One model (``LHA_SYSTEM_ONE_*``) and what uses it."""

from __future__ import annotations

from lha.config import Settings
from lha.contracts.system_one import SystemOneModel
from lha.governor.metering import CostMeter
from lha.safety.egress import EgressDenied, parse_url
from lha.systemone.client import SystemOneClient, default_price_in_per_mtok, is_local_endpoint
from lha.systemone.stub import StubSystemOne
from lha.systemone.triage import StallTriage


def build_system_one(settings: Settings, meter: CostMeter | None) -> SystemOneModel | None:
    """The configured model, metered by ``meter``; ``None`` when ``system_one_backend=off``.

    Raises ``ValueError`` for a configuration that cannot be used safely: a malformed or
    non-https remote endpoint, a remote endpoint without a key, an endpoint with no price (its
    spend would make the ledger unverifiable), or a remote endpoint on a run that declares
    ``private_data`` without ``system_one_private_data_ok``.
    """
    if settings.system_one_backend == "off":
        return None
    if settings.system_one_backend == "stub":
        return StubSystemOne()
    endpoint = settings.system_one_endpoint
    try:
        target = parse_url(endpoint)
    except EgressDenied as exc:
        raise ValueError(f"LHA_SYSTEM_ONE_ENDPOINT is not usable: {exc}") from exc
    local = is_local_endpoint(target)
    if not local and target.scheme != "https":
        raise ValueError("LHA_SYSTEM_ONE_ENDPOINT is not usable: a remote endpoint must use https")
    if settings.private_data and not local and not settings.system_one_private_data_ok:
        raise ValueError(
            "LHA_PRIVATE_DATA is set and LHA_SYSTEM_ONE_ENDPOINT is remote: it would receive "
            "workspace text. Use a loopback endpoint (a self-hosted Kev) or set "
            "LHA_SYSTEM_ONE_PRIVATE_DATA_OK=true"
        )
    key = settings.system_one_api_key.get_secret_value() if settings.system_one_api_key else ""
    if not local and not key:
        raise ValueError("LHA_SYSTEM_ONE_API_KEY is required for a remote System One endpoint")
    price = settings.system_one_price_in_per_mtok
    if price is None:
        price = default_price_in_per_mtok(endpoint)
    if price is None and not settings.allow_unpriced_models:
        raise ValueError(
            f"no price for the System One endpoint {endpoint!r}: set "
            "LHA_SYSTEM_ONE_PRICE_IN_PER_MTOK (or LHA_ALLOW_UNPRICED_MODELS=true)"
        )
    try:
        return SystemOneClient(
            model=settings.system_one_model,
            endpoint=endpoint,
            api_key=key,
            price_in_per_mtok=price,
            timeout_s=settings.system_one_timeout_s,
            meter=meter,
        )
    except EgressDenied as exc:
        raise ValueError(f"LHA_SYSTEM_ONE_ENDPOINT is not usable: {exc}") from exc


def build_stall_triage(settings: Settings, model: SystemOneModel | None) -> StallTriage | None:
    """Stall triage over ``model`` when it is configured and ``system_one_triage`` is on."""
    if model is None or not settings.system_one_triage:
        return None
    return StallTriage(
        model,
        threshold=settings.system_one_triage_threshold,
        min_failures=settings.system_one_triage_min_failures,
    )


async def close_system_one(model: SystemOneModel | None) -> None:
    aclose = getattr(model, "aclose", None)
    if callable(aclose):
        await aclose()

"""Per-role model routing.

High-leverage roles (Planner/Lead/Reviewer/Judge) get Opus; mid roles Sonnet; cheap search Haiku.
Only meaningful for the Claude backend; other backends use the single configured model. This keeps
cost proportional to the value of each role's judgment.

Pass one shared ``httpx.AsyncClient`` for every role so all providers reuse a single connection
pool (the caller owns and closes it) instead of each role leaking its own client.
"""

from __future__ import annotations

import httpx

from lha.agents.roles import ROLES, claude_model_for
from lha.config import Settings, get_settings
from lha.contracts.model import ModelProvider
from lha.model import build_provider


def model_for_role(
    role_name: str,
    settings: Settings | None = None,
    *,
    client: httpx.AsyncClient | None = None,
) -> ModelProvider:
    """Return a model tuned to ``role_name``'s tier (Claude backend) or the default model."""
    settings = settings or get_settings()
    role = ROLES.get(role_name)
    if settings.model_backend == "claude" and role is not None:
        return build_provider(settings, model_name=claude_model_for(role.tier), client=client)
    return build_provider(settings, client=client)

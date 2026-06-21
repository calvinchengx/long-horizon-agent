"""The mission contract.

A ``Mission`` is the long-horizon goal the org works toward. Keeping it behind a Protocol
is what makes the engine domain-agnostic: software-engineering is the v1 implementation, but
any domain can supply its own ``Mission`` (and matching ``Verifier``) without touching the
durable spine.
"""

from __future__ import annotations

from typing import Protocol, runtime_checkable

from pydantic import BaseModel


class MissionSpec(BaseModel):
    """The immutable specification of a mission."""

    mission_id: str
    title: str
    description: str
    # Human-readable definition of done. The machine-checkable gate lives in the Verifier;
    # this is the narrative the agents recite and reason about.
    acceptance: str
    repo_url: str | None = None
    schema_version: int = 1


@runtime_checkable
class Mission(Protocol):
    """A concrete mission. Implementations live in ``src/lha/state/``/domain packages."""

    spec: MissionSpec

    def system_anchor_text(self) -> str:
        """Return the mission anchor recited into the agent's context every cycle.

        This is the anti-drift mechanism: a compact, authoritative restatement of the goal
        and constraints, re-read on every cycle so a context reset never loses the mission.
        """
        ...

"""Long-Horizon Agent (LHA).

A durable, self-improving agent organization for long-horizon software missions.

The package is organized by plane (see docs/05-architecture.md):
- ``contracts``: the swappable Protocols that keep every plane decoupled.
- ``durable``:   the Temporal control plane (the spine).
- ``agents``:    the role "brains" (Claude Agent SDK loops).
- ``state``:     the git mission anchor (the source of truth outside the window).
- ``memory``:    tiered memory + skill library.
- ``execution``: sandbox + tools.
- ``verify``:    the deterministic verifier (the only merge gate).
"""

__version__ = "0.1.0"

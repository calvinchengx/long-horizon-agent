"""CAN-orphan reconciliation.

When the orchestrator Continue-As-News (or recovers from a crash), an in-flight sub-agent may have
left real-world side effects (a pushed branch) that the journal doesn't fully own. Before
re-spawning any ``in_progress`` ticket, reconcile against ACTUAL git state: adopt a ticket whose
branch exists (locally or as a remote-tracking branch) AND carries work beyond the base, re-spawn
one whose branch is missing or empty. This makes re-derivation idempotent against external state
rather than blindly trusting the checklist.
"""

from __future__ import annotations

from dataclasses import dataclass

from lha.state import git_ops


@dataclass
class ReconcileAction:
    ticket_id: str
    action: str  # "adopt" | "respawn"
    detail: str


def _resolve_branch(branch: str, local: set[str], remote: list[str]) -> str | None:
    """The ref holding ``branch``: the local branch, else a remote-tracking ``<remote>/<branch>``."""
    if branch in local:
        return branch
    for ref in remote:
        if ref == branch or ref.split("/", 1)[-1] == branch:
            return ref
    return None


def reconcile_in_flight(
    workdir: str, in_progress: list[tuple[str, str | None]], *, base: str = "HEAD"
) -> list[ReconcileAction]:
    """For each (ticket_id, branch) still in progress, decide adopt vs re-spawn from real git.

    ``base`` is what a ticket branch must have commits beyond to count as real work (a branch
    created but never committed to is re-spawned, not adopted).
    """
    local = set(git_ops.list_branches(workdir))
    remote = [b for b in git_ops.list_branches(workdir, include_remote=True) if b not in local]
    actions: list[ReconcileAction] = []
    for ticket_id, branch in in_progress:
        if branch is None:
            actions.append(ReconcileAction(ticket_id, "respawn", "no branch recorded"))
            continue
        ref = _resolve_branch(branch, local, remote)
        if ref is None:
            actions.append(ReconcileAction(ticket_id, "respawn", f"branch {branch!r} missing"))
            continue
        ahead = git_ops.commits_ahead(workdir, base, ref)
        if ahead == 0:
            actions.append(
                ReconcileAction(
                    ticket_id, "respawn", f"branch {ref!r} has no commits beyond {base}"
                )
            )
        else:
            actions.append(
                ReconcileAction(ticket_id, "adopt", f"branch {ref!r} exists ({ahead} commit(s))")
            )
    return actions

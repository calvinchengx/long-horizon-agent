"""Per-writer worktrees and the ``BranchIntegrator`` — the sole writer to the mission branch.

In an ``orchestrate`` parallel wave every implementer works in its own git worktree, on its own
branch (``lha/<writer>/<cycle_id>``), created from the mission branch's ``HEAD``. Nothing an
implementer does touches the mission workspace. After the implementers finish, the integrator
takes each verified branch in turn and:

1. refuses it if its own verification failed, or if it changed files its writer does not own
   (the git-layer ownership check — this catches writes made through the shell);
2. ``git merge --no-ff --no-commit`` it into the mission branch (a conflict aborts the merge);
3. re-runs the gating checks on the MERGED mission workspace; if they fail, ``git merge --abort``
   puts the mission branch back exactly as it was.

Only a branch that passed both verifications stays merged; the caller then commits the merge
together with the anchor checkpoint (``GitMissionAnchor.commit_checkpoint``), so the merge commit
IS the checkpoint. The integrator is deterministic code, not a model: with file ownership keeping
write-sets disjoint there is nothing for a model to resolve, and a conflict is reported as a
failed attempt rather than papered over.
"""

from __future__ import annotations

import asyncio
import shutil
from dataclasses import dataclass, field
from pathlib import Path

from lha.contracts.sandbox import SandboxSession
from lha.contracts.verify import Check, VerificationResult, Verifier
from lha.coordination.ownership import OwnershipViolation
from lha.state import git_ops

WORKTREE_DIR = "lha-worktrees"  # under the repository's .git directory
BRANCH_PREFIX = "lha/implementer-"


def worktree_root(workdir: str | Path) -> Path:
    """Where implementer worktrees live: inside ``.git`` (never tracked, never in the diff)."""
    return git_ops.git_dir(workdir) / WORKTREE_DIR


def branch_for(writer: str, cycle_id: str) -> str:
    return f"lha/{writer}/{cycle_id}"


def add_worktree(workdir: str | Path, *, branch: str, base: str) -> Path:
    """Create a worktree on a new ``branch`` at ``base``; returns its path."""
    path = worktree_root(workdir) / branch.replace("/", "_")
    if path.exists():
        remove_worktree(workdir, path=path, branch=branch)
    path.parent.mkdir(parents=True, exist_ok=True)
    git_ops.run_git(workdir, "worktree", "add", "--quiet", "-B", branch, str(path), base)
    return path


def remove_worktree(workdir: str | Path, *, path: Path, branch: str | None = None) -> None:
    """Remove a worktree (and its branch); tolerant of half-created state."""
    git_ops.run_git(workdir, "worktree", "remove", "--force", str(path), check=False)
    if path.exists():
        shutil.rmtree(path, ignore_errors=True)
    git_ops.run_git(workdir, "worktree", "prune", check=False)
    if branch:
        git_ops.run_git(workdir, "branch", "-D", branch, check=False)


def prune_worktrees(workdir: str | Path) -> None:
    """Remove every implementer worktree and branch left behind by an interrupted run."""
    root = worktree_root(workdir)
    if root.exists():
        for child in sorted(root.iterdir()):
            remove_worktree(workdir, path=child)
    git_ops.run_git(workdir, "worktree", "prune", check=False)
    for branch in git_ops.list_branches(workdir):
        if branch.startswith(BRANCH_PREFIX):
            git_ops.run_git(workdir, "branch", "-D", branch, check=False)


def commit_worktree(path: Path, message: str) -> str:
    """Commit an implementer's work on its branch, excluding the harness-owned ``.lha/``."""
    if git_ops.exists_at_head(path, ".lha"):
        git_ops.run_git(path, "checkout", "HEAD", "--", ".lha")
        git_ops.run_git(path, "clean", "-fdq", "--", ".lha")
    return git_ops.commit_all(path, message)


@dataclass
class IntegrationOutcome:
    """What happened when one implementer branch was offered to the mission branch."""

    merged: bool  # the branch is merged (uncommitted, MERGE_HEAD set) and re-verified
    reason: str = ""  # why it was refused (empty when merged)
    verification: VerificationResult | None = None  # the post-merge verification, if it ran
    violations: list[OwnershipViolation] = field(default_factory=list)


class BranchIntegrator:
    """Merges verified implementer branches into the mission branch, re-verifying each merge."""

    def __init__(
        self,
        *,
        workdir: str | Path,
        session: SandboxSession,
        verifier: Verifier,
        checks: list[Check],
    ) -> None:
        self._workdir = Path(workdir)
        self._session = session
        self._verifier = verifier
        self._checks = checks

    async def integrate(
        self,
        *,
        branch: str,
        branch_head: str,
        base: str,
        verified: bool,
        violations: list[OwnershipViolation],
        checks: list[Check] | None = None,
    ) -> IntegrationOutcome:
        """Merge ``branch`` into the mission branch iff it is verified, owned and green merged.

        ``checks`` overrides the integrator's checks for this branch (e.g. the mission checks
        plus the item's witnesses). On success the merge is left staged (``MERGE_HEAD`` set) for
        the caller's checkpoint commit; on any refusal the mission workspace is left as it was.
        """
        if not verified:
            return IntegrationOutcome(merged=False, reason="the branch failed verification")
        if violations:
            files = ", ".join(f"{v.path} (owner: {v.owner or 'lead'})" for v in violations)
            return IntegrationOutcome(
                merged=False,
                reason=f"ownership violation: the branch changed files it does not own: {files}",
                violations=violations,
            )
        if branch_head and branch_head != base:
            try:
                await asyncio.to_thread(
                    git_ops.run_git, self._workdir, "merge", "--no-ff", "--no-commit", branch
                )
            except git_ops.GitError as exc:  # conflict, or git refused (e.g. files in the way)
                await asyncio.to_thread(self.abort)
                return IntegrationOutcome(
                    merged=False, reason=f"merge of {branch} failed: {str(exc)[:500]}"
                )
        gate = self._checks if checks is None else checks
        verification = await self._verifier.verify(self._session, gate)
        if not verification.all_green:
            await asyncio.to_thread(self.abort)
            return IntegrationOutcome(
                merged=False,
                reason="verification failed on the merged mission branch:\n"
                + verification.failure_report(),
                verification=verification,
            )
        return IntegrationOutcome(merged=True, verification=verification)

    def abort(self) -> None:
        """Undo an in-progress merge (no-op when none is in progress)."""
        if (git_ops.git_dir(self._workdir) / "MERGE_HEAD").exists():
            git_ops.run_git(self._workdir, "merge", "--abort", check=False)

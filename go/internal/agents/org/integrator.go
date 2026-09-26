package org

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/coordination"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Per-writer worktrees and the BranchIntegrator — the sole writer to the mission branch (python:
// lha.agents.integrator).
//
// In a parallel wave every implementer works in its own git worktree, on its own branch
// (lha/<writer>/<cycle_id>), created from the mission branch's HEAD. After the implementers
// finish, the integrator takes each verified branch in turn and:
//
//  1. refuses it if its own verification failed, or if it changed files its writer does not own
//     (the git-layer ownership check — this catches writes made through the shell);
//  2. `git merge --no-ff --no-commit` it into the mission branch (a conflict aborts the merge);
//  3. re-runs the gating checks on the MERGED mission workspace; if they fail, `git merge
//     --abort` puts the mission branch back exactly as it was.
//
// Only a branch that passed both verifications stays merged; the caller then commits the merge
// together with the anchor checkpoint, so the merge commit IS the checkpoint.

// WorktreeDir is where implementer worktrees live, under the repository's .git directory.
const WorktreeDir = "lha-worktrees"

// BranchPrefix is the prefix of every implementer branch.
const BranchPrefix = "lha/implementer-"

// WorktreeRoot is where implementer worktrees live: inside .git (never tracked, never in a diff).
func WorktreeRoot(ctx context.Context, workdir string) (string, error) {
	gitDir, err := state.GitDir(ctx, workdir)
	if err != nil {
		return "", err
	}
	return filepath.Join(gitDir, WorktreeDir), nil
}

// BranchFor is the branch of writer's work in cycle cycleID.
func BranchFor(writer, cycleID string) string { return "lha/" + writer + "/" + cycleID }

func gitNoCheck(ctx context.Context, cwd string, args ...string) {
	_, _ = state.RunGitWith(ctx, cwd, state.RunOptions{NoCheck: true}, args...)
}

// AddWorktree creates a worktree on a new branch at base and returns its path.
func AddWorktree(ctx context.Context, workdir, branch, base string) (string, error) {
	root, err := WorktreeRoot(ctx, workdir)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, strings.ReplaceAll(branch, "/", "_"))
	if _, err := os.Stat(path); err == nil {
		RemoveWorktree(ctx, workdir, path, branch)
	}
	if err := os.MkdirAll(root, 0o777); err != nil {
		return "", err
	}
	if _, err := state.RunGit(ctx, workdir, "worktree", "add", "--quiet", "-B", branch, path, base); err != nil {
		return "", err
	}
	return path, nil
}

// RemoveWorktree removes a worktree (and its branch, when given); tolerant of half-created state.
func RemoveWorktree(ctx context.Context, workdir, path, branch string) {
	ctx = context.WithoutCancel(ctx)
	gitNoCheck(ctx, workdir, "worktree", "remove", "--force", path)
	if _, err := os.Stat(path); err == nil {
		_ = os.RemoveAll(path)
	}
	gitNoCheck(ctx, workdir, "worktree", "prune")
	if branch != "" {
		gitNoCheck(ctx, workdir, "branch", "-D", branch)
	}
}

// PruneWorktrees removes every implementer worktree and branch left behind by an interrupted run.
func PruneWorktrees(ctx context.Context, workdir string) error {
	ctx = context.WithoutCancel(ctx)
	root, err := WorktreeRoot(ctx, workdir)
	if err != nil {
		return err
	}
	if entries, err := os.ReadDir(root); err == nil {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			RemoveWorktree(ctx, workdir, filepath.Join(root, name), "")
		}
	}
	gitNoCheck(ctx, workdir, "worktree", "prune")
	branches, err := state.ListBranches(ctx, workdir, false)
	if err != nil {
		return err
	}
	for _, branch := range branches {
		if strings.HasPrefix(branch, BranchPrefix) {
			gitNoCheck(ctx, workdir, "branch", "-D", branch)
		}
	}
	return nil
}

// CommitWorktree commits an implementer's work on its branch, excluding the harness-owned .lha/.
func CommitWorktree(ctx context.Context, path, message string) (string, error) {
	tracked, err := state.ExistsAtHead(ctx, path, ".lha")
	if err != nil {
		return "", err
	}
	if tracked {
		if _, err := state.RunGit(ctx, path, "checkout", "HEAD", "--", ".lha"); err != nil {
			return "", err
		}
		if _, err := state.RunGit(ctx, path, "clean", "-fdq", "--", ".lha"); err != nil {
			return "", err
		}
	}
	return state.CommitAll(ctx, path, message)
}

// IntegrationOutcome is what happened when one implementer branch was offered to the mission
// branch.
type IntegrationOutcome struct {
	// Merged: the branch is merged (uncommitted, MERGE_HEAD set) and re-verified.
	Merged bool
	// Reason is why it was refused ("" when merged).
	Reason string
	// Verification is the post-merge verification, if it ran.
	Verification *contracts.VerificationResult
	Violations   []coordination.OwnershipViolation
}

// BranchIntegrator merges verified implementer branches into the mission branch, re-verifying
// each merge. It is deterministic code, not a model: with file ownership keeping write-sets
// disjoint there is nothing for a model to resolve, and a conflict is a failed attempt.
type BranchIntegrator struct {
	Workdir  string
	Session  contracts.SandboxSession
	Verifier contracts.Verifier
	Checks   []contracts.Check
}

// Integrate merges branch into the mission branch iff it is verified, owned and green merged.
// checks (non-nil) override the integrator's checks for this branch (e.g. the mission checks plus
// the item's witnesses). On success the merge is left staged (MERGE_HEAD set) for the caller's
// checkpoint commit; on any refusal the mission workspace is left as it was.
func (b *BranchIntegrator) Integrate(ctx context.Context, branch, branchHead, base string, verified bool, violations []coordination.OwnershipViolation, checks []contracts.Check) (IntegrationOutcome, error) {
	if !verified {
		return IntegrationOutcome{Reason: "the branch failed verification"}, nil
	}
	if len(violations) > 0 {
		files := make([]string, len(violations))
		for i, v := range violations {
			owner := v.Owner
			if owner == "" {
				owner = "lead"
			}
			files[i] = v.Path + " (owner: " + owner + ")"
		}
		return IntegrationOutcome{
			Reason:     "ownership violation: the branch changed files it does not own: " + strings.Join(files, ", "),
			Violations: violations,
		}, nil
	}
	if branchHead != "" && branchHead != base {
		if _, err := state.RunGit(ctx, b.Workdir, "merge", "--no-ff", "--no-commit", branch); err != nil {
			var gitErr *state.GitError
			if !errors.As(err, &gitErr) {
				return IntegrationOutcome{}, err
			}
			// A conflict, or git refused (e.g. files in the way).
			if abortErr := b.Abort(ctx); abortErr != nil {
				return IntegrationOutcome{}, abortErr
			}
			return IntegrationOutcome{Reason: "merge of " + branch + " failed: " + pyfmt.Head(err.Error(), 500)}, nil
		}
	}
	gate := b.Checks
	if checks != nil {
		gate = checks
	}
	verification, err := b.Verifier.Verify(ctx, b.Session, gate)
	if err != nil {
		_ = b.Abort(ctx)
		return IntegrationOutcome{}, err
	}
	if !verification.AllGreen {
		if err := b.Abort(ctx); err != nil {
			return IntegrationOutcome{}, err
		}
		return IntegrationOutcome{
			Reason:       "verification failed on the merged mission branch:\n" + verification.FailureReport(0),
			Verification: &verification,
		}, nil
	}
	return IntegrationOutcome{Merged: true, Verification: &verification}, nil
}

// Abort undoes an in-progress merge (no-op when none is in progress).
func (b *BranchIntegrator) Abort(ctx context.Context) error {
	return AbortMerge(ctx, b.Workdir)
}

// AbortMerge runs `git merge --abort` in workdir when a merge is in progress.
func AbortMerge(ctx context.Context, workdir string) error {
	ctx = context.WithoutCancel(ctx)
	gitDir, err := state.GitDir(ctx, workdir)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(gitDir, "MERGE_HEAD")); err == nil {
		gitNoCheck(ctx, workdir, "merge", "--abort")
	}
	return nil
}

// Package state is the mission-anchor plane: git as the durable source of truth.
//
// It mirrors python/src/lha/state. gitops.go is a thin wrapper over the git CLI (git itself is
// the durable substrate); every invocation runs with a locale-independent, non-interactive
// environment (LC_ALL=C, GIT_TERMINAL_PROMPT=0) and a timeout, and decisions are made from exit
// codes / plumbing output, never from localizable porcelain messages.
package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GitTimeout bounds any single git invocation (a hung credential helper or lock must not wedge a
// worker forever).
const GitTimeout = 120 * time.Second

// ResetKeep lists the paths ResetToHead never deletes even though they are untracked/ignored:
// dependency environments, local env/secret files, and the harness's local object store.
var ResetKeep = []string{".venv", "venv", "node_modules", ".env", ".env.*", ".lha/objects"}

// GitError is a git command that exited non-zero (or timed out).
type GitError struct{ Message string }

func (e *GitError) Error() string { return e.Message }

// gitEnv is the process environment with the non-interactive C-locale overrides applied.
func gitEnv() []string {
	overrides := map[string]string{
		"LC_ALL":              "C",
		"LANG":                "C",
		"GIT_TERMINAL_PROMPT": "0", // never block on a credential prompt
		"GIT_ASKPASS":         "",
		"SSH_ASKPASS":         "",
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := overrides[k]; !ok {
			env = append(env, kv)
		}
	}
	for _, k := range []string{"LC_ALL", "LANG", "GIT_TERMINAL_PROMPT", "GIT_ASKPASS", "SSH_ASKPASS"} {
		env = append(env, k+"="+overrides[k])
	}
	return env
}

// gitResult is a completed git process (stdout/stderr decoded with universal newlines).
type gitResult struct {
	ReturnCode int
	Stdout     string
	Stderr     string
}

func runRaw(ctx context.Context, cwd string, args []string, timeout time.Duration) (gitResult, error) {
	return runRawEnv(ctx, cwd, args, timeout, nil)
}

func runRawEnv(ctx context.Context, cwd string, args []string, timeout time.Duration, extraEnv map[string]string) (gitResult, error) {
	if timeout <= 0 {
		timeout = GitTimeout
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, "git", args...)
	cmd.Dir = cwd
	cmd.Env = gitEnv()
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return gitResult{}, fmt.Errorf("git %s: %w", strings.Join(args, " "), ctx.Err())
	}
	if tctx.Err() != nil {
		return gitResult{}, &GitError{fmt.Sprintf("git %s timed out after %ss",
			strings.Join(args, " "), pyFloatRepr(timeout.Seconds()))}
	}
	res := gitResult{
		Stdout: universalNewlines(stdout.String()),
		Stderr: universalNewlines(stderr.String()),
	}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return gitResult{}, err // git missing, cwd missing, ...
		}
		res.ReturnCode = exitErr.ExitCode()
	}
	return res, nil
}

// RunOptions tunes RunGitWith. The zero value is Python's default (check=True, timeout=None).
type RunOptions struct {
	NoCheck bool          // do not turn a non-zero exit into a GitError
	Timeout time.Duration // 0 means GitTimeout
	// Env is added to the git environment (python: _git_with_env), e.g. GIT_INDEX_FILE.
	Env map[string]string
}

// RunGit runs `git <args>` in cwd and returns stdout (stripped); non-zero exit is a *GitError.
func RunGit(ctx context.Context, cwd string, args ...string) (string, error) {
	return RunGitWith(ctx, cwd, RunOptions{}, args...)
}

// RunGitWith is RunGit with options.
func RunGitWith(ctx context.Context, cwd string, opts RunOptions, args ...string) (string, error) {
	res, err := runRawEnv(ctx, cwd, args, opts.Timeout, opts.Env)
	if err != nil {
		return "", err
	}
	if !opts.NoCheck && res.ReturnCode != 0 {
		return "", &GitError{fmt.Sprintf("git %s failed (%d): %s",
			strings.Join(args, " "), res.ReturnCode, pyStrip(res.Stderr))}
	}
	return pyStrip(res.Stdout), nil
}

// resolvePath mirrors pathlib's Path.resolve(): absolute, symlinks resolved where possible.
func resolvePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// Toplevel returns the resolved top-level directory of the work tree containing cwd, or "" if
// cwd is not inside a work tree.
func Toplevel(ctx context.Context, cwd string) (string, error) {
	res, err := runRaw(ctx, cwd, []string{"rev-parse", "--show-toplevel"}, 0)
	if err != nil {
		return "", err
	}
	out := pyStrip(res.Stdout)
	if res.ReturnCode != 0 || out == "" {
		return "", nil
	}
	return resolvePath(out), nil
}

// IsRepo reports whether cwd is the TOP LEVEL of a git work tree. Merely being inside some work
// tree is not enough: a workdir nested in another repository must get its own repo, otherwise
// config/add/commit would act on the enclosing repository.
func IsRepo(ctx context.Context, cwd string) (bool, error) {
	root, err := Toplevel(ctx, cwd)
	if err != nil || root == "" {
		return false, err
	}
	return root == resolvePath(cwd), nil
}

// Default commit identity for InitRepo.
const (
	DefaultAuthorName  = "LHA Agent"
	DefaultAuthorEmail = "agent@lha.local"
)

// InitRepo initializes a repo at cwd (idempotent) with the default local identity.
func InitRepo(ctx context.Context, cwd string) error {
	return InitRepoAs(ctx, cwd, DefaultAuthorName, DefaultAuthorEmail)
}

// InitRepoAs initializes a repo at cwd (idempotent) and sets a local identity so commits work.
func InitRepoAs(ctx context.Context, cwd, authorName, authorEmail string) error {
	if err := os.MkdirAll(cwd, 0o777); err != nil {
		return err
	}
	ok, err := IsRepo(ctx, cwd)
	if err != nil {
		return err
	}
	if !ok {
		if _, err := RunGit(ctx, cwd, "init", "-b", "main"); err != nil {
			return err
		}
	}
	for _, kv := range [][2]string{
		{"user.name", authorName}, {"user.email", authorEmail}, {"commit.gpgsign", "false"},
	} {
		if _, err := RunGit(ctx, cwd, "config", kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// HasCommits reports whether the repo has at least one commit.
func HasCommits(ctx context.Context, cwd string) (bool, error) {
	res, err := runRaw(ctx, cwd, []string{"rev-parse", "--verify", "--quiet", "HEAD"}, 0)
	if err != nil {
		return false, err
	}
	return res.ReturnCode == 0, nil
}

// HeadSHA returns the current HEAD sha, or "" if there are no commits yet.
func HeadSHA(ctx context.Context, cwd string) (string, error) {
	ok, err := HasCommits(ctx, cwd)
	if err != nil || !ok {
		return "", err
	}
	return RunGit(ctx, cwd, "rev-parse", "HEAD")
}

// CommitAll stages everything and commits; it returns the resulting HEAD sha.
//
// forcePaths are additionally staged with `git add -f` so they are committed even when the
// repository's .gitignore excludes them. If there is nothing to commit this is a no-op that
// returns the current HEAD (a "checkpoint with no changes" is benign and idempotent).
func CommitAll(ctx context.Context, cwd, message string, forcePaths ...string) (string, error) {
	if _, err := RunGit(ctx, cwd, "add", "-A"); err != nil {
		return "", err
	}
	var existing []string
	for _, p := range forcePaths {
		if _, err := os.Stat(filepath.Join(cwd, p)); err == nil {
			existing = append(existing, p)
		}
	}
	if len(existing) > 0 {
		if _, err := RunGit(ctx, cwd, append([]string{"add", "-f", "--"}, existing...)...); err != nil {
			return "", err
		}
	}
	// Exit code, not porcelain text: 0 = index matches HEAD (nothing staged), 1 = changes.
	staged, err := runRaw(ctx, cwd, []string{"diff", "--cached", "--quiet"}, 0)
	if err != nil {
		return "", err
	}
	if staged.ReturnCode == 0 {
		return HeadSHA(ctx, cwd)
	}
	if staged.ReturnCode != 1 {
		return "", &GitError{fmt.Sprintf("git diff --cached failed (%d): %s",
			staged.ReturnCode, pyStrip(staged.Stderr))}
	}
	if _, err := RunGit(ctx, cwd, "commit", "-m", message); err != nil {
		return "", err
	}
	return HeadSHA(ctx, cwd)
}

// CommitPaths commits ONLY paths (force-added, so ignored files count) and returns the HEAD sha.
// Every other change in the work tree or the index is left exactly as it was (git commit
// --only). A no-op returning the current HEAD when those paths are unchanged.
func CommitPaths(ctx context.Context, cwd, message string, paths ...string) (string, error) {
	var existing []string
	for _, p := range paths {
		if _, err := os.Stat(filepath.Join(cwd, p)); err == nil {
			existing = append(existing, p)
		}
	}
	if len(existing) == 0 {
		return HeadSHA(ctx, cwd)
	}
	if _, err := RunGit(ctx, cwd, append([]string{"add", "-f", "--"}, existing...)...); err != nil {
		return "", err
	}
	staged, err := runRaw(ctx, cwd, append([]string{"diff", "--cached", "--quiet", "--"}, existing...), 0)
	if err != nil {
		return "", err
	}
	if staged.ReturnCode == 0 {
		return HeadSHA(ctx, cwd)
	}
	if staged.ReturnCode != 1 {
		return "", &GitError{fmt.Sprintf("git diff --cached failed (%d): %s",
			staged.ReturnCode, pyStrip(staged.Stderr))}
	}
	if _, err := RunGit(ctx, cwd, append([]string{"commit", "--only", "-m", message, "--"}, existing...)...); err != nil {
		return "", err
	}
	return HeadSHA(ctx, cwd)
}

// ExistsAtHead reports whether relpath (relative to cwd, not the repo root) exists in HEAD.
func ExistsAtHead(ctx context.Context, cwd, relpath string) (bool, error) {
	ok, err := HasCommits(ctx, cwd)
	if err != nil || !ok {
		return false, err
	}
	res, err := runRaw(ctx, cwd, []string{"cat-file", "-e", "HEAD:./" + relpath}, 0)
	if err != nil {
		return false, err
	}
	return res.ReturnCode == 0, nil
}

// ShowAtHead returns the content of relpath (relative to cwd) at HEAD, stripped like every
// RunGit output (a *GitError when it does not exist).
func ShowAtHead(ctx context.Context, cwd, relpath string) (string, error) {
	return RunGit(ctx, cwd, "show", "HEAD:./"+relpath)
}

// ShowAtHeadRaw returns the content of relpath at HEAD without stripping it (a hash-chained log's
// trailing newline is significant); line endings are normalized like ShowAtHead's.
func ShowAtHeadRaw(ctx context.Context, cwd, relpath string) ([]byte, error) {
	args := []string{"show", "HEAD:./" + relpath}
	res, err := runRaw(ctx, cwd, args, 0)
	if err != nil {
		return nil, err
	}
	if res.ReturnCode != 0 {
		return nil, &GitError{fmt.Sprintf("git %s failed (%d): %s",
			strings.Join(args, " "), res.ReturnCode, pyStrip(res.Stderr))}
	}
	return []byte(res.Stdout), nil
}

// GitDir returns the absolute path of the repository's .git directory.
func GitDir(ctx context.Context, cwd string) (string, error) {
	return RunGit(ctx, cwd, "rev-parse", "--absolute-git-dir")
}

// ResetToHead discards ALL uncommitted work (tracked edits, staged changes, untracked and ignored
// files), keeping ResetKeep. No-op on a repo without commits.
func ResetToHead(ctx context.Context, cwd string) error {
	return ResetToHeadKeep(ctx, cwd, ResetKeep)
}

// ResetToHeadKeep is ResetToHead with an explicit list of paths that survive the clean.
func ResetToHeadKeep(ctx context.Context, cwd string, keep []string) error {
	ok, err := HasCommits(ctx, cwd)
	if err != nil || !ok {
		return err
	}
	if _, err := RunGit(ctx, cwd, "reset", "--hard", "--quiet", "HEAD"); err != nil {
		return err
	}
	args := []string{"clean", "-ffdxq"}
	for _, p := range keep {
		args = append(args, "-e", p)
	}
	_, err = RunGit(ctx, cwd, args...)
	return err
}

// DiscardChanges returns the work tree to HEAD: tracked edits and untracked (NOT ignored) files
// go. Unlike ResetToHead it keeps ignored files (dependency caches, build outputs, local
// remotes), because it runs after an ordinary failed attempt, not after a crash.
func DiscardChanges(ctx context.Context, cwd string) error {
	ok, err := HasCommits(ctx, cwd)
	if err != nil || !ok {
		return err
	}
	if _, err := RunGit(ctx, cwd, "reset", "--hard", "--quiet", "HEAD"); err != nil {
		return err
	}
	_, err = RunGit(ctx, cwd, "clean", "-fdq")
	return err
}

// ListBranches returns branch names (empty if no commits yet). With includeRemote the
// remote-tracking branches are included as <remote>/<name> (the <remote>/HEAD alias is skipped).
func ListBranches(ctx context.Context, cwd string, includeRemote bool) ([]string, error) {
	ok, err := HasCommits(ctx, cwd)
	if err != nil || !ok {
		return []string{}, err
	}
	args := []string{"for-each-ref", "--format=%(refname:short)", "refs/heads"}
	if includeRemote {
		args = append(args, "refs/remotes")
	}
	out, err := RunGit(ctx, cwd, args...)
	if err != nil {
		return nil, err
	}
	branches := []string{}
	for _, line := range pySplitlines(out, false) {
		if s := pyStrip(line); s != "" && !strings.HasSuffix(line, "/HEAD") {
			branches = append(branches, s)
		}
	}
	return branches, nil
}

// CommitsAhead is the number of commits reachable from ref but not from base.
func CommitsAhead(ctx context.Context, cwd, base, ref string) (int, error) {
	out, err := RunGit(ctx, cwd, "rev-list", "--count", base+".."+ref, "--")
	if err != nil {
		return 0, err
	}
	if out == "" {
		return 0, nil
	}
	return strconv.Atoi(out)
}

// LogOneline returns up to n recent commits as "<short-sha> <subject>" lines.
func LogOneline(ctx context.Context, cwd string, n int) ([]string, error) {
	ok, err := HasCommits(ctx, cwd)
	if err != nil || !ok {
		return []string{}, err
	}
	out, err := RunGit(ctx, cwd, "log", fmt.Sprintf("-%d", n), "--pretty=format:%h %s")
	if err != nil {
		return nil, err
	}
	lines := []string{}
	for _, line := range pySplitlines(out, false) {
		if pyStrip(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

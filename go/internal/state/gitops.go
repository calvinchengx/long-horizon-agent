// Package state is the mission-anchor plane: git as the durable source of truth.
//
// It mirrors python/src/lha/state. gitops.go is a thin wrapper over the git CLI (git itself is
// the durable substrate); every invocation runs with a locale-independent, non-interactive
// environment (LC_ALL=C, GIT_TERMINAL_PROMPT=0) and a timeout, and decisions are made from exit
// codes / plumbing output, never from localizable porcelain messages.
//
// Hardening (as in python/src/lha/state/git_ops.py): the work tree a harness git command runs in
// was just written by an untrusted agent, so nothing in it (nor anything in the operator's
// environment) may make git execute code. Every invocation gets a MINIMAL environment (PATH, a
// private empty HOME, C locale, no prompts; no operator GIT_* variable passes through, and neither
// the system nor the global config is read); fixed -c overrides of every exec-capable scalar key
// (HardeningConfig: hooks, fsmonitor, ssh/editor/pager/askpass/credential helpers, external diff,
// signing, transports); and, before every command that may run one, an empty -c override of each
// attribute-driven driver (filter.<x>.clean|smudge|process, diff.<x>.textconv|command,
// merge.<x>.driver) the repository's config defines: .gitattributes (agent-writable) only NAMES
// a driver, config DEFINES it, and the config is enumerated with the same hardened environment. A
// config value from a file inside the work tree (an include.path into it) is refused, and a .git
// FILE (linked worktree) is re-validated first (gitlink.go).
package state

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
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

// HardeningConfig is the exec-capable config with a scalar key, always overridden on the
// command line (-c beats every config file). See the package doc.
var HardeningConfig = [][2]string{
	{"core.hooksPath", os.DevNull}, // no hook can exist under /dev/null
	{"core.fsmonitor", "false"},
	{"core.sshCommand", ""},
	{"core.gitProxy", ""},
	{"core.askPass", ""},
	{"core.editor", ":"},
	{"sequence.editor", ":"},
	{"core.pager", "cat"},
	{"core.alternateRefsCommand", ""},
	{"credential.helper", ""},
	{"diff.external", ""},
	{"commit.gpgSign", "false"},
	{"tag.gpgSign", "false"},
	{"gpg.program", ""},
	{"protocol.allow", "never"}, // push/fetch-free: no transport, no remote helper
}

// driverKey matches the attribute-driven drivers: the attribute (agent-writable) only names one;
// config defines it.
var driverKey = regexp.MustCompile(`(?i)^(filter\..+\.(clean|smudge|process)|diff\..+\.(textconv|command)|merge\..+\.driver)$`)

// noDriverSubcommands never run a filter, textconv or merge driver (as LHA invokes them): they
// skip the per-command config enumeration.
var noDriverSubcommands = map[string]bool{
	"branch": true, "cat-file": true, "commit-tree": true, "config": true, "for-each-ref": true,
	"init": true, "ls-files": true, "read-tree": true, "rev-list": true, "rev-parse": true,
	"symbolic-ref": true, "update-ref": true, "write-tree": true,
}

// passthroughEnv are the only operator variables a harness git inherits; every GIT_* and
// everything else is dropped.
var passthroughEnv = []string{"PATH", "TMPDIR", "SYSTEMROOT"}

var (
	homeMu  sync.Mutex
	homeDir string
)

// safeHome is a private, empty directory used as git's HOME (no ~/.gitconfig, no XDG config).
func safeHome() string {
	homeMu.Lock()
	defer homeMu.Unlock()
	if homeDir == "" || !isDir(homeDir) {
		dir, err := os.MkdirTemp("", "lha-git-home-")
		if err != nil {
			dir = os.DevNull // still no config: git finds no file under it
		}
		homeDir = dir
	}
	return homeDir
}

// gitEnv is the minimal environment every harness git runs with (nothing GIT_* inherited).
func gitEnv() []string {
	env := []string{}
	havePath := false
	for _, k := range passthroughEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
			havePath = havePath || k == "PATH"
		}
	}
	if !havePath {
		env = append(env, "PATH=/usr/bin:/bin")
	}
	home := safeHome()
	return append(env,
		"HOME="+home,
		"XDG_CONFIG_HOME="+home,
		"LC_ALL=C",
		"LANG=C",
		"GIT_TERMINAL_PROMPT=0", // never block on a credential prompt
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_ATTR_NOSYSTEM=1",
	)
}

func configArgs(pairs [][2]string) []string {
	args := make([]string, 0, 2*len(pairs))
	for _, kv := range pairs {
		args = append(args, "-c", kv[0]+"="+kv[1])
	}
	return args
}

func subcommand(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// discoveredRoot is the work tree git will use from cwd: the nearest ancestor holding a .git
// (git's own discovery, which the minimal environment leaves unconfigured); cwd itself, resolved,
// when there is none. Relative config origins are relative to this directory.
func discoveredRoot(cwd string) string {
	here := resolvePath(cwd)
	for dir := here; ; {
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return here
		}
		dir = parent
	}
}

// untrustedOrigin is the config file behind origin if it lies in the agent-writable part of root.
func untrustedOrigin(root, origin string) (string, bool) {
	rest, ok := strings.CutPrefix(origin, "file:")
	if !ok {
		return "", false
	}
	source := rest
	if !filepath.IsAbs(source) {
		source = filepath.Join(root, source)
	}
	source = resolvePath(source)
	if !isWithin(source, root) {
		return "", false
	}
	dotgit := filepath.Join(root, ".git")
	st, err := os.Lstat(dotgit)
	protected := err == nil && st.IsDir() && isWithin(source, dotgit) &&
		!isWithin(source, filepath.Join(dotgit, "lha-worktrees")) // implementers write there
	return source, !protected
}

// driverOverrides returns {key, ""} for every filter/textconv/merge driver defined in cwd's
// config. A config value from a file inside the work tree (outside .git) is a *GitError.
func driverOverrides(ctx context.Context, cwd string) ([][2]string, error) {
	tctx, cancel := context.WithTimeout(ctx, GitTimeout)
	defer cancel()
	args := append(configArgs(HardeningConfig), "config", "--list", "--show-origin", "--name-only", "-z")
	cmd := exec.CommandContext(tctx, "git", args...)
	cmd.Dir = cwd
	cmd.Env = gitEnv()
	cmd.WaitDelay = 5 * time.Second
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("git config --list: %w", ctx.Err())
		}
		if tctx.Err() != nil {
			return nil, &GitError{fmt.Sprintf("git config --list timed out after %ss", pyFloatRepr(GitTimeout.Seconds()))}
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return nil, nil // not a repository (yet): the command itself fails or creates one
		}
		return nil, err
	}
	fields := strings.Split(stdout.String(), "\x00")
	root := discoveredRoot(cwd)
	var out [][2]string
	seen := map[string]bool{}
	for i := 0; i+1 < len(fields); i += 2 {
		origin, key := fields[i], fields[i+1]
		if source, bad := untrustedOrigin(root, origin); bad {
			return nil, &GitError{fmt.Sprintf("refusing to run git in %s: config '%s' comes from %s, a file inside the work tree", cwd, key, source)}
		}
		if driverKey.MatchString(key) && !seen[key] {
			seen[key] = true
			out = append(out, [2]string{key, ""})
		}
	}
	return out, nil
}

// CheckGitLink refuses (*GitError) a .git in cwd that is not the repository it should be (see
// CheckGitLinkPath); expectedCommonDir may be "".
func CheckGitLink(cwd, expectedCommonDir string) error {
	if err := CheckGitLinkPath(cwd, expectedCommonDir); err != nil {
		return &GitError{fmt.Sprintf("refusing to run git in %s: %s", cwd, err)}
	}
	return nil
}

// GitArgv is the hardened git argument list (without "git") for args run in cwd; a *GitError
// when cwd's .git pointer or its config cannot be trusted.
func GitArgv(ctx context.Context, cwd string, args []string) ([]string, error) {
	if err := CheckGitLink(discoveredRoot(cwd), ""); err != nil {
		return nil, err
	}
	pairs := append([][2]string(nil), HardeningConfig...)
	if !noDriverSubcommands[subcommand(args)] {
		extra, err := driverOverrides(ctx, cwd)
		if err != nil {
			return nil, err
		}
		pairs = append(pairs, extra...)
	}
	return append(configArgs(pairs), args...), nil
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
	argv, err := GitArgv(ctx, cwd, args)
	if err != nil {
		return gitResult{}, err
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(tctx, "git", argv...)
	cmd.Dir = cwd
	cmd.Env = gitEnv()
	for k, v := range extraEnv {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
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

// CommonDir returns the resolved shared repository directory (GitDir for all but a linked
// worktree).
func CommonDir(ctx context.Context, cwd string) (string, error) {
	out, err := RunGit(ctx, cwd, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(cwd, out)
	}
	return resolvePath(out), nil
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

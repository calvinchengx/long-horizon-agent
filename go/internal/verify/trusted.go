package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Trusted checks: operator-defined commands run OUTSIDE the sandbox, on the candidate commit
// (python: lha.verify.trusted). A Check with Where == "trusted" is run by a TrustedRunner against
// a clean, detached git worktree of the candidate commit — a commit object of the current working
// tree (tracked + untracked, non-ignored files) made WITHOUT touching HEAD, the real index or the
// working tree.
//
// Security trade-off: a trusted check executes code the AGENT wrote outside the sandbox, as the
// operator's user. Defining LHA_TRUSTED_CHECKS is the explicit opt-in. Only operator-defined
// commands can be trusted checks (items reference them by name); each runs in a throwaway worktree
// of a pinned commit that is always removed, with a MINIMAL environment (TrustedEnv: PATH, the
// locale, a fresh empty HOME and TMPDIR, LHA_CHECK_*, and only the names the operator lists in
// LHA_TRUSTED_CHECK_ENV — never LHA_*), and its process group is killed on timeout. That does not
// make agent code safe: it can still read any file the operator's user can. Run it on a dedicated,
// disposable machine, or make the command hand the commit off to CI.

// CandidateMessage is the commit message of a candidate commit.
const CandidateMessage = "lha: candidate commit for trusted checks"

var verifierIdentity = map[string]string{
	"GIT_AUTHOR_NAME":     "LHA Verifier",
	"GIT_AUTHOR_EMAIL":    "verifier@lha.local",
	"GIT_COMMITTER_NAME":  "LHA Verifier",
	"GIT_COMMITTER_EMAIL": "verifier@lha.local",
}

// CandidateCommit commits the CURRENT working tree of workdir's repo without touching
// HEAD/index/tree: everything is staged into a temporary index (GIT_INDEX_FILE), its tree written
// and a commit created whose parent is HEAD (no parent without commits). No ref points at it.
// message "" means CandidateMessage.
func CandidateCommit(ctx context.Context, workdir, message string) (string, error) {
	if message == "" {
		message = CandidateMessage
	}
	root, err := state.Toplevel(ctx, workdir)
	if err != nil {
		return "", err
	}
	if root == "" {
		return "", &state.GitError{Message: workdir + " is not inside a git work tree"}
	}
	tmp, err := os.MkdirTemp("", "lha-candidate-index-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	env := map[string]string{"GIT_INDEX_FILE": filepath.Join(tmp, "index")}
	for k, v := range verifierIdentity {
		env[k] = v
	}
	opts := state.RunOptions{Env: env}
	parent, err := state.HasCommits(ctx, root)
	if err != nil {
		return "", err
	}
	if parent {
		if _, err := state.RunGitWith(ctx, root, opts, "read-tree", "HEAD"); err != nil {
			return "", err
		}
	}
	if _, err := state.RunGitWith(ctx, root, opts, "add", "-A"); err != nil {
		return "", err
	}
	tree, err := state.RunGitWith(ctx, root, opts, "write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", "--no-gpg-sign", tree, "-m", message}
	if parent {
		args = []string{"commit-tree", "-p", "HEAD", "--no-gpg-sign", tree, "-m", message}
	}
	return state.RunGitWith(ctx, root, opts, args...)
}

// trustedBaseEnv are the host variables every trusted check inherits (the Windows ones are needed
// to start processes).
var trustedBaseEnv = []string{
	"PATH", "LANG", "LC_ALL", "LC_CTYPE", "TZ",
	"SYSTEMROOT", "SYSTEMDRIVE", "COMSPEC", "PATHEXT", "WINDIR",
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// ValidateEnvAllowList checks LHA_TRUSTED_CHECK_ENV names (python: validate_env_allow_list): each
// must be a plain environment variable name and may not start with LHA_ (LHA's own settings carry
// its API keys; the LHA_CHECK_* variables are set by the runner).
func ValidateEnvAllowList(names []string) ([]string, error) {
	out := []string{}
	for _, name := range names {
		if !envName.MatchString(name) {
			return nil, fmt.Errorf("LHA_TRUSTED_CHECK_ENV: %s is not an environment variable name", contracts.PyRepr(name))
		}
		if strings.HasPrefix(strings.ToUpper(name), "LHA_") {
			return nil, fmt.Errorf("LHA_TRUSTED_CHECK_ENV: %s cannot be passed to trusted checks (LHA_* "+
				"settings hold LHA's own credentials)", contracts.PyRepr(name))
		}
		out = append(out, name)
	}
	return out, nil
}

// TrustedEnv is the environment of a trusted check (python: trusted_env): PATH and the locale
// (plus the Windows process essentials) from base (nil = the process environment); HOME = home and
// TMPDIR = tmpdir (on Windows also USERPROFILE / TEMP / TMP); then each allow name present in base,
// verbatim (the operator's explicit choice, so it may override those defaults); check last.
// allow must already be validated (ValidateEnvAllowList).
func TrustedEnv(home, tmpdir string, allow []string, check, base map[string]string) map[string]string {
	if base == nil {
		base = map[string]string{}
		for _, kv := range os.Environ() {
			if k, v, ok := strings.Cut(kv, "="); ok {
				base[k] = v
			}
		}
	}
	env := map[string]string{}
	for _, k := range trustedBaseEnv {
		if v, ok := base[k]; ok {
			env[k] = v
		}
	}
	if _, ok := env["PATH"]; !ok {
		env["PATH"] = "/bin:/usr/bin"
	}
	if _, ok := env["LANG"]; !ok {
		env["LANG"] = "C.UTF-8"
	}
	env["HOME"], env["TMPDIR"], env["GIT_TERMINAL_PROMPT"] = home, tmpdir, "0"
	if runtime.GOOS == "windows" {
		env["USERPROFILE"], env["TEMP"], env["TMP"] = home, tmpdir, tmpdir
	}
	for _, name := range allow {
		if v, ok := base[name]; ok {
			env[name] = v
		}
	}
	for k, v := range check {
		env[k] = v
	}
	return env
}

// TrustedRunner runs a Where == "trusted" check outside the sandbox against commit; it never
// fails the verification with an error (a failure is a failed CheckResult).
type TrustedRunner interface {
	Run(ctx context.Context, check contracts.Check, workdir, commit string) (contracts.CheckResult, error)
}

func failedCheck(check contracts.Check, message string, started time.Time, timedOut bool) contracts.CheckResult {
	return contracts.CheckResult{
		Name:       check.Name,
		Passed:     false,
		ExitCode:   -1,
		Gating:     check.Gating,
		DurationS:  time.Since(started).Seconds(),
		TimedOut:   timedOut,
		OutputTail: message,
	}
}

// CommandTrustedRunner runs a trusted check's argv on the HOST in a detached worktree of the
// candidate commit, with the minimal TrustedEnv: PATH, the locale, a fresh HOME and TMPDIR, the
// EnvAllow names and LHA_CHECK_COMMIT, LHA_CHECK_WORKTREE and LHA_CHECK_NAME. On timeout the whole
// process group is killed; the worktree and the temporary home are always removed.
type CommandTrustedRunner struct {
	TimeoutS   int      // default 3600
	OutputTail int      // default OutputTailChars
	EnvAllow   []string // LHA_TRUSTED_CHECK_ENV (validated by NewCommandTrustedRunnerWithEnv)
}

// NewCommandTrustedRunner returns a runner with the Python defaults.
func NewCommandTrustedRunner() *CommandTrustedRunner {
	return &CommandTrustedRunner{TimeoutS: 3600, OutputTail: OutputTailChars}
}

// NewCommandTrustedRunnerWithEnv is NewCommandTrustedRunner passing the envAllow names through
// (python: CommandTrustedRunner(env_allow=...)); an invalid or LHA_* name is an error.
func NewCommandTrustedRunnerWithEnv(envAllow []string) (*CommandTrustedRunner, error) {
	names, err := ValidateEnvAllowList(envAllow)
	if err != nil {
		return nil, err
	}
	r := NewCommandTrustedRunner()
	r.EnvAllow = names
	return r, nil
}

// Run executes check on commit of the repository at workdir.
func (r *CommandTrustedRunner) Run(ctx context.Context, check contracts.Check, workdir, commit string) (contracts.CheckResult, error) {
	started := time.Now()
	root, err := state.Toplevel(ctx, workdir)
	if err != nil || root == "" {
		return failedCheck(check, "[trusted] "+workdir+" is not a git work tree", started, false), nil
	}
	dir, err := os.MkdirTemp("", "lha-trusted-")
	if err != nil {
		return failedCheck(check, "[trusted] could not create worktree: "+err.Error(), started, false), nil
	}
	worktree, _ := filepath.EvalSymlinks(dir)
	if worktree == "" {
		worktree = dir
	}
	defer removeWorktree(root, worktree)
	if err := addWorktree(ctx, root, worktree, commit); err != nil {
		return failedCheck(check, "[trusted] could not create worktree: "+err.Error(), started, false), nil
	}
	absWorkdir, _ := filepath.Abs(workdir)
	if real, err := filepath.EvalSymlinks(absWorkdir); err == nil {
		absWorkdir = real
	}
	rel, err := filepath.Rel(root, absWorkdir)
	if err != nil {
		rel = "."
	}
	allow, err := ValidateEnvAllowList(r.EnvAllow)
	if err != nil {
		return failedCheck(check, "[trusted] "+err.Error(), started, false), nil
	}
	homeDir, err := os.MkdirTemp("", "lha-trusted-home-")
	if err != nil {
		return failedCheck(check, "[trusted] could not create a home directory: "+err.Error(), started, false), nil
	}
	defer os.RemoveAll(homeDir)
	home := homeDir
	if real, err := filepath.EvalSymlinks(homeDir); err == nil {
		home = real
	}
	tmpdir := filepath.Join(home, "tmp")
	if err := os.Mkdir(tmpdir, 0o700); err != nil {
		return failedCheck(check, "[trusted] could not create a home directory: "+err.Error(), started, false), nil
	}
	envMap := TrustedEnv(home, tmpdir, allow, map[string]string{
		"LHA_CHECK_COMMIT": commit, "LHA_CHECK_WORKTREE": worktree, "LHA_CHECK_NAME": check.Name,
	}, nil)
	env := make([]string, 0, len(envMap))
	for k, v := range envMap {
		env = append(env, k+"="+v)
	}
	return r.exec(ctx, check, filepath.Join(worktree, rel), env, started), nil
}

func (r *CommandTrustedRunner) exec(ctx context.Context, check contracts.Check, cwd string, env []string, started time.Time) contracts.CheckResult {
	timeout := r.TimeoutS
	if timeout <= 0 {
		timeout = 3600
	}
	if check.TimeoutS != nil && *check.TimeoutS != 0 {
		timeout = *check.TimeoutS
	}
	tail := r.OutputTail
	if tail <= 0 {
		tail = OutputTailChars
	}
	if len(check.Command) == 0 {
		return failedCheck(check, "[trusted] could not execute check: empty command", started, false)
	}
	cmd := exec.Command(check.Command[0], check.Command[1:]...)
	cmd.Dir = cwd
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = newProcessGroupAttr()
	if err := cmd.Start(); err != nil {
		return failedCheck(check, fmt.Sprintf("[trusted] could not execute check: %s", err), started, false)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timedOut := false
	var waitErr error
	select {
	case waitErr = <-done:
	case <-time.After(time.Duration(timeout) * time.Second):
		timedOut = true
		killProcessGroup(cmd.Process)
		waitErr = <-done
	case <-ctx.Done():
		killProcessGroup(cmd.Process)
		<-done
		return failedCheck(check, "[trusted] cancelled", started, false)
	}
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = processExitCode(exitErr.ProcessState)
		} else {
			exitCode = -1
		}
	}
	output := ClipOutputTail(out.String(), "", tail)
	if timedOut {
		output = trimLeftNewlines(fmt.Sprintf("%s\n[trusted] timed out after %ds", output, timeout))
	}
	return contracts.CheckResult{
		Name:       check.Name,
		Passed:     exitCode == 0 && !timedOut,
		ExitCode:   exitCode,
		Gating:     check.Gating,
		DurationS:  time.Since(started).Seconds(),
		TimedOut:   timedOut,
		OutputTail: output,
	}
}

func trimLeftNewlines(s string) string {
	for len(s) > 0 && s[0] == '\n' {
		s = s[1:]
	}
	return s
}

func addWorktree(ctx context.Context, root, worktree, commit string) error {
	unlock := state.WorktreeLock(ctx, root)
	defer unlock()
	_, err := state.RunGit(ctx, root, "worktree", "add", "--detach", worktree, commit)
	return err
}

func removeWorktree(root, worktree string) {
	ctx := context.Background()
	unlock := state.WorktreeLock(ctx, root)
	_, _ = state.RunGitWith(ctx, root, state.RunOptions{NoCheck: true}, "worktree", "remove", "--force", worktree)
	_, _ = state.RunGitWith(ctx, root, state.RunOptions{NoCheck: true}, "worktree", "prune")
	unlock()
	_ = os.RemoveAll(worktree)
}

// TrustedAwareVerifier sends sandbox checks to Inner and trusted checks to Runner. Trusted checks
// all run against ONE candidate commit of HostWorkdir; results are merged in order (sandbox, then
// trusted). A failure to build the candidate commit turns every trusted check into a failed
// gating result — this verifier never returns an error for it.
type TrustedAwareVerifier struct {
	Inner       contracts.Verifier
	Runner      TrustedRunner
	HostWorkdir string
}

var _ contracts.Verifier = (*TrustedAwareVerifier)(nil)

// NewTrustedAwareVerifier returns the lead's verifier (python: lead_verifier).
func NewTrustedAwareVerifier(inner contracts.Verifier, runner TrustedRunner, hostWorkdir string) *TrustedAwareVerifier {
	return &TrustedAwareVerifier{Inner: inner, Runner: runner, HostWorkdir: hostWorkdir}
}

// Verify runs the sandbox checks, then the trusted checks.
func (v *TrustedAwareVerifier) Verify(ctx context.Context, session contracts.SandboxSession, checks []contracts.Check) (contracts.VerificationResult, error) {
	sandbox, trusted := []contracts.Check{}, []contracts.Check{}
	for _, c := range checks {
		if c.Where == "trusted" {
			trusted = append(trusted, c)
		} else {
			sandbox = append(sandbox, c)
		}
	}
	results := []contracts.CheckResult{}
	if len(sandbox) > 0 || len(trusted) == 0 {
		inner, err := v.Inner.Verify(ctx, session, sandbox)
		if err != nil {
			return contracts.VerificationResult{}, err
		}
		results = append(results, inner.Results...)
	}
	if len(trusted) > 0 {
		results = append(results, v.runTrusted(ctx, trusted)...)
	}
	return contracts.NewVerificationResult(results), nil
}

func (v *TrustedAwareVerifier) runTrusted(ctx context.Context, checks []contracts.Check) []contracts.CheckResult {
	started := time.Now()
	commit, err := CandidateCommit(ctx, v.HostWorkdir, "")
	out := []contracts.CheckResult{}
	if err != nil {
		msg := fmt.Sprintf("[trusted] could not create the candidate commit: %s: %s", pyfmt.ExcTypeName(err), err)
		for _, c := range checks {
			out = append(out, failedCheck(c, msg, started, false))
		}
		return out
	}
	for _, c := range checks {
		r, err := v.Runner.Run(ctx, c, v.HostWorkdir, commit)
		if err != nil { // a runner failure is a failed check, never a pass
			r = failedCheck(c, fmt.Sprintf("[trusted] runner failed: %s: %s", pyfmt.ExcTypeName(err), err), time.Now(), false)
		}
		out = append(out, r)
	}
	return out
}

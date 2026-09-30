package verify

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// The opt-in mutation gate (python: lha.verify.mutation_gate). With LHA_MUTATION_CHECK set,
// MutationGateVerifier runs that command (sh -c, in the sandbox) after every gating check passed,
// and only then, with the files the item changed (git diff HEAD plus untracked files, outside
// .lha/) in LHA_CHANGED_FILES, one per line. Exit 0 passes; any other exit keeps the item red and
// the output tail (the surviving mutants) goes into the failure report. With no changed files it
// does not run. It can keep an item red and never make it green; it is not re-run as a flake.

// MutationCheckName is the gate's check name in verdicts and failure reports.
const MutationCheckName = "mutation"

// ChangedFilesEnv carries the changed files to the command.
const ChangedFilesEnv = "LHA_CHANGED_FILES"

// ChangedFiles is the files changed in workdir's work tree against HEAD (tracked and untracked, not
// ignored), outside the harness-owned .lha/, sorted.
func ChangedFiles(ctx context.Context, workdir string) ([]string, error) {
	names := map[string]bool{}
	for _, args := range [][]string{
		{"diff", "--name-only", "-z", "HEAD"},
		{"ls-files", "--others", "--exclude-standard", "-z"},
	} {
		res, err := state.RunGitBytes(ctx, workdir, 0, args...)
		if err != nil {
			return nil, err
		}
		if res.ReturnCode != 0 {
			return nil, fmt.Errorf("git %s failed (%d): %s", strings.Join(args, " "), res.ReturnCode,
				strings.TrimSpace(string(res.Stderr)))
		}
		for _, name := range strings.Split(string(res.Stdout), "\x00") {
			if name != "" && name != ".lha" && !strings.HasPrefix(name, ".lha/") {
				names[name] = true
			}
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// MutationGateVerifier adds the mutation check to a green verdict (see above).
type MutationGateVerifier struct {
	Inner    contracts.Verifier
	Command  string
	Workdir  string
	TimeoutS int
}

var _ contracts.Verifier = (*MutationGateVerifier)(nil)

// DrainEvents forwards the inner verifier's events (the flaky-check quarantine's).
func (v *MutationGateVerifier) DrainEvents() []contracts.EventRecord {
	if d, ok := v.Inner.(interface {
		DrainEvents() []contracts.EventRecord
	}); ok {
		return d.DrainEvents()
	}
	return nil
}

// Verify runs the inner checks, then the mutation check on a green verdict with changed files.
func (v *MutationGateVerifier) Verify(ctx context.Context, session contracts.SandboxSession, checks []contracts.Check) (contracts.VerificationResult, error) {
	verdict, err := v.Inner.Verify(ctx, session, checks)
	if err != nil || verdict.Verdict != contracts.VerdictPassed {
		return verdict, err // red or unverified: mutating failing or untested code tells nothing
	}
	changed, err := ChangedFiles(ctx, filepath.Clean(v.Workdir))
	if err != nil { // fail closed: configured, and cannot tell what changed
		return verdict.WithResults([]contracts.CheckResult{mutationFailed(
			"[verifier] mutation check: cannot list the changed files: " + err.Error())}), nil
	}
	if len(changed) == 0 {
		return verdict, nil
	}
	out, err := session.Exec(ctx, []string{"sh", "-c", v.Command}, contracts.ExecOptions{
		TimeoutS: v.TimeoutS, Env: map[string]string{ChangedFilesEnv: strings.Join(changed, "\n")},
	})
	if err != nil { // a sandbox failure is a failed check, never a pass
		return verdict.WithResults([]contracts.CheckResult{mutationFailed(fmt.Sprintf(
			"[verifier] could not execute check: %s: %s", pyfmt.ExcTypeName(err), err))}), nil
	}
	return verdict.WithResults([]contracts.CheckResult{{
		Name: MutationCheckName, Passed: out.OK(), ExitCode: out.ExitCode, Gating: true,
		TimedOut: out.TimedOut, OutputTail: ClipOutputTail(out.Stdout, out.Stderr, OutputTailChars),
	}}), nil
}

func mutationFailed(message string) contracts.CheckResult {
	return contracts.CheckResult{Name: MutationCheckName, ExitCode: -1, Gating: true, OutputTail: message}
}

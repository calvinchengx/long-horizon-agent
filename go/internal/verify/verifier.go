// Package verify is the verification plane: the deterministic gate that decides what "done"
// means. It mirrors python/src/lha/verify.
package verify

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// OutputTailChars is the tail of each check's output kept on the CheckResult (what the agent
// sees on failure), in characters.
const OutputTailChars = 4000

// DefaultCheckTimeoutS is the verifier's default per-check timeout.
const DefaultCheckTimeoutS = 1200

func pyIsSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

func pyRstrip(s string) string { return strings.TrimRightFunc(s, pyIsSpace) }

// ClipOutputTail combines stdout/stderr and keeps the last limit characters (the end is where
// failures are). Slicing follows Python's combined[-limit:] exactly, including limit <= 0.
func ClipOutputTail(stdout, stderr string, limit int) string {
	var parts []string
	for _, p := range []string{pyRstrip(stdout), pyRstrip(stderr)} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	combined := strings.Join(parts, "\n--- stderr ---\n")
	runes := []rune(combined)
	if len(runes) <= limit {
		return combined
	}
	start := -limit
	if start < 0 {
		start += len(runes)
		if start < 0 {
			start = 0
		}
	}
	if start > len(runes) {
		start = len(runes)
	}
	return "...[truncated]...\n" + string(runes[start:])
}

// DeterministicVerifier implements contracts.Verifier by running real check commands through
// the sandbox session's Exec (so checks run WHERE the code lives), aggregating exit codes into a
// VerificationResult. Advisory (non-gating) checks are recorded but never block, and a run with
// zero gating checks is unverified (never a vacuous pass).
type DeterministicVerifier struct {
	DefaultTimeoutS int // used when a check has no timeout_s (Python default 1200)
	OutputTail      int // characters of output kept per check (Python default 4000)
}

var _ contracts.Verifier = (*DeterministicVerifier)(nil)

// NewDeterministicVerifier returns a verifier with the Python defaults.
func NewDeterministicVerifier() *DeterministicVerifier {
	return &DeterministicVerifier{DefaultTimeoutS: DefaultCheckTimeoutS, OutputTail: OutputTailChars}
}

// Verify runs every check in order. A sandbox error is a failed check, never a pass; only a
// cancelled ctx aborts the run with an error.
func (v *DeterministicVerifier) Verify(ctx context.Context, session contracts.SandboxSession, checks []contracts.Check) (contracts.VerificationResult, error) {
	results := []contracts.CheckResult{}
	for _, check := range contracts.EnsureUniqueCheckNames(checks, []string{}...) {
		r, err := v.runOne(ctx, session, check)
		if err != nil {
			return contracts.VerificationResult{}, err
		}
		results = append(results, r)
	}
	// The gate requires >=1 gating check and every GATING check to pass.
	return contracts.NewVerificationResult(results), nil
}

func (v *DeterministicVerifier) runOne(ctx context.Context, session contracts.SandboxSession, check contracts.Check) (contracts.CheckResult, error) {
	started := time.Now()
	if check.Where == "trusted" {
		// Never run an operator's trusted check inside the agent's sandbox (it would not have
		// what it needs, and passing it here would be meaningless): fail it loudly instead.
		return contracts.CheckResult{
			Name: check.Name, Passed: false, ExitCode: -1, Gating: check.Gating,
			OutputTail: "[verifier] trusted check needs a trusted runner, and none is configured " +
				"for this run (see lha.verify.trusted)",
		}, nil
	}
	timeout := v.DefaultTimeoutS
	if check.TimeoutS != nil && *check.TimeoutS != 0 {
		timeout = *check.TimeoutS
	}
	outcome, err := session.Exec(ctx, check.Command, contracts.ExecOptions{TimeoutS: timeout})
	if err != nil {
		if ctx.Err() != nil { // cancellation propagates (Python's CancelledError is not caught)
			return contracts.CheckResult{}, ctx.Err()
		}
		return contracts.CheckResult{
			Name:      check.Name,
			Passed:    false,
			ExitCode:  -1,
			Gating:    check.Gating,
			DurationS: time.Since(started).Seconds(),
			OutputTail: fmt.Sprintf("[verifier] could not execute check: %s: %s",
				pyfmt.ExcTypeName(err), err.Error()),
		}, nil
	}
	return contracts.CheckResult{
		Name:       check.Name,
		Passed:     outcome.OK(),
		ExitCode:   outcome.ExitCode,
		Gating:     check.Gating,
		DurationS:  time.Since(started).Seconds(),
		TimedOut:   outcome.TimedOut,
		OutputTail: ClipOutputTail(outcome.Stdout, outcome.Stderr, v.OutputTail),
	}, nil
}

// DefaultPythonCheckCommands is the standard deterministic gate for a uv-managed Python repo.
var DefaultPythonCheckCommands = [][]string{
	{"uv", "run", "ruff", "check", "."},
	{"uv", "run", "ty", "check"},
	{"uv", "run", "pytest", "-q"},
}

// DefaultPythonChecks returns ruff, ty and pytest via uv as gating checks: the sensible default
// for run paths that were not given explicit checks.
func DefaultPythonChecks() []contracts.Check {
	cmds := make([][]string, len(DefaultPythonCheckCommands))
	for i, c := range DefaultPythonCheckCommands {
		cmds[i] = append([]string{}, c...)
	}
	return contracts.ChecksFromCommands(cmds, true)
}

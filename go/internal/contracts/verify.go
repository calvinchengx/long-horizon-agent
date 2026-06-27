package contracts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Verdicts.
const (
	VerdictPassed     = "passed"
	VerdictFailed     = "failed"
	VerdictUnverified = "unverified"
)

// HarnessIntegrityCheck is the reserved name of the harness-integrity check.
const HarnessIntegrityCheck = "harness_integrity"

// Check is a single deterministic check: an argv run in the sandbox session's work directory.
type Check struct {
	Name     string   `json:"name"`
	Command  []string `json:"command"`
	Gating   bool     `json:"gating"`
	TimeoutS *int     `json:"timeout_s"`
	// Where is "sandbox" (default) or "trusted" (an operator-defined check run outside it).
	Where string `json:"where"`
}

// UnmarshalJSON applies defaults (gating=true) and validates a non-empty command.
func (c *Check) UnmarshalJSON(data []byte) error {
	type alias Check
	a := alias{Gating: true, Where: "sandbox"}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	if a.Where != "sandbox" && a.Where != "trusted" {
		return fmt.Errorf("check where must be sandbox or trusted, got %q", a.Where)
	}
	if len(a.Command) == 0 {
		return errors.New("check command must have at least 1 item")
	}
	*c = Check(a)
	return nil
}

// CheckResult is the real outcome of running a Check.
type CheckResult struct {
	Name       string  `json:"name"`
	Passed     bool    `json:"passed"`
	ExitCode   int     `json:"exit_code"`
	Gating     bool    `json:"gating"`
	DurationS  float64 `json:"duration_s"`
	TimedOut   bool    `json:"timed_out"`
	OutputTail string  `json:"output_tail"`
	OutputRef  *string `json:"output_ref"`
}

// UnmarshalJSON applies defaults (gating=true).
func (r *CheckResult) UnmarshalJSON(data []byte) error {
	type alias CheckResult
	a := alias{Gating: true}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*r = CheckResult(a)
	return nil
}

// VerificationResult is the aggregate verdict; AllGreen and Verdict are always derived.
type VerificationResult struct {
	AllGreen bool          `json:"all_green"`
	Verdict  string        `json:"verdict"`
	Results  []CheckResult `json:"results"`
}

// NewVerificationResult derives the verdict: AllGreen needs >=1 gating check, all passing.
func NewVerificationResult(results []CheckResult) VerificationResult {
	v := VerificationResult{Results: append([]CheckResult{}, results...), Verdict: VerdictUnverified}
	gating := 0
	allPass := true
	for _, r := range v.Results {
		if r.Gating {
			gating++
			allPass = allPass && r.Passed
		}
	}
	if gating > 0 {
		v.AllGreen = allPass
		if allPass {
			v.Verdict = VerdictPassed
		} else {
			v.Verdict = VerdictFailed
		}
	}
	return v
}

// UnmarshalJSON re-derives the verdict from the results (any value passed in is overridden).
func (v *VerificationResult) UnmarshalJSON(data []byte) error {
	type alias VerificationResult
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*v = NewVerificationResult(a.Results)
	return nil
}

// Unverified is true when no gating check ran.
func (v VerificationResult) Unverified() bool { return v.Verdict == VerdictUnverified }

// WithResults returns a new verdict including extra results.
func (v VerificationResult) WithResults(extra []CheckResult) VerificationResult {
	return NewVerificationResult(append(append([]CheckResult{}, v.Results...), extra...))
}

// FailureReport is a compact, model-facing explanation of why the verdict is not green.
func (v VerificationResult) FailureReport(perCheckChars int) string {
	if perCheckChars <= 0 {
		perCheckChars = 1500
	}
	if v.Unverified() {
		return "UNVERIFIED: no gating checks ran, so the item cannot be marked done. " +
			"Configure at least one gating check for this mission."
	}
	lines := []string{}
	for _, r := range v.Results {
		if r.Passed || !r.Gating {
			continue
		}
		status := fmt.Sprintf("exit %d", r.ExitCode)
		if r.TimedOut {
			status = "timed out"
		}
		tail := "(no output)"
		if r.OutputTail != "" {
			tail = lastChars(r.OutputTail, perCheckChars)
		}
		lines = append(lines, fmt.Sprintf("- %s FAILED (%s):\n%s", r.Name, status, tail))
	}
	if len(lines) == 0 {
		return "all gating checks passed"
	}
	return strings.Join(lines, "\n")
}

// Verifier runs deterministic checks inside a sandbox session.
type Verifier interface {
	Verify(ctx context.Context, session SandboxSession, checks []Check) (VerificationResult, error)
}

var runnerTokens = map[string]bool{
	"uv": true, "uvx": true, "run": true, "exec": true, "poetry": true, "pipenv": true, "pdm": true,
	"hatch": true, "rye": true, "npx": true, "npm": true, "pnpm": true, "yarn": true, "bunx": true,
	"-m": true,
}

var (
	pythonRE          = regexp.MustCompile(`(?i)^python(\d+(\.\d+)?)?(\.exe)?$`)
	unsafeNameChars   = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
	checkNameMaxRunes = 40
)

// DeriveCheckName derives a readable check name from argv, skipping runner prefixes.
func DeriveCheckName(command []string) string {
	interpreter := ""
	for _, token := range command {
		if token == "-c" {
			if interpreter != "" {
				return interpreter
			}
			return "check"
		}
		if token == "" || strings.HasPrefix(token, "-") {
			continue
		}
		base := path.Base(token)
		if pythonRE.MatchString(base) {
			interpreter = "python"
			continue
		}
		if runnerTokens[token] || runnerTokens[base] {
			continue
		}
		name := strings.Trim(unsafeNameChars.ReplaceAllString(base, "_"), "_.-")
		if r := []rune(name); len(r) > checkNameMaxRunes {
			name = string(r[:checkNameMaxRunes])
		}
		if name != "" {
			return name
		}
	}
	return "check"
}

// EnsureUniqueCheckNames suffixes duplicate (or reserved) names -2, -3, ... in order.
func EnsureUniqueCheckNames(checks []Check, reserved ...string) []Check {
	if reserved == nil {
		reserved = []string{HarnessIntegrityCheck}
	}
	taken := map[string]bool{}
	for _, r := range reserved {
		taken[r] = true
	}
	out := make([]Check, 0, len(checks))
	for _, c := range checks {
		name := c.Name
		if taken[name] {
			n := 2
			for taken[fmt.Sprintf("%s-%d", name, n)] {
				n++
			}
			name = fmt.Sprintf("%s-%d", name, n)
		}
		taken[name] = true
		c.Name = name
		out = append(out, c)
	}
	return out
}

// ChecksFromCommands builds uniquely named checks from argv lists (empty lists are skipped).
func ChecksFromCommands(commands [][]string, gating bool) []Check {
	checks := []Check{}
	for _, cmd := range commands {
		if len(cmd) == 0 {
			continue
		}
		checks = append(checks, Check{Name: DeriveCheckName(cmd), Command: append([]string{}, cmd...), Gating: gating, Where: "sandbox"})
	}
	return EnsureUniqueCheckNames(checks)
}

func lastChars(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

package verify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Flaky-check quarantine, wired into every run path's verifier (python:
// lha.verify.flaky_quarantine.FlakyRetryVerifier).
//
// A flaky check makes "green" unreliable in both directions: it can fail good work, and a pass
// from it is weak evidence. FlakyRetryVerifier wraps the lead's verifier (agent.LeadVerifier)
// and applies these rules, in this order:
//
//  1. Re-run on failure. A gating check that fails (and did not time out) is re-run, on the same
//     work tree, up to LHA_FLAKY_RETRIES more times (default 1), stopping at the first pass.
//     LHA_FLAKY_RETRIES=0 turns re-runs and quarantine off: the first result stands.
//  2. Evidence before quarantine. A check is quarantined only when it both passed and failed on
//     the SAME revision (the tree id of the work tree, including uncommitted changes).
//  3. A consistent failure always gates. A check (quarantined or not) that fails every attempt on
//     the revision under test stays a failing gating check: the item is red.
//  4. A quarantined check is never the evidence for green. Once quarantined, its results are
//     recorded as NON-gating (a pass included), so an item needs another gating check to pass.
//  5. Quarantine is recorded and lasts for the mission. Quarantining emits a check_quarantined
//     event (check, revision, passes, fails) that the agent loop commits to .lha/events.ndjson
//     with the cycle's checkpoint; a quarantined check failing every attempt emits
//     quarantined_check_failed. Later cycles, including those of another process, read the
//     quarantined set from the COMMITTED event log at HEAD. Nothing lifts a quarantine.
//
// Timed-out results are never re-run. Witness checks follow the same rules as mission checks.

const (
	// QuarantineEvent is the committed event that quarantines a check.
	QuarantineEvent = "check_quarantined"
	// QuarantinedFailureEvent records a quarantined check failing every attempt (it gates).
	QuarantinedFailureEvent = "quarantined_check_failed"
	quarantineEventsPath    = ".lha/events.ndjson"
)

// CommittedQuarantine returns the check names quarantined by check_quarantined events committed
// at HEAD of workdir (the agent's uncommitted edits to .lha/ are ignored). Any git failure (no
// repository, no event log at HEAD) yields an empty set.
func CommittedQuarantine(ctx context.Context, workdir string) map[string]bool {
	names := map[string]bool{}
	raw, err := state.RunGit(ctx, workdir, "show", "HEAD:"+quarantineEventsPath)
	if err != nil {
		return names
	}
	for _, line := range strings.Split(raw, "\n") {
		if !strings.Contains(line, QuarantineEvent) {
			continue
		}
		var event struct {
			Kind    *string        `json:"kind"`
			Payload map[string]any `json:"payload"`
		}
		if json.Unmarshal([]byte(line), &event) != nil || event.Kind == nil {
			continue
		}
		if check, ok := event.Payload["check"].(string); ok && *event.Kind == QuarantineEvent {
			names[check] = true
		}
	}
	return names
}

// TreeRevision is the git tree id of workdir's CURRENT work tree (uncommitted changes included).
func TreeRevision(ctx context.Context, workdir string) (string, error) {
	commit, err := CandidateCommit(ctx, workdir, "lha: flake revision")
	if err != nil {
		return "", err
	}
	return state.RunGit(ctx, workdir, "rev-parse", commit+"^{tree}")
}

// FlakyRetryVerifier is a contracts.Verifier that re-runs failing gating checks and quarantines
// proven flakes (see the rules above).
//
// Workdir is the host checkout: the revision is its tree id and the committed quarantine is read
// from its HEAD. Without it (""), revisions are per-Verify call and nothing is read from git.
// Events for the checkpoint are collected until DrainEvents.
type FlakyRetryVerifier struct {
	Inner      contracts.Verifier
	Quarantine *FlakyQuarantine
	// Revision overrides how the revision under test is computed (tests and spec cases); nil
	// uses TreeRevision of the workdir, falling back to a per-call id without git.
	Revision func(ctx context.Context) (string, error)

	retries int
	workdir string

	mu     sync.Mutex
	events []contracts.EventRecord
	calls  int
}

var _ contracts.Verifier = (*FlakyRetryVerifier)(nil)

// NewFlakyRetryVerifier wraps inner; retries is clamped to >= 0 (0 turns re-runs off).
func NewFlakyRetryVerifier(inner contracts.Verifier, retries int, workdir string) *FlakyRetryVerifier {
	return &FlakyRetryVerifier{
		Inner:      inner,
		Quarantine: NewDefaultFlakyQuarantine(),
		retries:    max(0, retries),
		workdir:    workdir,
		events:     []contracts.EventRecord{},
	}
}

// Retries is the configured number of re-runs.
func (v *FlakyRetryVerifier) Retries() int { return v.retries }

// DrainEvents returns the flaky events since the last drain (committed with the checkpoint).
func (v *FlakyRetryVerifier) DrainEvents() []contracts.EventRecord {
	v.mu.Lock()
	defer v.mu.Unlock()
	events := v.events
	v.events = []contracts.EventRecord{}
	return events
}

// Verify runs checks through Inner and applies the retry/quarantine rules.
func (v *FlakyRetryVerifier) Verify(ctx context.Context, session contracts.SandboxSession, checks []contracts.Check) (contracts.VerificationResult, error) {
	checks = contracts.EnsureUniqueCheckNames(checks, []string{}...)
	first, err := v.Inner.Verify(ctx, session, checks)
	if err != nil || v.retries == 0 {
		return first, err
	}
	if v.workdir != "" {
		v.Quarantine.Restore(CommittedQuarantine(ctx, v.workdir))
	}
	byName := make(map[string]contracts.Check, len(checks))
	for _, c := range checks {
		byName[c.Name] = c
	}
	revision := ""
	results := make([]contracts.CheckResult, 0, len(first.Results))
	for _, result := range first.Results {
		check, ok := byName[result.Name]
		if !ok || !result.Gating {
			results = append(results, result)
			continue
		}
		quarantined := v.Quarantine.IsFlaky(result.Name)
		if result.Passed {
			if quarantined {
				result = nonGating(result, "quarantined")
			}
			results = append(results, result)
			continue
		}
		if result.TimedOut { // never re-run; a failure that gates (rule 3)
			results = append(results, result)
			continue
		}
		if revision == "" {
			revision = v.revision(ctx)
		}
		retried, err := v.retry(ctx, session, check, result, revision, quarantined)
		if err != nil {
			return contracts.VerificationResult{}, err
		}
		results = append(results, retried)
	}
	return contracts.NewVerificationResult(results), nil
}

// retry re-runs check after failed and applies the rules.
func (v *FlakyRetryVerifier) retry(ctx context.Context, session contracts.SandboxSession, check contracts.Check, failed contracts.CheckResult, revision string, quarantined bool) (contracts.CheckResult, error) {
	name := check.Name
	v.Quarantine.Record(name, revision, false)
	var passing *contracts.CheckResult
	for range v.retries {
		rerun, err := v.Inner.Verify(ctx, session, []contracts.Check{check})
		if err != nil {
			return contracts.CheckResult{}, err
		}
		if len(rerun.Results) == 0 {
			break
		}
		outcome := rerun.Results[0]
		v.Quarantine.Record(name, revision, outcome.Passed)
		if outcome.Passed {
			passing = &outcome
			break
		}
	}
	passes, fails := v.Quarantine.Counts(name, revision)
	payload := contracts.Payload("check", name, "revision", revision, "passes", passes, "fails", fails)
	if passing == nil {
		if quarantined { // rule 3: a consistent failure gates, quarantine or not
			v.event(QuarantinedFailureEvent, payload)
			failed.OutputTail = fmt.Sprintf(
				"[flaky] %s is quarantined but failed all %d attempt(s) on this revision, so it gates\n%s",
				name, fails, failed.OutputTail)
		}
		return failed, nil
	}
	if !quarantined {
		if err := v.Quarantine.MarkFlaky(name); err != nil { // unreachable: minRuns is 2
			return failed, nil
		}
		v.event(QuarantineEvent, payload)
	}
	return nonGating(*passing, fmt.Sprintf(
		"quarantined: %d pass(es) and %d failure(s) on revision %s", passes, fails, prefix(revision, 12))), nil
}

func (v *FlakyRetryVerifier) revision(ctx context.Context) string {
	v.mu.Lock()
	v.calls++
	calls := v.calls
	v.mu.Unlock()
	if v.Revision != nil {
		if rev, err := v.Revision(ctx); err == nil {
			return rev
		}
	} else if v.workdir != "" {
		rev, err := TreeRevision(ctx, v.workdir)
		if err == nil {
			return rev
		}
		// no git: evidence only within this Verify call
		slog.Warn("flake_revision_unavailable", "error", fmt.Sprintf("%s: %s", pyfmt.ExcTypeName(err), err))
	}
	return fmt.Sprintf("call-%p-%d", v, calls)
}

func (v *FlakyRetryVerifier) event(kind string, payload *contracts.OrderedMap) {
	args := make([]any, 0, 2*payload.Len())
	payload.Range(func(k string, val any) bool {
		args = append(args, k, val)
		return true
	})
	slog.Warn(kind, args...)
	v.mu.Lock()
	defer v.mu.Unlock()
	v.events = append(v.events, contracts.EventRecord{Kind: kind, Payload: payload})
}

func nonGating(result contracts.CheckResult, note string) contracts.CheckResult {
	result.Gating = false
	result.OutputTail = "[flaky] " + note + "; non-gating\n" + result.OutputTail
	return result
}

// prefix is s[:n] in code points (python slicing).
func prefix(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

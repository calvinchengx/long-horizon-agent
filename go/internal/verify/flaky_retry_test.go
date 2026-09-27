package verify

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Ported from python/tests/unit/test_flaky_retry_verifier.py: re-run on failure, quarantine only
// with evidence, and never let a quarantined check make an item green.

// scripted is a verifier whose checks pass/fail/time out per a script: one outcome per run (the
// last outcome repeats).
type scripted struct {
	outcomes map[string][]string
	runs     []string
	vanish   bool // every run after the first returns no results
}

func newScripted(outcomes map[string][]string) *scripted {
	copied := map[string][]string{}
	for k, v := range outcomes {
		copied[k] = append([]string{}, v...)
	}
	return &scripted{outcomes: copied}
}

func (s *scripted) Verify(_ context.Context, _ contracts.SandboxSession, checks []contracts.Check) (contracts.VerificationResult, error) {
	if s.vanish && len(s.runs) > 0 {
		return contracts.NewVerificationResult(nil), nil
	}
	results := []contracts.CheckResult{}
	for _, c := range checks {
		s.runs = append(s.runs, c.Name)
		seq := s.outcomes[c.Name]
		outcome := seq[0]
		if len(seq) > 1 {
			s.outcomes[c.Name] = seq[1:]
		}
		exit := 1
		if outcome == "pass" {
			exit = 0
		}
		results = append(results, contracts.CheckResult{
			Name: c.Name, Passed: outcome == "pass", ExitCode: exit, Gating: c.Gating,
			TimedOut: outcome == "timeout", OutputTail: outcome,
		})
	}
	return contracts.NewVerificationResult(results), nil
}

func flakyChecks(names ...string) []contracts.Check {
	out := []contracts.Check{}
	for _, n := range names {
		out = append(out, contracts.Check{Name: n, Command: []string{"x"}, Gating: true, Where: "sandbox"})
	}
	return out
}

func mustVerify(t *testing.T, v contracts.Verifier, checks []contracts.Check) contracts.VerificationResult {
	t.Helper()
	r, err := v.Verify(context.Background(), nil, checks)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func result(t *testing.T, v contracts.VerificationResult, name string) contracts.CheckResult {
	t.Helper()
	for _, r := range v.Results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result %s in %+v", name, v.Results)
	return contracts.CheckResult{}
}

func TestConsistentFailureGatesAndIsNotQuarantined(t *testing.T) {
	inner := newScripted(map[string][]string{"t": {"fail"}})
	v := NewFlakyRetryVerifier(inner, 2, "")
	r := mustVerify(t, v, flakyChecks("t"))
	if r.Verdict != contracts.VerdictFailed || !slices.Equal(inner.runs, []string{"t", "t", "t"}) {
		t.Fatal(r.Verdict, inner.runs) // first run + 2 re-runs
	}
	if v.Quarantine.IsFlaky("t") || len(v.DrainEvents()) != 0 {
		t.Fatal("quarantined a consistent failure")
	}
}

func TestPassOnRerunQuarantinesWithAnEventAndNeverGates(t *testing.T) {
	inner := newScripted(map[string][]string{"flaky": {"fail", "pass"}, "solid": {"pass"}})
	v := NewFlakyRetryVerifier(inner, 2, "")
	r := mustVerify(t, v, flakyChecks("flaky", "solid"))
	if !slices.Equal(inner.runs, []string{"flaky", "solid", "flaky"}) { // stops at the first passing re-run
		t.Fatal(inner.runs)
	}
	if r.Verdict != contracts.VerdictPassed { // the solid check gates; the flaky one is advisory
		t.Fatal(r.Verdict)
	}
	flaky := result(t, r, "flaky")
	if !flaky.Passed || flaky.Gating || !strings.Contains(flaky.OutputTail, "quarantined") {
		t.Fatalf("%+v", flaky)
	}
	events := v.DrainEvents()
	if len(events) != 1 || events[0].Kind != QuarantineEvent || events[0].Payload.Plain()["check"] != "flaky" ||
		events[0].Payload.Plain()["passes"] != 1 || events[0].Payload.Plain()["fails"] != 1 {
		t.Fatalf("%+v", events)
	}
	if len(v.DrainEvents()) != 0 { // drained once
		t.Fatal("drained twice")
	}
}

func TestAQuarantinedCheckAloneNeverMakesAnItemGreen(t *testing.T) {
	inner := newScripted(map[string][]string{"flaky": {"fail", "pass", "pass"}})
	v := NewFlakyRetryVerifier(inner, 1, "")
	first := mustVerify(t, v, flakyChecks("flaky"))
	if first.Verdict != contracts.VerdictUnverified || first.AllGreen { // fail -> pass: quarantined
		t.Fatal(first.Verdict)
	}
	second := mustVerify(t, v, flakyChecks("flaky"))
	if second.Verdict != contracts.VerdictUnverified { // a clean pass of a quarantined check is not evidence
		t.Fatal(second.Verdict)
	}
	if !second.Results[0].Passed || second.Results[0].Gating {
		t.Fatalf("%+v", second.Results[0])
	}
}

func TestAQuarantinedCheckFailingEveryAttemptStillGates(t *testing.T) {
	inner := newScripted(map[string][]string{"flaky": {"fail", "pass", "fail"}, "solid": {"pass"}})
	v := NewFlakyRetryVerifier(inner, 1, "")
	mustVerify(t, v, flakyChecks("flaky", "solid")) // quarantined here
	v.DrainEvents()
	r := mustVerify(t, v, flakyChecks("flaky", "solid"))
	if r.Verdict != contracts.VerdictFailed { // consistent failure beats quarantine
		t.Fatal(r.Verdict)
	}
	flaky := result(t, r, "flaky")
	if !flaky.Gating || flaky.Passed || !strings.Contains(flaky.OutputTail, "gates") {
		t.Fatalf("%+v", flaky)
	}
	events := v.DrainEvents()
	if len(events) != 1 || events[0].Kind != QuarantinedFailureEvent {
		t.Fatalf("%+v", events)
	}
}

func TestTimeoutsAndAdvisoryChecksAreNotRerun(t *testing.T) {
	inner := newScripted(map[string][]string{"slow": {"timeout"}, "advice": {"fail"}})
	v := NewFlakyRetryVerifier(inner, 2, "")
	advisory := contracts.Check{Name: "advice", Command: []string{"x"}, Gating: false, Where: "sandbox"}
	r := mustVerify(t, v, append(flakyChecks("slow"), advisory))
	if !slices.Equal(inner.runs, []string{"slow", "advice"}) || r.Verdict != contracts.VerdictFailed {
		t.Fatal(inner.runs, r.Verdict)
	}
}

func TestZeroRetriesTurnsItOff(t *testing.T) {
	inner := newScripted(map[string][]string{"flaky": {"fail", "pass"}})
	v := NewFlakyRetryVerifier(inner, 0, "")
	r := mustVerify(t, v, flakyChecks("flaky"))
	if r.Verdict != contracts.VerdictFailed || !slices.Equal(inner.runs, []string{"flaky"}) {
		t.Fatal(r.Verdict, inner.runs)
	}
	if NewFlakyRetryVerifier(inner, -3, "").Retries() != 0 {
		t.Fatal("negative retries not clamped")
	}
}

func TestEmptyRerunResultKeepsTheFailure(t *testing.T) {
	inner := newScripted(map[string][]string{"t": {"fail"}})
	inner.vanish = true
	v := NewFlakyRetryVerifier(inner, 2, "")
	if r := mustVerify(t, v, flakyChecks("t")); r.Verdict != contracts.VerdictFailed {
		t.Fatal(r.Verdict)
	}
}

// --- git-backed revision and committed quarantine ---------------------------------------------

func flakyRepo(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if err := state.InitRepo(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := state.CommitAll(ctx, dir, "init"); err != nil {
		t.Fatal(err)
	}
	return dir
}

func eventLine(t *testing.T, kind string, payload map[string]any) string {
	t.Helper()
	b, err := json.Marshal(contracts.EventRecord{Kind: kind, Payload: contracts.OrderedFromMap(payload)})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func commitEvents(t *testing.T, repo string, lines ...string) string {
	t.Helper()
	path := filepath.Join(repo, ".lha", "events.ndjson")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := state.CommitAll(context.Background(), repo, "events"); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRevisionIsTheWorkTreeIncludingUncommittedEdits(t *testing.T) {
	ctx := context.Background()
	repo := flakyRepo(t)
	first, err := TreeRevision(ctx, repo)
	if err != nil || first == "" {
		t.Fatal(first, err)
	}
	if again, _ := TreeRevision(ctx, repo); again != first { // stable for an unchanged tree
		t.Fatal(again, first)
	}
	_ = os.WriteFile(filepath.Join(repo, "a.txt"), []byte("two"), 0o644)
	if changed, _ := TreeRevision(ctx, repo); changed == first {
		t.Fatal("revision ignores uncommitted edits")
	}
}

func TestCommittedQuarantineIgnoresUncommittedEdits(t *testing.T) {
	ctx := context.Background()
	repo := flakyRepo(t)
	if got := CommittedQuarantine(ctx, repo); len(got) != 0 { // no event log at HEAD
		t.Fatal(got)
	}
	path := commitEvents(t, repo,
		eventLine(t, QuarantineEvent, map[string]any{"check": "pytest"}),
		eventLine(t, "cycle", map[string]any{"check": "check_quarantined"}),
		"not json check_quarantined",
	)
	raw, _ := os.ReadFile(path)
	forged := eventLine(t, QuarantineEvent, map[string]any{"check": "ruff"})
	_ = os.WriteFile(path, append(raw, []byte(forged+"\n")...), 0o644) // the agent's uncommitted edit
	if got := CommittedQuarantine(ctx, repo); len(got) != 1 || !got["pytest"] {
		t.Fatal(got)
	}
}

func TestCommittedQuarantineIsRestoredByANewVerifier(t *testing.T) {
	repo := flakyRepo(t)
	commitEvents(t, repo, eventLine(t, QuarantineEvent, map[string]any{"check": "t"}))
	v := NewFlakyRetryVerifier(newScripted(map[string][]string{"t": {"pass"}}), 1, repo)
	if r := mustVerify(t, v, flakyChecks("t")); r.Verdict != contracts.VerdictUnverified {
		t.Fatal(r.Verdict) // restored quarantine: the pass does not gate
	}
}

func TestRevisionFallsBackOutsideGit(t *testing.T) {
	v := NewFlakyRetryVerifier(newScripted(map[string][]string{"t": {"fail", "pass"}}), 1, t.TempDir())
	if r := mustVerify(t, v, flakyChecks("t")); r.Verdict != contracts.VerdictUnverified {
		t.Fatal(r.Verdict)
	}
	events := v.DrainEvents()
	if len(events) != 1 || !strings.HasPrefix(events[0].Payload.Plain()["revision"].(string), "call-") {
		t.Fatalf("%+v", events)
	}
}

func TestQuarantineUsesTheTreeRevisionInAGitWorkdir(t *testing.T) {
	repo := flakyRepo(t)
	v := NewFlakyRetryVerifier(newScripted(map[string][]string{"t": {"fail", "pass"}}), 1, repo)
	mustVerify(t, v, flakyChecks("t"))
	tree, _ := TreeRevision(context.Background(), repo)
	events := v.DrainEvents()
	if len(events) != 1 || events[0].Payload.Plain()["revision"] != tree {
		t.Fatalf("%+v want revision %s", events, tree)
	}
	r := mustVerify(t, NewFlakyRetryVerifier(newScripted(map[string][]string{"t": {"fail", "pass"}}), 1, repo),
		flakyChecks("t"))
	if !strings.Contains(r.Results[0].OutputTail, "on revision "+tree[:12]+"; non-gating") {
		t.Fatal(r.Results[0].OutputTail)
	}
}

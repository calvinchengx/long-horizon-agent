package durable

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agents/org"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Durability tests for the Temporal spine (python: tests/durability/test_durable_spine.py), on the
// SDK's in-process test environment (timers are skipped). Every item is gated by a REAL check
// command in the local sandbox; nothing completes on the model's say-so.

// 1. a mission runs to completion with real counts and head sha.
func TestMissionCompletes(t *testing.T) {
	inp := initMission(t, 3)
	store := &recordingStore{}
	env := newEnv(t, newActs(t, workingModel, store), nil)
	res, err := env.run(inp)
	if err != nil {
		t.Fatal(err)
	}
	head, _ := state.HeadSHA(context.Background(), inp.Workdir)
	if !res.Completed || res.Outcome != OutcomeCompleted || res.Cycles != 3 || res.ItemsDone != 3 ||
		res.ItemsTotal != 3 || res.HeadSHA != head || res.Status != StatusDone {
		t.Fatalf("result %+v (head %s)", res, head)
	}
	var status string
	env.query(t, QueryStatus, &status)
	if status != StatusDone {
		t.Fatalf("status %s", status)
	}
	for _, id := range []string{"01", "02", "03"} {
		if !fileExists(filepath.Join(inp.Workdir, "work", id+".txt")) {
			t.Fatalf("work/%s.txt missing", id)
		}
	}
	statuses := store.Statuses()
	if statuses[len(statuses)-1] != StatusDone {
		t.Fatalf("row statuses %v", statuses)
	}
	// Every metered call reached the ledger hook, keyed <cycle>@<attempt>#<n>.
	if len(store.costs) == 0 || store.costs[0] != "c1@1#0" {
		t.Fatalf("cost rows %v", store.costs)
	}
}

// crashingCycle wraps the real cycle: on attempt 1 of cycleID it runs `before` or the real cycle
// and then fails (a crash after the commit), or fails before running (a crash before the commit).
func crashingCycle(acts *Activities, cycleID string, afterCommit bool, crashes *atomic.Int32) func(context.Context, CycleInput) (CycleResult, error) {
	return func(ctx context.Context, inp CycleInput) (CycleResult, error) {
		if inp.CycleID == cycleID && activity.GetInfo(ctx).Attempt == 1 {
			crashes.Add(1)
			if afterCommit {
				if _, err := acts.RunAgentCycle(ctx, inp); err != nil {
					return CycleResult{}, err
				}
				return CycleResult{}, errors.New("worker crashed after the commit")
			}
			// Partial edits of a crashed attempt: they must never be committed.
			_ = os.MkdirAll(filepath.Join(inp.Workdir, "work"), 0o755)
			_ = os.WriteFile(filepath.Join(inp.Workdir, "residue.txt"), []byte("partial"), 0o644)
			_ = os.WriteFile(filepath.Join(inp.Workdir, "work", "99.txt"), []byte("fake"), 0o644)
			return CycleResult{}, errors.New("worker crashed before the commit")
		}
		return acts.RunAgentCycle(ctx, inp)
	}
}

// 2. a crash AFTER the commit is recovered without advancing another item (no double-apply).
func TestCrashAfterCommitIsIdempotent(t *testing.T) {
	inp := initMission(t, 3)
	acts := newActs(t, workingModel, nil)
	var crashes atomic.Int32
	env := newEnv(t, acts, map[string]any{ActivityRunAgentCycle: crashingCycle(acts, "c2", true, &crashes)})
	res, err := env.run(inp)
	if err != nil {
		t.Fatal(err)
	}
	if crashes.Load() != 1 || !res.Completed || res.Cycles != 3 || res.ItemsDone != 3 {
		t.Fatalf("crashes %d result %+v", crashes.Load(), res)
	}
	// The retried attempt found its cycle's checkpoint in HEAD and returned it instead of doing
	// another item under the same cycle id: 3 items, 3 commits, 3 cycles.
	if n := commitsWith(t, inp.Workdir, "lha: complete"); n != 3 {
		t.Fatalf("%d completion commits", n)
	}
}

// 3. a crash BEFORE the commit leaves no residue: the retry resets the checkout to HEAD.
func TestCrashBeforeCommitDiscardsResidue(t *testing.T) {
	inp := initMission(t, 2)
	acts := newActs(t, workingModel, nil)
	var crashes atomic.Int32
	env := newEnv(t, acts, map[string]any{ActivityRunAgentCycle: crashingCycle(acts, "c1", false, &crashes)})
	res, err := env.run(inp)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed || crashes.Load() != 1 {
		t.Fatalf("result %+v", res)
	}
	paths := allCommittedPaths(t, inp.Workdir)
	if paths["residue.txt"] || paths["work/99.txt"] {
		t.Fatalf("residue committed: %v", paths)
	}
}

// 4. Continue-As-New does not break long runs, and the carried state rides in MissionInput.State.
func TestContinueAsNewCompletes(t *testing.T) {
	inp := initMission(t, 3)
	inp.CyclesBeforeCAN = 1
	inp.MaxCycles = 10
	acts := newActs(t, workingModel, nil)
	res, runs := runFollowingCAN(t, func() *testEnv { return newEnv(t, acts, nil) }, inp)
	if !res.Completed || res.Cycles != 3 || res.ItemsDone != 3 || runs != 3 {
		t.Fatalf("result %+v runs %d", res, runs)
	}
}

// 5. a mission whose remaining item can never pass is reported deadlocked (not completed).
func TestDeadlockedMissionIsReportedAsDeadlocked(t *testing.T) {
	inp := initMission(t, 1)
	store := &recordingStore{}
	env := newEnv(t, newActs(t, idleModel, store), nil)
	res, err := env.run(inp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Completed || res.Outcome != OutcomeDeadlocked || res.Status != StatusImpossible || res.ItemsDone != 0 {
		t.Fatalf("result %+v", res)
	}
	if !strings.Contains(res.Reason, "blocked") {
		t.Fatalf("reason %q", res.Reason)
	}
	if s := store.Statuses(); s[len(s)-1] != StatusImpossible {
		t.Fatalf("row %v", s)
	}
}

// 6. an outage longer than the activity retries PARKS the mission; it resumes when healthy.
func TestOutageParksThenResumes(t *testing.T) {
	inp := initMission(t, 2)
	store := &recordingStore{}
	acts := newActs(t, workingModel, store)
	var failures atomic.Int32
	var probes atomic.Int32
	outage := func(ctx context.Context, in CycleInput) (CycleResult, error) {
		if failures.Load() < 7 { // more than one activity's 5 attempts
			failures.Add(1)
			return CycleResult{}, errors.New("model endpoint unreachable")
		}
		return acts.RunAgentCycle(ctx, in)
	}
	health := func(ctx context.Context, in HealthInput) (HealthReport, error) {
		if probes.Add(1) == 1 {
			return HealthReport{Healthy: false, Reason: "critical dependency down: ['model']"}, nil
		}
		return HealthReport{Healthy: true, Reason: "ok"}, nil
	}
	env := newEnv(t, acts, map[string]any{ActivityRunAgentCycle: outage, ActivityCheckMissionHealth: health})
	res, err := env.run(inp)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed || res.ItemsDone != 2 || probes.Load() < 2 {
		t.Fatalf("result %+v probes %d", res, probes.Load())
	}
	found := false
	for _, s := range store.Statuses() {
		found = found || s == StatusDegradedPark
	}
	if !found {
		t.Fatalf("never parked: %v", store.Statuses())
	}
}

// 7. budget exhaustion ends the mission explicitly (the governor refuses the first call).
func TestBudgetExhaustionEndsMission(t *testing.T) {
	inp := initMission(t, 1)
	zero := 0.0
	inp.BudgetUSD = &zero
	// Prior spend journaled by earlier attempts counts: the mission is already over budget.
	gitDir, _ := state.GitDir(context.Background(), inp.Workdir)
	_ = os.MkdirAll(filepath.Join(gitDir, "lha"), 0o755)
	_ = os.WriteFile(filepath.Join(gitDir, "lha", "spend.ndjson"),
		[]byte(`{"key": "k", "cycle_id": "c0", "usd": 5.0, "unknown": 0, "calls": 1}`+"\n"), 0o644)
	store := &recordingStore{}
	env := newEnv(t, newActs(t, workingModel, store), nil)
	res, err := env.run(inp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeBudgetExhausted || res.Status != StatusAborted || res.Completed {
		t.Fatalf("result %+v", res)
	}
	if s := store.Statuses(); s[len(s)-1] != StatusAborted {
		t.Fatalf("row %v", s)
	}
}

// 8. an explicit empty check list is a configuration error: the mission fails (row ABORTED).
func TestEmptyCheckListFailsTheMission(t *testing.T) {
	inp := initMission(t, 1)
	inp.CheckCommands = [][]string{}
	store := &recordingStore{}
	env := newEnv(t, newActs(t, workingModel, store), nil)
	_, err := env.run(inp)
	var app *temporal.ApplicationError
	if !errors.As(err, &app) || app.Type() != ErrorConfig || !strings.Contains(app.Message(), "check_commands is empty") {
		t.Fatalf("err %v", err)
	}
	if s := store.Statuses(); len(s) != 1 || s[0] != StatusAborted {
		t.Fatalf("row %v", s)
	}
}

// The cycle activity's own guard: an empty list reaching it is a non-retryable config error.
func TestCycleRejectsEmptyChecks(t *testing.T) {
	_, err := ResolveChecks([][]string{{}})
	var app *temporal.ApplicationError
	if !errors.As(err, &app) || app.Type() != ErrorConfig || !app.NonRetryable() {
		t.Fatalf("err %v", err)
	}
	if checks, err := ResolveChecks(nil); err != nil || len(checks) == 0 {
		t.Fatalf("default checks %v %v", checks, err)
	}
}

// A non-retryable cycle failure (e.g. a model that cannot be built) fails the mission.
func TestConfigErrorFailsTheMission(t *testing.T) {
	inp := initMission(t, 1)
	store := &recordingStore{}
	broken := func(ctx context.Context, in CycleInput) (CycleResult, error) {
		return CycleResult{}, configError("cannot build the model: LHA_OPENAI_BASE_URL is required", nil)
	}
	env := newEnv(t, newActs(t, workingModel, store), map[string]any{ActivityRunAgentCycle: broken})
	_, err := env.run(inp)
	var app *temporal.ApplicationError
	if !errors.As(err, &app) || app.Type() != ErrorConfig ||
		!strings.Contains(app.Message(), "mission "+inp.MissionID+" failed: cannot build the model") {
		t.Fatalf("err %v", err)
	}
	if s := store.Statuses(); s[len(s)-1] != StatusAborted {
		t.Fatalf("row %v", s)
	}
}

// 9. the deadlock gate honors an early "retry" decision (sent before the run starts).
func TestEarlyRetryDecisionUnblocksAndCompletes(t *testing.T) {
	inp := initMission(t, 1)
	inp.DeadlockGateSeconds = 3600
	var calls atomic.Int32
	flaky := func(s *config.Settings, snap contracts.SituationSnapshot) (contracts.ModelProvider, error) {
		if calls.Add(1) <= 3 { // three failed attempts block the item
			return idleModel(s, snap)
		}
		return workingModel(s, snap)
	}
	env := newEnv(t, newActs(t, flaky, nil), nil)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalHumanDecision, "retry") }, 0)
	res, err := env.run(inp)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed || res.ItemsDone != 1 {
		t.Fatalf("result %+v", res)
	}
	if commitsWith(t, inp.Workdir, "unblock") != 1 {
		t.Fatal("no unblock checkpoint")
	}
}

// 10. a decision outside the gate's options is recorded and discarded; the default applies.
func TestInvalidDecisionIsRejectedAndDefaultApplies(t *testing.T) {
	inp := initMission(t, 1)
	inp.DeadlockGateSeconds = 60
	env := newEnv(t, newActs(t, idleModel, nil), nil)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalHumanDecision, "  maybe ") }, 0)
	res, err := env.run(inp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeAborted || !strings.Contains(res.Reason, "by default (no human answered)") {
		t.Fatalf("result %+v", res)
	}
	var rejected []string
	env.query(t, QueryRejectedDecisions, &rejected)
	if len(rejected) != 1 || rejected[0] != "maybe" {
		t.Fatalf("rejected %v", rejected)
	}
}

// The park probe: git, a real model round trip (injected here) and the sandbox.
func TestCheckMissionHealthProbesTheCriticalDependencies(t *testing.T) {
	inp := initMission(t, 1)
	acts := newActs(t, workingModel, nil)
	ctx := context.Background()
	report, err := acts.CheckMissionHealth(ctx, HealthInput{MissionID: inp.MissionID, Workdir: inp.Workdir})
	if err != nil || !report.Healthy || report.Reason != "operational (some optionals degraded)" {
		t.Fatalf("%+v %v", report, err)
	}
	acts.ProbeModel = func(context.Context, *config.Settings) model.ModelHealth {
		return model.ModelHealth{Detail: "ollama:x: HTTP 503 from http://localhost:11434/api/tags"}
	}
	report, _ = acts.CheckMissionHealth(ctx, HealthInput{MissionID: inp.MissionID, Workdir: t.TempDir()})
	want := "critical dependency down: ['git', 'model'] (git: no usable repo; model: ollama:x: HTTP 503 from http://localhost:11434/api/tags)"
	if report.Healthy || report.Reason != want {
		t.Fatalf("%q", report.Reason)
	}
}

// TestAnchorTextCarriesTheBoardsNewestPostsAndTheItemsReflection is python's
// test_anchor_text_carries_the_boards_newest_posts_and_the_items_reflection.
func TestAnchorTextCarriesTheBoardsNewestPostsAndTheItemsReflection(t *testing.T) {
	item := contracts.NewChecklistItem("02", "d")
	snap := contracts.SituationSnapshot{HeadSHA: "x", ActiveItem: &item}
	inp := CycleInput{MissionID: "m", Workdir: ".", CycleID: "c3", SteerNotes: []string{"go"}}
	events := []contracts.EventRecord{org.ReflectionEventRecord("02", "\nReflection on 02: old\n", "")}
	for n := 1; n <= 8; n++ {
		events = append(events, org.BoardEventRecord(fmt.Sprintf("researcher:0%d", n), fmt.Sprintf("post %d", n), ""))
	}
	events = append(events, org.ReflectionEventRecord("01", "\nReflection on 01: other item\n", ""),
		org.ReflectionEventRecord("02", "\nReflection on 02: newest\n", ""))
	text := AnchorText(snap, inp, events)
	parts := strings.SplitN(text, "\n\n", 3)
	first, rest := parts[1], parts[2]
	if parts[0] != "Mission m" || first != "Reflection on 02: newest" || !strings.Contains(rest, "Operator steering (most recent last):\n- go") {
		t.Fatalf("%q", text)
	}
	_, board, _ := strings.Cut(rest, "Team board (earlier rounds):\n")
	if !strings.HasPrefix(board, "[researcher:03] post 3") || strings.Count(board, "\n---\n") != 5 || strings.Contains(board, "post 2") ||
		strings.Contains(text, "other item") {
		t.Fatalf("%q", text)
	}
	if got := AnchorText(contracts.SituationSnapshot{HeadSHA: "x"}, inp, events); !strings.HasPrefix(got, "Mission m\n\nOperator steering") {
		t.Fatalf("%q", got)
	}
}

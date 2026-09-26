package durable

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// Human gates, the escalation ladder, SLEEPING and the deadlock gate (python:
// tests/durability/test_approvals.py, test_human_gates.py).

// gatedArgv appends to marker (each real execution is countable), then a flagged git push to a
// remote that does not exist (harmless: it fails at once).
func gatedArgv(marker string) []string {
	return []string{"sh", "-c", "echo ran >> " + marker + "; git push lha-no-such-remote HEAD"}
}

// gatedModel: every cycle runs the flagged argv, writes the item's work file, and is done.
func gatedModel(argv []string) ModelFactory {
	return func(_ *config.Settings, snap contracts.SituationSnapshot) (contracts.ModelProvider, error) {
		return model.NewStub([]contracts.TurnResult{
			runTurn(argv), writeTurn("work/" + snap.ActiveItem.ID + ".txt"), doneTurn(),
		}), nil
	}
}

func ranCount(marker string) int {
	data, _ := os.ReadFile(marker)
	return strings.Count(string(data), "ran")
}

type webhook struct {
	mu    sync.Mutex
	posts []map[string]any
	srv   *httptest.Server
}

func newWebhook(t *testing.T) *webhook {
	w := &webhook{}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		w.mu.Lock()
		w.posts = append(w.posts, m)
		w.mu.Unlock()
		rw.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *webhook) events() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, p := range w.posts {
		out = append(out, p["event"].(string))
	}
	return out
}

// 1. a real mission end to end: the flagged command is queued, the gate shows the pending
// action, a human approves, and the next cycle runs it exactly once.
func TestRealMissionApprovalRunsTheActionOnce(t *testing.T) {
	inp := initMission(t, 2)
	marker := filepath.Join(t.TempDir(), "marker.txt")
	store := &recordingStore{}
	env := newEnv(t, newActs(t, gatedModel(gatedArgv(marker)), store), nil)
	var seen *GateView
	var status string
	env.RegisterDelayedCallback(func() {
		env.query(t, QueryGate, &seen)
		env.query(t, QueryStatus, &status)
		env.SignalWorkflow(SignalHumanDecision, "APPROVE")
	}, time.Minute)
	res, err := env.run(inp)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed {
		t.Fatalf("result %+v", res)
	}
	if seen == nil || seen.Kind != GateToolCall || seen.Request == nil || seen.Request.Tool != "run_command" ||
		!strings.HasPrefix(seen.GateID, "approval-") || seen.DefaultAction != "reject" || status != StatusWaitingOnHuman {
		t.Fatalf("gate %+v status %s", seen, status)
	}
	if !strings.Contains(seen.Question, "wants to run an irreversible action: run_command") {
		t.Fatalf("question %q", seen.Question)
	}
	if n := ranCount(marker); n != 1 {
		t.Fatalf("the approved action ran %d times", n)
	}
	// The anchor holds the queued request and the gate's opening + resolution.
	var kinds []string
	for _, ev := range committedEvents(t, inp.Workdir) {
		kinds = append(kinds, ev.Kind)
	}
	joined := strings.Join(kinds, ",")
	for _, want := range []string{"tool_approval", "gate_opened", "gate_resolved"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("events %v lack %s", kinds, want)
		}
	}
	// The hitl_gates hook saw the gate open and resolve, stamped with workflow time.
	if len(store.gates) != 2 || store.gates[0].Event != "opened" || store.gates[1].ResolvedBy != ResolvedBySignal ||
		store.gates[1].Decision != "approve" || store.gates[0].Risk != "irreversible" || store.gates[0].At == "" {
		t.Fatalf("gate rows %+v", store.gates)
	}
}

// 2. an unanswered gate walks the escalation ladder (reminders in the anchor, the gate log and on
// the webhook) and then REJECTS by default; the rejected action is not asked about again.
func TestUnansweredGateEscalatesThenRejects(t *testing.T) {
	inp := initMission(t, 2)
	inp.ApprovalTimeoutSeconds = 3600
	inp.GateEscalationSeconds = []int{600, 60, 60, 7200}
	marker := filepath.Join(t.TempDir(), "marker.txt")
	hook := newWebhook(t)
	acts := newActs(t, gatedModel(gatedArgv(marker)), nil)
	acts.Settings = testSettings(t, "LHA_GATE_WEBHOOK_URL="+hook.srv.URL)
	env := newEnv(t, acts, nil)
	var view *GateView
	env.RegisterDelayedCallback(func() { env.query(t, QueryGate, &view) }, 2*time.Minute)
	res, err := env.run(inp)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed || ranCount(marker) != 0 {
		t.Fatalf("result %+v ran %d", res, ranCount(marker))
	}
	if got := strings.Join(hook.events(), ","); got != "opened,reminder,reminder,defaulted" {
		t.Fatalf("webhook events %s", got)
	}
	if view == nil || view.EscalationsSent != 1 || view.NextEscalationAt == "" || view.Deadline == "" {
		t.Fatalf("view %+v", view)
	}
	var log []string
	env.query(t, QueryGateLog, &log)
	text := strings.Join(log, "\n")
	for _, want := range []string{"opened (default reject)", "reminder 1 (escalation)", "reminder 2 (escalation)", "timed out: default reject"} {
		if !strings.Contains(text, want) {
			t.Fatalf("gate log lacks %q:\n%s", want, text)
		}
	}
	reminders := 0
	for _, ev := range committedEvents(t, inp.Workdir) {
		if ev.Kind == "gate_reminder" {
			reminders++
		}
	}
	if reminders != 2 {
		t.Fatalf("%d reminder events", reminders)
	}
}

// The held-decision path: an approval sent before any gate opened is honored by the next gate.
func TestApprovedActionReachesTheNextCycleOnce(t *testing.T) {
	inp := initMission(t, 3)
	marker := filepath.Join(t.TempDir(), "marker.txt")
	env := newEnv(t, newActs(t, gatedModel(gatedArgv(marker)), nil), nil)
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalHumanDecision, "approve") }, 0)
	res, err := env.run(inp)
	if err != nil {
		t.Fatal(err)
	}
	// c1 queues it, the held approval resolves the gate, c2 runs it once; c3 queues it anew and
	// the fresh gate (no decision) rejects it by default.
	if !res.Completed || ranCount(marker) != 1 {
		t.Fatalf("result %+v ran %d", res, ranCount(marker))
	}
}

// 3. SLEEPING between cycles (cycle_pause_seconds).
func TestPauseBetweenCyclesIsSleeping(t *testing.T) {
	inp := initMission(t, 2)
	inp.CyclePauseSeconds = 600
	store := &recordingStore{}
	env := newEnv(t, newActs(t, workingModel, store), nil)
	res, err := env.run(inp)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed {
		t.Fatalf("result %+v", res)
	}
	statuses := strings.Join(store.Statuses(), ",")
	if !strings.Contains(statuses, StatusSleeping) {
		t.Fatalf("row %s", statuses)
	}
	var log []string
	env.query(t, QueryGateLog, &log)
	if !strings.Contains(strings.Join(log, "\n"), "sleeping until") || !strings.Contains(strings.Join(log, "\n"), "woke up") {
		t.Fatalf("gate log %v", log)
	}
}

// 3b. a scheduled start sleeps first; a snooze 0 wakes it early.
func TestScheduledStartSleepsFirstAndSnoozeWakes(t *testing.T) {
	inp := initMission(t, 1)
	t0 := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	inp.ResumeAt = epoch(t0) + 3600
	store := &recordingStore{}
	env := newEnv(t, newActs(t, workingModel, store), nil)
	env.SetStartTime(t0)
	var status string
	var resumeAt float64
	env.RegisterDelayedCallback(func() {
		env.query(t, QueryStatus, &status)
		env.query(t, QueryResumeAt, &resumeAt)
		env.SignalWorkflow(SignalSnooze, 0)
	}, time.Minute)
	res, err := env.run(inp)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed || status != StatusSleeping || resumeAt != inp.ResumeAt {
		t.Fatalf("result %+v status %s resume %v", res, status, resumeAt)
	}
	if s := store.Statuses(); s[0] != StatusSleeping || s[len(s)-1] != StatusDone {
		t.Fatalf("row %v", s)
	}
	var log []string
	env.query(t, QueryGateLog, &log)
	want := []string{"2026-09-24T10:00:00+00:00 sleeping until 2026-09-24T11:00:00+00:00", "2026-09-24T10:01:00+00:00 woke up"}
	if len(log) != 2 || log[0] != want[0] || log[1] != want[1] {
		t.Fatalf("gate log %q", log)
	}
}

// 4. the deadlock gate: a human declares the mission impossible (a final checkpoint records it);
// the gate recommends "impossible" after impossible_after_failures failures in a row.
func TestDeadlockGateHumanDeclaresImpossible(t *testing.T) {
	inp := initMission(t, 1)
	inp.DeadlockGateSeconds = 3600
	store := &recordingStore{}
	env := newEnv(t, newActs(t, idleModel, store), nil)
	var view *GateView
	env.RegisterDelayedCallback(func() {
		env.query(t, QueryGate, &view)
		env.SignalWorkflow(SignalHumanDecision, "impossible")
	}, time.Minute)
	res, err := env.run(inp)
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcome != OutcomeImpossible || res.Status != StatusImpossible || !strings.HasPrefix(res.Reason, "declared impossible by a human") {
		t.Fatalf("result %+v", res)
	}
	if view == nil || view.Kind != GateDeadlock || view.Recommended != "impossible" || view.GateID != "deadlock-3" ||
		!strings.Contains(view.Question, "Recommended: impossible (item 01 failed 3 times in a row).") {
		t.Fatalf("view %+v", view)
	}
	if commitsWith(t, inp.Workdir, "lha: mission declared impossible") != 1 {
		t.Fatal("no final checkpoint")
	}
	if s := store.Statuses(); s[len(s)-1] != StatusImpossible {
		t.Fatalf("row %v", s)
	}
}

// 4b. the deadlock gate's default (never "retry").
func TestDeadlockGateDefaults(t *testing.T) {
	for _, tc := range []struct{ configured, outcome string }{
		{"impossible", OutcomeImpossible}, {"abort", OutcomeAborted}, {"retry", OutcomeAborted}, {" IMPOSSIBLE ", OutcomeImpossible},
	} {
		t.Run(tc.configured, func(t *testing.T) {
			inp := initMission(t, 1)
			inp.DeadlockGateSeconds = 60
			inp.DeadlockGateDefault = tc.configured
			env := newEnv(t, newActs(t, idleModel, nil), nil)
			res, err := env.run(inp)
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != tc.outcome || !strings.Contains(res.Reason, "by default (no human answered)") {
				t.Fatalf("result %+v", res)
			}
		})
	}
}

// Steering notes reach every following cycle's prompt (and survive Continue-As-New).
func TestSteerNotesReachTheCycles(t *testing.T) {
	inp := initMission(t, 2)
	inp.CyclesBeforeCAN = 1
	var mu sync.Mutex
	var seen []CycleInput
	acts := newActs(t, workingModel, nil)
	spy := func(ctx context.Context, in CycleInput) (CycleResult, error) {
		mu.Lock()
		seen = append(seen, in)
		mu.Unlock()
		return acts.RunAgentCycle(ctx, in)
	}
	first := true
	res, _ := runFollowingCAN(t, func() *testEnv {
		env := newEnv(t, acts, map[string]any{ActivityRunAgentCycle: spy})
		if first {
			first = false
			env.RegisterDelayedCallback(func() { env.SignalWorkflow(SignalSteer, "  prefer small commits  ") }, 0)
		}
		return env
	}, inp)
	if !res.Completed || len(seen) != 2 {
		t.Fatalf("result %+v cycles %d", res, len(seen))
	}
	for _, in := range seen {
		if len(in.SteerNotes) != 1 || in.SteerNotes[0] != "prefer small commits" {
			t.Fatalf("steer notes %v", in.SteerNotes)
		}
	}
}

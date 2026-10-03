package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent/agenttest"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Ported from python/tests/unit/test_wiring_runner.py.

var redCheck = contracts.Check{Name: "red", Gating: true, Where: "sandbox", Command: []string{"false"}}

func runnerSettings(t *testing.T, env ...string) *config.Settings {
	t.Helper()
	base := []string{"LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true", "LHA_MODEL_BACKEND=stub",
		"LHA_BUDGET_USD_CEILING=100", "LHA_MAX_CYCLES=20", "LHA_MAX_TURNS_PER_CYCLE=3", "LHA_STALL_LIMIT=50"}
	s, err := config.LoadFrom(append(base, env...), "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func oneItem() contracts.Checklist {
	return contracts.Checklist{Items: []contracts.ChecklistItem{item("01", "do it")}, SchemaVersion: 1}
}

// fakeOpener refuses an unsafe local sandbox like the real execution layer, else opens a fake.
func fakeOpener(boxes *[]*agenttest.Toolbox) ToolboxOpener {
	return func(_ context.Context, req ToolboxRequest) (Toolbox, error) {
		if req.Settings.Sandbox == "local" && !req.Settings.AllowUnsafeLocal {
			return nil, errors.New("the 'local' sandbox runs agent commands directly on the host")
		}
		box := agenttest.NewToolbox(req.Workdir)
		if boxes != nil {
			*boxes = append(*boxes, box)
		}
		return box, nil
	}
}

func runOpts(t *testing.T, workdir string, s *config.Settings, m contracts.ModelProvider, checks ...contracts.Check) RunOptions {
	return RunOptions{Workdir: workdir, Title: "t", Description: "d", Checklist: oneItem(), Checks: checks,
		Settings: s, Model: m, OpenToolbox: fakeOpener(nil)}
}

func TestLocalSandboxRequiresExplicitOptIn(t *testing.T) {
	workdir := filepath.Join(t.TempDir(), "ws")
	o := runOpts(t, workdir, runnerSettings(t, "LHA_ALLOW_UNSAFE_LOCAL=false"), nil, passCheck)
	if _, err := RunMissionLocal(context.Background(), o); err == nil {
		t.Fatal("expected a refusal")
	}
	if _, err := os.Stat(workdir); !os.IsNotExist(err) {
		t.Fatal("refused run touched the workspace")
	}
}

func TestRunnerCompletesWhenVerified(t *testing.T) {
	var boxes []*agenttest.Toolbox
	o := runOpts(t, t.TempDir(), runnerSettings(t), model.NewStub([]contracts.TurnResult{done}), passCheck)
	o.OpenToolbox = fakeOpener(&boxes)
	s, err := RunMissionLocal(context.Background(), o)
	if err != nil || !s.Completed || s.StoppedReason != "complete" || s.HeadSHA == "" || !strings.HasPrefix(s.MissionID, "mission_") {
		t.Fatalf("%+v %v", s, err)
	}
	if len(boxes) != 1 || !boxes[0].Closed {
		t.Fatal("toolbox not closed")
	}
	if !strings.Contains(s.TraceJSONL, `"kind":"checkpoint"`) {
		t.Fatalf("trace: %s", s.TraceJSONL)
	}
}

func TestRunnerReportsDeadlockNeverComplete(t *testing.T) {
	o := runOpts(t, t.TempDir(), runnerSettings(t), model.NewStub([]contracts.TurnResult{done}), redCheck)
	s, err := RunMissionLocal(context.Background(), o)
	if err != nil || s.Completed || !strings.HasPrefix(s.StoppedReason, "deadlocked:") ||
		s.ItemsDone != 0 || s.ItemsTotal != 1 || s.Cycles != 3 {
		t.Fatalf("%+v %v", s, err)
	}
}

// pricey costs $1 per call: it writes a file, then says done.
type pricey struct{ calls int }

func (p *pricey) Name() string          { return "fake:pricey" }
func (p *pricey) DefaultMaxTokens() int { return 10 }
func (p *pricey) Complete(_ context.Context, _ []contracts.ModelMessage, _ []map[string]any, _ int) (contracts.TurnResult, error) {
	p.calls++
	t := `{"done": true}`
	if p.calls == 1 {
		t = `{"tool": "write_file", "arguments": {"path": "a.txt", "content": "x"}}`
	}
	return contracts.TurnResult{Text: t, Usage: contracts.Usage{InputTokens: 1, OutputTokens: 1, Model: "p"}}, nil
}
func (p *pricey) EstimateCostUSD(contracts.Usage) (float64, error) { return 1.0, nil }

func TestBudgetExceededMidCycleStopsCleanly(t *testing.T) {
	p := &pricey{}
	o := runOpts(t, t.TempDir(), runnerSettings(t, "LHA_BUDGET_USD_CEILING=1.5"), p, passCheck)
	s, err := RunMissionLocal(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(s.StoppedReason, "governor:") || !strings.Contains(s.StoppedReason, "worst-case") ||
		s.TotalUSD != 1.0 || p.calls != 1 || s.Completed {
		t.Fatalf("%+v calls=%d", s, p.calls)
	}
}

func TestPlannerAndLeadShareOneMeter(t *testing.T) {
	settings := runnerSettings(t)
	meter := BuildMeter(settings)
	o := PlanOptions{RunOptions: RunOptions{Workdir: t.TempDir(), Title: "t", Checks: []contracts.Check{passCheck},
		Settings: settings, Meter: meter, OpenToolbox: fakeOpener(nil)}, Task: "do it"}
	s, err := PlanAndRunLocal(context.Background(), o)
	if err != nil || !s.Completed {
		t.Fatalf("%+v %v", s, err)
	}
	roles := map[string]bool{}
	for _, e := range meter.Ledger.Entries() {
		roles[e.Role] = true
	}
	if !roles["planner"] || !roles["lead"] {
		t.Fatalf("roles: %v", roles)
	}
}

func TestPlannerBudgetRefusalIsAnError(t *testing.T) {
	settings := runnerSettings(t, "LHA_BUDGET_USD_CEILING=0.5")
	o := PlanOptions{RunOptions: RunOptions{Workdir: t.TempDir(), Title: "t", Settings: settings, OpenToolbox: fakeOpener(nil)},
		Task: "do it", PlannerModel: &pricey{}}
	_, err := PlanAndRunLocal(context.Background(), o)
	var budget *governor.BudgetExceeded
	if !errors.As(err, &budget) {
		t.Fatalf("err = %v", err)
	}
}

func TestLoopDetectorStopsARepeatingFailure(t *testing.T) {
	o := runOpts(t, t.TempDir(), runnerSettings(t, "LHA_STALL_LIMIT=2"), model.NewStub([]contracts.TurnResult{done}), redCheck)
	s, err := RunMissionLocal(context.Background(), o)
	if err != nil || s.StoppedReason != "loop on item 01" || s.Cycles != 2 {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestMaxCyclesGovernorStop(t *testing.T) {
	o := runOpts(t, t.TempDir(), runnerSettings(t, "LHA_MAX_CYCLES=1"), model.NewStub([]contracts.TurnResult{done}), redCheck)
	s, err := RunMissionLocal(context.Background(), o)
	// The loop condition ends first (cycles == max_cycles), exactly as in Python.
	if err != nil || s.StoppedReason != "max_cycles" || s.Cycles != 1 {
		t.Fatalf("%+v %v", s, err)
	}
}

func TestAlteredDecisionLogStopsTheRun(t *testing.T) {
	dir := t.TempDir()
	o := runOpts(t, dir, runnerSettings(t), &tamperDecisions{dir: dir, StubModel: model.NewStub([]contracts.TurnResult{done})}, redCheck)
	s, err := RunMissionLocal(context.Background(), o)
	if err != nil || !strings.HasPrefix(s.StoppedReason, DecisionChainStop+": ") {
		t.Fatalf("%+v %v", s, err)
	}
}

// tamperDecisions commits a forged decision log on its first turn (an attacker with git access).
type tamperDecisions struct {
	*model.StubModel
	dir  string
	done bool
}

func (m *tamperDecisions) Complete(ctx context.Context, msgs []contracts.ModelMessage, tools []map[string]any, n int) (contracts.TurnResult, error) {
	if !m.done {
		m.done = true
		_ = os.WriteFile(filepath.Join(m.dir, ".lha", "decisions.ndjson"), []byte("this is not a decision record\n"), 0o644)
		_, _ = state.CommitAll(ctx, m.dir, "forge")
	}
	return m.StubModel.Complete(ctx, msgs, tools, n)
}

func TestReferencesAreRecitedInTheMissionAnchor(t *testing.T) {
	var boxes []*agenttest.Toolbox
	rec := newRecording(done)
	o := runOpts(t, t.TempDir(), runnerSettings(t), rec, passCheck)
	o.References = []string{"reference/fabric-rest"}
	o.OpenToolbox = fakeOpener(&boxes)
	if _, err := RunMissionLocal(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.calls[0][0].Content, "- reference/fabric-rest") {
		t.Fatalf("system: %s", rec.calls[0][0].Content)
	}
}

func TestNoToolboxOpenerIsNotLinked(t *testing.T) {
	o := runOpts(t, t.TempDir(), runnerSettings(t), model.NewStub(nil), passCheck)
	o.OpenToolbox = nil
	if _, err := RunMissionLocal(context.Background(), o); !errors.Is(err, ErrExecutionNotLinked) {
		t.Fatalf("err = %v", err)
	}
}

func TestALocalMissionLeavesItsTraceInTheStore(t *testing.T) {
	db := filepath.Join(t.TempDir(), "lha.sqlite3")
	settings := runnerSettings(t, "LHA_SQLITE_PATH="+db)
	o := runOpts(t, filepath.Join(t.TempDir(), "ws"), settings,
		model.NewStub([]contracts.TurnResult{writeTurn("out.txt", "hello"), done}), fileHasHello)
	s, err := RunMissionLocal(context.Background(), o)
	if err != nil || !s.Completed {
		t.Fatalf("%+v %v", s, err)
	}
	store, err := persistence.OpenSQLite(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	rows, err := store.ReadMissionEvents(context.Background(), s.MissionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	var call *persistence.EventRow
	for i, r := range rows {
		kinds = append(kinds, r.Kind)
		if r.Kind == "tool_call" {
			call = &rows[i]
		}
	}
	if call == nil || call.Payload["tool"] != "write_file" || call.Payload["ok"] != true || call.CycleID != "c1" {
		t.Fatalf("%v %+v", kinds, call)
	}
	if !slices.Contains(kinds, "cycle_started") || !slices.Contains(kinds, "checkpoint") {
		t.Fatal(kinds)
	}
}

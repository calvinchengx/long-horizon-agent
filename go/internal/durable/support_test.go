package durable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/tools"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// Shared fixtures of the durability tests (python: tests/durability/_support.py): a scripted
// stub model (no network) that writes work/<item>.txt and then says done, and a REAL gating check
// run in the (local) sandbox that passes only if the working tree holds more work/*.txt files
// than the committed checklist has done items — i.e. only once THIS cycle's work exists.

func TestMain(m *testing.M) {
	obs.ConfigureLogging(io.Discard, false)
	os.Exit(m.Run())
}

// checkCommands is the gating check (a shell twin of the Python one).
var checkCommands = [][]string{{"sh", "-c",
	`done=$(grep -o '"status": *"done"' .lha/checklist.json | wc -l); ` +
		`work=$(ls work/*.txt 2>/dev/null | wc -l); [ "$work" -gt "$done" ]`}}

func testSettings(t *testing.T, extra ...string) *config.Settings {
	t.Helper()
	env := append([]string{
		"LHA_MODEL_BACKEND=stub", "LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true",
		"LHA_MAX_TURNS_PER_CYCLE=4", "LHA_MAX_REPLANS=0",
		"LHA_SQLITE_PATH=" + filepath.Join(t.TempDir(), "lha.sqlite3"),
		"LHA_OBJECT_STORE_ROOT=" + filepath.Join(t.TempDir(), "objects"),
	}, extra...)
	s, err := config.LoadFrom(env, "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// testToolbox is cmd/lha's openToolbox for the local sandbox (the CLI passes its own wiring).
func testToolbox(ctx context.Context, req agent.ToolboxRequest) (agent.Toolbox, error) {
	sandbox, err := execution.BuildSandbox(req.Settings.Sandbox, execution.SandboxOptions{AllowUnsafeLocal: req.Settings.AllowUnsafeLocal})
	if err != nil {
		return nil, err
	}
	dispatcher, err := tools.BuildRunDispatcher(req.Settings, tools.RunDispatcherOptions{AllowMutating: true, Gate: req.Gate})
	if err != nil {
		return nil, err
	}
	var lead contracts.ToolDispatcher = dispatcher
	if req.Anchor != nil {
		lead = tools.WithDecisionTool(dispatcher, req.Anchor)
	}
	session, err := sandbox.Open(ctx, req.Workdir, "")
	if err != nil {
		return nil, err
	}
	return &simpleToolbox{session: session, dispatcher: lead}, nil
}

type simpleToolbox struct {
	session    contracts.SandboxSession
	dispatcher contracts.ToolDispatcher
}

func (t *simpleToolbox) Session() contracts.SandboxSession    { return t.session }
func (t *simpleToolbox) Dispatcher() contracts.ToolDispatcher { return t.dispatcher }
func (t *simpleToolbox) Close(ctx context.Context) error      { return t.session.Close(ctx) }

func doneTurn() contracts.TurnResult {
	return contracts.TurnResult{Text: `{"done": true, "summary": "ok"}`, StopReason: strPtr("end_turn")}
}

func writeTurn(path string) contracts.TurnResult {
	return contracts.TurnResult{
		ToolCalls:  []contracts.ToolCall{{ID: "w1", Name: "write_file", Arguments: map[string]any{"path": path, "content": "done"}}},
		StopReason: strPtr("tool_use"),
	}
}

func runTurn(argv []string) contracts.TurnResult {
	args := make([]any, len(argv))
	for i, a := range argv {
		args[i] = a
	}
	return contracts.TurnResult{
		ToolCalls:  []contracts.ToolCall{{ID: "r1", Name: "run_command", Arguments: map[string]any{"argv": args}}},
		StopReason: strPtr("tool_use"),
	}
}

// workingModel writes the active item's work file, then signals done.
func workingModel(_ *config.Settings, snap contracts.SituationSnapshot) (contracts.ModelProvider, error) {
	if snap.ActiveItem == nil {
		return nil, errors.New("no active item")
	}
	return model.NewStub([]contracts.TurnResult{writeTurn("work/" + snap.ActiveItem.ID + ".txt"), doneTurn()}), nil
}

// idleModel claims done without doing any work (verification must fail).
func idleModel(*config.Settings, contracts.SituationSnapshot) (contracts.ModelProvider, error) {
	return model.NewStub([]contracts.TurnResult{doneTurn()}), nil
}

func testChecklist(n int, chain bool) contracts.Checklist {
	cl := contracts.Checklist{SchemaVersion: 1}
	for i := 1; i <= n; i++ {
		var deps []string
		if chain && i > 1 {
			deps = []string{fmt.Sprintf("%02d", i-1)}
		}
		cl.Items = append(cl.Items, contracts.NewChecklistItem(fmt.Sprintf("%02d", i), fmt.Sprintf("task %d", i), deps...))
	}
	return cl
}

// initMission initializes the anchor (a git repo) with n items and returns the mission input.
func initMission(t *testing.T, n int) MissionInput {
	t.Helper()
	workdir := t.TempDir()
	anchor := state.NewGitMissionAnchor(workdir)
	if _, err := anchor.Initialize(context.Background(), "Test mission", "durability", testChecklist(n, false)); err != nil {
		t.Fatal(err)
	}
	inp := NewMissionInput(fmt.Sprintf("m%d", time.Now().UnixNano()%1_000_000_000), workdir)
	inp.MaxCycles = 50
	inp.CheckCommands = checkCommands
	inp.ParkInitialSeconds = 30
	inp.ParkMaxSeconds = 600
	return inp
}

// recordingStore records every write (the missions row statuses, gate events, cost rows).
type recordingStore struct {
	mu       sync.Mutex
	statuses []string
	gates    []GateEvent
	costs    []string
	failRows bool
}

type recordingHandle struct{ s *recordingStore }

func (s *recordingStore) opener(context.Context, *config.Settings, string) (Store, error) {
	return recordingHandle{s}, nil
}

func (h recordingHandle) UpsertMission(_ context.Context, row MissionRow) error {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if h.s.failRows {
		return errors.New("store is down")
	}
	h.s.statuses = append(h.s.statuses, row.Status)
	return nil
}

func (h recordingHandle) RecordGateEvent(_ context.Context, e GateEvent) error {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	h.s.gates = append(h.s.gates, e)
	return nil
}

func (h recordingHandle) RecordCost(_ context.Context, _ string, _ governor.CostEntry, key string) (bool, error) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	h.s.costs = append(h.s.costs, key)
	return true, nil
}

func (h recordingHandle) Close(context.Context) error { return nil }

func (s *recordingStore) Statuses() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.statuses...)
}

func healthy(context.Context, *config.Settings) model.ModelHealth { return model.ModelHealth{OK: true} }

// newActs are the real activities over the local sandbox with the given model factory.
func newActs(t *testing.T, factory ModelFactory, store *recordingStore) *Activities {
	acts := &Activities{
		Settings:       testSettings(t),
		ModelFactory:   factory,
		OpenToolbox:    testToolbox,
		ProbeModel:     healthy,
		HeartbeatEvery: time.Second,
	}
	if store != nil {
		acts.OpenStore = store.opener
	}
	return acts
}

type testEnv struct {
	*testsuite.TestWorkflowEnvironment
	acts *Activities
}

// newEnv is a test workflow environment hosting the mission workflow and acts, with overrides
// replacing activities by name.
func newEnv(t *testing.T, acts *Activities, overrides map[string]any) *testEnv {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	suite.SetLogger(Logger(io.Discard, slog.LevelError))
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(120 * time.Second)
	env.RegisterWorkflowWithOptions(MissionWorkflow, workflow.RegisterOptions{Name: WorkflowMission})
	env.RegisterWorkflowWithOptions(SubAgentWorkflow, workflow.RegisterOptions{Name: WorkflowSubAgent})
	fns := map[string]any{
		ActivityRunAgentCycle:       acts.RunAgentCycle,
		ActivityCheckMissionHealth:  acts.CheckMissionHealth,
		ActivityNotifyGate:          acts.NotifyGate,
		ActivityDeclareImpossible:   acts.DeclareImpossible,
		ActivityUnblockItems:        acts.UnblockItems,
		ActivityReadMissionSnapshot: acts.ReadMissionSnapshot,
		ActivityRecordMissionStatus: acts.RecordMissionStatus,
		ActivityRunSubAgent:         acts.RunSubAgent,
		ActivityPlanRound:           acts.PlanRound,
		ActivityRunImplementer:      acts.RunImplementer,
		ActivityIntegrateBranch:     acts.IntegrateBranch,
		ActivityReviewCycle:         acts.ReviewCycle,
	}
	for name, fn := range overrides {
		fns[name] = fn
	}
	for name, fn := range fns {
		env.RegisterActivityWithOptions(fn, activity.RegisterOptions{Name: name})
	}
	return &testEnv{TestWorkflowEnvironment: env, acts: acts}
}

// run executes the mission and returns its result (or the workflow error).
func (e *testEnv) run(inp MissionInput) (MissionResult, error) {
	e.ExecuteWorkflow(WorkflowMission, inp)
	if !e.IsWorkflowCompleted() {
		return MissionResult{}, errors.New("workflow did not complete")
	}
	if err := e.GetWorkflowError(); err != nil {
		return MissionResult{}, err
	}
	var r MissionResult
	err := e.GetWorkflowResult(&r)
	return r, err
}

// runFollowingCAN runs the mission, following Continue-As-New into fresh environments.
func runFollowingCAN(t *testing.T, mk func() *testEnv, inp MissionInput) (MissionResult, int) {
	t.Helper()
	runs := 0
	for {
		runs++
		env := mk()
		res, err := env.run(inp)
		var can *workflow.ContinueAsNewError
		if errors.As(err, &can) {
			if err := converter.GetDefaultDataConverter().FromPayloads(can.Input, &inp); err != nil {
				t.Fatal(err)
			}
			if runs > 50 {
				t.Fatal("too many runs")
			}
			continue
		}
		if err != nil {
			t.Fatalf("mission failed: %v", err)
		}
		return res, runs
	}
}

func (e *testEnv) query(t *testing.T, name string, out any) {
	t.Helper()
	v, err := e.QueryWorkflow(name)
	if err != nil {
		t.Fatalf("query %s: %v", name, err)
	}
	if err := v.Get(out); err != nil {
		t.Fatalf("query %s: %v", name, err)
	}
}

func commitsWith(t *testing.T, workdir, marker string) int {
	t.Helper()
	lines, err := state.LogOneline(context.Background(), workdir, 1000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, l := range lines {
		if strings.Contains(l, marker) {
			n++
		}
	}
	return n
}

func allCommittedPaths(t *testing.T, workdir string) map[string]bool {
	t.Helper()
	out, err := state.RunGit(context.Background(), workdir, "log", "--all", "--name-only", "--pretty=format:")
	if err != nil {
		t.Fatal(err)
	}
	paths := map[string]bool{}
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			paths[l] = true
		}
	}
	return paths
}

func committedEvents(t *testing.T, workdir string) []contracts.EventRecord {
	t.Helper()
	raw, err := state.RunGit(context.Background(), workdir, "show", "HEAD:.lha/events.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	var out []contracts.EventRecord
	for _, l := range strings.Split(raw, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var ev contracts.EventRecord
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatal(err)
		}
		out = append(out, ev)
	}
	return out
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

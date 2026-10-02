package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/durable"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// The durable commands without a server: option validation, the MissionInput mission-start
// builds, the mission row it writes, and the gate formatting shared with Python.

// fakeTemporal records ExecuteWorkflow; any other client call panics (not expected here).
type fakeTemporal struct {
	client.Client
	started []startCall
	fail    error
}

type startCall struct {
	options  client.StartWorkflowOptions
	workflow any
	input    durable.MissionInput
}

// startedRE matches mission-start's "started mission" line.
var startedRE = regexp.MustCompile(`started mission (mission_[0-9a-f]{12}) \(workflow id: mission:(mission_[0-9a-f]{12})\)`)

func (f *fakeTemporal) ExecuteWorkflow(_ context.Context, o client.StartWorkflowOptions, wf any, args ...any) (client.WorkflowRun, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	f.started = append(f.started, startCall{o, wf, args[0].(durable.MissionInput)})
	return nil, nil
}

func (f *fakeTemporal) Close() {}

func useFakeTemporal(t *testing.T) *fakeTemporal {
	t.Helper()
	fake := &fakeTemporal{}
	saved := dialTemporal
	t.Cleanup(func() { dialTemporal = saved })
	dialTemporal = func(context.Context, *config.Settings) (client.Client, error) { return fake, nil }
	return fake
}

type rowStore struct {
	rows  []durable.MissionRow
	costs []string
}

func (s *rowStore) UpsertMission(_ context.Context, row durable.MissionRow) error {
	s.rows = append(s.rows, row)
	return nil
}
func (s *rowStore) RecordGateEvent(context.Context, durable.GateEvent) error { return nil }
func (s *rowStore) RecordCost(_ context.Context, _ string, _ governor.CostEntry, key string) (bool, error) {
	s.costs = append(s.costs, key)
	return true, nil
}
func (s *rowStore) Close(context.Context) error { return nil }

func useRowStore(t *testing.T) *rowStore {
	t.Helper()
	store := &rowStore{}
	saved := durable.DefaultStoreOpener
	t.Cleanup(func() { durable.DefaultStoreOpener = saved })
	durable.DefaultStoreOpener = func(context.Context, *config.Settings, string) (durable.Store, error) { return store, nil }
	return store
}

func TestMissionStartImportsAChecklistAndStartsTheWorkflow(t *testing.T) {
	dir := cleanEnv(t, "LHA_TASK_QUEUE=q-test", "LHA_APPROVAL_TIMEOUT_S=600", "LHA_CYCLE_PAUSE_SECONDS=5",
		"LHA_GATE_ESCALATION_SECONDS=[60, 120]", "LHA_MAX_CYCLES=77")
	fake := useFakeTemporal(t)
	store := useRowStore(t)
	_ = os.WriteFile(filepath.Join(dir, "plan.md"), []byte("# Imported\n\n- [ ] one\n- [ ] two\n"), 0o644)
	before := float64(time.Now().Unix())
	r := runCLI(t, nil, "mission-start", "--checklist", "plan.md", "--no-default-checks", "--check", "uv run pytest -q",
		"--workdir", "ws", "--deadlock-gate-hours", "1.5", "--deadlock-default", " Impossible ", "--start-in-seconds", "60",
		"--max-parallel", "1")
	if r.code != 0 || !startedRE.MatchString(r.stdout) {
		t.Fatalf("%+v", r)
	}
	if len(fake.started) != 1 {
		t.Fatalf("started %d", len(fake.started))
	}
	call := fake.started[0]
	in := call.input
	id := startedRE.FindStringSubmatch(r.stdout)[1]
	abs, _ := filepath.Abs("ws")
	if call.options.ID != "mission:"+id || call.options.TaskQueue != "q-test" || call.workflow != durable.WorkflowMission {
		t.Fatalf("options %+v", call.options)
	}
	if in.MissionID != id || in.Workdir != abs || in.MaxCycles != 77 || in.DeadlockGateSeconds != 5400 ||
		in.ApprovalTimeoutSeconds != 600 || in.DeadlockGateDefault != "impossible" || in.CyclePauseSeconds != 5 ||
		in.ResumeAt < before+59 || in.MaxParallel != 1 || len(in.GateEscalationSeconds) != 2 ||
		len(in.CheckCommands) != 1 || strings.Join(in.CheckCommands[0], " ") != "uv run pytest -q" {
		t.Fatalf("input %+v", in)
	}
	// The row is written before the workflow starts: SLEEPING for a scheduled start.
	if len(store.rows) != 1 || store.rows[0].Status != durable.StatusSleeping || store.rows[0].Title != "Imported" ||
		store.rows[0].WorkflowID != "mission:"+id {
		t.Fatalf("rows %+v", store.rows)
	}
	snap, err := state.NewGitMissionAnchor(abs).ReadSituationalAwareness(context.Background())
	if err != nil || snap.ItemsTotal != 2 {
		t.Fatalf("anchor %+v %v", snap, err)
	}
}

func TestMissionStartPlansWithThePlannerAndRecordsItsSpend(t *testing.T) {
	cleanEnv(t, "LHA_MODEL_BACKEND=stub")
	fake := useFakeTemporal(t)
	store := useRowStore(t)
	planner := model.NewStub([]contracts.TurnResult{{Text: `[{"description": "step one"}, {"description": "step two"}]`}})
	r := runCLI(t, planner, "mission-start", "--task", "do it", "--no-default-checks", "--check", "true", "--workdir", "ws")
	if r.code != 0 || len(fake.started) != 1 {
		t.Fatalf("%+v", r)
	}
	in := fake.started[0].input
	if in.ResumeAt != 0 || in.DeadlockGateSeconds != 86400 || in.DeadlockGateDefault != "abort" || in.ApprovalTimeoutSeconds != 86400 {
		t.Fatalf("input %+v", in)
	}
	if len(store.rows) != 1 || store.rows[0].Status != durable.StatusRunning || len(store.costs) != 1 || store.costs[0] != "planner#0" {
		t.Fatalf("rows %+v costs %v", store.rows, store.costs)
	}
}

// The organization's options reach the workflow; waves plan with the Planner's file ownership.
func TestMissionStartWithTheOrganization(t *testing.T) {
	dir := cleanEnv(t, "LHA_MODEL_BACKEND=stub")
	fake := useFakeTemporal(t)
	useRowStore(t)
	planner := model.NewStub([]contracts.TurnResult{{Text: `[{"description": "write a", "files": ["a.py"]}, ` +
		`{"description": "write b", "files": ["b.py"]}]`}})
	r := runCLI(t, planner, "mission-start", "--task", "two files", "--no-default-checks", "--check", "true",
		"--workdir", "ws", "--research", "2", "--review", "--max-parallel", "3")
	if r.code != 0 || len(fake.started) != 1 || r.stderr != "" {
		t.Fatalf("%+v", r)
	}
	if in := fake.started[0].input; in.ResearchPerItem != 2 || !in.Review || in.MaxParallel != 3 {
		t.Fatalf("input %+v", in)
	}
	owners := git(t, filepath.Join(dir, "ws"), "show", "HEAD:.lha/ownership.json")
	if !strings.Contains(owners, `"a.py": "implementer-01"`) || !strings.Contains(owners, `"b.py": "implementer-02"`) {
		t.Fatalf("ownership %s", owners)
	}
	// Without waves the mission declares no ownership (as in Python).
	planner = model.NewStub([]contracts.TurnResult{{Text: `[{"description": "write a", "files": ["a.py"]}]`}})
	if r := runCLI(t, planner, "mission-start", "--task", "one file", "--workdir", "ws2", "--review", "--no-review",
		"--research", "1"); r.code != 0 || fake.started[1].input.Review || fake.started[1].input.ResearchPerItem != 1 {
		t.Fatalf("%+v %+v", r, fake.started[1].input)
	}
	if ok, _ := state.ExistsAtHead(context.Background(), filepath.Join(dir, "ws2"), ".lha/ownership.json"); ok {
		t.Fatal("ownership.json without --max-parallel")
	}
	// An imported checklist has no ownership: a note says the items run serially.
	_ = os.WriteFile(filepath.Join(dir, "plan.md"), []byte("- [ ] one\n- [ ] two\n"), 0o644)
	r = runCLI(t, nil, "mission-start", "--checklist", "plan.md", "--workdir", "ws3", "--max-parallel", "2")
	if r.code != 0 || r.stderr != "note: an imported checklist declares no file ownership, so no parallel wave can run "+
		"(items are worked serially)\n" {
		t.Fatalf("%+v", r)
	}
}

func TestMissionStartRecordsAbortedWhenTheStartFails(t *testing.T) {
	dir := cleanEnv(t)
	fake := useFakeTemporal(t)
	fake.fail = errors.New("connection refused")
	store := useRowStore(t)
	_ = os.WriteFile(filepath.Join(dir, "plan.md"), []byte("- [ ] one\n"), 0o644)
	r := runCLI(t, nil, "mission-start", "--checklist", "plan.md", "--workdir", "ws")
	if r.code != 1 || !strings.Contains(r.stderr, "connection refused") {
		t.Fatalf("%+v", r)
	}
	if len(store.rows) != 2 || store.rows[1].Status != durable.StatusAborted {
		t.Fatalf("rows %+v", store.rows)
	}
}

func TestDurableCommandsValidateBeforeConnecting(t *testing.T) {
	cleanEnv(t)
	saved := dialTemporal
	t.Cleanup(func() { dialTemporal = saved })
	dialTemporal = func(context.Context, *config.Settings) (client.Client, error) {
		t.Fatal("connected before validating")
		return nil, nil
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"mission-start", "--task", "x", "--max-parallel", "9"}, "Error: Invalid value for '--max-parallel': 9 is not in the range 0<=x<=8."},
		{[]string{"mission-start", "--task", "x", "--research", "5"}, "Error: Invalid value for '--research': 5 is not in the range 0<=x<=4."},
		{[]string{"mission-start", "--task", "x", "--start-in-seconds", "-1"}, "is not in the range x>=0."},
		{[]string{"mission-start"}, "error: give --task (to plan) or --checklist FILE (to import a checklist)"},
		{[]string{"mission-start", "--task", "x", "--deadlock-default", "retry"}, "error: --deadlock-default must be one of abort, impossible"},
		{[]string{"mission-start", "--task", "x", "--no-default-checks"}, "error: --no-default-checks requires at least one non-empty --check"},
		{[]string{"mission-approve", "m1", "--decision", "maybe"}, "error: unknown --decision 'maybe'; expected approve, reject, retry, abort, impossible"},
		{[]string{"mission-approve", "m1"}, "Error: Missing option '--decision'."},
		{[]string{"mission-approve", "--decision", "approve"}, "Error: Missing argument 'MISSION_ID'."},
		{[]string{"mission-snooze", "m1", "--seconds", "-5"}, "Error: Invalid value for '--seconds': -5 is not in the range x>=0."},
		{[]string{"mission-steer", "m1"}, "Error: Missing option '--note'."},
		{[]string{"mission-approve", "m1", "--decision", "approve", "--as", strings.Repeat("x", 201)}, "error: --as is longer than 200 characters"},
		{[]string{"mission-steer", "m1", "--note", "   "}, "error: --note must not be empty"},
		{[]string{"mission-steer", "m1", "--note", strings.Repeat("x", 2001)}, "error: --note is 2001 characters; at most 2000 are kept"},
		{[]string{"mission-status", "a", "b"}, "Error: Got unexpected extra argument (b)"},
		{[]string{"mission-abort"}, "Error: Missing argument 'MISSION_ID'."},
	} {
		r := runCLI(t, nil, tc.args...)
		if r.code != 2 || !strings.Contains(r.stderr, tc.want) {
			t.Fatalf("%v: %+v", tc.args, r)
		}
	}
}

// format_gate / check_decision are byte-identical to Python's.
func TestGateFormattingMatchesPython(t *testing.T) {
	cleanEnv(t)
	gate := &durable.GateView{GateID: "approval-5d41402abc4b", Kind: durable.GateToolCall, Question: "Mission m wants to run: x? Approve or reject?",
		Options: []string{"approve", "reject"}, DefaultAction: "reject", OpenedAt: "2026-09-24T10:00:00+00:00",
		Deadline: "2026-09-25T10:00:00+00:00", EscalationsSent: 1, NextEscalationAt: "2026-09-24T10:45:00+00:00",
		Recommended: "", Request: &durable.PendingApproval{Fingerprint: "5d41402abc4b2a76b9719d911017c592", Tool: "run_command",
			Reason: "git push (outward-facing / rewrites history)", Arguments: "{'argv': ['git', 'push', 'origin', 'main']}"}}
	deadlock := &durable.GateView{GateID: "deadlock-3", Kind: durable.GateDeadlock, Question: "Deadlocked?",
		Options: []string{"retry", "abort", "impossible"}, DefaultAction: "abort", Recommended: "impossible"}
	var goLines []string
	for _, g := range []*durable.GateView{gate, deadlock} {
		goLines = append(goLines, formatGate(g)...)
	}
	for _, d := range []string{" APPROVE ", "retry"} {
		if _, err := checkDecision(gate, d); err != nil {
			goLines = append(goLines, "ValueError: "+err.Error())
		} else {
			choice, _ := checkDecision(gate, d)
			goLines = append(goLines, "ok: "+choice)
		}
	}
	if _, err := checkDecision(nil, "bogus"); err != nil {
		goLines = append(goLines, "ValueError: "+err.Error())
	}
	if !pythonAvailable(t) {
		return
	}
	g1, _ := json.Marshal(gate)
	g2, _ := json.Marshal(deadlock)
	script := `
import sys
from temporalio.converter import DataConverter
from temporalio.api.common.v1 import Payload
from lha.cli.main import check_decision, format_gate
from lha.durable.types import GateView
conv = DataConverter.default.payload_converter
gates = [conv.from_payloads([Payload(metadata={"encoding": b"json/plain"}, data=a.encode())], [GateView])[0] for a in sys.argv[1:3]]
lines = format_gate(gates[0]) + format_gate(gates[1])
for d in (" APPROVE ", "retry"):
    try:
        lines.append("ok: " + check_decision(gates[0], d))
    except ValueError as e:
        lines.append(f"ValueError: {e}")
try:
    check_decision(None, "bogus")
except ValueError as e:
    lines.append(f"ValueError: {e}")
print("\n".join(lines))
`
	r := runProcess(t, repoRoot, processEnv(), "uv", "run", "--quiet", "--project", filepath.Join(repoRoot, "python"),
		"python", "-c", script, string(g1), string(g2))
	if r.code != 0 {
		t.Fatalf("python: %+v", r)
	}
	if got, want := strings.Join(goLines, "\n")+"\n", r.stdout; got != want {
		t.Fatalf("go:\n%s\npython:\n%s", got, want)
	}
}

func TestPyValueAndIsoFull(t *testing.T) {
	if pyValue(float64(3)) != "3" || pyValue("RUNNING") != "RUNNING" || pyValue(nil) != "None" {
		t.Fatal("pyValue")
	}
	if durable.IsoFull(1790000000.25) != "2026-09-21T14:13:20.250000+00:00" {
		t.Fatal(durable.IsoFull(1790000000.25))
	}
}

func TestWorkerRefusesHalfSetVersioning(t *testing.T) {
	cleanEnv(t, "LHA_WORKER_BUILD_ID=b1")
	r := runCLI(t, nil, "worker")
	if r.code != 2 || r.stderr != "error: LHA_WORKER_DEPLOYMENT and LHA_WORKER_BUILD_ID must be set together (worker versioning)\n" {
		t.Fatalf("%+v", r)
	}
}

//go:build unix

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.temporal.io/sdk/client"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/coordination"
	"github.com/calvinchengx/long-horizon-agent/go/internal/durable"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

// The durable organization across implementations, against a real Temporal server: a Go-served
// org mission started and driven with the Python CLI, and the same scripted org mission (a
// researched, reviewed parallel wave then a serial round) served once by a Go worker and once by
// a Python worker leaving the same commits and anchor.

func stopTurn(s string) *string { return &s }

// orgWorkingModel writes the active item's work file, then is done (implementers and the Lead).
func orgWorkingModel(_ *config.Settings, snap contracts.SituationSnapshot) (contracts.ModelProvider, error) {
	return model.NewStub([]contracts.TurnResult{
		{ToolCalls: []contracts.ToolCall{{ID: "w1", Name: "write_file", Arguments: map[string]any{
			"path": "work/" + snap.ActiveItem.ID + ".txt", "content": "done"}}}, StopReason: stopTurn("tool_use")},
		{Text: `{"done": true, "summary": "ok"}`, StopReason: stopTurn("end_turn")},
	}), nil
}

func approvingReviewer(*config.Settings, contracts.SituationSnapshot) (contracts.ModelProvider, error) {
	return model.NewStub([]contracts.TurnResult{{Text: `{"done": true, "verdict": "approve", "blocking_issues": []}`}}), nil
}

func fakeResearcher(_ context.Context, inp durable.SubAgentInput) (durable.SubAgentOutput, error) {
	return durable.SubAgentOutput{Role: inp.RoleName, Brief: "BRIEF[" + inp.Objective + "]", Turns: 1}, nil
}

// startGoOrgWorker runs a Go worker in-process with acts completed from env.
func startGoOrgWorker(t *testing.T, env []string, acts *durable.Activities) client.Client {
	t.Helper()
	settings, err := config.LoadFrom(env, "")
	if err != nil {
		t.Fatal(err)
	}
	cl, err := durable.Dial(context.Background(), settings, nil)
	if err != nil {
		t.Fatal(err)
	}
	acts.Settings, acts.OpenToolbox = settings, openToolbox
	w := durable.NewWorker(cl, settings.TaskQueue, acts)
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Stop(); cl.Close() })
	return cl
}

func orgEvents(t *testing.T, workdir, kind string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(git(t, workdir, "show", "HEAD:.lha/events.ndjson"), "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) == nil && ev["kind"] == kind {
			out = append(out, ev)
		}
	}
	return out
}

// A mission started with the Python CLI (--research --review), served by a Go worker running the
// real sub-agent activity, and driven (status, approval) by the Python CLI.
func TestGoOrgMissionDrivenByThePythonCLI(t *testing.T) {
	addr := temporalAddress(t)
	dir := t.TempDir()
	env := durableEnv(t, addr, dir, "lha-it-go-org-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	marker := filepath.Join(dir, "marker.txt")
	cl := startGoOrgWorker(t, env, &durable.Activities{
		ModelFactory: gatedFactory(marker), ReviewerModel: approvingReviewer, // researchers: the real run_subagent (stub model)
	})
	roadmap, check := missionFiles(t, dir)
	ws := filepath.Join(dir, "ws")
	id := startedID(t, runPythonLHA(t, dir, processEnv(env...), "mission-start", "--checklist", roadmap,
		"--no-default-checks", "--check", "sh "+check, "--workdir", ws, "--deadlock-gate-hours", "0",
		"--research", "1", "--review"))
	waitStatus(t, cl, id, durable.StatusWaitingOnHuman)
	out := sameStatus(t, dir, env, id)
	if !strings.Contains(out, "gate: tool_call approval-") || !strings.Contains(out, "pending action: run_command") {
		t.Fatalf("status:\n%s", out)
	}
	if r := runPythonLHA(t, dir, processEnv(env...), "mission-approve", id, "--decision", "approve"); r.code != 0 {
		t.Fatalf("python approve: %+v", r)
	}
	res, err := missionResult(t, cl, id)
	if err != nil || !res.Completed || res.ItemsDone != 2 || res.Cycles != 2 {
		t.Fatalf("result %+v %v", res, err)
	}
	if data, _ := os.ReadFile(marker); strings.Count(string(data), "ran") != 1 {
		t.Fatalf("the approved action ran %q", data)
	}
	research, reviews := orgEvents(t, ws, "research"), orgEvents(t, ws, "review")
	if len(research) != 2 || len(reviews) != 2 {
		t.Fatalf("research %v\nreviews %v", research, reviews)
	}
	for _, r := range research { // the real sub-agent (the stub model) answered
		if p := r["payload"].(map[string]any); p["n"] != 1.0 || p["failed"] != 0.0 {
			t.Fatalf("research %v", r)
		}
	}
	sameStatus(t, dir, env, id) // the finished mission reads the same from both CLIs
}

const pythonOrgWorkerScript = `
import asyncio, sys
from temporalio import activity
from temporalio.service import RPCError, RPCStatusCode
from temporalio.worker import Worker
from lha.config import get_settings
from lha.contracts.model import ToolCall, TurnResult
from lha.durable.activities import (check_mission_health, declare_impossible, make_cycle_activity,
    notify_gate, read_mission_snapshot, record_mission_status, unblock_items)
from lha.durable.org_activities import make_org_activities
from lha.durable.subagent_workflow import SubAgentWorkflow
from lha.durable.types import SubAgentInput, SubAgentOutput
from lha.durable.worker import check_task_queue_pollers, connect_client, worker_identity
from lha.durable.workflows import MissionWorkflow
from lha.model.stub import StubModel

def working(settings, snap):
    return StubModel(script=[
        TurnResult(tool_calls=[ToolCall(id="w1", name="write_file", arguments={"path": f"work/{snap.active_item.id}.txt", "content": "done"})], stop_reason="tool_use"),
        TurnResult(text='{"done": true, "summary": "ok"}', stop_reason="end_turn"),
    ])

def reviewer(settings, snap):
    return StubModel(script=[TurnResult(text='{"done": true, "verdict": "approve", "blocking_issues": []}')])

@activity.defn(name="run_subagent")
async def researcher(inp: SubAgentInput) -> SubAgentOutput:
    return SubAgentOutput(role=inp.role_name, brief=f"BRIEF[{inp.objective}]", tool_calls=0, turns=1)

async def main():
    settings = get_settings()
    client = await connect_client(settings)
    try:
        await check_task_queue_pollers(client, settings.task_queue)
    except RPCError as exc:  # a test server without DescribeTaskQueue
        if exc.status != RPCStatusCode.UNIMPLEMENTED:
            raise
    worker = Worker(client, task_queue=settings.task_queue, identity=worker_identity(),
        workflows=[MissionWorkflow, SubAgentWorkflow],
        activities=[make_cycle_activity(model_factory=working),
                    *make_org_activities(implementer_factory=working, reviewer_factory=reviewer, lead_factory=working),
                    researcher, check_mission_health, notify_gate, declare_impossible, unblock_items,
                    read_mission_snapshot, record_mission_status])
    async with worker:
        print("ready", flush=True)
        await asyncio.Event().wait()

asyncio.run(main())
`

// startPythonOrgWorker runs a Python lha worker with the scripted org models until the test ends.
func startPythonOrgWorker(t *testing.T, dir string, env []string) {
	t.Helper()
	cmd := exec.Command("uv", "run", "--quiet", "--project", filepath.Join(repoRoot, "python"), "python", "-c", pythonOrgWorkerScript)
	cmd.Dir, cmd.Env = dir, processEnv(env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 5 * time.Second
	stdout, _ := cmd.StdoutPipe()
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "ready" {
				close(ready)
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(90 * time.Second):
		t.Fatalf("python worker did not start: %s", stderr.String())
	}
}

// initOrgWorkspace: three items; 01 and 02 each own work/<id>.txt (a wave), 03 owns nothing.
func initOrgWorkspace(t *testing.T, workdir string) {
	t.Helper()
	ownership := coordination.NewFileOwnershipMap()
	cl := contracts.Checklist{SchemaVersion: 1}
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("%02d", i)
		cl.Items = append(cl.Items, contracts.NewChecklistItem(id, fmt.Sprintf("task %d", i)))
		if i <= 2 {
			if err := ownership.Assign("work/"+id+".txt", coordination.WriterForItem(id)); err != nil {
				t.Fatal(err)
			}
		}
	}
	data, err := coordination.OwnershipJSON(ownership)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := state.NewGitMissionAnchor(workdir).InitializeSpecWithOwnership(context.Background(),
		contracts.MissionSpec{Title: "Org mission", Description: "durable organization"}, cl, data); err != nil {
		t.Fatal(err)
	}
}

// The same scripted org mission — a researched, reviewed wave (01, 02) then a serial round (03)
// — served by a Go worker and by a Python worker leaves the same commits (topological order) and
// the same anchor (events of a wave sorted, shas masked).
func TestGoAndPythonOrgMissionsLeaveTheSameAnchor(t *testing.T) {
	addr := temporalAddress(t)
	dir := t.TempDir()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	goEnv := durableEnv(t, addr, filepath.Join(dir, "go"), "lha-it-org-go-"+suffix)
	pyEnv := durableEnv(t, addr, filepath.Join(dir, "py"), "lha-it-org-py-"+suffix)
	cl := startGoOrgWorker(t, goEnv, &durable.Activities{
		ModelFactory: orgWorkingModel, ImplementerModel: orgWorkingModel, ReviewerModel: approvingReviewer,
		SubAgent: fakeResearcher,
	})
	startPythonOrgWorker(t, dir, pyEnv)
	check := filepath.Join(dir, "check.sh")
	_ = os.WriteFile(check, []byte(`done=$(grep -o '"status": *"done"' .lha/checklist.json | wc -l)
work=$(ls work/*.txt 2>/dev/null | wc -l)
[ "$work" -gt "$done" ]
`), 0o755)
	workspaces := map[string]string{}
	for i, side := range []struct{ name, queue string }{
		{"go", "lha-it-org-go-" + suffix}, {"py", "lha-it-org-py-" + suffix},
	} {
		ws := filepath.Join(dir, side.name, "ws")
		initOrgWorkspace(t, ws)
		id := fmt.Sprintf("mission_%012x", (time.Now().UnixNano()+int64(i))&0xffffffffffff)
		inp := durable.NewMissionInput(id, ws)
		inp.CheckCommands = [][]string{{"sh", check}}
		inp.MaxCycles, inp.ResearchPerItem, inp.Review, inp.MaxParallel = 20, 1, true, 2
		run, err := cl.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{
			ID: durable.MissionWorkflowID(id), TaskQueue: side.queue,
		}, durable.WorkflowMission, inp)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		var res durable.MissionResult
		err = run.Get(ctx, &res)
		cancel()
		if err != nil || !res.Completed || res.Cycles != 3 || res.ItemsDone != 3 {
			t.Fatalf("%s: result %+v %v", side.name, res, err)
		}
		workspaces[side.name] = ws
	}
	norm := func(w workspace) workspace {
		for f, body := range w.Anchor {
			w.Anchor[f] = shaRE.ReplaceAllString(body, "SHA")
		}
		return w
	}
	goWS, pyWS := norm(orgWorkspace(t, workspaces["go"], true)), norm(orgWorkspace(t, workspaces["py"], true))
	if !strings.Contains(strings.Join(goWS.Commits, "\n"), "[merged lha/implementer-02/c2]") {
		t.Fatalf("no wave: %q", goWS.Commits)
	}
	compareOrgWorkspaces(t, goWS, pyWS)
}

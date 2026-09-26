package durable

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/temporalproto"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// The replay safety net (python: tests/durability/test_replay.py + replay_test_harness): every
// recorded Go history in testdata/histories must replay against the current workflow code, so a
// change that would break a weeks-long in-flight mission fails here before it can be deployed.
// Go histories never replay Python histories (the SDKs number commands differently): these are
// histories recorded by the Go worker. A deliberate behaviour change is guarded with
// workflow.GetVersion and a new history is recorded next to the old ones:
//
//	LHA_IT_TEMPORAL_ADDRESS=<a Temporal server> LHA_RECORD_HISTORIES=1 go test ./internal/durable -run TestRecord
//
// (or with the temporal CLI on PATH instead of LHA_IT_TEMPORAL_ADDRESS).

const historiesDir = "testdata/histories"

func TestRecordedHistoriesStillReplay(t *testing.T) {
	dc, err := NewDataConverter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	n, err := ReplayHistories(historiesDir, dc, Logger(io.Discard, slog.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	if n < len(recordedScenarios) {
		t.Fatalf("only %d recorded histories (want one per scenario: %d)", n, len(recordedScenarios))
	}
}

// The recorded histories cover what they claim: timers (sleep / gate ladder / park), signals,
// Continue-As-New and a cancellation that waited for the cycle.
func TestCommittedHistoriesCoverWhatTheyClaim(t *testing.T) {
	want := map[string][]string{
		"spine":        {"ActivityTaskCompleted", "WorkflowExecutionCompleted"},
		"approval":     {"WorkflowExecutionSignaled", "TimerStarted", "TimerFired"},
		"sleeping":     {"TimerFired", "WorkflowExecutionSignaled"},
		"deadlock":     {"WorkflowExecutionSignaled", "TimerStarted"},
		"park":         {"ActivityTaskFailed", "TimerFired"},
		"can":          {"WorkflowExecutionContinuedAsNew"},
		"cancel_cycle": {"WorkflowExecutionCancelRequested", "ActivityTaskCancelRequested", "WorkflowExecutionCanceled"},
	}
	for name, events := range want {
		data, err := os.ReadFile(filepath.Join(historiesDir, name+".json"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		hist := &historypb.History{}
		if err := (temporalproto.CustomJSONUnmarshalOptions{DiscardUnknown: true}).Unmarshal(data, hist); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, ev := range hist.Events {
			seen[strings.TrimPrefix(ev.EventType.String(), "EVENT_TYPE_")] = true
		}
		for _, e := range events {
			if !seen[e] {
				t.Fatalf("%s.json has no %s event (%v)", name, e, seen)
			}
		}
		if name == "cancel_cycle" {
			// WaitForCancellation: the cycle acknowledged the cancellation before ABORTED was
			// written (record_mission_status is scheduled only after ActivityTaskCanceled).
			canceled, row := -1, -1
			for i, ev := range hist.Events {
				switch {
				case ev.EventType.String() == "ActivityTaskCanceled":
					canceled = i
				case ev.GetActivityTaskScheduledEventAttributes().GetActivityType().GetName() == ActivityRecordMissionStatus:
					row = i
				}
			}
			if canceled < 0 || row < canceled {
				t.Fatalf("the ABORTED row (%d) was not written after the cycle acknowledged (%d)", row, canceled)
			}
		}
	}
}

// A changed workflow does not replay a recorded history (the safety net catches it).
func TestReplayDetectsAChangedWorkflow(t *testing.T) {
	changed := func(ctx workflow.Context, inp MissionInput) (MissionResult, error) {
		// Skips the cycle activity the recorded history starts with.
		if err := workflow.Sleep(ctx, time.Second); err != nil {
			return MissionResult{}, err
		}
		return MissionResult{MissionID: inp.MissionID}, nil
	}
	dc, _ := NewDataConverter(t.TempDir())
	r, err := worker.NewWorkflowReplayerWithOptions(worker.WorkflowReplayerOptions{DataConverter: dc})
	if err != nil {
		t.Fatal(err)
	}
	r.RegisterWorkflowWithOptions(changed, workflow.RegisterOptions{Name: WorkflowMission})
	if err := r.ReplayWorkflowHistoryFromJSONFile(Logger(io.Discard, slog.LevelError), filepath.Join(historiesDir, "spine.json")); err == nil {
		t.Fatal("a changed workflow replayed the recorded history")
	}
}

// --- recording (needs a Temporal server) ----------------------------------------------------

func temporalServer(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("needs a Temporal server")
	}
	if addr := os.Getenv("LHA_IT_TEMPORAL_ADDRESS"); addr != "" {
		return addr
	}
	bin, err := exec.LookPath("temporal")
	if err != nil {
		t.Skip("no Temporal server: set LHA_IT_TEMPORAL_ADDRESS or put the temporal CLI on PATH")
	}
	port := freeTCPPort(t)
	cmd := exec.Command(bin, "server", "start-dev", "--headless", "--ip", "127.0.0.1", "--port", strconv.Itoa(port),
		"--http-port", strconv.Itoa(freeTCPPort(t)), "--metrics-port", strconv.Itoa(freeTCPPort(t)), "--log-level", "error")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(60 * time.Second)
	for {
		cl, err := client.Dial(client.Options{HostPort: addr, Logger: Logger(io.Discard, slog.LevelError)})
		if err == nil {
			_, err = cl.CheckHealth(context.Background(), &client.CheckHealthRequest{})
			cl.Close()
			if err == nil {
				return addr
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("temporal dev server did not start: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

type scenario struct {
	name  string
	items int
	model ModelFactory
	setup func(inp *MissionInput)
	// drive runs while the mission is live (signals, cancellation); nil = just wait.
	drive func(t *testing.T, cl client.Client, run client.WorkflowRun)
	// cycle replaces run_agent_cycle (nil = the real one).
	cycle func(acts *Activities) any
	// health replaces check_mission_health (nil = the real one).
	health any
}

func waitQuery(t *testing.T, cl client.Client, wid, name, want string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if v, err := cl.QueryWorkflow(context.Background(), wid, "", name); err == nil {
			var got string
			if v.Get(&got) == nil && got == want {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s never answered %s", name, want)
}

var recordedScenarios = []scenario{
	{name: "spine", items: 2, model: workingModel},
	{name: "approval", items: 2, setup: func(inp *MissionInput) {
		inp.ApprovalTimeoutSeconds = 5
		inp.GateEscalationSeconds = []int{1}
	}, drive: func(t *testing.T, cl client.Client, run client.WorkflowRun) {
		// Gate 1: approved after its reminder; gate 2 (item 02's own push) defaults to reject.
		waitQuery(t, cl, run.GetID(), QueryStatus, StatusWaitingOnHuman)
		time.Sleep(1500 * time.Millisecond)
		if err := cl.SignalWorkflow(context.Background(), run.GetID(), "", SignalHumanDecision, "approve"); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "sleeping", items: 2, model: workingModel, setup: func(inp *MissionInput) {
		inp.CyclePauseSeconds = 1
		inp.ResumeAt = epoch(time.Now()) + 3600
	}, drive: func(t *testing.T, cl client.Client, run client.WorkflowRun) {
		waitQuery(t, cl, run.GetID(), QueryStatus, StatusSleeping)
		if err := cl.SignalWorkflow(context.Background(), run.GetID(), "", SignalSteer, "keep it small"); err != nil {
			t.Fatal(err)
		}
		if err := cl.SignalWorkflow(context.Background(), run.GetID(), "", SignalSnooze, 0); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "deadlock", items: 1, model: idleModel, setup: func(inp *MissionInput) {
		inp.DeadlockGateSeconds = 3600
	}, drive: func(t *testing.T, cl client.Client, run client.WorkflowRun) {
		waitQuery(t, cl, run.GetID(), QueryStatus, StatusWaitingOnHuman)
		time.Sleep(time.Second) // the gate is waiting on its first reminder's timer
		if err := cl.SignalWorkflow(context.Background(), run.GetID(), "", SignalHumanDecision, "impossible"); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "park", items: 1, model: workingModel, setup: func(inp *MissionInput) {
		inp.ParkInitialSeconds = 1
		inp.ParkMaxSeconds = 2
	}, cycle: func(acts *Activities) any {
		var failures atomic.Int32
		return func(ctx context.Context, in CycleInput) (CycleResult, error) {
			if failures.Add(1) <= 5 {
				return CycleResult{}, errors.New("model endpoint unreachable")
			}
			return acts.RunAgentCycle(ctx, in)
		}
	}},
	{name: "can", items: 2, model: workingModel, setup: func(inp *MissionInput) { inp.CyclesBeforeCAN = 1 }},
	{name: "cancel_cycle", items: 1, model: workingModel, cycle: func(acts *Activities) any {
		return func(ctx context.Context, in CycleInput) (CycleResult, error) {
			for { // heartbeats until the cancellation reaches it, then acknowledges it
				heartbeat(ctx, in.CycleID)
				select {
				case <-ctx.Done():
					return CycleResult{}, ctx.Err()
				case <-time.After(200 * time.Millisecond):
				}
			}
		}
	}, drive: func(t *testing.T, cl client.Client, run client.WorkflowRun) {
		time.Sleep(time.Second)
		if err := cl.CancelWorkflow(context.Background(), run.GetID(), ""); err != nil {
			t.Fatal(err)
		}
	}},
}

// TestRecordHistories runs every scenario on a real server with the Go worker, replays each fresh
// history, and (LHA_RECORD_HISTORIES=1) stores them in testdata/histories.
func TestRecordHistories(t *testing.T) {
	addr := temporalServer(t)
	dc, err := NewDataConverter(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cl, err := client.Dial(client.Options{HostPort: addr, DataConverter: dc, Logger: Logger(io.Discard, slog.LevelError)})
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	out := t.TempDir()
	record := os.Getenv("LHA_RECORD_HISTORIES") == "1"
	for _, sc := range recordedScenarios {
		t.Run(sc.name, func(t *testing.T) {
			queue := fmt.Sprintf("lha-record-%s-%d", sc.name, time.Now().UnixNano())
			factory := sc.model
			marker := filepath.Join(t.TempDir(), "marker")
			if factory == nil {
				factory = gatedModel(gatedArgv(marker))
			}
			acts := newActs(t, factory, nil)
			w := worker.New(cl, queue, worker.Options{Identity: WorkerIdentity(), MaxHeartbeatThrottleInterval: 200 * time.Millisecond})
			overrides := map[string]any{}
			if sc.cycle != nil {
				overrides[ActivityRunAgentCycle] = sc.cycle(acts)
			}
			registerWith(w, acts, overrides)
			if err := w.Start(); err != nil {
				t.Fatal(err)
			}
			defer w.Stop()
			inp := initMission(t, sc.items)
			inp.MissionID = sc.name
			if sc.setup != nil {
				sc.setup(&inp)
			}
			wid := fmt.Sprintf("mission:%s-%d", sc.name, time.Now().UnixNano())
			run, err := cl.ExecuteWorkflow(context.Background(), client.StartWorkflowOptions{ID: wid, TaskQueue: queue}, WorkflowMission, inp)
			if err != nil {
				t.Fatal(err)
			}
			runID := run.GetRunID() // the first run (Get follows Continue-As-New)
			if sc.drive != nil {
				sc.drive(t, cl, run)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			var res MissionResult
			if err := run.Get(ctx, &res); err != nil && sc.name != "cancel_cycle" && sc.name != "can" {
				t.Fatalf("mission: %v", err)
			}
			hist := &historypb.History{}
			iter := cl.GetWorkflowHistory(context.Background(), wid, runID, false, 0)
			for iter.HasNext() {
				ev, err := iter.Next()
				if err != nil {
					t.Fatal(err)
				}
				hist.Events = append(hist.Events, ev)
			}
			data, err := temporalproto.CustomJSONMarshalOptions{Indent: "  "}.Marshal(hist)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(out, sc.name+".json")
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			r, _ := NewReplayer(dc)
			if err := r.ReplayWorkflowHistoryFromJSONFile(Logger(io.Discard, slog.LevelError), path); err != nil {
				t.Fatalf("the fresh history does not replay: %v", err)
			}
			if record {
				if err := os.WriteFile(filepath.Join(historiesDir, sc.name+".json"), data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

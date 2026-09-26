package durable

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"
)

// The missions row the workflow owns (python: tests/durability/test_mission_row.py): SLEEPING,
// WAITING_ON_HUMAN, the outcome, and ABORTED on a cancellation wherever it lands.

func last(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[len(s)-1]
}

func TestRowWaitsOnTheDeadlockGateThenRecordsTheOutcome(t *testing.T) {
	inp := initMission(t, 1)
	inp.DeadlockGateSeconds = 60
	store := &recordingStore{}
	env := newEnv(t, newActs(t, idleModel, store), nil)
	var during []string
	env.RegisterDelayedCallback(func() { during = store.Statuses() }, 30*time.Second)
	if _, err := env.run(inp); err != nil {
		t.Fatal(err)
	}
	if last(during) != StatusWaitingOnHuman || last(store.Statuses()) != StatusAborted {
		t.Fatalf("during %v after %v", during, store.Statuses())
	}
}

func TestCancellingASleepingMissionRecordsAborted(t *testing.T) {
	inp := initMission(t, 1)
	inp.ResumeAt = epoch(time.Now()) + 7*24*3600
	store := &recordingStore{}
	env := newEnv(t, newActs(t, workingModel, store), nil)
	env.RegisterDelayedCallback(env.CancelWorkflow, time.Hour)
	_, err := env.run(inp)
	if !temporal.IsCanceledError(err) {
		t.Fatalf("err %v", err)
	}
	if s := store.Statuses(); len(s) != 2 || s[0] != StatusSleeping || s[1] != StatusAborted {
		t.Fatalf("row %v", s)
	}
}

func TestCancellingAtAnOpenGateRecordsAborted(t *testing.T) {
	inp := initMission(t, 1)
	inp.DeadlockGateSeconds = 3600
	store := &recordingStore{}
	env := newEnv(t, newActs(t, idleModel, store), nil)
	env.RegisterDelayedCallback(env.CancelWorkflow, time.Minute)
	_, err := env.run(inp)
	if !temporal.IsCanceledError(err) {
		t.Fatalf("err %v", err)
	}
	if last(store.Statuses()) != StatusAborted {
		t.Fatalf("row %v", store.Statuses())
	}
}

// A store that fails every write never fails (or blocks) the mission.
func TestAFailingRowWriteNeverFailsTheMission(t *testing.T) {
	inp := initMission(t, 2)
	inp.CyclePauseSeconds = 60
	store := &recordingStore{failRows: true}
	env := newEnv(t, newActs(t, workingModel, store), nil)
	res, err := env.run(inp)
	if err != nil || !res.Completed {
		t.Fatalf("result %+v err %v", res, err)
	}
}

// A cancellation that lands while a cycle runs ends ABORTED and never parks.
func TestCancellingDuringACycleRecordsAbortedAndNeverParks(t *testing.T) {
	inp := initMission(t, 1)
	store := &recordingStore{}
	var env *testEnv
	var started atomic.Bool
	blocking := func(ctx context.Context, in CycleInput) (CycleResult, error) {
		if !started.Swap(true) {
			env.CancelWorkflow() // the operator's `lha mission-abort` lands mid-cycle
		}
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Second):
		}
		return CycleResult{}, ctx.Err()
	}
	env = newEnv(t, newActs(t, workingModel, store), map[string]any{ActivityRunAgentCycle: blocking})
	_, err := env.run(inp)
	if !temporal.IsCanceledError(err) {
		t.Fatalf("err %v", err)
	}
	for _, s := range store.Statuses() {
		if s == StatusDegradedPark {
			t.Fatalf("parked on a cancellation: %v", store.Statuses())
		}
	}
	if last(store.Statuses()) != StatusAborted {
		t.Fatalf("row %v", store.Statuses())
	}
}

package durable

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Operator checklist edits on a running mission (python: tests/durability/test_checklist_edit.py).

// item2NeverWorks does item 01, and claims 02 done without any work (so it blocks).
func item2NeverWorks(s *config.Settings, snap contracts.SituationSnapshot) (contracts.ModelProvider, error) {
	if snap.ActiveItem != nil && snap.ActiveItem.ID == "02" {
		return idleModel(s, snap)
	}
	return workingModel(s, snap)
}

func editBatch(by string, edits ...map[string]any) map[string]any {
	list := make([]any, len(edits))
	for i, e := range edits {
		list[i] = e
	}
	return map[string]any{"edits": list, "by": by}
}

// 1. a batch sent while the deadlock gate is open lands at "retry", before the blocked items are
// reset: removing the item that can never pass and adding one that can completes the mission.
func TestEditAtTheDeadlockGateResolvesIt(t *testing.T) {
	inp := initMission(t, 2)
	inp.DeadlockGateSeconds = 3600
	env := newEnv(t, newActs(t, item2NeverWorks, nil), nil)
	var pending int
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalChecklistEdit, editBatch("calvin",
			map[string]any{"op": "remove", "id": "02"},
			map[string]any{"op": "add", "description": "task 3", "depends_on": []any{"01"}},
		))
	}, time.Minute)
	env.RegisterDelayedCallback(func() {
		env.query(t, QueryPendingEdits, &pending) // held while the gate is open
		env.SignalWorkflow(SignalHumanDecision, "retry")
	}, 2*time.Minute)
	res, err := env.run(inp)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed || res.ItemsDone != 2 || res.ItemsTotal != 2 {
		var log []string
		env.query(t, QueryGateLog, &log)
		t.Fatalf("result %+v\ngate log:\n%s", res, strings.Join(log, "\n"))
	}
	if pending != 1 {
		t.Fatalf("pending edits at the gate: %d", pending)
	}
	if commitsWith(t, inp.Workdir, "lha: checklist edited by calvin") != 1 || commitsWith(t, inp.Workdir, "lha: unblock") != 0 {
		t.Fatal("expected one edit commit and no unblock commit")
	}
	var log []string
	env.query(t, QueryGateLog, &log)
	if !strings.Contains(strings.Join(log, "\n"), "checklist edited by calvin: removed 02; added 03") {
		t.Fatalf("gate log %v", log)
	}
	events := committedEvents(t, inp.Workdir)
	n := 0
	for _, e := range events {
		if e.Kind == EditEvent {
			n++
			if e.CycleID != "e1" {
				t.Fatalf("edit event %+v", e)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d edit events", n)
	}
}

// 2. a batch sent while the mission SLEEPS is applied at once (the sleep goes on); a refused
// batch is reported in the gate log without touching the anchor.
func TestEditWhileSleepingAndARefusedBatch(t *testing.T) {
	inp := initMission(t, 2)
	inp.CyclePauseSeconds = 3600
	env := newEnv(t, newActs(t, workingModel, nil), nil)
	var status string
	var pending int
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalChecklistEdit, editBatch("", map[string]any{"op": "remove", "id": "99"}))
		env.SignalWorkflow(SignalChecklistEdit, editBatch("ops", map[string]any{"op": "add", "description": "task 3"}))
	}, time.Minute)
	env.RegisterDelayedCallback(func() {
		env.query(t, QueryStatus, &status)
		env.query(t, QueryPendingEdits, &pending)
		env.SignalWorkflow(SignalSnooze, float64(0))
	}, 2*time.Minute)
	res, err := env.run(inp)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed || res.ItemsDone != 3 || res.ItemsTotal != 3 {
		t.Fatalf("result %+v", res)
	}
	if status != StatusSleeping || pending != 0 {
		t.Fatalf("after the edits: status %s, pending %d", status, pending)
	}
	if commitsWith(t, inp.Workdir, "lha: checklist edited") != 1 {
		t.Fatal("expected exactly one edit commit")
	}
	var log []string
	env.query(t, QueryGateLog, &log)
	var order []string
	refused := 0
	for _, line := range log {
		for _, kind := range []string{"sleeping until", "checklist edit refused", "checklist edited by ops", "woke up"} {
			if strings.Contains(line, kind) {
				order = append(order, kind)
			}
		}
		if strings.Contains(line, "checklist edit refused") {
			refused++
			if !strings.HasSuffix(line, "edit #1: unknown item '99'") {
				t.Fatalf("refusal %q", line)
			}
		}
	}
	want := "sleeping until,checklist edit refused,checklist edited by ops,woke up"
	if refused != 1 || !strings.HasPrefix(strings.Join(order, ","), want) {
		t.Fatalf("gate log %v", log)
	}
}

// 3. a batch sent before the first cycle is applied before it.
func TestEarlyEditLandsBeforeTheFirstCycle(t *testing.T) {
	inp := initMission(t, 2)
	env := newEnv(t, newActs(t, item2NeverWorks, nil), nil)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(SignalChecklistEdit, editBatch("", map[string]any{"op": "remove", "id": "02"}))
	}, 0)
	res, err := env.run(inp)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed || res.Cycles != 1 || res.ItemsDone != 1 || res.ItemsTotal != 1 {
		t.Fatalf("result %+v", res)
	}
}

// The activity: one anchor-only commit, idempotent for the same (id, batch), a refusal commits
// nothing (python: test_checklist_edit.py).
func TestEditChecklistActivity(t *testing.T) {
	inp := initMission(t, 2)
	acts := newActs(t, nil, nil)
	ctx := context.Background()
	edit := EditInput{MissionID: inp.MissionID, Workdir: inp.Workdir, CycleID: "e1", By: "calvin", Edits: []map[string]any{
		{"op": "add", "description": "three (witness: cmd:true)"}, {"op": "remove", "id": "02"},
	}}
	first, err := acts.EditChecklist(ctx, edit)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Advanced || first.Note != "checklist edited by calvin: added 03; removed 02" || first.ItemsTotal != 2 {
		t.Fatalf("result %+v", first)
	}
	again, err := acts.EditChecklist(ctx, edit)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Advanced || again.Note != first.Note || commitsWith(t, inp.Workdir, "lha: checklist edited by calvin") != 1 {
		t.Fatalf("retry applied again: %+v", again)
	}
	other, err := acts.EditChecklist(ctx, EditInput{Workdir: inp.Workdir, CycleID: "e1", Edits: []map[string]any{{"op": "remove", "id": "03"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !other.Advanced || other.Note != "checklist edited by an operator: removed 03" {
		t.Fatalf("result %+v", other)
	}
	before := commitsWith(t, inp.Workdir, "lha: checklist edited")
	refused, err := acts.EditChecklist(ctx, EditInput{Workdir: inp.Workdir, CycleID: "e2", Edits: []map[string]any{{"op": "remove", "id": "9"}}})
	if err != nil {
		t.Fatal(err)
	}
	if refused.Advanced || refused.Note != "checklist edit refused: edit #1: unknown item '9'" || commitsWith(t, inp.Workdir, "lha: checklist edited") != before {
		t.Fatalf("result %+v", refused)
	}
}

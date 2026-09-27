package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agents"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/systemone"
)

// Ported from python/tests/unit/test_system_one.py: stall triage through the REAL loop, verifier
// and git anchor, with a stub System One model.

func causeStub(defect, scope, environment float64) *systemone.Stub {
	return &systemone.Stub{Respond: func(any, []systemone.Named) map[string]systemone.Answer {
		return map[string]systemone.Answer{systemone.TriageQuestionID: systemone.ChoiceAnswer(
			[]string{"defect", "scope", "environment"}, []float64{defect, scope, environment})}
	}}
}

func runTriaged(t *testing.T, stub *systemone.Stub, replan ...contracts.TurnResult) (*fixture, []CycleOutcome) {
	t.Helper()
	f := setup(t, withWitnesses(item("01", "coarse"), "cmd:exit 1"), contracts.NewChecklistItem("02", "after", "01"))
	l := f.loop(model.NewStub([]contracts.TurnResult{done, done}), func(o *LoopOptions) {
		o.MaxTurns, o.MaxConsecutiveFailures, o.MaxReplans = 2, 5, 5
		o.Replanner = agents.NewReplanner(model.NewStub(replan))
	})
	l.SetTriage(systemone.NewStallTriage(stub))
	return f, []CycleOutcome{f.run(t, l, "c1", passCheck), f.run(t, l, "c2", passCheck)}
}

func systemOneEvents(t *testing.T, dir string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, ".lha", "events.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	out := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		if e["kind"] == "system_one" {
			out = append(out, e["payload"].(map[string]any))
		}
	}
	return out
}

func TestConfidentScopeSplitsTheItemBeforeTheFailureLimit(t *testing.T) {
	stub := causeStub(0.01, 0.98, 0.01)
	f, outs := runTriaged(t, stub, text(`[{"description": "part A"}, {"description": "part B"}]`))
	if outs[0].ItemSplit || stub.Calls() != 1 || !outs[1].ItemSplit {
		t.Fatalf("outcomes %+v, calls %d", outs, stub.Calls())
	}
	cl, _ := f.anchor.ReadChecklist(context.Background())
	if cl.Get("01").Status != "split" || cl.Get("01.2").Witnesses[0] != "cmd:exit 1" {
		t.Fatalf("checklist: %+v", cl.Items)
	}
	events := systemOneEvents(t, f.dir)
	if len(events) != 1 || events[0]["action"] != "split" || events[0]["answer"] != "scope" ||
		events[0]["use"] != "stall_triage" || events[0]["model"] != "stub-1" {
		t.Fatalf("events: %v", events)
	}
}

func TestConfidentEnvironmentBlocksTheItemWithoutSplitting(t *testing.T) {
	f, outs := runTriaged(t, causeStub(0.01, 0.01, 0.98))
	if !outs[1].ItemBlocked || outs[1].ItemSplit {
		t.Fatalf("outcome: %+v", outs[1])
	}
	cl, _ := f.anchor.ReadChecklist(context.Background())
	it := cl.Get("01")
	if it.ConsecutiveFailures != 2 || !strings.HasPrefix(it.LastFailure, "Blocked before the failure limit") {
		t.Fatalf("item: %+v", it)
	}
}

func TestOtherTriageAnswersAndFailuresChangeNothing(t *testing.T) {
	for name, stub := range map[string]*systemone.Stub{
		"defect":      causeStub(0.98, 0.01, 0.01),
		"unconfident": causeStub(0.3, 0.4, 0.3),
		"unavailable": {Err: "system one call failed: timeout"},
	} {
		t.Run(name, func(t *testing.T) {
			f, outs := runTriaged(t, stub)
			if outs[1].ItemBlocked || outs[1].ItemSplit {
				t.Fatalf("outcome: %+v", outs[1])
			}
			cl, _ := f.anchor.ReadChecklist(context.Background())
			if cl.Get("01").Status != "in_progress" {
				t.Fatalf("status %s", cl.Get("01").Status)
			}
			if ev := systemOneEvents(t, f.dir); len(ev) != 1 || ev[0]["action"] != "continue" {
				t.Fatalf("events: %v", ev)
			}
		})
	}
}

func TestASplitTheReplannerCannotMakeKeepsTheItemGoing(t *testing.T) {
	f, outs := runTriaged(t, causeStub(0.01, 0.98, 0.01), text("no idea"))
	if outs[1].ItemBlocked || outs[1].ItemSplit {
		t.Fatalf("outcome: %+v", outs[1])
	}
	cl, _ := f.anchor.ReadChecklist(context.Background())
	if cl.Get("01").Status != "in_progress" {
		t.Fatalf("status %s", cl.Get("01").Status)
	}
}

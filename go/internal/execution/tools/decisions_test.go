package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution"
	"github.com/calvinchengx/long-horizon-agent/go/internal/state"
)

type innerDispatcher struct {
	events []contracts.EventRecord
	calls  []string
}

func (d *innerDispatcher) Specs() []contracts.ToolSpec {
	return []contracts.ToolSpec{ReadFileTool{}.Spec()}
}

func (d *innerDispatcher) Dispatch(_ context.Context, c contracts.ToolCall, _ contracts.ToolContext) contracts.ToolResult {
	d.calls = append(d.calls, c.Name)
	return contracts.Success("ok")
}

func (d *innerDispatcher) DrainEvents() []contracts.EventRecord {
	out := d.events
	d.events = nil
	return out
}

func TestRecordDecisionTool(t *testing.T) {
	buf := &DecisionBuffer{}
	inner := &innerDispatcher{events: []contracts.EventRecord{{Kind: "tool_approval", CycleID: "c1", Payload: contracts.Payload("x", 1)}}}
	d := WithDecisionTool(inner, buf)
	if WithDecisionTool(d, buf) != d {
		t.Fatal("wrapped twice")
	}
	specs := d.Specs()
	if len(specs) != 2 || specs[1].Name != RecordDecision || !specs[1].Mutating {
		t.Fatal(specs)
	}
	tc := contracts.ToolContext{MissionID: "m"}
	ctx := context.Background()
	if r := d.Dispatch(ctx, call("1", "read_file", map[string]any{"path": "x"}), tc); !r.OK || inner.calls[0] != "read_file" {
		t.Fatal(r)
	}
	r := d.Dispatch(ctx, call("2", RecordDecision, map[string]any{
		"decision": "  Use JSON  ", "rationale": "simple", "alternatives_rejected": " yaml ",
		"affected": []any{" a.py ", "", "  ", strings.Repeat("p", 400)},
	}), tc)
	if !r.OK || r.Content != "recorded decision (1 this cycle); it is committed with this cycle's checkpoint" {
		t.Fatal(r)
	}
	rec := buf.Records[0]
	if rec.Decision != "Use JSON" || rec.AlternativesRejected != "yaml" || len(rec.Affected) != 2 ||
		rec.Affected[0] != "a.py" || len(rec.Affected[1]) != 300 {
		t.Fatal(rec)
	}
	for _, c := range []struct {
		args map[string]any
		want string
	}{
		{map[string]any{"decision": "x"}, "invalid args for 'record_decision': unexpected argument 'extra'"},
		{map[string]any{"decision": " ", "rationale": "r"}, "record_decision needs a non-empty decision and rationale"},
		{map[string]any{"decision": strings.Repeat("d", 501), "rationale": "r"},
			"decision is too long (501 > 500 chars); state it briefly and put the detail in the rationale"},
		{map[string]any{"decision": "d", "rationale": 3.0}, "invalid args for 'record_decision': rationale must be of type string, got int"},
	} {
		if c.want == "invalid args for 'record_decision': unexpected argument 'extra'" {
			c.args["extra"] = true
		}
		if r := d.Dispatch(ctx, call("3", RecordDecision, c.args), tc); r.OK || r.ErrorText() != c.want {
			t.Errorf("%v: %q", c.args, r.ErrorText())
		}
	}
	drainer := d.(contracts.EventDrainer)
	if ev := drainer.DrainEvents(); len(ev) != 1 || ev[0].Kind != "tool_approval" {
		t.Fatal(ev)
	}
	if ev := drainer.DrainEvents(); len(ev) != 0 {
		t.Fatal(ev)
	}
}

func TestAnchorIsADecisionSink(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	anchor := state.NewGitMissionAnchor(dir)
	if _, err := anchor.Initialize(ctx, "t", "d", contracts.Checklist{Items: []contracts.ChecklistItem{contracts.NewChecklistItem("a", "A")}}); err != nil {
		t.Fatal(err)
	}
	var sink DecisionSink = anchor
	d := WithDecisionTool(forTools(t, DefaultLocalTools(), execution.DispatcherOptions{AllowMutating: true}), sink)
	r := d.Dispatch(ctx, call("1", RecordDecision, map[string]any{"decision": "Use JSON", "rationale": "simple"}), tctx(t, dir))
	if !r.OK || len(anchor.PendingDecisions()) != 1 {
		t.Fatal(r)
	}
	checklist, _ := anchor.ReadChecklist(ctx)
	if _, err := anchor.CommitCheckpoint(ctx, contracts.Checkpoint{CycleID: "c1", Checklist: checklist}); err != nil {
		t.Fatal(err)
	}
	if len(anchor.PendingDecisions()) != 0 {
		t.Fatal("pending not cleared")
	}
	decisions, err := anchor.ReadDecisions(ctx)
	if err != nil || len(decisions) != 1 || decisions[0].Decision != "Use JSON" || decisions[0].CycleID != "c1" {
		t.Fatal(decisions, err)
	}
	if v, err := anchor.VerifyDecisions(ctx); err != nil || !v.OK {
		t.Fatal(v, err)
	}
}

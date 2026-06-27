package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

func stubMsgs() []contracts.ModelMessage {
	return []contracts.ModelMessage{
		{Role: "system", Content: "you are a coding agent"},
		user("add a function"),
	}
}

func TestStubNameIsUnmistakable(t *testing.T) {
	if !strings.HasPrefix(NewStubNamed("foo", nil).Name(), "stub:") || NewStub(nil).Name() != "stub:stub-1" {
		t.Error("stub name")
	}
}

func TestStubIsDeterministic(t *testing.T) {
	a, _ := NewStub(nil).Complete(ctx, stubMsgs(), nil, 0)
	b, _ := NewStub(nil).Complete(ctx, stubMsgs(), nil, 0)
	if a.Text != b.Text || a.Usage.InputTokens != b.Usage.InputTokens || a.Usage.InputTokens <= 0 {
		t.Errorf("a=%+v b=%+v", a, b)
	}
}

// The exact TurnResult Python's StubModel returns (python: StubModel().complete(...) dumped).
func TestStubMatchesPythonOutput(t *testing.T) {
	msgs := []contracts.ModelMessage{
		{Role: "system", Content: "you are a coding agent \u00fc"},
		user("add a function"),
	}
	r, err := NewStub(nil).Complete(ctx, msgs, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(r)
	want := `{"text":"[stub:fe59dea9] acknowledged 2 message(s).","thinking":"deterministic stub reasoning for digest fe59dea9","tool_calls":[],"usage":{"input_tokens":13,"output_tokens":10,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"cache_creation_1h_input_tokens":0,"model":"stub:stub-1","provider":"","reported_cost_usd":null},"stop_reason":"end_turn","session_id":null}`
	if string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestStubCostIsZero(t *testing.T) {
	m := NewStub(nil)
	r, _ := m.Complete(ctx, stubMsgs(), nil, 0)
	if c, err := m.EstimateCostUSD(r.Usage); c != 0 || err != nil {
		t.Errorf("cost = %v, %v", c, err)
	}
}

func TestStubScriptDrivesTurnsInOrder(t *testing.T) {
	script := []contracts.TurnResult{
		{Text: "first", Usage: contracts.Usage{OutputTokens: 1}},
		{Text: "second", Usage: contracts.Usage{OutputTokens: 1}, ToolCalls: []contracts.ToolCall{
			{ID: "1", Name: "x", Arguments: map[string]any{"k": []any{map[string]any{"a": 1}}}},
		}, StopReason: contracts.Str("end")},
	}
	m := NewStub(script)
	script[0].Text = "mutated after construction"
	for i, want := range []string{"first", "second", "second"} { // cycles on the last entry
		r, err := m.Complete(ctx, stubMsgs(), nil, 0)
		if err != nil || r.Text != want {
			t.Fatalf("turn %d = %q, %v", i, r.Text, err)
		}
		if i == 1 {
			// Deep copies: mutating a returned turn never changes the script.
			r.ToolCalls[0].Arguments["k"].([]any)[0].(map[string]any)["a"] = 2
			*r.StopReason = "changed"
		}
		if i == 2 {
			if r.ToolCalls[0].Arguments["k"].([]any)[0].(map[string]any)["a"] != 1 || *r.StopReason != "end" {
				t.Errorf("script was mutated through a returned turn: %+v", r)
			}
		}
	}
	if m.Turns() != 3 {
		t.Errorf("turns = %d", m.Turns())
	}
	if r, _ := NewStub([]contracts.TurnResult{}).Complete(ctx, stubMsgs(), nil, 0); !strings.HasPrefix(r.Text, "[stub:") {
		t.Errorf("an empty script means echo mode: %q", r.Text)
	}
}

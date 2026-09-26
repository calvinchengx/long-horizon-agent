package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

type innerOnly struct{}

func (innerOnly) Specs() []contracts.ToolSpec { return []contracts.ToolSpec{} }
func (innerOnly) Dispatch(_ context.Context, call contracts.ToolCall, _ contracts.ToolContext) contracts.ToolResult {
	return contracts.Success("inner:" + call.Name)
}

// Port of test_request_lease_tool (python/tests/unit/test_leases_and_resume.py).
func TestRequestLeaseTool(t *testing.T) {
	var calls [][2]string
	handler := func(_ context.Context, path, reason string) (bool, string, error) {
		calls = append(calls, [2]string{path, reason})
		if path == "boom" {
			return false, "", errors.New("anchor unavailable")
		}
		return path == "ok.py", "decided " + path, nil
	}
	dispatcher := WithLeaseTool(innerOnly{}, handler)
	if specs := dispatcher.Specs(); len(specs) != 1 || specs[0].Name != RequestLease || !specs[0].Mutating {
		t.Fatalf("%+v", specs)
	}
	if WithLeaseTool(dispatcher, handler) != dispatcher {
		t.Fatal("wrapped twice")
	}
	call := func(args map[string]any, name string) contracts.ToolResult {
		return dispatcher.Dispatch(context.Background(), contracts.ToolCall{ID: "1", Name: name, Arguments: args}, contracts.ToolContext{})
	}
	if !call(map[string]any{"path": "ok.py", "reason": "r"}, RequestLease).OK {
		t.Fatal("granted")
	}
	if r := call(map[string]any{"path": "no.py", "reason": "r"}, RequestLease); r.OK || r.ErrorText() != "decided no.py" {
		t.Fatalf("%+v", r)
	}
	if call(map[string]any{"path": "x"}, RequestLease).OK {
		t.Fatal("no reason")
	}
	if r := call(map[string]any{"path": 3, "reason": "r"}, RequestLease); !strings.Contains(r.ErrorText(), "invalid args") {
		t.Fatal(r.ErrorText())
	}
	if r := call(map[string]any{"path": " ", "reason": "r"}, RequestLease); !strings.Contains(r.ErrorText(), "non-empty") {
		t.Fatal(r.ErrorText())
	}
	if r := call(map[string]any{"path": "boom", "reason": "r"}, RequestLease); !strings.Contains(r.ErrorText(), "anchor unavailable") ||
		!strings.HasPrefix(r.ErrorText(), "lease request failed: ") {
		t.Fatal(r.ErrorText())
	}
	if r := call(map[string]any{}, "read_file"); r.Content != "inner:read_file" {
		t.Fatal(r.Content)
	}
	if len(calls) != 3 || calls[0] != [2]string{"ok.py", "r"} || calls[2] != [2]string{"boom", "r"} {
		t.Fatalf("%v", calls)
	}
}

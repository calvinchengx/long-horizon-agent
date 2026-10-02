package org

import (
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/coordination"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
)

// Shared fakes of the org tests (python: tests/unit/test_ownership_integration.py helpers). The
// models are offline fakes that act on what the orchestrator actually sends them; the tools,
// worktrees, merges, verification and checkpoints are all real.

var (
	pass     = contracts.Check{Name: "always_green", Command: []string{"true"}, Gating: true, Where: "sandbox"}
	doneTurn = contracts.TurnResult{Text: `{"done": true, "summary": "done"}`}
	approve  = contracts.TurnResult{Text: `{"done": true, "verdict": "approve", "blocking_issues": []}`}
	writeSet = regexp.MustCompile(`Your write-set \(the ONLY files you may create or modify\): (.*)`)
	itemIDRE = regexp.MustCompile(`Checklist item \[([\w.]+)\]`)
)

func settingsFor(t *testing.T, env ...string) *config.Settings {
	t.Helper()
	// Each test gets its own mission store: the parallel tests of this package must not contend
	// for one file (and never touch the developer's per-user store).
	base := []string{"LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true", "LHA_MODEL_BACKEND=stub",
		"LHA_BUDGET_USD_CEILING=100", "LHA_MAX_CYCLES=20",
		"LHA_SQLITE_PATH=" + filepath.Join(t.TempDir(), "store", "lha.sqlite3")}
	s, err := config.LoadFrom(append(base, env...), "")
	if err != nil {
		t.Fatal(err)
	}
	obs.ConfigureLogging(nopWriter{}, false)
	return s
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func stub(turns ...contracts.TurnResult) *model.StubModel { return model.NewStub(turns) }

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func twoItems() (contracts.Checklist, *coordination.FileOwnershipMap) {
	checklist := contracts.Checklist{SchemaVersion: 1, Items: []contracts.ChecklistItem{
		contracts.NewChecklistItem("01", "write a"), contracts.NewChecklistItem("02", "write b"),
	}}
	owners := coordination.NewFileOwnershipMap()
	_ = owners.Assign("a.py", coordination.WriterForItem("01"))
	_ = owners.Assign("b.py", coordination.WriterForItem("02"))
	return checklist, owners
}

func actText(tool string, arguments map[string]any) contracts.TurnResult {
	data, _ := json.Marshal(map[string]any{"tool": tool, "arguments": arguments})
	return contracts.TurnResult{Text: string(data)}
}

// implementers plays every implementer: records a decision, writes its write-set, runs the
// item's extra actions, then says done.
type implementers struct {
	*model.StubModel
	extra map[string][]contracts.TurnResult

	mu           sync.Mutex
	observations []string
	before       func() // runs once, before the first reply
}

func newImplementers(extra map[string][]contracts.TurnResult) *implementers {
	return &implementers{StubModel: model.NewStubNamed("implementers", nil), extra: extra}
}

func (m *implementers) Complete(_ context.Context, messages []contracts.ModelMessage, _ []map[string]any, _ int) (contracts.TurnResult, error) {
	m.mu.Lock()
	if m.before != nil {
		m.before()
		m.before = nil
	}
	for _, msg := range messages {
		if strings.HasPrefix(msg.Content, "OBSERVATION") {
			m.observations = append(m.observations, msg.Content)
		}
	}
	m.mu.Unlock()
	objective := messages[1].Content
	item := itemIDRE.FindStringSubmatch(objective)
	ws := writeSet.FindStringSubmatch(objective)
	if item == nil || ws == nil {
		return contracts.TurnResult{}, errors.New("not an implementer objective")
	}
	actions := []contracts.TurnResult{actText("record_decision", map[string]any{"decision": "item " + item[1] + " layout", "rationale": "r"})}
	for _, p := range strings.Split(ws[1], ",") {
		if p = strings.TrimSpace(p); p != "" {
			actions = append(actions, actText("write_file", map[string]any{"path": p, "content": "x\n"}))
		}
	}
	actions = append(actions, m.extra[item[1]]...)
	turn := 0
	for _, msg := range messages {
		if msg.Role == "assistant" {
			turn++
		}
	}
	if turn < len(actions) {
		return actions[turn], nil
	}
	return doneTurn, nil
}

func (m *implementers) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.observations...)
}

// broken fails every call.
type broken struct{ *model.StubModel }

type runtimeError struct{ msg string }

func (e *runtimeError) Error() string      { return e.msg }
func (e *runtimeError) PyTypeName() string { return "RuntimeError" }

func (broken) Complete(context.Context, []contracts.ModelMessage, []map[string]any, int) (contracts.TurnResult, error) {
	return contracts.TurnResult{}, &runtimeError{"model fell over"}
}

// scripted is a scripted stub that keeps every message it was shown.
type scripted struct {
	*model.StubModel
	mu   sync.Mutex
	seen []string
	// fn, when set, replaces the script.
	fn func(messages []contracts.ModelMessage) contracts.TurnResult
}

func newScripted(turns ...contracts.TurnResult) *scripted {
	return &scripted{StubModel: model.NewStub(turns)}
}

func (s *scripted) Complete(ctx context.Context, messages []contracts.ModelMessage, tools []map[string]any, maxTokens int) (contracts.TurnResult, error) {
	s.mu.Lock()
	for _, m := range messages {
		s.seen = append(s.seen, m.Content)
	}
	fn := s.fn
	s.mu.Unlock()
	if fn != nil {
		return fn(messages), nil
	}
	return s.StubModel.Complete(ctx, messages, tools, maxTokens)
}

func (s *scripted) saw(parts ...string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, text := range s.seen {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(text, p)
		}
		if all {
			return true
		}
	}
	return false
}

type traceEvent struct {
	Kind string         `json:"kind"`
	Data map[string]any `json:"data"`
}

func traceOf(t *testing.T, summary agent.MissionSummary) []traceEvent {
	t.Helper()
	out := []traceEvent{}
	for _, line := range strings.Split(summary.TraceJSONL, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e traceEvent
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func kinds(events []traceEvent, kind string) []traceEvent {
	out := []traceEvent{}
	for _, e := range events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

type anchorEvent struct {
	Kind    string         `json:"kind"`
	CycleID string         `json:"cycle_id"`
	Payload map[string]any `json:"payload"`
}

// committedEvents are the events committed at HEAD of kind ("" = all).
func committedEvents(t *testing.T, dir, kind string) []anchorEvent {
	t.Helper()
	out := []anchorEvent{}
	for _, line := range strings.Split(git(t, dir, "show", "HEAD:.lha/events.ndjson"), "\n") {
		var e anchorEvent
		if json.Unmarshal([]byte(line), &e) == nil && (kind == "" || e.Kind == kind) {
			out = append(out, e)
		}
	}
	return out
}

func lhaBranches(t *testing.T, dir string) []string {
	t.Helper()
	out := []string{}
	for _, b := range strings.Split(git(t, dir, "branch", "--format=%(refname:short)"), "\n") {
		if strings.HasPrefix(b, "lha/") {
			out = append(out, b)
		}
	}
	return out
}

func intp(n int) *int { return &n }

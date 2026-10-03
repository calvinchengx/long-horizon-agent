package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent/agenttest"
	"github.com/calvinchengx/long-horizon-agent/go/internal/agent/mcpbridge"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model/claudecodetest"
)

// Ported from python/tests/unit/test_claude_code.py (the claude_code lead engine, end to end).
// The fake claude (claudecodetest) is a real MCP client of the engine's bridge: the argv, the
// bridge, the dispatcher, the verifier, the checkpoint and the ledger are all real.

func ccSettings(t *testing.T, env ...string) *config.Settings {
	t.Helper()
	s, err := config.LoadFrom(env, "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// engineSettings is python's _engine_settings (no explicit model backend, so the other roles go
// through claude -p too).
func engineSettings(t *testing.T, bin string, env ...string) *config.Settings {
	base := []string{"LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true", "LHA_LEAD_ENGINE=claude_code",
		"LHA_CLAUDE_CODE_BIN=" + bin, "LHA_CLAUDE_CODE_MAX_BUDGET_USD=1.0", "LHA_BUDGET_USD_CEILING=5.0",
		"LHA_MAX_CYCLES=3"}
	return ccSettings(t, append(base, env...)...)
}

func helloChecklist(witnesses ...string) contracts.Checklist {
	it := item("01", "Create hello.txt")
	it.Witnesses = append([]string{}, witnesses...)
	return contracts.Checklist{Items: []contracts.ChecklistItem{it}, SchemaVersion: 1}
}

func engineRun(t *testing.T, ws string, s *config.Settings, checklist contracts.Checklist, boxes *[]*agenttest.Toolbox) MissionSummary {
	t.Helper()
	summary, err := RunMissionLocal(context.Background(), RunOptions{
		Workdir: ws, Title: "Hello", Description: "Write hello.txt", Checklist: checklist,
		Checks:   contracts.ChecksFromCommands([][]string{{"test", "-f", "hello.txt"}}, true),
		Settings: s, OpenToolbox: fakeOpener(boxes),
	})
	if err != nil {
		t.Fatal(err)
	}
	return summary
}

func setCalls(t *testing.T, calls ...any) {
	raw, _ := json.Marshal(calls)
	t.Setenv("FAKE_CLAUDE_CALLS", string(raw))
}

func after(argv []string, flag string) []string {
	for i, a := range argv {
		if a == flag {
			return argv[i+1:]
		}
	}
	return nil
}

func TestLeadEngineFromSettings(t *testing.T) {
	if e, err := LeadEngine(ccSettings(t), false); e != nil || err != nil {
		t.Fatalf("%v %v", e, err)
	}
	e, err := LeadEngine(ccSettings(t, "LHA_LEAD_ENGINE=claude_code", "LHA_MODEL_NAME=haiku"), false)
	if err != nil || e.Native() || e.Name() != "claude_code_engine:haiku" {
		t.Fatalf("%v %v", e, err)
	}
	// An explicit non-claude_code backend's model name is not Claude Code's.
	e, _ = LeadEngine(ccSettings(t, "LHA_LEAD_ENGINE=claude_code", "LHA_MODEL_BACKEND=stub", "LHA_MODEL_NAME=haiku"), false)
	if e.Name() != "claude_code_engine:default" {
		t.Fatal(e.Name())
	}

	native := []string{"LHA_LEAD_ENGINE=claude_code", "LHA_CLAUDE_CODE_TOOLS=native"}
	if _, err := LeadEngine(ccSettings(t, native...), false); err == nil || !strings.Contains(err.Error(), "no isolation") {
		t.Fatalf("err = %v", err)
	}
	unsafe := ccSettings(t, append(native, "LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true")...)
	if e, err := LeadEngine(unsafe, false); err != nil || !e.Native() {
		t.Fatalf("%v %v", e, err)
	}
	if _, err := LeadEngine(unsafe, true); err == nil || !strings.Contains(err.Error(), "ownership guard") {
		t.Fatalf("err = %v", err)
	}
	if _, err := NewClaudeCodeEngine(ClaudeCodeEngineOptions{Tools: "bogus"}); err == nil || err.Error() != "unknown Claude Code tool mode: 'bogus'" {
		t.Fatalf("err = %v", err)
	}
}

func TestACycleIsOneClaudeSessionUsingLHATools(t *testing.T) {
	bin, log := claudecodetest.Install(t)
	t.Setenv("FAKE_CLAUDE_MODE", "mcp")
	setCalls(t, []any{"write_file", map[string]any{"path": "hello.txt", "content": "hi\n"}}, []any{"verify", map[string]any{}})
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	summary := engineRun(t, ws, engineSettings(t, bin), helloChecklist("cmd:grep -qx hi hello.txt"), nil)

	if !summary.Completed || summary.Cycles != 1 {
		t.Fatalf("%+v", summary)
	}
	if d := summary.TotalUSD - 0.42; d > 1e-9 || d < -1e-9 { // the cost Claude Code reported
		t.Fatalf("total %v", summary.TotalUSD)
	}
	calls := claudecodetest.Calls(t, log)
	if len(calls) != 1 {
		t.Fatalf("sessions: %d", len(calls))
	}
	session := calls[0]
	argv := session.Argv
	if real, _ := filepath.EvalSymlinks(ws); session.Cwd != ws && session.Cwd != real {
		t.Fatalf("cwd %s", session.Cwd)
	}
	if tools := after(argv, "--tools"); len(tools) == 0 || tools[0] != "" { // Claude Code's own tools are off
		t.Fatalf("argv %q", argv)
	}
	if after(argv, "--strict-mcp-config") == nil {
		t.Fatalf("argv %q", argv)
	}
	allowed := strings.Join(after(argv, "--allowedTools"), " ")
	if !strings.Contains(allowed, "mcp__lha__write_file") || !strings.Contains(allowed, "mcp__lha__verify") {
		t.Fatalf("allowed %s", allowed)
	}
	system := after(argv, "--append-system-prompt")[0]
	if !strings.Contains(system, "call the `verify` tool") || strings.Contains(system, `{"tool"`) {
		t.Fatalf("system %s", system)
	}
	if !strings.Contains(session.Stdin, "Create hello.txt") || !strings.Contains(session.Stdin, "cmd:grep -qx hi hello.txt") {
		t.Fatalf("stdin %s", session.Stdin)
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "hello.txt")); string(got) != "hi\n" { // through LHA's write_file
		t.Fatalf("hello.txt %q", got)
	}
	if !strings.Contains(summary.TraceJSONL, "claude_code_session") {
		t.Fatalf("trace %s", summary.TraceJSONL)
	}
}

func TestVerifyReportsFailuresAndTheHarnessStillDecides(t *testing.T) {
	bin, log := claudecodetest.Install(t)
	t.Setenv("FAKE_CLAUDE_MODE", "mcp")
	// The session writes the wrong content and stops after a failing verify.
	setCalls(t, []any{"write_file", map[string]any{"path": "hello.txt", "content": "bye\n"}}, []any{"verify", map[string]any{}})
	ws := filepath.Join(t.TempDir(), "ws")
	summary := engineRun(t, ws, engineSettings(t, bin, "LHA_MAX_REPLANS=0"), helloChecklist("cmd:grep -qx hi hello.txt"), nil)

	if summary.Completed || summary.Cycles != 3 || !strings.HasPrefix(summary.StoppedReason, "deadlocked") {
		t.Fatalf("%+v", summary)
	}
	if _, err := os.Stat(filepath.Join(ws, "hello.txt")); !os.IsNotExist(err) { // every failed attempt was rolled back
		t.Fatal("hello.txt survived a failed attempt")
	}
	raw, _ := os.ReadFile(filepath.Join(ws, ".lha", "checklist.json"))
	var checklist contracts.Checklist
	if err := json.Unmarshal(raw, &checklist); err != nil || checklist.Items[0].Status != contracts.StatusBlocked {
		t.Fatalf("checklist %s %v", raw, err)
	}
	// The second session was told why the first failed; verify said FAILED to the session.
	second := claudecodetest.Calls(t, log)[1].Stdin
	if !strings.Contains(second, "FAILED verification") || !strings.Contains(second, "grep -qx hi hello.txt") {
		t.Fatalf("second stdin %s", second)
	}
}

func TestTheSessionCapIsLoweredToWhatIsLeftOfTheBudget(t *testing.T) {
	bin, log := claudecodetest.Install(t)
	t.Setenv("FAKE_CLAUDE_MODE", "mcp")
	setCalls(t, []any{"write_file", map[string]any{"path": "hello.txt", "content": "hi\n"}})
	ws := filepath.Join(t.TempDir(), "ws")
	summary := engineRun(t, ws, engineSettings(t, bin, "LHA_BUDGET_USD_CEILING=0.5"), helloChecklist(), nil) // below the $1 session cap
	// Not refused for its $1 cap.
	if !summary.Completed {
		t.Fatalf("%+v", summary)
	}
	if got := after(claudecodetest.Calls(t, log)[0].Argv, "--max-budget-usd"); len(got) == 0 || got[0] != "0.5000" {
		t.Fatalf("--max-budget-usd %q", got)
	}
}

func TestTheSessionIsRefusedWhenAlmostNothingIsLeft(t *testing.T) {
	bin, log := claudecodetest.Install(t)
	t.Setenv("FAKE_CLAUDE_MODE", "mcp")
	ws := filepath.Join(t.TempDir(), "ws")
	summary := engineRun(t, ws, engineSettings(t, bin, "LHA_BUDGET_USD_CEILING=0.005"), helloChecklist(), nil) // under a cent left
	if !strings.HasPrefix(summary.StoppedReason, "governor") {
		t.Fatalf("%+v", summary)
	}
	if calls := claudecodetest.Calls(t, log); len(calls) != 0 { // claude -p never ran
		t.Fatalf("claude ran: %v", calls)
	}
}

func TestACappedSessionIsStillVerifiedAndCharged(t *testing.T) {
	bin, _ := claudecodetest.Install(t)
	t.Setenv("FAKE_CLAUDE_MODE", "budget")
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("already here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	summary := engineRun(t, ws, engineSettings(t, bin), helloChecklist(), nil)
	if !summary.Completed { // the work in the workdir passed, though the session hit its cap
		t.Fatalf("%+v", summary)
	}
	if d := summary.TotalUSD - 0.9; d > 1e-9 || d < -1e-9 {
		t.Fatalf("total %v", summary.TotalUSD)
	}
	if !strings.Contains(summary.TraceJSONL, "error_max_budget_usd") { // the session event says why it stopped
		t.Fatalf("trace %s", summary.TraceJSONL)
	}
}

func TestATimedOutSessionIsChargedItsWorstCaseAndVerified(t *testing.T) {
	bin, _ := claudecodetest.Install(t)
	t.Setenv("FAKE_CLAUDE_MODE", "sleep")
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	summary := engineRun(t, ws, engineSettings(t, bin, "LHA_CLAUDE_CODE_TIMEOUT_S=0.5"), helloChecklist(), nil)
	if !summary.Completed || summary.TotalUSD != 1.0 { // nothing reported: the full $1 cap is charged
		t.Fatalf("%+v", summary)
	}
	if !strings.Contains(summary.TraceJSONL, "did not finish within") {
		t.Fatalf("trace %s", summary.TraceJSONL)
	}
}

func TestRecordDecisionAndDispatcherFailuresGoThroughTheBridge(t *testing.T) {
	bin, log := claudecodetest.Install(t)
	t.Setenv("FAKE_CLAUDE_MODE", "mcp")
	setCalls(t,
		[]any{"record_decision", map[string]any{"decision": "greet in English", "rationale": "the audience"}},
		[]any{"read_file", map[string]any{"path": "missing.txt"}},
		[]any{"write_file", map[string]any{"path": "hello.txt", "content": "hi\n"}})
	var boxes []*agenttest.Toolbox
	opener := fakeOpener(&boxes)
	ws := filepath.Join(t.TempDir(), "ws")
	summary, err := RunMissionLocal(context.Background(), RunOptions{
		Workdir: ws, Title: "Hello", Description: "d", Checklist: helloChecklist(),
		Checks:   contracts.ChecksFromCommands([][]string{{"test", "-f", "hello.txt"}}, true),
		Settings: engineSettings(t, bin),
		OpenToolbox: func(ctx context.Context, req ToolboxRequest) (Toolbox, error) {
			box, err := opener(ctx, req)
			if err != nil {
				return nil, err
			}
			box.(*agenttest.Toolbox).Disp.RecordDecision = req.Anchor.RecordDecision
			return decisionToolbox{box.(*agenttest.Toolbox)}, nil
		},
	})
	if err != nil || !summary.Completed {
		t.Fatalf("%+v %v", summary, err)
	}
	calls := claudecodetest.Calls(t, log)
	if len(calls) != 1 {
		t.Fatalf("sessions: %d", len(calls))
	}
	disp := boxes[0].Disp
	if len(disp.Calls) != 3 || disp.Calls[0].ID != "c1-cc-1" || disp.Calls[2].ID != "c1-cc-3" {
		t.Fatalf("dispatched %+v", disp.Calls)
	}
	raw, _ := os.ReadFile(filepath.Join(ws, ".lha", "decisions.ndjson"))
	if !strings.Contains(string(raw), "greet in English") {
		t.Fatalf("decisions %s", raw)
	}
	// Every bridged call is traced like a built-in one, failures included.
	trace := summary.TraceJSONL
	if !strings.Contains(trace, `"tool":"record_decision"`) || !strings.Contains(trace, `"ok":false`) {
		t.Fatalf("trace %s", trace)
	}
}

func TestEngineRunReportsToolTextsAndErrors(t *testing.T) {
	bin, _ := claudecodetest.Install(t)
	t.Setenv("FAKE_CLAUDE_MODE", "mcp")
	setCalls(t, []any{"read_file", map[string]any{"path": "nope"}}, []any{"verify", map[string]any{}})
	e, _ := NewClaudeCodeEngine(ClaudeCodeEngineOptions{Binary: bin, MaxBudgetUSD: 1})
	meter := governor.NewCostMeter(governor.NewCostLedger(), governor.NewBudgetGovernor(10, 10, false))
	lead := meter.Wrap(model.NewStub(nil), "lead")
	var dispatched []contracts.ToolCall
	run, err := e.Run(context.Background(), EngineRequest{
		Messages: []contracts.ModelMessage{{Role: "system", Content: "S"}, {Role: "user", Content: "U"}},
		Cwd:      t.TempDir(), CycleID: "c7",
		Specs: (&agenttest.Dispatcher{}).Specs(),
		Dispatch: func(_ context.Context, call contracts.ToolCall) contracts.ToolResult {
			dispatched = append(dispatched, call)
			return contracts.Failure("no such file: nope")
		},
		Verify: func(context.Context) (contracts.VerificationResult, error) {
			return contracts.NewVerificationResult(nil), nil
		},
		Meter: lead,
	})
	if err != nil {
		t.Fatal(err)
	}
	var reply struct {
		Tools []string `json:"tools"`
		Texts []string `json:"texts"`
	}
	if err := json.Unmarshal([]byte(run.Summary), &reply); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reply.Tools, []string{"read_file", "write_file", "verify"}) ||
		!reflect.DeepEqual(reply.Texts, []string{"no such file: nope", "UNVERIFIED: this mission has no checks to run."}) {
		t.Fatalf("%+v", reply)
	}
	if run.ToolCalls != 1 || run.Turns != 3 || run.SessionID != "sess-1" || !reflect.DeepEqual(run.ToolsUsed, []string{"read_file"}) {
		t.Fatalf("%+v", run)
	}
	if len(dispatched) != 1 || dispatched[0].ID != "c7-cc-1" {
		t.Fatalf("%+v", dispatched)
	}
	entries := meter.Ledger.Entries()
	if len(entries) != 1 || entries[0].Role != "lead" || meter.Ledger.TotalUSD() != 0.42 || meter.ReservedUSD() != 0 {
		t.Fatalf("ledger %+v", entries)
	}
}

func TestEngineRunPassesOtherFailuresThrough(t *testing.T) {
	bin, _ := claudecodetest.Install(t)
	t.Setenv("FAKE_CLAUDE_MODE", "auth")
	e, _ := NewClaudeCodeEngine(ClaudeCodeEngineOptions{Binary: bin})
	meter := governor.NewCostMeter(governor.NewCostLedger(), governor.NewBudgetGovernor(10, 10, false))
	_, err := e.Run(context.Background(), EngineRequest{
		Messages: []contracts.ModelMessage{{Role: "user", Content: "U"}}, Cwd: t.TempDir(), CycleID: "c1",
		Dispatch: func(context.Context, contracts.ToolCall) contracts.ToolResult { return contracts.Success("") },
		Verify: func(context.Context) (contracts.VerificationResult, error) {
			return contracts.VerificationResult{}, nil
		},
		Meter: meter.Wrap(model.NewStub(nil), "lead"),
	})
	var cc *model.ClaudeCodeError
	if !errors.As(err, &cc) || !strings.Contains(err.Error(), "authenticate") {
		t.Fatalf("err = %v", err)
	}
	if len(meter.Ledger.Entries()) != 0 || meter.ReservedUSD() != 0 { // a failed session records nothing
		t.Fatal("recorded a failed session")
	}
}

func TestNativeModeDeniesHistoryPublishingAndTheWeb(t *testing.T) {
	e, _ := NewClaudeCodeEngine(ClaudeCodeEngineOptions{Tools: "native"})
	args := e.Args(mcpbridge.New(nil), "sys")
	for _, a := range args {
		if a == "--tools" { // Claude Code's own tools stay on
			t.Fatalf("argv %q", args)
		}
	}
	denied := after(args, "--disallowedTools")
	if !reflect.DeepEqual(denied, NativeDeny) {
		t.Fatalf("denied %q", denied)
	}
	allowed := after(args, "--allowedTools")
	if !reflect.DeepEqual(allowed[:len(NativeTools)], NativeTools) || allowed[len(NativeTools)] != "--disallowedTools" {
		t.Fatalf("allowed %q", allowed)
	}
	// Only record_decision (and verify) are bridged in native mode.
	tools := e.bridgeTools([]contracts.ToolSpec{{Name: "write_file"}, {Name: "record_decision"}}, nil, nil, "c1", &usedTools{})
	names := []string{}
	for _, tool := range tools {
		names = append(names, tool.Name)
	}
	if !reflect.DeepEqual(names, []string{"record_decision", "verify"}) {
		t.Fatalf("bridged %v", names)
	}
}

// decisionToolbox lists record_decision among the fake dispatcher's specs (it serves it already).
type decisionToolbox struct{ *agenttest.Toolbox }

func (b decisionToolbox) Dispatcher() contracts.ToolDispatcher { return decisionSpecs{b.Disp} }

type decisionSpecs struct{ *agenttest.Dispatcher }

func (d decisionSpecs) Specs() []contracts.ToolSpec {
	return append(d.Dispatcher.Specs(), contracts.ToolSpec{Name: "record_decision", Description: "Record a decision.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{"decision": map[string]any{"type": "string"}}}})
}

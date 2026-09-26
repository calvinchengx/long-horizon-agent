package model

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model/claudecodetest"
)

// Ported from python/tests/unit/test_claude_code.py (the parsing and model-backend half; the
// bridge and the lead engine are tested in internal/agent).

func TestMain(m *testing.M) {
	claudecodetest.Main() // this binary doubles as the fake claude
	os.Exit(m.Run())
}

func argAfter(argv []string, flag string) (string, bool) {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

func TestParseResultReadsUsageCostAndTheServingModel(t *testing.T) {
	out := `{"type": "result", "subtype": "success", "is_error": false, "result": "hi", "num_turns": 2,
		"session_id": "s", "total_cost_usd": 0.0123,
		"usage": {"input_tokens": 5, "output_tokens": 7, "cache_read_input_tokens": 9},
		"modelUsage": {"small": {"outputTokens": 1}, "big": {"outputTokens": 6}}}`
	parsed, err := ParseClaudeCodeResult(out, "p", "default")
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Text != "hi" || *parsed.SessionID != "s" || parsed.NumTurns != 2 {
		t.Fatalf("%+v", parsed)
	}
	u := parsed.Usage
	if u.Model != "big" { // most of the output, not the helper model
		t.Fatalf("model %q", u.Model)
	}
	if u.InputTokens != 5 || u.OutputTokens != 7 || u.CacheReadInputTokens != 9 || u.Provider != "p" {
		t.Fatalf("%+v", u)
	}
	approx(t, "reported", *u.ReportedCostUSD, 0.0123)
}

func TestParseResultServingModelTieAndFallback(t *testing.T) {
	tie, _ := ParseClaudeCodeResult(`{"modelUsage": {"b": {"outputTokens": 3}, "a": {"outputTokens": 3}}}`, "p", "fb")
	if tie.Usage.Model != "b" { // Python's max keeps the first of equal keys
		t.Fatalf("tie: %q", tie.Usage.Model)
	}
	none, _ := ParseClaudeCodeResult(`{"result": "x"}`, "p", "fb")
	if none.Usage.Model != "fb" || none.Usage.ReportedCostUSD != nil || none.SessionID != nil {
		t.Fatalf("fallback: %+v", none)
	}
}

func TestParseResultClassifiesErrors(t *testing.T) {
	_, err := ParseClaudeCodeResult(`{"is_error": true, "subtype": "success", "result": "Failed to authenticate"}`, "p", "m")
	var cc *ClaudeCodeError
	if !errors.As(err, &cc) || IsRetryable(err) { // a login problem is not fixed by retrying
		t.Fatalf("auth: %v", err)
	}
	if cc.Error() != "claude -p failed: Failed to authenticate" {
		t.Fatalf("message: %q", cc.Error())
	}

	_, err = ParseClaudeCodeResult(`{"is_error": true, "result": "API error", "api_error_status": 429}`, "p", "m")
	if !IsRetryable(err) {
		t.Fatalf("429: %v", err)
	}
	_, err = ParseClaudeCodeResult(`{"is_error": true, "result": "API error", "api_error_status": 429.0}`, "p", "m")
	if IsRetryable(err) { // Python: isinstance(429.0, int) is False
		t.Fatalf("429.0: %v", err)
	}
	_, err = ParseClaudeCodeResult(`{"is_error": true, "result": "Overloaded, try later"}`, "p", "m")
	if !IsRetryable(err) {
		t.Fatalf("overloaded: %v", err)
	}

	_, err = ParseClaudeCodeResult(`{"subtype": "error_max_budget_usd", "total_cost_usd": 0.5}`, "p", "m")
	if !errors.As(err, &cc) || cc.Subtype != "error_max_budget_usd" {
		t.Fatalf("capped: %v", err)
	}
	if cc.Usage == nil || *cc.Usage.ReportedCostUSD != 0.5 || cc.Error() != "claude -p failed: error_max_budget_usd" {
		t.Fatalf("capped usage: %+v %q", cc.Usage, cc.Error())
	}

	_, err = ParseClaudeCodeResult("Error: unknown option", "p", "m")
	if err == nil || err.Error() != "claude -p printed no JSON result: 'Error: unknown option'" {
		t.Fatalf("no JSON: %v", err)
	}
	_, err = ParseClaudeCodeResult("[1]", "p", "m")
	if err == nil || !strings.Contains(err.Error(), "unexpected result") {
		t.Fatalf("list: %v", err)
	}
}

func TestClaudeCodeRunsAToolLessTurnThroughTheCLI(t *testing.T) {
	bin, log := claudecodetest.Install(t)
	m := NewClaudeCode(ClaudeCodeOptions{ModelName: "sonnet", Binary: bin, MaxBudgetUSD: 2.0})
	id := "t1"
	result, err := m.Complete(context.Background(), []contracts.ModelMessage{
		{Role: "system", Content: "SYSTEM PROMPT"},
		{Role: "user", Content: "do the thing"},
		{Role: "assistant", ToolCalls: []contracts.ToolCall{{ID: "t1", Name: "read_file", Arguments: map[string]any{"path": "a"}}}},
		{Role: "tool", Content: "file a", ToolCallID: &id},
	}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != `{"done": true, "summary": "ok"}` || *result.SessionID != "sess-1" || *result.StopReason != "end_turn" {
		t.Fatalf("%+v", result)
	}
	approx(t, "reported", *result.Usage.ReportedCostUSD, 0.01)
	cost, _ := m.EstimateCostUSD(result.Usage)
	approx(t, "estimate", cost, 0.01)
	if result.Usage.Model != "claude-sonnet-4-6" || result.Usage.CacheCreation1hInputTokens != 50 || result.Usage.Provider != "claude_code:sonnet" {
		t.Fatalf("usage %+v", result.Usage)
	}

	calls := claudecodetest.Calls(t, log)
	if len(calls) != 1 {
		t.Fatalf("calls: %d", len(calls))
	}
	argv := calls[0].Argv
	if strings.Join(argv[:3], " ") != "-p --output-format json" {
		t.Fatalf("argv %q", argv)
	}
	checks := map[string]string{"--tools": "", "--model": "sonnet", "--max-budget-usd": "2.0000", "--system-prompt": "SYSTEM PROMPT"}
	for flag, want := range checks {
		if got, ok := argAfter(argv, flag); !ok || got != want {
			t.Errorf("%s = %q (present %v), want %q", flag, got, ok, want)
		}
	}
	stdin := calls[0].Stdin
	for _, want := range []string{"do the thing", "file a", `{"tool": "read_file", "arguments": {"path": "a"}}`, "<tool_result id='t1'>"} {
		if !strings.Contains(stdin, want) {
			t.Errorf("stdin lacks %q:\n%s", want, stdin)
		}
	}
}

func TestClaudeCodeSingleTurnPromptIsSentVerbatim(t *testing.T) {
	bin, log := claudecodetest.Install(t)
	m := NewClaudeCode(ClaudeCodeOptions{Binary: bin})
	if _, err := m.Complete(context.Background(), []contracts.ModelMessage{user("plan this")}, nil, 0); err != nil {
		t.Fatal(err)
	}
	call := claudecodetest.Calls(t, log)[0]
	if call.Stdin != "plan this" {
		t.Fatalf("stdin %q", call.Stdin)
	}
	if _, ok := argAfter(call.Argv, "--model"); ok { // the default model is Claude Code's own choice
		t.Fatalf("argv %q", call.Argv)
	}
	if _, ok := argAfter(call.Argv, "--system-prompt"); ok {
		t.Fatalf("argv %q", call.Argv)
	}
}

func TestClaudeCodeDoesNotRetryAnExpiredLogin(t *testing.T) {
	bin, log := claudecodetest.Install(t)
	t.Setenv("FAKE_CLAUDE_MODE", "auth")
	zero := 0.0
	m := NewClaudeCode(ClaudeCodeOptions{Binary: bin, RetryBaseDelayS: &zero})
	_, err := m.Complete(context.Background(), []contracts.ModelMessage{user("x")}, nil, 0)
	if err == nil || !strings.Contains(err.Error(), "authenticate") {
		t.Fatalf("err = %v", err)
	}
	if n := len(claudecodetest.Calls(t, log)); n != 1 {
		t.Fatalf("calls = %d", n)
	}
}

func TestClaudeCodeRetriesATimeout(t *testing.T) {
	bin, log := claudecodetest.Install(t)
	t.Setenv("FAKE_CLAUDE_MODE", "sleep")
	s := &sleeps{}
	m := NewClaudeCode(ClaudeCodeOptions{Binary: bin, TimeoutS: 0.3, MaxRetries: Int(1), Sleep: s.sleep})
	_, err := m.Complete(context.Background(), []contracts.ModelMessage{user("x")}, nil, 0)
	var timeout *ClaudeCodeTimeoutError
	if !errors.As(err, &timeout) || err.Error() != "claude -p did not finish within 0s" || !IsRetryable(err) {
		t.Fatalf("err = %v", err)
	}
	if n := len(claudecodetest.Calls(t, log)); n != 2 {
		t.Fatalf("calls = %d", n)
	}
	if len(s.calls) != 1 || s.calls[0] != 2.0 { // base delay 2s, as in Python
		t.Fatalf("delays %v", s.calls)
	}
}

func TestClaudeCodeReportsANonZeroExit(t *testing.T) {
	bin, _ := claudecodetest.Install(t)
	t.Setenv("FAKE_CLAUDE_MODE", "crash")
	m := NewClaudeCode(ClaudeCodeOptions{Binary: bin, MaxRetries: Int(0)})
	_, err := m.Complete(context.Background(), []contracts.ModelMessage{user("x")}, nil, 0)
	if err == nil || err.Error() != "claude -p exited 2: Error: unknown option '--frobnicate'\n" || IsRetryable(err) {
		t.Fatalf("err = %q", err)
	}
}

func TestClaudeCodeReportsAMissingBinary(t *testing.T) {
	m := NewClaudeCode(ClaudeCodeOptions{Binary: "/nonexistent/claude", MaxRetries: Int(0)})
	_, err := m.Complete(context.Background(), []contracts.ModelMessage{user("x")}, nil, 0)
	want := "cannot run '/nonexistent/claude': [Errno 2] No such file or directory: '/nonexistent/claude'. " +
		"Install Claude Code or set LHA_CLAUDE_CODE_BIN."
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v", err)
	}
	ok, detail := m.HealthCheck(context.Background(), 1)
	if ok || !strings.Contains(detail, "not found") {
		t.Fatalf("health %v %q", ok, detail)
	}
}

func TestClaudeCodeHealthCheckRunsVersion(t *testing.T) {
	bin, _ := claudecodetest.Install(t)
	ok, detail := NewClaudeCode(ClaudeCodeOptions{Binary: bin}).HealthCheck(context.Background(), 10)
	if !ok || detail != "claude_code:default: 2.0.0 (Claude Code, fake)" {
		t.Fatalf("%v %q", ok, detail)
	}
}

func TestClaudeCodeWorstCaseIsTheBudgetCapUnlessTheModelIsPriced(t *testing.T) {
	unpriced := NewClaudeCode(ClaudeCodeOptions{ModelName: "opus", MaxBudgetUSD: 3.0})
	big := contracts.Usage{InputTokens: 10_000_000, OutputTokens: 10_000_000}
	if c, err := unpriced.EstimateCostUSD(big); err != nil || c != 3.0 {
		t.Fatalf("unpriced %v %v", c, err)
	}
	price := NewModelPrice(1.0, 1.0)
	priced := NewClaudeCode(ClaudeCodeOptions{ModelName: "x", MaxBudgetUSD: 3.0, Price: &price})
	c, _ := priced.EstimateCostUSD(contracts.Usage{InputTokens: 1_000_000})
	approx(t, "priced", c, 1.0)
	if c, _ := priced.EstimateCostUSD(big); c != 3.0 { // never above the cap the CLI enforces
		t.Fatalf("capped %v", c)
	}
	table := NewClaudeCode(ClaudeCodeOptions{ModelName: "claude-haiku-4-5", MaxBudgetUSD: 3.0})
	c, _ = table.EstimateCostUSD(contracts.Usage{InputTokens: 1_000_000})
	approx(t, "table", c, 1.0)
}

func TestTranscriptFraming(t *testing.T) {
	// A lone user turn after a system message is sent verbatim; otherwise it is framed.
	if got := RenderTranscript([]contracts.ModelMessage{{Role: "system", Content: "s"}, user("u")}); got != "u" {
		t.Fatalf("%q", got)
	}
	got := RenderTranscript([]contracts.ModelMessage{user("a"), {Role: "assistant", Content: "b"}})
	want := "<user>\na\n</user>\n\n<assistant>\nb\n</assistant>\n\nWrite the assistant's next reply only."
	if got != want {
		t.Fatalf("%q", got)
	}
}

func TestBuildProviderClaudeCode(t *testing.T) {
	load := func(env ...string) *config.Settings {
		s, err := config.LoadFrom(env, "")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	p, err := BuildProvider(load("LHA_MODEL_BACKEND=claude_code"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*ClaudeCodeModel); !ok || p.Name() != "claude_code:default" {
		t.Fatalf("%T %s", p, p.Name())
	}
	named, _ := BuildProvider(load("LHA_MODEL_BACKEND=claude_code", "LHA_MODEL_NAME=opus"), "", nil)
	if named.Name() != "claude_code:opus" {
		t.Fatal(named.Name())
	}
	// The engine alone routes the other roles through claude -p too; an explicit backend wins.
	if s := load("LHA_LEAD_ENGINE=claude_code"); s.ModelBackend != "claude_code" {
		t.Fatal(s.ModelBackend)
	}
	if s := load("LHA_LEAD_ENGINE=claude_code", "LHA_MODEL_BACKEND=stub"); s.ModelBackend != "stub" {
		t.Fatal(s.ModelBackend)
	}
}

package model

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model/opencodetest"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// Ported from python/tests/unit/test_opencode.py (the JSON event stream and the opencode model
// backend; the bridge and the lead engine are tested in internal/agent and cmd/lha). The fake
// opencode (opencodetest) is this test binary.

func om(t *testing.T, raw string) *pyfmt.OrderedMap {
	t.Helper()
	decoded, err := pyfmt.DecodeOrdered([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := decoded.(*pyfmt.OrderedMap)
	if !ok {
		t.Fatalf("not an object: %s", raw)
	}
	return m
}

func TestOpenCodeSessionProgressCountsEachTurnAndToolOnce(t *testing.T) {
	progress := &OpenCodeSessionProgress{}
	if _, ok := progress.SpentUSD(); ok { // nothing streamed: unknown, so the cap is charged, not $0
		t.Fatal("spend known before the first step")
	}
	stepStart := func(mid string) *pyfmt.OrderedMap {
		return om(t, `{"type": "step_start", "sessionID": "s-9", "part": {"messageID": "`+mid+`"}}`)
	}
	stepFinish := func(mid, cost string) *pyfmt.OrderedMap {
		return om(t, `{"type": "step_finish", "sessionID": "s-9", "part": {"messageID": "`+mid+`",
			"cost": `+cost+`, "tokens": {"input": 100, "output": 1000, "cache": {"read": 50, "write": 20}}}}`)
	}
	tool := func(callID, name string) *pyfmt.OrderedMap {
		return om(t, `{"type": "tool_use", "sessionID": "s-9", "part": {"id": "`+callID+`", "tool": "`+name+`"}}`)
	}

	if !progress.Observe(stepStart("m1")) || progress.Observe(stepStart("m1")) { // the same step is not news
		t.Fatalf("%+v", progress)
	}
	if !progress.Observe(tool("t1", "lha_run_command")) || progress.Observe(tool("t1", "lha_run_command")) {
		t.Fatalf("%+v", progress)
	}
	if progress.Observe(om(t, `{"type": "text", "part": {"text": "x"}}`)) {
		t.Fatalf("%+v", progress)
	}
	if progress.Observe(stepFinish("m1", "0.25")) { // already counted as a turn
		t.Fatalf("%+v", progress)
	}
	if progress.Turns != 1 || progress.ToolCalls != 1 || progress.Tool != "lha_run_command" || progress.SessionID != "s-9" {
		t.Fatalf("%+v", progress)
	}
	if usd, ok := progress.SpentUSD(); !ok || usd != 0.25 {
		t.Fatalf("spent %v %v", usd, ok)
	}
	total := progress.Usage("p", "default")
	if total.InputTokens != 100 || total.OutputTokens != 1000 ||
		total.CacheReadInputTokens != 50 || total.CacheCreationInputTokens != 20 {
		t.Fatalf("%+v", total)
	}
	if total.ReportedCostUSD == nil || *total.ReportedCostUSD != 0.25 {
		t.Fatalf("%+v", total)
	}

	// A step with no cost makes the whole session's spend unknown (never $0).
	progress.Observe(stepStart("m2"))
	progress.Observe(om(t, `{"type": "step_finish", "sessionID": "s-9", "part": {"messageID": "m2"}}`))
	if _, ok := progress.SpentUSD(); ok {
		t.Fatal("spend known though a step reported no cost")
	}
}

func TestRunOpenCodeReadsTextUsageAndCost(t *testing.T) {
	bin, _ := opencodetest.Install(t)
	result, err := RunOpenCode(context.Background(), OpenCodeRun{
		Args: OpenCodeBaseArgs("", "a", false), Prompt: "hi", Binary: bin,
		Cwd: t.TempDir(), TimeoutS: 10, Provider: "opencode:x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Text != `{"done": true, "summary": "ok"}` || result.SessionID == nil || *result.SessionID != "sess-oc-1" || result.NumTurns != 1 {
		t.Fatalf("%+v", result)
	}
	if result.Usage.ReportedCostUSD == nil || *result.Usage.ReportedCostUSD != 0.01 {
		t.Fatalf("%+v", result.Usage)
	}
}

func TestRunOpenCodeClassifiesErrors(t *testing.T) {
	bin, _ := opencodetest.Install(t)
	t.Setenv("FAKE_OPENCODE_MODE", "error")
	_, err := RunOpenCode(context.Background(), OpenCodeRun{
		Args: OpenCodeBaseArgs("", "a", false), Prompt: "hi", Binary: bin,
		Cwd: t.TempDir(), TimeoutS: 10, Provider: "p",
	})
	if err == nil || !IsRetryable(err) { // 529 overload is transient
		t.Fatalf("err = %v", err)
	}

	_, err = RunOpenCode(context.Background(), OpenCodeRun{
		Args: OpenCodeBaseArgs("", "a", false), Prompt: "hi", Binary: "/nonexistent/opencode",
		Cwd: t.TempDir(), TimeoutS: 10, Provider: "p",
	})
	if err == nil || !strings.Contains(err.Error(), "LHA_OPENCODE_BIN") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunOpenCodeKillsASessionThatReachesItsCap(t *testing.T) {
	bin, _ := opencodetest.Install(t)
	t.Setenv("FAKE_OPENCODE_MODE", "budget")
	cap := 0.5
	_, err := RunOpenCode(context.Background(), OpenCodeRun{
		Args: OpenCodeBaseArgs("", "a", false), Prompt: "hi", Binary: bin,
		Cwd: t.TempDir(), TimeoutS: 30, Provider: "p", MaxCostUSD: &cap,
	})
	var oc *OpenCodeError
	if !errors.As(err, &oc) || oc.Subtype != "error_max_budget_usd" {
		t.Fatalf("err = %v", err)
	}
	if oc.Usage == nil || oc.Usage.ReportedCostUSD == nil || *oc.Usage.ReportedCostUSD != 0.9 {
		t.Fatalf("usage %+v", oc.Usage)
	}
}

func TestRunOpenCodeTimesOutAndReportsProgress(t *testing.T) {
	bin, _ := opencodetest.Install(t)
	t.Setenv("FAKE_OPENCODE_MODE", "hang")
	_, err := RunOpenCode(context.Background(), OpenCodeRun{
		Args: OpenCodeBaseArgs("", "a", false), Prompt: "hi", Binary: bin,
		Cwd: t.TempDir(), TimeoutS: 2, Provider: "p",
	})
	var timeout *OpenCodeTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("err = %v", err)
	}
	if timeout.Progress == nil || timeout.Progress.Turns != 1 {
		t.Fatalf("progress %+v", timeout.Progress)
	}
	if usd, ok := timeout.Progress.SpentUSD(); !ok || usd != 0.0153 {
		t.Fatalf("spent %v %v", usd, ok)
	}
}

func TestOpenCodeModelRunsAToolLessTurnThroughTheCLI(t *testing.T) {
	bin, log := opencodetest.Install(t)
	m := NewOpenCode(OpenCodeOptions{ModelName: "anthropic/claude-sonnet-4-5", Binary: bin, MaxBudgetUSD: 2.0})
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
	if result.Text != `{"done": true, "summary": "ok"}` || result.SessionID == nil || *result.SessionID != "sess-oc-1" {
		t.Fatalf("%+v", result)
	}
	if result.Usage.ReportedCostUSD == nil || *result.Usage.ReportedCostUSD != 0.01 {
		t.Fatalf("%+v", result.Usage)
	}
	if cost, _ := m.EstimateCostUSD(result.Usage); cost != 0.01 {
		t.Fatalf("estimate %v", cost)
	}

	calls := opencodetest.Calls(t, log)
	if len(calls) != 1 {
		t.Fatalf("calls: %d", len(calls))
	}
	call := calls[0]
	argv := call.Argv
	if strings.Join(argv[:5], " ") != "run --format json --auto --agent" {
		t.Fatalf("argv %q", argv)
	}
	if got, _ := argAfter(argv, "--agent"); got != "lha-model" {
		t.Fatalf("agent %q", got)
	}
	if _, ok := argAfter(argv, "--standalone"); !ok {
		t.Fatalf("argv %q", argv)
	}
	if got, _ := argAfter(argv, "--model"); got != "anthropic/claude-sonnet-4-5" {
		t.Fatalf("model %q", got)
	}
	if call.ProjectDisabled != "true" {
		t.Fatalf("project_disabled %q", call.ProjectDisabled)
	}
	agents, _ := call.Config["agents"].(map[string]any)
	agent, _ := agents["lha-model"].(map[string]any)
	if agent["system"] != "SYSTEM PROMPT" {
		t.Fatalf("config agents: %v", agent)
	}
	perms, _ := agent["permissions"].([]any)
	if len(perms) != 1 || !reflect.DeepEqual(perms[0], map[string]any{"action": "*", "resource": "*", "effect": "deny"}) {
		t.Fatalf("permissions %v", perms)
	}
	if _, ok := call.Config["mcp"]; ok {
		t.Fatalf("a plain text turn must have no tools: %v", call.Config)
	}
	for _, want := range []string{"do the thing", "file a", `{"tool": "read_file", "arguments": {"path": "a"}}`} {
		if !strings.Contains(call.Stdin, want) {
			t.Errorf("stdin lacks %q:\n%s", want, call.Stdin)
		}
	}
}

func TestOpenCodeModelSingleTurnPromptIsSentVerbatim(t *testing.T) {
	bin, log := opencodetest.Install(t)
	m := NewOpenCode(OpenCodeOptions{Binary: bin})
	if _, err := m.Complete(context.Background(), []contracts.ModelMessage{user("plan this")}, nil, 0); err != nil {
		t.Fatal(err)
	}
	call := opencodetest.Calls(t, log)[0]
	if call.Stdin != "plan this" {
		t.Fatalf("stdin %q", call.Stdin)
	}
	if _, ok := argAfter(call.Argv, "--model"); ok { // the default model is OpenCode's own choice
		t.Fatalf("argv %q", call.Argv)
	}
}

func TestOpenCodeModelReportsAMissingBinary(t *testing.T) {
	m := NewOpenCode(OpenCodeOptions{Binary: "/nonexistent/opencode", MaxRetries: Int(0)})
	_, err := m.Complete(context.Background(), []contracts.ModelMessage{user("x")}, nil, 0)
	if err == nil || !strings.Contains(err.Error(), "LHA_OPENCODE_BIN") {
		t.Fatalf("err = %v", err)
	}
	ok, detail := m.HealthCheck(context.Background(), 1)
	if ok || !strings.Contains(detail, "not found") {
		t.Fatalf("health %v %q", ok, detail)
	}
}

func TestOpenCodeWorstCaseIsTheBudgetCapUnlessTheModelIsPriced(t *testing.T) {
	unpriced := NewOpenCode(OpenCodeOptions{ModelName: "x", MaxBudgetUSD: 3.0})
	big := contracts.Usage{InputTokens: 10_000_000, OutputTokens: 10_000_000}
	if c, _ := unpriced.EstimateCostUSD(big); c != 3.0 {
		t.Fatalf("unpriced %v", c)
	}
	price := NewModelPrice(1.0, 1.0)
	priced := NewOpenCode(OpenCodeOptions{ModelName: "x", MaxBudgetUSD: 3.0, Price: &price})
	if c, _ := priced.EstimateCostUSD(contracts.Usage{InputTokens: 1_000_000}); c != 1.0 {
		t.Fatalf("priced %v", c)
	}
	if c, _ := priced.EstimateCostUSD(big); c != 3.0 { // never above the cap the engine enforces
		t.Fatalf("capped %v", c)
	}
}

func TestOpenCodeHealthRunsTheCLIVersion(t *testing.T) {
	bin, _ := opencodetest.Install(t)
	ok, detail := NewOpenCode(OpenCodeOptions{Binary: bin}).HealthCheck(context.Background(), 10)
	if !ok || detail != "opencode:default: 0.0.0-fake (OpenCode)" {
		t.Fatalf("%v %q", ok, detail)
	}
}

func TestBuildProviderOpenCode(t *testing.T) {
	load := func(env ...string) *config.Settings {
		s, err := config.LoadFrom(env, "")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	p, err := BuildProvider(load("LHA_MODEL_BACKEND=opencode"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*OpenCodeModel); !ok || p.Name() != "opencode:default" {
		t.Fatalf("%T %s", p, p.Name())
	}
	named, _ := BuildProvider(load("LHA_MODEL_BACKEND=opencode", "LHA_MODEL_NAME=anthropic/claude-sonnet-4-5"), "", nil)
	if named.Name() != "opencode:anthropic/claude-sonnet-4-5" {
		t.Fatal(named.Name())
	}
	// The engine alone routes the other roles through opencode too; an explicit backend wins.
	if s := load("LHA_LEAD_ENGINE=opencode"); s.ModelBackend != "opencode" {
		t.Fatal(s.ModelBackend)
	}
	if s := load("LHA_LEAD_ENGINE=opencode", "LHA_MODEL_BACKEND=stub"); s.ModelBackend != "stub" {
		t.Fatal(s.ModelBackend)
	}
}

func TestOpenCodeHealthReportsTheVersion(t *testing.T) {
	// The fake prints its version without spending tokens; the health check passes it through.
	bin, _ := opencodetest.Install(t)
	out, timedOut, err := runOpenCodeCLI(context.Background(), bin, []string{"--version"}, 10)
	if err != nil || timedOut || !strings.Contains(string(out), "0.0.0-fake") {
		t.Fatalf("%q %v %v", out, timedOut, err)
	}
}

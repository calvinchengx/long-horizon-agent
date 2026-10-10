package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent/mcpbridge"
	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model/opencodetest"
)

// Ported from python/tests/unit/test_opencode.py (the opencode lead engine, end to end). The fake
// opencode (opencodetest) is a real MCP client of the engine's bridge: the argv, the injected
// config, the bridge, the dispatcher, the verifier, the checkpoint and the ledger are all real.

// openCodeEngineSettings is python's _engine_settings (no explicit model backend, so the other
// roles go through opencode too).
func openCodeEngineSettings(t *testing.T, bin string, env ...string) *config.Settings {
	base := []string{"LHA_SANDBOX=local", "LHA_ALLOW_UNSAFE_LOCAL=true", "LHA_LEAD_ENGINE=opencode",
		"LHA_OPENCODE_BIN=" + bin, "LHA_OPENCODE_MAX_BUDGET_USD=1.0", "LHA_BUDGET_USD_CEILING=5.0",
		"LHA_MAX_CYCLES=3"}
	return ccSettings(t, append(base, env...)...)
}

func setOpenCodeCalls(t *testing.T, calls ...any) {
	raw, _ := json.Marshal(calls)
	t.Setenv("FAKE_OPENCODE_CALLS", string(raw))
}

func argAfter(argv []string, flag string) (string, bool) {
	for i, a := range argv {
		if a == flag && i+1 < len(argv) {
			return argv[i+1], true
		}
	}
	return "", false
}

func TestOpenCodeLeadEngineFromSettings(t *testing.T) {
	if e, err := LeadEngine(ccSettings(t), false); e != nil || err != nil {
		t.Fatalf("%v %v", e, err)
	}
	e, err := LeadEngine(ccSettings(t, "LHA_LEAD_ENGINE=opencode", "LHA_OPENCODE_MODEL=m"), false)
	if err != nil || e.Native() || e.Name() != "opencode_engine:m" || e.SessionEvent() != "opencode_session" {
		t.Fatalf("%v %v", e, err)
	}

	native := []string{"LHA_LEAD_ENGINE=opencode", "LHA_OPENCODE_TOOLS=native"}
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
	if _, err := NewOpenCodeEngine(OpenCodeEngineOptions{Tools: "bogus"}); err == nil || err.Error() != "unknown OpenCode tool mode: 'bogus'" {
		t.Fatalf("err = %v", err)
	}
}

func TestOpenCodeLhaModeDeniesEveryActionButLhaTools(t *testing.T) {
	e, _ := NewOpenCodeEngine(OpenCodeEngineOptions{})
	want := []map[string]string{
		{"action": "*", "resource": "*", "effect": "deny"},
		{"action": "lha_*", "resource": "*", "effect": "allow"},
	}
	if !reflect.DeepEqual(e.permissions(), want) {
		t.Fatalf("permissions %v", e.permissions())
	}
	config := e.config("sys", mcpbridge.New(nil))
	mcp, _ := config["mcp"].(map[string]any)
	servers, _ := mcp["servers"].(map[string]any)
	server, _ := servers["lha"].(map[string]any)
	if server["type"] != "remote" || server["codemode"] != false {
		t.Fatalf("server %v", server)
	}
	agents, _ := config["agents"].(map[string]any)
	agent, _ := agents["lha"].(map[string]any)
	if agent["system"] != "sys" {
		t.Fatalf("agent %v", agent)
	}
}

func TestOpenCodeNativeModeDeniesHistoryPublishingAndTheWeb(t *testing.T) {
	e, _ := NewOpenCodeEngine(OpenCodeEngineOptions{Tools: "native"})
	rules := e.permissions()
	if !reflect.DeepEqual(rules, OpenCodeNativeDeny) {
		t.Fatalf("rules %v", rules)
	}
	has := func(want map[string]string) bool {
		for _, rule := range rules {
			if reflect.DeepEqual(rule, want) {
				return true
			}
		}
		return false
	}
	if !has(map[string]string{"action": "shell", "resource": "git push *", "effect": "deny"}) ||
		!has(map[string]string{"action": "webfetch", "resource": "*", "effect": "deny"}) {
		t.Fatalf("rules %v", rules)
	}
}

func TestACycleIsOneOpenCodeSessionUsingLHATools(t *testing.T) {
	bin, log := opencodetest.Install(t)
	t.Setenv("FAKE_OPENCODE_MODE", "mcp")
	setOpenCodeCalls(t, []any{"write_file", map[string]any{"path": "hello.txt", "content": "hi\n"}}, []any{"verify", map[string]any{}})
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	summary := engineRun(t, ws, openCodeEngineSettings(t, bin), helloChecklist("cmd:grep -qx hi hello.txt"), nil)

	if !summary.Completed || summary.Cycles != 1 {
		t.Fatalf("%+v", summary)
	}
	if d := summary.TotalUSD - 0.21*3; d > 1e-9 || d < -1e-9 { // the cost OpenCode reported
		t.Fatalf("total %v", summary.TotalUSD)
	}
	calls := opencodetest.Calls(t, log)
	if len(calls) != 1 {
		t.Fatalf("sessions: %d", len(calls))
	}
	session := calls[0]
	argv := session.Argv
	if strings.Join(argv[:5], " ") != "run --format json --auto --agent" {
		t.Fatalf("argv %q", argv)
	}
	if after(argv, "--standalone") == nil {
		t.Fatalf("argv %q", argv)
	}
	if got, _ := argAfter(argv, "--agent"); got != "lha" {
		t.Fatalf("agent %q", got)
	}
	mcp, _ := session.Config["mcp"].(map[string]any)
	servers, _ := mcp["servers"].(map[string]any)
	server, _ := servers["lha"].(map[string]any)
	if server["codemode"] != false {
		t.Fatalf("server %v", server)
	}
	agents, _ := session.Config["agents"].(map[string]any)
	agent, _ := agents["lha"].(map[string]any)
	perms, _ := agent["permissions"].([]any)
	if len(perms) != 2 || perms[0].(map[string]any)["action"] != "*" || perms[1].(map[string]any)["action"] != "lha_*" {
		t.Fatalf("permissions %v", perms)
	}
	if system, _ := agent["system"].(string); strings.Contains(system, `{"tool"`) {
		t.Fatalf("system %s", system)
	}
	if !strings.Contains(session.Stdin, "Create hello.txt") || !strings.Contains(session.Stdin, "cmd:grep -qx hi hello.txt") {
		t.Fatalf("stdin %s", session.Stdin)
	}
	if got, _ := os.ReadFile(filepath.Join(ws, "hello.txt")); string(got) != "hi\n" { // through LHA's write_file
		t.Fatalf("hello.txt %q", got)
	}
	if !strings.Contains(summary.TraceJSONL, "opencode_session") {
		t.Fatalf("trace %s", summary.TraceJSONL)
	}
	var verified []map[string]any
	var progress []map[string]any
	for _, line := range strings.Split(summary.TraceJSONL, "\n") {
		var e struct {
			Kind string         `json:"kind"`
			Data map[string]any `json:"data"`
		}
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		switch e.Kind {
		case "verify":
			verified = append(verified, e.Data)
		case "session_progress":
			progress = append(progress, e.Data)
		}
	}
	if len(verified) != 1 || verified[0]["trigger"] != "tool" || verified[0]["verdict"] != "passed" {
		t.Fatalf("verify events %v", verified)
	}
	if len(progress) == 0 || progress[len(progress)-1]["turns"].(float64) < 2 {
		t.Fatalf("progress %v", progress)
	}
}

func TestAnOpenCodeSessionKilledAtItsCapIsStillVerifiedAndCharged(t *testing.T) {
	bin, _ := opencodetest.Install(t)
	t.Setenv("FAKE_OPENCODE_MODE", "budget")
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("already here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	summary := engineRun(t, ws, openCodeEngineSettings(t, bin, "LHA_OPENCODE_MAX_BUDGET_USD=0.5"), helloChecklist(), nil)
	if !summary.Completed { // the work in the workdir passed, though the session hit its cap
		t.Fatalf("%+v", summary)
	}
	if d := summary.TotalUSD - 0.9; d > 1e-9 || d < -1e-9 { // what the session streamed before the kill
		t.Fatalf("total %v", summary.TotalUSD)
	}
	if !strings.Contains(summary.TraceJSONL, "error_max_budget_usd") {
		t.Fatalf("trace %s", summary.TraceJSONL)
	}
}

func TestAnOpenCodeSessionKilledAtItsTimeoutIsChargedWhatItSpent(t *testing.T) {
	bin, _ := opencodetest.Install(t)
	t.Setenv("FAKE_OPENCODE_MODE", "hang")
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("already here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	summary := engineRun(t, ws, openCodeEngineSettings(t, bin, "LHA_OPENCODE_TIMEOUT_S=3"), helloChecklist(), nil)
	if !summary.Completed { // the work in the workdir is still verified
		t.Fatalf("%+v", summary)
	}
	if d := summary.TotalUSD - 0.0153; d > 1e-9 || d < -1e-9 { // its one turn, not the $1 cap
		t.Fatalf("total %v", summary.TotalUSD)
	}
	stopped := ""
	for _, line := range strings.Split(summary.TraceJSONL, "\n") {
		var e struct {
			Kind string         `json:"kind"`
			Data map[string]any `json:"data"`
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.Kind == "opencode_session" {
			stopped, _ = e.Data["stopped"].(string)
		}
	}
	if !strings.Contains(stopped, "did not finish within 3s") {
		t.Fatalf("stopped %q", stopped)
	}
}

func TestAKilledOpenCodeSessionOnAnUnpricedModelIsChargedItsCap(t *testing.T) {
	bin, _ := opencodetest.Install(t)
	t.Setenv("FAKE_OPENCODE_MODE", "hang-unpriced")
	ws := filepath.Join(t.TempDir(), "ws")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "hello.txt"), []byte("already here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	summary := engineRun(t, ws, openCodeEngineSettings(t, bin, "LHA_OPENCODE_TIMEOUT_S=3"), helloChecklist(), nil)
	if d := summary.TotalUSD - 1.0; d > 1e-9 || d < -1e-9 { // the session's cap: its spend had no cost
		t.Fatalf("total %v", summary.TotalUSD)
	}
}

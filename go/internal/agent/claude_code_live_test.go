package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent/agenttest"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// TestLiveClaudeCodeEngineSmoke runs one real claude -p session against the MCP bridge: Claude
// Code must list and call LHA's tools. Opt-in only (it spends tokens against the local Claude
// Code login): LHA_IT_CLAUDE_CODE=1 and a claude binary on PATH (or LHA_CLAUDE_CODE_BIN).
func TestLiveClaudeCodeEngineSmoke(t *testing.T) {
	if os.Getenv("LHA_IT_CLAUDE_CODE") != "1" {
		t.Skip("set LHA_IT_CLAUDE_CODE=1 to run the live claude -p engine smoke test")
	}
	bin := os.Getenv("LHA_CLAUDE_CODE_BIN")
	if bin == "" {
		bin = "claude"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s not on PATH", bin)
	}
	dir := t.TempDir()
	box := agenttest.NewToolbox(dir)
	e, err := NewClaudeCodeEngine(ClaudeCodeEngineOptions{Binary: bin, Model: os.Getenv("LHA_MODEL_NAME"), MaxBudgetUSD: 1.0, TimeoutS: 600})
	if err != nil {
		t.Fatal(err)
	}
	verified := false
	run, err := e.Run(context.Background(), EngineRequest{
		Messages: []contracts.ModelMessage{
			{Role: "system", Content: "You are a test fixture in a harness. Use only the tools you are given."},
			{Role: "user", Content: "Use the write_file tool to write the text hi (no newline) to hello.txt, then call the verify tool, then stop."},
		},
		Cwd: dir, CycleID: "c1", Specs: box.Disp.Specs(),
		Dispatch: func(ctx context.Context, call contracts.ToolCall) contracts.ToolResult {
			return box.Disp.Dispatch(ctx, call, contracts.ToolContext{Session: box.Sess})
		},
		Verify: func(context.Context) (contracts.VerificationResult, error) {
			data, _ := os.ReadFile(filepath.Join(dir, "hello.txt"))
			verified = string(data) == "hi"
			return contracts.NewVerificationResult([]contracts.CheckResult{{Name: "hello", Passed: verified, Gating: true}}), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%+v", run)
	if run.Stopped != "" || !verified || run.ToolCalls == 0 {
		t.Fatalf("session did not use the bridged tools: %+v", run)
	}
}

package model

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// TestLiveClaudeCodeSmoke runs one real claude -p turn. Opt-in only (it spends tokens against the
// local Claude Code login): LHA_IT_CLAUDE_CODE=1 and a claude binary on PATH (or
// LHA_CLAUDE_CODE_BIN).
func TestLiveClaudeCodeSmoke(t *testing.T) {
	if os.Getenv("LHA_IT_CLAUDE_CODE") != "1" {
		t.Skip("set LHA_IT_CLAUDE_CODE=1 to run the live claude -p smoke test")
	}
	bin := os.Getenv("LHA_CLAUDE_CODE_BIN")
	if bin == "" {
		bin = "claude"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s not on PATH", bin)
	}
	m := NewClaudeCode(ClaudeCodeOptions{Binary: bin, ModelName: os.Getenv("LHA_MODEL_NAME"), MaxBudgetUSD: 0.5, TimeoutS: 300, MaxRetries: Int(0)})
	if ok, detail := m.HealthCheck(context.Background(), 30); !ok {
		t.Fatal(detail)
	}
	result, err := m.Complete(context.Background(), []contracts.ModelMessage{
		{Role: "system", Content: "You are a test fixture. Reply with the single word you are asked for."},
		{Role: "user", Content: "Reply with exactly: pong"},
	}, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToLower(result.Text), "pong") || result.SessionID == nil || result.Usage.ReportedCostUSD == nil {
		t.Fatalf("%+v", result)
	}
	t.Logf("model %s cost $%.4f", result.Usage.Model, *result.Usage.ReportedCostUSD)
}

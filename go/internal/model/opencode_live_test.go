package model

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// TestLiveOpenCodeSmoke runs one real opencode run turn. Opt-in only (it spends against the local
// OpenCode login): LHA_IT_OPENCODE=1 and an opencode binary on PATH (or LHA_OPENCODE_BIN).
func TestLiveOpenCodeSmoke(t *testing.T) {
	if os.Getenv("LHA_IT_OPENCODE") != "1" {
		t.Skip("set LHA_IT_OPENCODE=1 to run the live opencode run smoke test")
	}
	bin := os.Getenv("LHA_OPENCODE_BIN")
	if bin == "" {
		bin = "opencode"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s not on PATH", bin)
	}
	m := NewOpenCode(OpenCodeOptions{
		Binary: bin, ModelName: os.Getenv("LHA_OPENCODE_MODEL"),
		MaxBudgetUSD: 0.5, TimeoutS: 300, MaxRetries: Int(0),
	})
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
	t.Logf("model %s cost $%.6f", result.Usage.Model, *result.Usage.ReportedCostUSD)
}

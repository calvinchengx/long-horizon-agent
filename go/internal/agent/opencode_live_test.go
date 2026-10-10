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

// liveXDGDataHome is the developer's XDG_DATA_HOME captured before TestMain repoints it at a temp
// store for the fake-binary tests. The live smoke restores it, so the child OpenCode finds its
// own auth and model catalog: TestMain's isolated data dir would hide them, and every model would
// come back "unavailable" (no route).
var liveXDGDataHome = os.Getenv("XDG_DATA_HOME")

// TestLiveOpenCodeEngineSmoke runs one real opencode run session against the MCP bridge: OpenCode
// must list and call LHA's tools. Opt-in only (it spends against the local OpenCode login):
// LHA_IT_OPENCODE=1 and an opencode binary on PATH (or LHA_OPENCODE_BIN). Pin the model with
// LHA_OPENCODE_MODEL when OpenCode's default is a free model that cannot route.
func TestLiveOpenCodeEngineSmoke(t *testing.T) {
	if os.Getenv("LHA_IT_OPENCODE") != "1" {
		t.Skip("set LHA_IT_OPENCODE=1 to run the live opencode run engine smoke test")
	}
	bin := os.Getenv("LHA_OPENCODE_BIN")
	if bin == "" {
		bin = "opencode"
	}
	if _, err := exec.LookPath(bin); err != nil {
		t.Skipf("%s not on PATH", bin)
	}
	t.Setenv("XDG_DATA_HOME", liveXDGDataHome) // the child's real OpenCode data (see above)
	dir := t.TempDir()
	box := agenttest.NewToolbox(dir)
	e, err := NewOpenCodeEngine(OpenCodeEngineOptions{Binary: bin, Model: os.Getenv("LHA_OPENCODE_MODEL"), MaxBudgetUSD: 1.0, TimeoutS: 600})
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

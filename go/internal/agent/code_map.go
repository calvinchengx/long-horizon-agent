package agent

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// The cycle-start code map (python: lha.agent.code_map), LHA_CODE_MAP=ripwire: the harness runs
// `ripwire . --pack-task=<item> --token-budget=N` once per cycle, inside the sandbox (it parses
// agent-written code), and puts the task bundle into the lead's first message after the memory
// block. ripwire is deterministic, offline and writes nothing into the workspace. The map is
// advisory; a missing ripwire, a failure or a timeout means no map for that cycle, never a failed
// cycle.

// CodeMapArgv is the ripwire command for item (no shell: the description is one argument).
func CodeMapArgv(item contracts.ChecklistItem, tokenBudget int) []string {
	return []string{"ripwire", ".", "--pack-task=" + item.Description, fmt.Sprintf("--token-budget=%d", tokenBudget)}
}

// RipwireCodeMap renders ripwire's task bundle for an item, in the cycle's sandbox session.
type RipwireCodeMap struct {
	TokenBudget int
	TimeoutS    float64
}

// Render is the rendered section ("" when unavailable) and the code_map event fields.
func (m *RipwireCodeMap) Render(ctx context.Context, session contracts.SandboxSession, item contracts.ChecklistItem) (string, *contracts.OrderedMap) {
	started := time.Now()
	res, err := session.Exec(ctx, CodeMapArgv(item, m.TokenBudget), contracts.ExecOptions{TimeoutS: max(1, int(m.TimeoutS))})
	if err != nil {
		return "", contracts.NewOrderedMap("ok", false, "error", pyfmt.Head(fmt.Sprintf("%T: %v", err, err), 300))
	}
	info := contracts.NewOrderedMap(
		"ok", res.OK(),
		"exit_code", res.ExitCode,
		"timed_out", res.TimedOut,
		"bytes", len(res.Stdout),
		"duration_s", math.Round(time.Since(started).Seconds()*1000)/1000,
	)
	if !res.OK() {
		detail := res.Stderr
		if detail == "" {
			detail = res.Stdout
		}
		info.Set("error", pyfmt.Tail(detail, 300))
		return "", info
	}
	return RenderCodeMap(res.Stdout), info
}

package agent

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
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

// TraceChars is how much of the failure report (its end, where the errors are) --from-trace gets.
const TraceChars = 4000

// TraceScript feeds the failure report to ripwire; the report is only ever data (python:
// TRACE_SCRIPT).
const TraceScript = `printf %s "$LHA_TRACE" | ripwire . --from-trace=- --token-budget="$LHA_TRACE_BUDGET"`

// CodeMapQuery is the task query: the description, plus the acceptance checks when the item has
// any (python: code_map_query).
func CodeMapQuery(item contracts.ChecklistItem) string {
	if len(item.Witnesses) == 0 {
		return item.Description
	}
	return item.Description + "\nAcceptance checks: " + strings.Join(item.Witnesses, ", ")
}

// CodeMapArgv is the ripwire task command for item (no shell: the query is one argument).
func CodeMapArgv(item contracts.ChecklistItem, tokenBudget int) []string {
	return []string{"ripwire", ".", "--pack-task=" + CodeMapQuery(item), fmt.Sprintf("--token-budget=%d", tokenBudget)}
}

// TraceArgv runs TraceScript.
func TraceArgv() []string { return []string{"sh", "-c", TraceScript} }

// TraceEnv is the trace command's environment: the end of the last failure report and the budget.
func TraceEnv(item contracts.ChecklistItem, tokenBudget int) map[string]string {
	return map[string]string{
		"LHA_TRACE":        pyfmt.Tail(item.LastFailure, TraceChars),
		"LHA_TRACE_BUDGET": strconv.Itoa(tokenBudget),
	}
}

// FoundCode reports whether a ripwire answer names any code location (a row with a p= path).
func FoundCode(output string) bool { return strings.Contains(output, ` p="`) }

// RipwireCodeMap renders ripwire's task bundle for an item, in the cycle's sandbox session.
type RipwireCodeMap struct {
	TokenBudget int
	TimeoutS    float64
}

// Render is the rendered section ("" when unavailable) and the code_map event fields. A retry (the
// item has a failure report) tries the trace first; the task query is used when there is no report
// or the trace finds no code.
func (m *RipwireCodeMap) Render(ctx context.Context, session contracts.SandboxSession, item contracts.ChecklistItem) (string, *contracts.OrderedMap) {
	started := time.Now()
	elapsed := func() float64 { return math.Round(time.Since(started).Seconds()*1000) / 1000 }
	fellBack := false
	if pyfmt.PyStrip(item.LastFailure) != "" {
		out, info := m.run(ctx, session, TraceArgv(), TraceEnv(item, m.TokenBudget))
		if info.Value("ok") == true && FoundCode(out) {
			info.Set("mode", "trace")
			info.Set("duration_s", elapsed())
			return RenderCodeMap(out), info
		}
		fellBack = true
	}
	out, info := m.run(ctx, session, CodeMapArgv(item, m.TokenBudget), nil)
	info.Set("mode", "task")
	info.Set("fell_back", fellBack)
	info.Set("duration_s", elapsed())
	if info.Value("ok") != true {
		return "", info
	}
	return RenderCodeMap(out), info
}

func (m *RipwireCodeMap) run(ctx context.Context, session contracts.SandboxSession, argv []string, env map[string]string) (string, *contracts.OrderedMap) {
	res, err := session.Exec(ctx, argv, contracts.ExecOptions{TimeoutS: max(1, int(m.TimeoutS)), Env: env})
	if err != nil {
		return "", contracts.NewOrderedMap("ok", false, "error", pyfmt.Head(fmt.Sprintf("%T: %v", err, err), 300))
	}
	info := contracts.NewOrderedMap("ok", res.OK(), "exit_code", res.ExitCode, "timed_out", res.TimedOut, "bytes", len(res.Stdout))
	if !res.OK() {
		detail := res.Stderr
		if detail == "" {
			detail = res.Stdout
		}
		info.Set("error", pyfmt.Tail(detail, 300))
	}
	return res.Stdout, info
}

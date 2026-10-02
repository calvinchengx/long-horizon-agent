package ops

// lha mission-report: one page about a mission, from its anchor and the mission store
// (python: lha.ops.report). Everything a mission leaves behind is spread over the anchor's
// committed files, the store's rows and the git history; RenderReport joins what an operator
// asks for after the fact into one deterministic text, the same bytes from both implementations
// (spec/state/report.json). It reads; it changes nothing.

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

func usd(value *float64) string {
	if value == nil {
		return "unknown"
	}
	return fmt.Sprintf("$%.4f", *value)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// FormatGateRow is the human-readable lines for one recorded gate (lha gates, the report;
// python: format_gate_row).
func FormatGateRow(row persistence.GateRow) []string {
	var decided string
	if row.Status == persistence.GateResolved || row.Status == persistence.GateDefaulted {
		decision, by := "", "-"
		if row.Decision != nil {
			decision = *row.Decision
		}
		if row.ResolvedBy != nil && *row.ResolvedBy != "" {
			by = *row.ResolvedBy
		}
		decided = fmt.Sprintf("%s by %s at %s", decision, by, pyfmt.Head(row.ResolvedAt, 19))
	} else {
		decided = fmt.Sprintf("open, default %s at %s", orDash(row.DefaultAction), orDash(pyfmt.Head(row.Deadline, 19)))
	}
	lines := []string{
		fmt.Sprintf("%s  %s  %s  %-9s %-9s reminders %d  %s", pyfmt.Head(row.OpenedAt, 19), row.MissionID, row.GateID,
			row.Kind, row.Status, row.Reminders, decided),
		"  question: " + row.Question,
		"  options: " + strings.Join(row.Options, " | "),
	}
	if len(row.Request) > 0 {
		what := row.Request["argv"]
		if what == "" {
			what = row.Request["arguments"]
		}
		lines = append(lines, strings.TrimRight("  request: "+row.Request["tool"]+" "+what, " \t\n\r\v\f"))
	}
	return lines
}

// FormatCostTotal is the lha costs total line (python: format_cost_total).
func FormatCostTotal(summary persistence.CostSummary) string {
	known := summary.KnownUSD
	return fmt.Sprintf("total: %d calls  known %s  unknown-cost calls %d  tokens in %d out %d",
		summary.Calls, usd(&known), summary.UnknownCostCalls, summary.InputTokens, summary.OutputTokens)
}

// ReportInput is what the report is rendered from; nil store parts render as absent.
type ReportInput struct {
	MissionID string // "" when neither the anchor nor the caller names one
	Spec      *contracts.MissionSpec
	Checklist contracts.Checklist
	Events    []contracts.EventRecord
	Row       *persistence.MissionRow
	Gates     []persistence.GateRow
	Cost      *persistence.CostSummary
	Commits   int
	HeadSHA   string
}

func firstLine(text string, limit int) string {
	text = pyfmt.PyStrip(text)
	if text == "" {
		return ""
	}
	line := text
	if i := strings.IndexAny(text, "\n\r\v\f\x1c\x1d\x1e\u0085  "); i >= 0 {
		line = text[:i]
	}
	if utf8.RuneCountInString(line) <= limit {
		return line
	}
	return string([]rune(line)[:limit-3]) + "..."
}

func payloadString(e contracts.EventRecord, key string) (string, bool) {
	if e.Payload == nil {
		return "", false
	}
	v, ok := e.Payload.Get(key)
	if !ok || v == nil {
		return "", false
	}
	return fmt.Sprint(v), true
}

// RenderReport is the mission report (see the package comment); it ends with a newline.
func RenderReport(inp ReportInput) string {
	out := []string{}
	title, description := "(unknown)", ""
	if inp.Spec != nil {
		title, description = inp.Spec.Title, inp.Spec.Description
	} else if inp.Row != nil {
		title, description = inp.Row.Title, inp.Row.Description
	}
	out = append(out, "# Mission: "+title)
	if pyfmt.PyStrip(description) != "" {
		out = append(out, pyfmt.PyStrip(description))
	}
	if inp.Spec != nil && pyfmt.PyStrip(inp.Spec.Acceptance) != "" {
		out = append(out, "Definition of done: "+pyfmt.PyStrip(inp.Spec.Acceptance))
	}
	status := "-"
	if inp.Row != nil {
		status = inp.Row.Status
	}
	out = append(out, fmt.Sprintf("mission %s  status %s  head %s  commits %d", orDash(inp.MissionID), status,
		orDash(pyfmt.Head(inp.HeadSHA, 12)), inp.Commits))

	items := inp.Checklist.Items
	state := ""
	if len(items) > 0 && inp.Checklist.IsComplete() {
		state = ", complete"
	} else if len(items) > 0 && inp.Checklist.IsDeadlocked() {
		state = ", deadlocked: " + inp.Checklist.DeadlockReason()
	}
	out = append(out, "", fmt.Sprintf("## Items (%d/%d done%s)", inp.Checklist.ItemsDone(), len(items), state))
	if len(items) == 0 {
		out = append(out, "(no items)")
	}
	for _, item := range items {
		out = append(out, fmt.Sprintf("%s  %-11s attempts %2d  %s", item.ID, item.Status, item.Attempts, item.Description))
		if len(item.Witnesses) > 0 {
			out = append(out, "      witnesses: "+strings.Join(item.Witnesses, ", "))
		}
		if pyfmt.PyStrip(item.LastFailure) != "" && item.Status != contracts.StatusDone {
			out = append(out, "      last failure: "+firstLine(item.LastFailure, 100))
		}
	}

	cycles, passed, failed := 0, 0, 0
	reviews, approve, block := 0, 0, 0
	screens, flagged, reflections := 0, 0, 0
	for _, e := range inp.Events {
		switch e.Kind {
		case "cycle":
			if verdict, ok := payloadString(e, "verdict"); ok {
				cycles++
				switch verdict {
				case "passed":
					passed++
				case "failed":
					failed++
				}
			}
		case "review":
			reviews++
			verdict, _ := payloadString(e, "verdict")
			switch verdict {
			case "approve":
				approve++
			case "block":
				block++
			}
		case "review_screen":
			screens++
			if e.Payload != nil {
				if list, ok := contracts.PlainJSON(e.Payload.Value("findings")).([]any); ok && len(list) > 0 {
					flagged++
				}
			}
		case "reflection":
			reflections++
		}
	}
	out = append(out, "", fmt.Sprintf("## Cycles (%d)", cycles),
		fmt.Sprintf("verdicts: passed %d, failed %d, other %d", passed, failed, cycles-passed-failed))
	if reviews > 0 {
		out = append(out, fmt.Sprintf("reviews: approve %d, block %d, unparsed %d", approve, block, reviews-approve-block))
	}
	if screens > 0 {
		out = append(out, fmt.Sprintf("screens: %d of %d diffs flagged", flagged, screens))
	}
	if reflections > 0 {
		out = append(out, fmt.Sprintf("reflections: %d", reflections))
	}

	out = append(out, "", fmt.Sprintf("## Gates (%d)", len(inp.Gates)))
	switch {
	case inp.MissionID == "":
		out = append(out, "(mission id unknown: pass MISSION_ID to read the store)")
	case len(inp.Gates) == 0:
		out = append(out, "none recorded")
	}
	for _, gate := range inp.Gates {
		out = append(out, FormatGateRow(gate)...)
	}

	out = append(out, "", "## Cost")
	switch {
	case inp.MissionID == "":
		out = append(out, "(mission id unknown: pass MISSION_ID to read the store)")
	case inp.Cost == nil || inp.Cost.Calls == 0:
		out = append(out, "no cost ledger rows")
	default:
		out = append(out, FormatCostTotal(*inp.Cost))
	}
	return strings.Join(out, "\n") + "\n"
}

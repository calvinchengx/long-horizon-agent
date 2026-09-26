package agents

import (
	"context"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// The Reviewer's objective and verdict parsing (python: lha.agents.reviewer), and within-mission
// reflection (python: lha.agents.reflection). The model-driving halves (the Reviewer role
// runner) live in agents/org, next to the SubAgent they use.

// ReviewDiffCap bounds the diff shown to the Reviewer.
const ReviewDiffCap = 8000

// ReviewObjective is the Reviewer's objective.
const ReviewObjective = "Review the diff against the acceptance criteria. Identify correctness, security, and scope " +
	"issues. Investigate with the tools if needed. When finished, reply with EXACTLY one JSON " +
	`object: {"done": true, "verdict": "approve" | "block", ` +
	`"blocking_issues": ["<issue that must be fixed before merge>", ...], ` +
	`"advisory": ["<non-blocking suggestion>", ...], "summary": "<one line>"}. ` +
	`Use "block" only if blocking_issues is non-empty.`

var (
	approveVerdicts = map[string]bool{"approve": true, "approved": true, "pass": true, "lgtm": true, "accept": true, "ok": true}
	blockVerdicts   = map[string]bool{"block": true, "blocked": true, "blocking": true, "reject": true, "request_changes": true, "fail": true}
)

// ReviewExtraContext is the Reviewer's extra context: the acceptance criteria and the (capped)
// diff.
func ReviewExtraContext(criteria, diff string) string {
	return "Acceptance criteria: " + criteria + "\n\nDIFF:\n" + pyfmt.Head(diff, ReviewDiffCap)
}

// ReviewResult is a structured review verdict.
type ReviewResult struct {
	Brief          string
	Blocking       bool
	ToolCalls      int
	Verdict        string // "approve" | "block" | "unparsed"
	BlockingIssues []string
	Advisory       []string
}

// Notes are the human/agent-readable review notes (used when reopening an item).
func (r ReviewResult) Notes() string {
	lines := []string{"Review verdict: " + r.Verdict}
	for _, issue := range r.BlockingIssues {
		lines = append(lines, "- BLOCKING: "+issue)
	}
	for _, note := range r.Advisory {
		lines = append(lines, "- advisory: "+note)
	}
	if len(r.BlockingIssues) == 0 && r.Brief != "" {
		lines = append(lines, pyfmt.Head(r.Brief, 1000))
	}
	return strings.Join(lines, "\n")
}

// ExtractJSONObject is the outermost {...} in text parsed as a JSON object (key order kept), or
// nil.
func ExtractJSONObject(text string) *pyfmt.OrderedMap {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start == -1 || end == -1 || end < start {
		return nil
	}
	parsed, err := pyfmt.DecodeOrdered([]byte(text[start : end+1]))
	if err != nil {
		return nil
	}
	obj, _ := parsed.(*pyfmt.OrderedMap)
	return obj
}

func strList(value any) []string {
	list, ok := value.([]any)
	if !ok {
		return []string{}
	}
	out := []string{}
	for _, v := range list {
		if s := pyfmt.PyStrip(pyfmt.PyStr(v)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ParseReview parses a review reply into (verdict, blocking, blocking issues, advisory). A JSON
// verdict wins; otherwise explicit "BLOCK:" lines are the blocking issues; otherwise the result
// is conservative: blocking and "unparsed" (an unreviewable change is not approved).
func ParseReview(text string) (verdict string, blocking bool, issues []string, advisory []string) {
	obj := ExtractJSONObject(text)
	if obj != nil {
		_, hasVerdict := obj.Values["verdict"]
		rawIssues, hasIssues := obj.Values["blocking_issues"]
		if hasVerdict || hasIssues {
			issues = strList(rawIssues)
			advisory = strList(obj.Values["advisory"])
			raw, _ := obj.Values["verdict"].(string)
			v := strings.ToLower(pyfmt.PyStrip(raw))
			approveOrIssues := func() (string, bool, []string, []string) {
				// An "approve" that still lists blocking issues is contradictory: trust the issues.
				if len(issues) > 0 {
					return "block", true, issues, advisory
				}
				return "approve", false, []string{}, advisory
			}
			switch {
			case blockVerdicts[v]:
				return "block", true, issues, advisory
			case approveVerdicts[v]:
				return approveOrIssues()
			}
			if _, isList := rawIssues.([]any); isList {
				return approveOrIssues()
			}
			return "unparsed", true, issues, advisory
		}
	}
	// Fallback: explicit BLOCK: lines, else conservative (unparseable => not approved).
	marked := []string{}
	for _, line := range splitlines(text) {
		stripped := pyfmt.PyStrip(line)
		if strings.HasPrefix(strings.ToUpper(stripped), "BLOCK:") {
			runes := []rune(stripped)
			marked = append(marked, pyfmt.PyStrip(string(runes[min(6, len(runes)):])))
		}
	}
	if len(marked) > 0 {
		return "block", true, marked, []string{}
	}
	return "unparsed", true, marked, []string{}
}

// splitlines is Python's str.splitlines() (no keepends).
func splitlines(s string) []string {
	var out []string
	var cur strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch r {
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
			out = append(out, cur.String())
			cur.Reset()
		case '\r':
			out = append(out, cur.String())
			cur.Reset()
			if i+1 < len(runes) && runes[i+1] == '\n' {
				i++
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// --- reflection ------------------------------------------------------------------------------

// ReflectionCap bounds a reflection's text.
const ReflectionCap = 2000

// ReflectionSystemPrompt is the reflection call's system prompt.
const ReflectionSystemPrompt = "You are reflecting on a failed attempt (Reflexion-style). Write a SHORT " +
	"post-mortem: the likely root cause and a concrete different approach to try " +
	"next. No code."

// ReflectionMessages are the messages of a reflection call.
func ReflectionMessages(itemDescription, failureSummary string) []contracts.ModelMessage {
	return []contracts.ModelMessage{
		{Role: "system", Content: ReflectionSystemPrompt},
		{Role: "user", Content: "Item: " + itemDescription + "\n\nFailure:\n" + pyfmt.Head(failureSummary, 4000)},
	}
}

// ReflectOnFailure produces a short post-mortem for a failed attempt (no code, just lessons),
// prepended to the item's next attempt (Reflexion-style).
func ReflectOnFailure(ctx context.Context, model contracts.ModelProvider, itemDescription, failureSummary string) (string, error) {
	result, err := model.Complete(ctx, ReflectionMessages(itemDescription, failureSummary), nil, 0)
	if err != nil {
		return "", err
	}
	return pyfmt.Head(result.Text, ReflectionCap), nil
}

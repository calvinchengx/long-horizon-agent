package systemone

// Exporting a mission's own judgments as labels (lha labels export).
//
// A mission records three judgments that can fit the System One thresholds still to be measured
// (docs/25-system-one.md, "Not built yet"): a human's approve or reject of a gated tool call, the
// verifier's verdict on an attempt, and the reviewer's verdict on a verified diff. LabelRows turns
// the anchor's committed events and the store's hitl_gates rows into rows, redacted like every
// other record LHA writes; ToJSONL writes them. Both are pure and pinned by
// spec/systemone/labels.json (python: lha.systemone.labels).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
)

// LabelSchema is bumped when a row's shape changes, so a training script can refuse rows it
// does not know.
const LabelSchema = 1

// The sources a row comes from.
const (
	SourceGate         = "gate"          // hitl_gates: a human (or the default) answered a gate
	SourceToolApproval = "tool_approval" // the dispatcher's record of a gated tool call
	SourceVerifier     = "verifier"      // a cycle's verification verdict
	SourceReview       = "review"        // the reviewer's verdict on a verified diff
)

// Sources lists every source, in the order the summary line prints them.
var Sources = []string{SourceGate, SourceToolApproval, SourceVerifier, SourceReview}

// Anchor event kinds the rows come from.
const (
	RunEvent          = "orchestrate"
	CycleEvent        = "cycle"
	ToolApprovalEvent = "tool_approval"
	ReviewEvent       = "review"
	ReviewScreenEvent = "review_screen"
)

// DiffCap is how many characters of a reviewed diff a row keeps (--diffs).
const DiffCap = 20_000

var (
	checkFields    = []string{"name", "passed", "gating", "exit_code"} // no timings: they vary
	verifierFields = []string{"status", "verified", "tool_calls", "rolled_back", "split_into"}
	approvalFields = []string{"tool", "arguments", "reason", "fingerprint"}
	reviewFields   = []string{"base", "head", "blocking", "blocking_issues", "advisory"}
)

// LabelRow is one judgment: what was judged (Input) and the judgment (Label), by whom.
type LabelRow struct {
	Source    string
	Label     string
	By        string
	MissionID string
	CycleID   string
	ItemID    string
	At        string
	Input     map[string]any
}

// ToJSON is the row as the object a JSON Lines line holds.
func (r LabelRow) ToJSON() map[string]any {
	return map[string]any{
		"schema":     LabelSchema,
		"source":     r.Source,
		"label":      r.Label,
		"by":         r.By,
		"mission_id": r.MissionID,
		"cycle_id":   r.CycleID,
		"item_id":    r.ItemID,
		"at":         r.At,
		"input":      r.Input,
	}
}

// ToJSONL writes rows as JSON Lines: sorted keys, compact separators, no HTML escaping, so both
// implementations write the same bytes.
func ToJSONL(rows []LabelRow) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, row := range rows {
		if err := enc.Encode(row.ToJSON()); err != nil {
			panic(err) // maps of JSON values and strings always encode
		}
	}
	return buf.String()
}

// MissionIDOf is the mission id the anchor's latest orchestrate event names ("" when none does).
func MissionIDOf(events []contracts.EventRecord) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Kind == RunEvent {
			return strOr(events[i].Payload.Value("mission_id"), "")
		}
	}
	return ""
}

// LabelRows are the rows an anchor's committed events and the store's gates yield.
//
// Event rows come first, in log order; then the closed gates (RESOLVED or DEFAULTED), oldest
// opening first. A tool_approval event whose fingerprint a gate row also carries is the same
// decision recorded twice (the approver writes the gate, the dispatcher the event), so only the
// gate row is kept. missionID defaults to the one the latest orchestrate event names.
// diffs(base, head) supplies a reviewed diff (cut to DiffCap); nil leaves review rows with only
// the commit range.
func LabelRows(events []contracts.EventRecord, gates []persistence.GateRow, missionID string, diffs func(base, head string) string) []LabelRow {
	mission := missionID
	if mission == "" {
		mission = MissionIDOf(events)
	}
	closed := []persistence.GateRow{}
	for _, g := range gates {
		if g.Status == persistence.GateResolved || g.Status == persistence.GateDefaulted {
			closed = append(closed, g)
		}
	}
	sort.SliceStable(closed, func(i, j int) bool {
		if closed[i].OpenedAt != closed[j].OpenedAt {
			return closed[i].OpenedAt < closed[j].OpenedAt
		}
		return closed[i].GateID < closed[j].GateID
	})
	gated := map[string]bool{}
	for _, g := range closed {
		if fp := g.Request["fingerprint"]; fp != "" {
			gated[fp] = true
		}
	}
	// The pre-review screen's findings for each reviewed (cycle, item): the review row carries
	// them, so a threshold can be fitted against the reviewer's verdict.
	screens := map[[2]string][]any{}
	for _, e := range events {
		if e.Kind == ReviewScreenEvent && e.Payload != nil {
			list, _ := contracts.PlainJSON(e.Payload.Value("findings")).([]any)
			if list == nil {
				list = []any{}
			}
			screens[[2]string{e.CycleID, strOr(e.Payload.Value("item_id"), "")}] = list
		}
	}
	rows := []LabelRow{}
	for _, event := range events {
		if row, ok := eventRow(event, mission, gated, diffs, screens); ok {
			rows = append(rows, row)
		}
	}
	for _, g := range closed {
		rows = append(rows, gateRow(g))
	}
	return rows
}

func eventRow(event contracts.EventRecord, mission string, gated map[string]bool, diffs func(base, head string) string, screens map[[2]string][]any) (LabelRow, bool) {
	payload := event.Payload
	if payload == nil {
		payload = contracts.NewOrderedMap()
	}
	switch event.Kind {
	case ToolApprovalEvent:
		if gated[strOr(payload.Value("fingerprint"), "")] {
			return LabelRow{}, false
		}
		by := strOr(payload.Value("resolved_by"), "human")
		if truthy(payload.Value("defaulted")) {
			by = "default"
		}
		return row(SourceToolApproval, strOr(payload.Value("decision"), ""), by, mission, event.CycleID, "", pick(payload, approvalFields), ""), true
	case CycleEvent:
		verdict, ok := payload.Get("verdict")
		if !ok || verdict == nil {
			return LabelRow{}, false
		}
		data := pick(payload, verifierFields)
		if checks, ok := contracts.PlainJSON(payload.Value("checks")).([]any); ok {
			picked := []any{}
			for _, c := range checks {
				if m, ok := c.(map[string]any); ok {
					picked = append(picked, pickPlain(m, checkFields))
				}
			}
			data["checks"] = picked
		}
		return row(SourceVerifier, fmt.Sprint(verdict), "verifier", mission, event.CycleID, strOr(payload.Value("item_id"), ""), data, ""), true
	case ReviewEvent:
		verdict, ok := payload.Get("verdict")
		if !ok || verdict == nil {
			return LabelRow{}, false
		}
		data := pick(payload, reviewFields)
		if list, ok := screens[[2]string{event.CycleID, strOr(payload.Value("item_id"), "")}]; ok {
			data["screen_findings"] = list
		}
		base, head := strOr(payload.Value("base"), ""), strOr(payload.Value("head"), "")
		if diffs != nil && base != "" && head != "" {
			data["diff"] = headRunes(diffs(base, head), DiffCap)
		}
		return row(SourceReview, fmt.Sprint(verdict), "reviewer", mission, event.CycleID, strOr(payload.Value("item_id"), ""), data, ""), true
	}
	return LabelRow{}, false
}

func gateRow(gate persistence.GateRow) LabelRow {
	by := "human"
	if gate.Status == persistence.GateDefaulted {
		by = "default"
	} else if gate.ResolvedBy != nil && *gate.ResolvedBy != "" {
		by = *gate.ResolvedBy
	}
	options := []any{}
	for _, o := range gate.Options {
		options = append(options, o)
	}
	request := map[string]any{}
	for k, v := range gate.Request {
		request[k] = v
	}
	data := map[string]any{
		"kind":     gate.Kind,
		"question": gate.Question,
		"options":  options,
		"risk":     gate.Risk,
		"request":  request,
	}
	decision := ""
	if gate.Decision != nil {
		decision = *gate.Decision
	}
	return row(SourceGate, decision, by, gate.MissionID, "", "", data, gate.ResolvedAt)
}

func row(source, label, by, mission, cycleID, itemID string, data map[string]any, at string) LabelRow {
	return LabelRow{
		Source: source, Label: label, By: by, MissionID: mission, CycleID: cycleID, ItemID: itemID, At: at,
		Input: obs.RedactMapping(data),
	}
}

// pick copies the payload's keys (as plain JSON values) in the order given; missing keys are left out.
func pick(payload *contracts.OrderedMap, keys []string) map[string]any {
	out := map[string]any{}
	for _, key := range keys {
		if v, ok := payload.Get(key); ok {
			out[key] = contracts.PlainJSON(v)
		}
	}
	return out
}

func pickPlain(m map[string]any, keys []string) map[string]any {
	out := map[string]any{}
	for _, key := range keys {
		if v, ok := m[key]; ok {
			out[key] = v
		}
	}
	return out
}

// strOr is python's str(value or default) for the string-or-missing payload fields.
func strOr(v any, def string) string {
	switch x := v.(type) {
	case nil:
		return def
	case string:
		if x == "" {
			return def
		}
		return x
	}
	if !truthy(v) {
		return def
	}
	return fmt.Sprint(v)
}

// truthy is python's bool(value) for JSON values.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case json.Number:
		f, _ := x.Float64()
		return f != 0
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	}
	if f, ok := contracts.AsFloat(v); ok {
		return f != 0
	}
	return true
}

func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

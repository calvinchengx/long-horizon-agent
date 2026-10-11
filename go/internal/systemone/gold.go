package systemone

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

// Gold evaluation sets (eval/gold/*.jsonl, lha eval; python: lha.systemone.gold): label rows with
// the judgment a correct judge would have given, and the scoring of a judge against them. A gold
// row is an lha labels export row (schema 1) plus a "gold" object ({"label", "by", "note"}) and
// "tags". gold.label is the right answer for what was judged (the row's input), not for the item.
// Two judges are built in and need no model: "recorded" (the label the mission recorded) and
// "screen" (the pre-review screen re-run on each review row's diff). Parsing, checking, scoring
// and rendering match Python byte for byte (spec/systemone/gold.json).

// Vocabulary is the labels a gold row may carry per source; Positive the one that refuses.
var (
	Vocabulary = map[string][]string{
		SourceGate:         {"approve", "reject"},
		SourceToolApproval: {"approve", "reject"},
		SourceVerifier:     {"passed", "failed"},
		SourceReview:       {"approve", "block"},
	}
	Positive = map[string]string{
		SourceGate:         "reject",
		SourceToolApproval: "reject",
		SourceVerifier:     "failed",
		SourceReview:       "block",
	}
	// Judges are the built-in judge names.
	Judges = []string{"recorded", "screen", "system_one"}
)

var goldStringKeys = []string{"label", "by", "mission_id", "cycle_id", "item_id", "at"}

// GoldError is python's GoldError: a gold file that cannot be read as gold rows.
type GoldError struct{ Message string }

func (e *GoldError) Error() string { return e.Message }

// GoldRow is one judged input with the label it got and the label it should have got.
type GoldRow struct {
	Source    string
	Label     string
	By        string
	MissionID string
	CycleID   string
	ItemID    string
	At        string
	Input     map[string]any
	Gold      string
	GoldBy    string
	Note      string
	Tags      []string
	Where     string // "<file>:<line>", for messages
}

// Place is "mission cycle item" (the non-empty parts) for a report line.
func (r GoldRow) Place() string {
	parts := []string{}
	for _, p := range []string{r.MissionID, r.CycleID, r.ItemID} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return "?"
	}
	return strings.Join(parts, " ")
}

// ToJSON is the row as the object a JSON Lines line holds.
func (r GoldRow) ToJSON() map[string]any {
	tags := r.Tags
	if tags == nil {
		tags = []string{}
	}
	return map[string]any{
		"schema": LabelSchema, "source": r.Source, "label": r.Label, "by": r.By,
		"mission_id": r.MissionID, "cycle_id": r.CycleID, "item_id": r.ItemID, "at": r.At,
		"input": r.Input,
		"gold":  map[string]any{"label": r.Gold, "by": r.GoldBy, "note": r.Note},
		"tags":  tags,
	}
}

// GoldToJSONL writes rows as JSON Lines, byte for byte as Python does.
func GoldToJSONL(rows []GoldRow) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, row := range rows {
		if err := enc.Encode(row.ToJSON()); err != nil {
			panic(err)
		}
	}
	return buf.String()
}

// ParseGold is python's parse_gold: the gold rows in a JSON Lines text; a *GoldError names the
// first bad line.
func ParseGold(text, name string) ([]GoldRow, error) {
	rows := []GoldRow{}
	for i, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		where := fmt.Sprintf("%s:%d", name, i+1)
		var data map[string]any
		if err := json.Unmarshal([]byte(line), &data); err != nil || data == nil {
			return nil, &GoldError{where + ": line is not a JSON object"}
		}
		if schema, ok := data["schema"].(float64); !ok || schema != float64(LabelSchema) {
			return nil, &GoldError{fmt.Sprintf("%s: schema %s is not %d", where, pyReprAny(data["schema"]), LabelSchema)}
		}
		source, _ := data["source"].(string)
		if _, known := Vocabulary[source]; !known || data["source"] == nil {
			return nil, &GoldError{fmt.Sprintf("%s: unknown source %s", where, pyReprAny(data["source"]))}
		}
		for _, key := range goldStringKeys {
			if v, present := data[key]; present {
				if _, ok := v.(string); !ok {
					return nil, &GoldError{fmt.Sprintf("%s: %s must be a string", where, contracts.PyRepr(key))}
				}
			}
		}
		input := map[string]any{}
		if v, present := data["input"]; present {
			m, ok := v.(map[string]any)
			if !ok {
				return nil, &GoldError{where + ": 'input' must be an object"}
			}
			input = m
		}
		gold, ok := data["gold"].(map[string]any)
		goldLabel, labelOK := gold["label"].(string)
		goldBy, byOK := gold["by"].(string)
		note, noteOK := "", true
		if v, present := gold["note"]; present {
			note, noteOK = v.(string)
		}
		if !ok || !labelOK || goldLabel == "" || !byOK || goldBy == "" || !noteOK {
			return nil, &GoldError{where + ": 'gold' must be an object with a non-empty 'label' and 'by'"}
		}
		tags := []string{}
		if v, present := data["tags"]; present {
			list, ok := v.([]any)
			if !ok {
				return nil, &GoldError{where + ": 'tags' must be a list of strings"}
			}
			for _, t := range list {
				s, ok := t.(string)
				if !ok {
					return nil, &GoldError{where + ": 'tags' must be a list of strings"}
				}
				tags = append(tags, s)
			}
		}
		rows = append(rows, GoldRow{
			Source: source, Label: str(data["label"]), By: str(data["by"]),
			MissionID: str(data["mission_id"]), CycleID: str(data["cycle_id"]), ItemID: str(data["item_id"]), At: str(data["at"]),
			Input: input, Gold: goldLabel, GoldBy: goldBy, Note: note, Tags: tags, Where: where,
		})
	}
	return rows, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// pyReprAny is python's repr() of a decoded JSON scalar.
func pyReprAny(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return contracts.PyRepr(x)
	case bool:
		if x {
			return "True"
		}
		return "False"
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprint(x)
	}
	return fmt.Sprint(v)
}

// JudgmentKey is what makes two rows the same judgment: source, mission, cycle, item, and for a
// gate or approval the request's fingerprint.
func JudgmentKey(row GoldRow) string {
	parts := []string{row.Source, row.MissionID, row.CycleID, row.ItemID}
	if row.Source == SourceGate || row.Source == SourceToolApproval {
		fp := ""
		if req, ok := row.Input["request"].(map[string]any); ok {
			fp = str(req["fingerprint"])
		}
		if fp == "" {
			fp = str(row.Input["fingerprint"])
		}
		parts = append(parts, fp)
	}
	return strings.Join(parts, " ")
}

// CheckGold is python's check_gold: why the set is not usable (nil when it is fine).
func CheckGold(rows []GoldRow) []string {
	errors := []string{}
	seen := map[string]string{}
	for _, row := range rows {
		vocab := Vocabulary[row.Source]
		if !contains(vocab, row.Gold) {
			errors = append(errors, fmt.Sprintf("%s: gold label %s is not one of %s for source %s",
				row.Where, contracts.PyRepr(row.Gold), strings.Join(vocab, ", "), row.Source))
		}
		key := JudgmentKey(row)
		if first, dup := seen[key]; dup {
			errors = append(errors, fmt.Sprintf("%s: duplicate of %s (%s)", row.Where, first, key))
		} else {
			seen[key] = row.Where
		}
	}
	return errors
}

// Judge answers a row ("" abstains).
type Judge func(row GoldRow) (string, bool)

// JudgeRecorded is the label the mission recorded.
func JudgeRecorded(row GoldRow) (string, bool) { return row.Label, true }

// JudgeScreen is the pre-review screen re-run on a review row's diff; it abstains elsewhere.
func JudgeScreen(row GoldRow) (string, bool) {
	if row.Source != SourceReview {
		return "", false
	}
	diff, ok := row.Input["diff"].(string)
	if !ok {
		return "", false
	}
	if len(verify.ScreenDiff(diff)) > 0 {
		return "block", true
	}
	return "approve", true
}

// JudgeNamed is the built-in judge of that name. model is the System One model the "system_one"
// judge asks (ignored by the judges that need no model; nil there is fine).
func JudgeNamed(name string, model Model) (Judge, error) {
	switch name {
	case "recorded":
		return JudgeRecorded, nil
	case "screen":
		return JudgeScreen, nil
	case "system_one":
		if model == nil {
			return nil, fmt.Errorf("judge %s needs a System One model", contracts.PyRepr(name))
		}
		return JudgeSystemOne(model), nil
	}
	return nil, fmt.Errorf("unknown judge %s; expected one of %s", contracts.PyRepr(name), strings.Join(Judges, ", "))
}

// Scorecard is one source's agreement with the gold labels under a judge.
type Scorecard struct {
	Source              string
	Rows, Judged, Agree int
	TP, FP, FN, TN      int
	Disagreements       []string
}

// ScoreGold is python's score: a scorecard per source present, in Sources order.
func ScoreGold(rows []GoldRow, judge Judge) []Scorecard {
	cards := map[string]*Scorecard{}
	for _, s := range Sources {
		cards[s] = &Scorecard{Source: s}
	}
	for _, row := range rows {
		card := cards[row.Source]
		card.Rows++
		judged, ok := judge(row)
		if !ok {
			continue
		}
		card.Judged++
		positive := Positive[row.Source]
		if judged == row.Gold {
			card.Agree++
		} else {
			line := fmt.Sprintf("  %s: judged %s, gold %s", row.Place(), judged, row.Gold)
			if len(row.Tags) > 0 {
				line += " [" + strings.Join(row.Tags, ",") + "]"
			}
			if row.Note != "" {
				line += " (" + row.Note + ")"
			}
			card.Disagreements = append(card.Disagreements, line)
		}
		switch {
		case judged == positive && row.Gold == positive:
			card.TP++
		case judged == positive:
			card.FP++
		case row.Gold == positive:
			card.FN++
		default:
			card.TN++
		}
	}
	out := []Scorecard{}
	for _, s := range Sources {
		if cards[s].Rows > 0 {
			out = append(out, *cards[s])
		}
	}
	return out
}

func ratio(num, den int) string {
	if den == 0 {
		return "n/a"
	}
	return fmt.Sprintf("%.2f", float64(num)/float64(den))
}

// RenderScorecards is the report lha eval run prints.
func RenderScorecards(cards []Scorecard, judgeName string) string {
	lines := []string{"judge: " + judgeName}
	rows, judged, agree := 0, 0, 0
	for _, card := range cards {
		rows, judged, agree = rows+card.Rows, judged+card.Judged, agree+card.Agree
		if card.Judged == 0 {
			lines = append(lines, fmt.Sprintf("%s: %d rows, 0 judged (the judge abstains)", card.Source, card.Rows))
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %d rows, %d judged, %d agree (%s); %s: precision %s, recall %s (tp %d, fp %d, fn %d, tn %d)",
			card.Source, card.Rows, card.Judged, card.Agree, ratio(card.Agree, card.Judged), Positive[card.Source],
			ratio(card.TP, card.TP+card.FP), ratio(card.TP, card.TP+card.FN), card.TP, card.FP, card.FN, card.TN))
		lines = append(lines, card.Disagreements...)
	}
	lines = append(lines, fmt.Sprintf("total: %d rows, %d judged, %d agree (%s)", rows, judged, agree, ratio(agree, judged)))
	return strings.Join(lines, "\n") + "\n"
}

// CountBySource is "<n> gold rows (<n> gate, <n> tool_approval, <n> verifier, <n> review)".
func CountBySource(rows []GoldRow) string {
	counts := make([]string, 0, len(Sources))
	for _, s := range Sources {
		n := 0
		for _, r := range rows {
			if r.Source == s {
				n++
			}
		}
		counts = append(counts, fmt.Sprintf("%d %s", n, s))
	}
	return fmt.Sprintf("%d gold rows (%s)", len(rows), strings.Join(counts, ", "))
}

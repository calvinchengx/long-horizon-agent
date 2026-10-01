package spec

import (
	"encoding/json"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/systemone"
)

// TestLabels is python's test_system_one_labels: the label rows `lha labels export` derives
// from an anchor's events and the store's gates, and their JSON Lines bytes.
func TestLabels(t *testing.T) {
	var s struct {
		Cases []struct {
			Name      string                  `json:"name"`
			MissionID string                  `json:"mission_id"`
			Events    []contracts.EventRecord `json:"events"`
			Gates     []struct {
				MissionID     string            `json:"mission_id"`
				GateID        string            `json:"gate_id"`
				Kind          string            `json:"kind"`
				Status        string            `json:"status"`
				Question      string            `json:"question"`
				Options       []string          `json:"options"`
				DefaultAction string            `json:"default_action"`
				Risk          string            `json:"risk"`
				Deadline      string            `json:"deadline"`
				Decision      *string           `json:"decision"`
				ResolvedBy    *string           `json:"resolved_by"`
				Reminders     int               `json:"reminders"`
				Request       map[string]string `json:"request"`
				OpenedAt      string            `json:"opened_at"`
				ResolvedAt    string            `json:"resolved_at"`
				UpdatedAt     string            `json:"updated_at"`
			} `json:"gates"`
			Diffs    map[string]string `json:"diffs"`
			Expected json.RawMessage   `json:"expected"`
			JSONL    string            `json:"jsonl"`
		} `json:"cases"`
	}
	Load(t, "systemone/labels.json", &s)
	if len(s.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range s.Cases {
		gates := []persistence.GateRow{}
		for _, g := range c.Gates {
			gates = append(gates, persistence.GateRow{
				MissionID: g.MissionID, GateID: g.GateID, Kind: g.Kind, Status: g.Status, Question: g.Question,
				Options: g.Options, DefaultAction: g.DefaultAction, Risk: g.Risk, Deadline: g.Deadline,
				Decision: g.Decision, ResolvedBy: g.ResolvedBy, Reminders: g.Reminders, Request: g.Request,
				OpenedAt: g.OpenedAt, ResolvedAt: g.ResolvedAt, UpdatedAt: g.UpdatedAt,
			})
		}
		var supplier func(base, head string) string
		if c.Diffs != nil {
			table := c.Diffs
			supplier = func(base, head string) string { return table[base+".."+head] }
		}
		rows := systemone.LabelRows(c.Events, gates, c.MissionID, supplier)
		got := []any{}
		for _, row := range rows {
			got = append(got, row.ToJSON())
		}
		JSONEqual(t, c.Name, got, c.Expected)
		if jsonl := systemone.ToJSONL(rows); jsonl != c.JSONL {
			t.Errorf("%s: jsonl\n got %q\nwant %q", c.Name, jsonl, c.JSONL)
		}
	}
}

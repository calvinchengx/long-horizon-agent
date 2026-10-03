package spec

import (
	"reflect"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/ops"
	"github.com/calvinchengx/long-horizon-agent/go/internal/persistence"
	"github.com/calvinchengx/long-horizon-agent/go/internal/verify"
)

func TestHarnessFiles(t *testing.T) {
	var s struct {
		Cases []struct {
			Path    string `json:"path"`
			Harness bool   `json:"harness"`
			Config  bool   `json:"config"`
		} `json:"cases"`
		Violations []struct {
			Name       string            `json:"name"`
			Before     map[string]string `json:"before"`
			After      map[string]string `json:"after"`
			Violations []string          `json:"violations"`
		} `json:"violations"`
		WitnessPaths []struct {
			Witnesses []string `json:"witnesses"`
			Paths     []string `json:"paths"`
		} `json:"witness_paths"`
	}
	Load(t, "verify/harness_files.json", &s)
	if len(s.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range s.Cases {
		if got := verify.IsHarnessFile(c.Path); got != c.Harness {
			t.Errorf("IsHarnessFile(%q) = %v, want %v", c.Path, got, c.Harness)
		}
	}
	for _, c := range s.Cases {
		if verify.IsHarnessConfig(c.Path) != c.Config {
			t.Errorf("IsHarnessConfig(%q) = %v", c.Path, !c.Config)
		}
	}
	if len(s.Violations) == 0 {
		t.Fatal("no violation cases")
	}
	for _, c := range s.Violations {
		if got := verify.HarnessViolations(c.Before, c.After); !reflect.DeepEqual(got, c.Violations) {
			t.Errorf("%s: %q, want %q", c.Name, got, c.Violations)
		}
	}
	for _, c := range s.WitnessPaths {
		if got := verify.WitnessPaths(c.Witnesses); !reflect.DeepEqual(got, c.Paths) {
			t.Errorf("WitnessPaths(%q) = %q, want %q", c.Witnesses, got, c.Paths)
		}
	}
}

// TestMissionReport is python's test_mission_report: the page lha mission-report renders from an
// anchor and the store, byte for byte.
func TestMissionReport(t *testing.T) {
	type gate struct {
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
	}
	var s struct {
		Cases []struct {
			Name      string                 `json:"name"`
			MissionID string                 `json:"mission_id"`
			Spec      *contracts.MissionSpec `json:"spec"`
			Checklist struct {
				Items []contracts.ChecklistItem `json:"items"`
			} `json:"checklist"`
			Events []contracts.EventRecord `json:"events"`
			Row    *struct {
				MissionID   string  `json:"mission_id"`
				Title       string  `json:"title"`
				Status      string  `json:"status"`
				Description string  `json:"description"`
				HeadSHA     *string `json:"head_sha"`
				WorkflowID  *string `json:"workflow_id"`
				CreatedAt   string  `json:"created_at"`
				UpdatedAt   string  `json:"updated_at"`
			} `json:"row"`
			Gates []gate `json:"gates"`
			Cost  *struct {
				MissionID        string  `json:"mission_id"`
				Calls            int     `json:"calls"`
				KnownUSD         float64 `json:"known_usd"`
				UnknownCostCalls int     `json:"unknown_cost_calls"`
				InputTokens      int     `json:"input_tokens"`
				OutputTokens     int     `json:"output_tokens"`
			} `json:"cost"`
			Commits  int    `json:"commits"`
			HeadSHA  string `json:"head_sha"`
			Expected string `json:"expected"`
		} `json:"cases"`
	}
	Load(t, "state/report.json", &s)
	if len(s.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range s.Cases {
		inp := ops.ReportInput{MissionID: c.MissionID, Spec: c.Spec, Checklist: contracts.Checklist{SchemaVersion: 1, Items: c.Checklist.Items},
			Events: c.Events, Commits: c.Commits, HeadSHA: c.HeadSHA}
		if c.Row != nil {
			row := persistence.MissionRow{MissionID: c.Row.MissionID, Title: c.Row.Title, Status: c.Row.Status, Description: c.Row.Description,
				CreatedAt: c.Row.CreatedAt, UpdatedAt: c.Row.UpdatedAt}
			if c.Row.HeadSHA != nil {
				row.HeadSHA = *c.Row.HeadSHA
			}
			if c.Row.WorkflowID != nil {
				row.WorkflowID = *c.Row.WorkflowID
			}
			inp.Row = &row
		}
		for _, g := range c.Gates {
			inp.Gates = append(inp.Gates, persistence.GateRow{MissionID: g.MissionID, GateID: g.GateID, Kind: g.Kind, Status: g.Status,
				Question: g.Question, Options: g.Options, DefaultAction: g.DefaultAction, Risk: g.Risk, Deadline: g.Deadline,
				Decision: g.Decision, ResolvedBy: g.ResolvedBy, Reminders: g.Reminders, Request: g.Request,
				OpenedAt: g.OpenedAt, ResolvedAt: g.ResolvedAt, UpdatedAt: g.UpdatedAt})
		}
		if c.Cost != nil {
			inp.Cost = &persistence.CostSummary{MissionID: c.Cost.MissionID, Calls: c.Cost.Calls, KnownUSD: c.Cost.KnownUSD,
				UnknownCostCalls: c.Cost.UnknownCostCalls, InputTokens: c.Cost.InputTokens, OutputTokens: c.Cost.OutputTokens}
		}
		if got := ops.RenderReport(inp); got != c.Expected {
			t.Errorf("%s:\n--- got\n%s\n--- want\n%s", c.Name, got, c.Expected)
		}
	}
}

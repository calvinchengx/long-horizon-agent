package spec

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent"
	"github.com/calvinchengx/long-horizon-agent/go/internal/agents"
	"github.com/calvinchengx/long-horizon-agent/go/internal/agents/org"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/coordination"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/tools"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// spec/agent/org.json: the multi-agent organization's prompts and parsers, byte for byte — the
// role chart, sub-agent prompts and tool visibility, the Reviewer's prompt and verdict parsing,
// reflection, the implementer's objective, ownership-guard refusals, lease decisions and the
// ticket lifecycle.

type orgSpec struct {
	Roles map[string]struct {
		Tier          string `json:"tier"`
		ClaudeModel   string `json:"claude_model"`
		SystemPrompt  string `json:"system_prompt"`
		AllowMutating bool   `json:"allow_mutating"`
		AllowEgress   bool   `json:"allow_egress"`
		MaxTurns      int    `json:"max_turns"`
	} `json:"roles"`
	Specs         []json.RawMessage `json:"specs"`
	RenderedSpecs string            `json:"rendered_specs"`
	Subagent      []struct {
		Role         string    `json:"role"`
		Objective    string    `json:"objective"`
		ExtraContext string    `json:"extra_context"`
		Visible      []string  `json:"visible"`
		Messages     []message `json:"messages"`
	} `json:"subagent"`
	SubagentFinalTurn string `json:"subagent_final_turn"`
	ReviewerMessages  []struct {
		Criteria string    `json:"criteria"`
		Diff     string    `json:"diff"`
		Messages []message `json:"messages"`
	} `json:"reviewer_messages"`
	ParseReview []struct {
		Text           string   `json:"text"`
		Verdict        string   `json:"verdict"`
		Blocking       bool     `json:"blocking"`
		BlockingIssues []string `json:"blocking_issues"`
		Advisory       []string `json:"advisory"`
		Notes          string   `json:"notes"`
	} `json:"parse_review"`
	Reflection struct {
		ItemDescription string    `json:"item_description"`
		FailureSummary  string    `json:"failure_summary"`
		Messages        []message `json:"messages"`
	} `json:"reflection"`
	ImplementerObjective []struct {
		Name       string                  `json:"name"`
		Item       contracts.ChecklistItem `json:"item"`
		Owners     map[string]string       `json:"owners"`
		CycleID    string                  `json:"cycle_id"`
		Acceptance []string                `json:"acceptance"`
		Inputs     struct {
			MissionText   string   `json:"mission_text"`
			Reflection    string   `json:"reflection"`
			DecisionsText string   `json:"decisions_text"`
			Briefs        []string `json:"briefs"`
			Board         string   `json:"board"`
			LeaseTool     *bool    `json:"lease_tool"`
		} `json:"inputs"`
		TicketID  string   `json:"ticket_id"`
		WriteSet  []string `json:"write_set"`
		Objective string   `json:"objective"`
		Extra     string   `json:"extra"`
	} `json:"implementer_objective"`
	OwnershipGuard []struct {
		Owners    map[string]string `json:"owners"`
		Writers   []string          `json:"writers"`
		LeaseTool bool              `json:"lease_tool"`
		Path      string            `json:"path"`
		Refusal   *string           `json:"refusal"`
	} `json:"ownership_guard"`
	Leases []struct {
		Owners   map[string]string `json:"owners"`
		Writer   string            `json:"writer"`
		Path     string            `json:"path"`
		Reason   string            `json:"reason"`
		Finished []string          `json:"finished"`
		Decision map[string]any    `json:"decision"`
		Message  string            `json:"message"`
	} `json:"leases"`
	Tickets []struct {
		From  string `json:"from"`
		To    string `json:"to"`
		Legal bool   `json:"legal"`
	} `json:"tickets"`
}

func loadOrg(t *testing.T) orgSpec {
	t.Helper()
	var s orgSpec
	Load(t, "agent/org.json", &s)
	return s
}

// specsOnly lists specs; no tool is ever called.
type specsOnly struct{ specs []contracts.ToolSpec }

func (d specsOnly) Specs() []contracts.ToolSpec { return d.specs }
func (d specsOnly) Dispatch(context.Context, contracts.ToolCall, contracts.ToolContext) contracts.ToolResult {
	panic("no tool call expected")
}

// recording records every message it is sent and answers "[]" (no action: the sub-agent stops).
type orgRecording struct {
	*model.StubModel
	seen []contracts.ModelMessage
}

func newOrgRecording() *orgRecording { return &orgRecording{StubModel: model.NewStub(nil)} }

func (r *orgRecording) Complete(_ context.Context, messages []contracts.ModelMessage, _ []map[string]any, _ int) (contracts.TurnResult, error) {
	r.seen = append(r.seen, messages...)
	return contracts.TurnResult{Text: "[]"}, nil
}

func ownersMap(owners map[string]string) *coordination.FileOwnershipMap {
	m := coordination.NewFileOwnershipMap()
	for k, v := range owners {
		m.Owners[k] = v
	}
	return m
}

func TestOrgRolesAndToolRendering(t *testing.T) {
	s := loadOrg(t)
	if len(s.Roles) != len(agents.Roles) {
		t.Fatalf("roles: %d in spec, %d in Go", len(s.Roles), len(agents.Roles))
	}
	for name, want := range s.Roles {
		got, ok := agents.Roles[name]
		if !ok || string(got.Tier) != want.Tier || agents.ClaudeModelFor(got.Tier) != want.ClaudeModel ||
			got.SystemPrompt != want.SystemPrompt || got.AllowMutating != want.AllowMutating ||
			got.AllowEgress != want.AllowEgress || got.MaxTurns != want.MaxTurns {
			t.Errorf("role %s: %+v, want %+v", name, got, want)
		}
	}
	specs := []contracts.ToolSpec{}
	for _, raw := range s.Specs {
		specs = append(specs, toolSpec(t, raw))
	}
	if got := agent.RenderTools(specs); got != s.RenderedSpecs {
		t.Errorf("render_tools:\n go:     %q\n python: %q", got, s.RenderedSpecs)
	}
}

// TestOrgRealToolsRenderLikePython: the Go tools themselves (not the spec's dumps) render like
// Python's — property order included, so every role's prompt is byte-identical.
func TestOrgRealToolsRenderLikePython(t *testing.T) {
	s := loadOrg(t)
	real := []contracts.ToolSpec{}
	for _, tool := range tools.DefaultLocalTools() {
		real = append(real, tool.Spec())
	}
	real = append(real, tools.RecordDecisionTool{}.Spec(), tools.LeaseSpec(),
		tools.NewFetchURLTool(tools.FetchURLOptions{}).Spec(), (&tools.WebSearchTool{}).Spec())
	if got := agent.RenderTools(real); got != s.RenderedSpecs {
		t.Errorf("render_tools of the Go tools:\n go:     %q\n python: %q", got, s.RenderedSpecs)
	}
}

func TestOrgSubAgentAndReviewerPrompts(t *testing.T) {
	s := loadOrg(t)
	specs := []contracts.ToolSpec{}
	for _, raw := range s.Specs {
		specs = append(specs, toolSpec(t, raw))
	}
	ctx := context.Background()
	if s.SubagentFinalTurn != org.FinalTurnMessage {
		t.Errorf("final turn message: %q", s.SubagentFinalTurn)
	}
	for _, c := range s.Subagent {
		rec := newOrgRecording()
		sa := org.NewSubAgent(agents.Roles[c.Role], rec, specsOnly{specs}, 0)
		visible := []string{}
		for _, spec := range sa.VisibleSpecs() {
			visible = append(visible, spec.Name)
		}
		if !reflect.DeepEqual(visible, c.Visible) {
			t.Errorf("%s visible: %v, want %v", c.Role, visible, c.Visible)
		}
		if _, err := sa.Run(ctx, c.Objective, contracts.ToolContext{}, c.ExtraContext); err != nil {
			t.Fatal(err)
		}
		if got := messagesOf(rec.seen); !reflect.DeepEqual(got, c.Messages) {
			t.Errorf("%s messages:\n go:     %q\n python: %q", c.Role, got, c.Messages)
		}
	}
	for _, c := range s.ReviewerMessages {
		rec := newOrgRecording()
		if _, err := org.NewReviewer(rec, specsOnly{specs}).Review(ctx, c.Diff, c.Criteria, contracts.ToolContext{}); err != nil {
			t.Fatal(err)
		}
		if got := messagesOf(rec.seen); !reflect.DeepEqual(got, c.Messages) {
			t.Errorf("reviewer messages:\n go:     %q\n python: %q", got, c.Messages)
		}
	}
	rec := newOrgRecording()
	if _, err := agents.ReflectOnFailure(ctx, rec, s.Reflection.ItemDescription, s.Reflection.FailureSummary); err != nil {
		t.Fatal(err)
	}
	if got := messagesOf(rec.seen); !reflect.DeepEqual(got, s.Reflection.Messages) {
		t.Errorf("reflection messages:\n go:     %q\n python: %q", got, s.Reflection.Messages)
	}
}

func TestOrgParseReview(t *testing.T) {
	for _, c := range loadOrg(t).ParseReview {
		verdict, blocking, issues, advisory := agents.ParseReview(c.Text)
		if verdict != c.Verdict || blocking != c.Blocking || !reflect.DeepEqual(issues, c.BlockingIssues) ||
			!reflect.DeepEqual(advisory, c.Advisory) {
			t.Errorf("parse_review(%q) = %q %v %q %q, want %q %v %q %q", c.Text, verdict, blocking, issues,
				advisory, c.Verdict, c.Blocking, c.BlockingIssues, c.Advisory)
		}
		notes := agents.ReviewResult{Brief: c.Text, Blocking: blocking, Verdict: verdict,
			BlockingIssues: issues, Advisory: advisory}.Notes()
		if notes != c.Notes {
			t.Errorf("notes(%q) = %q, want %q", c.Text, notes, c.Notes)
		}
	}
}

func TestOrgImplementerObjective(t *testing.T) {
	for _, c := range loadOrg(t).ImplementerObjective {
		run := org.NewImplementerRun(c.Item, c.CycleID, ownersMap(c.Owners), 12, c.Acceptance)
		if run.Ticket.ID != c.TicketID || !reflect.DeepEqual(run.Ticket.Contract.WriteSet, c.WriteSet) {
			t.Errorf("%s ticket: %s %v", c.Name, run.Ticket.ID, run.Ticket.Contract.WriteSet)
		}
		objective, extra := org.ImplementerObjective(run, org.ObjectiveInput{
			MissionText: c.Inputs.MissionText, Reflection: c.Inputs.Reflection,
			DecisionsText: c.Inputs.DecisionsText, Briefs: c.Inputs.Briefs, Board: c.Inputs.Board,
			NoLeaseTool: c.Inputs.LeaseTool != nil && !*c.Inputs.LeaseTool,
		})
		if objective != c.Objective {
			t.Errorf("%s objective:\n go:     %q\n python: %q", c.Name, objective, c.Objective)
		}
		if extra != c.Extra {
			t.Errorf("%s extra:\n go:     %q\n python: %q", c.Name, extra, c.Extra)
		}
	}
}

func TestOrgOwnershipGuardLeasesAndTickets(t *testing.T) {
	s := loadOrg(t)
	for _, c := range s.OwnershipGuard {
		guard := coordination.NewOwnershipGuard(specsOnly{}, ownersMap(c.Owners), c.Writers, c.LeaseTool)
		want := ""
		if c.Refusal != nil {
			want = *c.Refusal
		}
		if got := guard.Refusal(c.Path); got != want {
			t.Errorf("refusal(%v, %q):\n go:     %q\n python: %q", c.Writers, c.Path, got, want)
		}
	}
	for _, c := range s.Leases {
		d := coordination.DecideLease(ownersMap(c.Owners),
			coordination.LeaseRequest{Writer: c.Writer, Path: c.Path, Reason: c.Reason}, c.Finished)
		if got := plain(t, d.Payload()); !reflect.DeepEqual(got, plain(t, c.Decision)) {
			t.Errorf("decide_lease(%q):\n go:     %v\n python: %v", c.Path, got, c.Decision)
		}
		if d.Message() != c.Message {
			t.Errorf("message(%q):\n go:     %q\n python: %q", c.Path, d.Message(), c.Message)
		}
	}
	for _, c := range s.Tickets {
		ticket := coordination.Ticket{ID: "t", Status: coordination.TicketStatus(c.From)}
		if got := ticket.CanTransition(coordination.TicketStatus(c.To)); got != c.Legal {
			t.Errorf("%s -> %s: %v, want %v", c.From, c.To, got, c.Legal)
		}
	}
}

package spec

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent"
	"github.com/calvinchengx/long-horizon-agent/go/internal/agents"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// spec/agent/prompts.json: the lead's prompts, the JSON reply protocol, and the Planner /
// Replanner prompts and parsing, byte for byte.

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

func messagesOf(msgs []contracts.ModelMessage) []message {
	out := make([]message, len(msgs))
	for i, m := range msgs {
		out[i] = message{m.Role, m.Content}
	}
	return out
}

// toolSpec decodes a ToolSpec dump keeping the parameters' key order (render_tools shows the
// properties as a Python dict repr, in declaration order).
func toolSpec(t *testing.T, raw json.RawMessage) contracts.ToolSpec {
	t.Helper()
	var spec contracts.ToolSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	var shape struct {
		Parameters json.RawMessage `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &shape); err != nil {
		t.Fatal(err)
	}
	ordered, err := pyfmt.DecodeOrdered(shape.Parameters)
	if err != nil {
		t.Fatal(err)
	}
	om := ordered.(*pyfmt.OrderedMap)
	spec.Parameters = map[string]any{}
	for _, k := range om.Keys {
		spec.Parameters[k] = om.Values[k]
	}
	return spec
}

func plain(t *testing.T, v any) any {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func rawAny(t *testing.T, raw json.RawMessage) any {
	t.Helper()
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

type promptSpec struct {
	BuildMessages []struct {
		Name        string                      `json:"name"`
		AnchorText  string                      `json:"anchor_text"`
		MissionText string                      `json:"mission_text"`
		MemoryText  string                      `json:"memory_text"`
		CodeMapText string                      `json:"code_map_text"`
		Engine      bool                        `json:"engine"`
		Snapshot    contracts.SituationSnapshot `json:"snapshot"`
		Item        contracts.ChecklistItem     `json:"item"`
		Specs       []json.RawMessage           `json:"specs"`
		Messages    []message                   `json:"messages"`
	} `json:"build_messages"`
	CodeMap struct {
		Header  string `json:"header"`
		HardCap int    `json:"hard_cap"`
		Argv    []struct {
			Item   contracts.ChecklistItem `json:"item"`
			Budget int                     `json:"budget"`
			Argv   []string                `json:"argv"`
		} `json:"argv"`
		Render []struct {
			Output   string `json:"output"`
			Rendered string `json:"rendered"`
		} `json:"render"`
		Query []struct {
			Item  contracts.ChecklistItem `json:"item"`
			Query string                  `json:"query"`
		} `json:"query"`
		TraceChars  int      `json:"trace_chars"`
		TraceScript string   `json:"trace_script"`
		TraceArgv   []string `json:"trace_argv"`
		TraceEnv    []struct {
			Item   contracts.ChecklistItem `json:"item"`
			Budget int                     `json:"budget"`
			Env    map[string]string       `json:"env"`
		} `json:"trace_env"`
		FoundCode []struct {
			Output string `json:"output"`
			Found  bool   `json:"found"`
		} `json:"found_code"`
	} `json:"code_map"`
	LeadTools struct {
		Specs    []json.RawMessage `json:"specs"`
		Rendered string            `json:"rendered"`
	} `json:"lead_tools"`
	RenderMemoryBlock []struct {
		Sections []struct {
			Title string   `json:"title"`
			Lines []string `json:"lines"`
		} `json:"sections"`
		BudgetChars int       `json:"budget_chars"`
		Weights     []float64 `json:"weights"`
		Rendered    string    `json:"rendered"`
	} `json:"render_memory_block"`
	Corrective  string `json:"corrective"`
	ParseAction []struct {
		Text       string          `json:"text"`
		StopReason *string         `json:"stop_reason"`
		Done       bool            `json:"done"`
		Tool       *string         `json:"tool"`
		Arguments  json.RawMessage `json:"arguments"`
		Summary    string          `json:"summary"`
		Error      string          `json:"error"`
	} `json:"parse_action"`
	ParsePlan []struct {
		Text   string              `json:"text"`
		Items  json.RawMessage     `json:"items"`
		Files  map[string][]string `json:"files"`
		Owners map[string]string   `json:"owners"`
	} `json:"parse_plan"`
	PlannerMessages           []message `json:"planner_messages"`
	PlannerMessagesAcceptance []message `json:"planner_messages_acceptance"`
	Replanner                 struct {
		MissionText string                  `json:"mission_text"`
		Item        contracts.ChecklistItem `json:"item"`
		Messages    []message               `json:"messages"`
	} `json:"replanner"`
}

func loadPrompts(t *testing.T) promptSpec {
	var s promptSpec
	Load(t, "agent/prompts.json", &s)
	if len(s.BuildMessages) == 0 || len(s.ParseAction) == 0 || len(s.ParsePlan) == 0 {
		t.Fatal("no cases")
	}
	return s
}

func TestAgentBuildMessages(t *testing.T) {
	s := loadPrompts(t)
	for _, c := range s.BuildMessages {
		specs := []contracts.ToolSpec{}
		for _, raw := range c.Specs {
			specs = append(specs, toolSpec(t, raw))
		}
		got := messagesOf(agent.BuildMessages(agent.PromptInput{
			AnchorText: c.AnchorText, MissionText: c.MissionText, Snapshot: c.Snapshot, Item: c.Item,
			Specs: specs, MemoryText: c.MemoryText, CodeMapText: c.CodeMapText, Engine: c.Engine,
		}))
		if !reflect.DeepEqual(got, c.Messages) {
			for i := range got {
				if i < len(c.Messages) && got[i] != c.Messages[i] {
					t.Errorf("%s: message %d differs:\n go:     %q\n python: %q", c.Name, i, got[i].Content, c.Messages[i].Content)
				}
			}
			if len(got) != len(c.Messages) {
				t.Errorf("%s: %d messages, want %d", c.Name, len(got), len(c.Messages))
			}
		}
	}
}

func TestAgentLeadToolsRendering(t *testing.T) {
	s := loadPrompts(t)
	specs := []contracts.ToolSpec{}
	for _, raw := range s.LeadTools.Specs {
		specs = append(specs, toolSpec(t, raw))
	}
	if got := agent.RenderTools(specs); got != s.LeadTools.Rendered {
		t.Errorf("render_tools:\n go:     %q\n python: %q", got, s.LeadTools.Rendered)
	}
}

func TestAgentMemoryBlockAndCorrective(t *testing.T) {
	s := loadPrompts(t)
	for i, c := range s.RenderMemoryBlock {
		sections := []agent.MemorySection{}
		for _, sec := range c.Sections {
			sections = append(sections, agent.MemorySection{Title: sec.Title, Lines: sec.Lines})
		}
		got, err := agent.RenderMemoryBlock(sections, c.BudgetChars, c.Weights)
		if err != nil || got != c.Rendered {
			t.Errorf("case %d: %v\n go:     %q\n python: %q", i, err, got, c.Rendered)
		}
	}
	if got := agent.CorrectiveMessage("no JSON object found in reply").Content; got != s.Corrective {
		t.Errorf("corrective: %q != %q", got, s.Corrective)
	}
}

func TestAgentParseAction(t *testing.T) {
	s := loadPrompts(t)
	for _, c := range s.ParseAction {
		a := agent.ParseAction(c.Text, nil, c.StopReason)
		wantTool := ""
		if c.Tool != nil {
			wantTool = *c.Tool
		}
		if a.Done != c.Done || a.Tool != wantTool || a.Summary != c.Summary || a.Error != c.Error {
			t.Errorf("%q: got %+v, want done=%v tool=%q summary=%q error=%q", c.Text, a, c.Done, wantTool, c.Summary, c.Error)
		}
		if !reflect.DeepEqual(plain(t, a.Arguments), rawAny(t, c.Arguments)) {
			t.Errorf("%q: arguments %v, want %s", c.Text, a.Arguments, c.Arguments)
		}
	}
}

func TestAgentParsePlanAndOwnership(t *testing.T) {
	s := loadPrompts(t)
	for _, c := range s.ParsePlan {
		items, files := agents.ParsePlan(c.Text)
		owners := agents.AssignOwnership(items, files).Owners
		if !reflect.DeepEqual(plain(t, items), rawAny(t, c.Items)) {
			t.Errorf("%q: items\n go:     %s", c.Text, mustJSON(t, items))
		}
		if !reflect.DeepEqual(files, c.Files) || !reflect.DeepEqual(owners, c.Owners) {
			t.Errorf("%q: files %v owners %v, want %v %v", c.Text, files, owners, c.Files, c.Owners)
		}
	}
}

func mustJSON(t *testing.T, v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// recording captures the messages of every call.
type recording struct {
	*model.StubModel
	seen []contracts.ModelMessage
}

func (r *recording) Complete(ctx context.Context, msgs []contracts.ModelMessage, tools []map[string]any, n int) (contracts.TurnResult, error) {
	r.seen = append(r.seen, msgs...)
	return contracts.TurnResult{Text: "[]"}, nil
}

func TestAgentPlannerAndReplannerMessages(t *testing.T) {
	s := loadPrompts(t)
	for _, c := range []struct {
		acceptance string
		want       []message
	}{{"", s.PlannerMessages}, {"all tests pass", s.PlannerMessagesAcceptance}} {
		rec := &recording{StubModel: model.NewStub(nil)}
		if _, err := agents.NewPlanner(rec).PlanMission(context.Background(), "T", "Build the thing.", c.acceptance); err != nil {
			t.Fatal(err)
		}
		if got := messagesOf(rec.seen); !reflect.DeepEqual(got, c.want) {
			t.Errorf("planner messages (acceptance %q):\n go:     %q\n python: %q", c.acceptance, got, c.want)
		}
	}
	rec := &recording{StubModel: model.NewStub(nil)}
	if _, err := agents.NewReplanner(rec).Split(context.Background(), s.Replanner.MissionText, s.Replanner.Item); err != nil {
		t.Fatal(err)
	}
	if got := messagesOf(rec.seen); !reflect.DeepEqual(got, s.Replanner.Messages) {
		t.Errorf("replanner messages:\n go:     %q\n python: %q", got, s.Replanner.Messages)
	}
}

func TestSharedPaths(t *testing.T) {
	var s struct {
		Cases []struct {
			Path   string `json:"path"`
			Shared bool   `json:"shared"`
		} `json:"cases"`
	}
	Load(t, "coordination/shared_paths.json", &s)
	if len(s.Cases) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range s.Cases {
		got, err := agents.IsShared(c.Path)
		if err != nil || got != c.Shared {
			t.Errorf("IsShared(%q) = %v, %v; want %v", c.Path, got, err, c.Shared)
		}
	}
}

// TestAgentCodeMap runs agent/prompts.json's code_map section: the ripwire command for an item and
// how its output becomes the prompt section (python: lha.agent.code_map, render_code_map).
func TestAgentCodeMap(t *testing.T) {
	s := loadPrompts(t)
	if s.CodeMap.Header != agent.CodeMapHeader || s.CodeMap.HardCap != agent.CodeMapHardCap {
		t.Fatalf("constants differ from python: %q %d", s.CodeMap.Header, s.CodeMap.HardCap)
	}
	if len(s.CodeMap.Argv) == 0 || len(s.CodeMap.Render) == 0 {
		t.Fatal("no cases")
	}
	for _, c := range s.CodeMap.Argv {
		if got := agent.CodeMapArgv(c.Item, c.Budget); !reflect.DeepEqual(got, c.Argv) {
			t.Errorf("CodeMapArgv(%q, %d) = %q, want %q", c.Item.Description, c.Budget, got, c.Argv)
		}
	}
	for i, c := range s.CodeMap.Render {
		if got := agent.RenderCodeMap(c.Output); got != c.Rendered {
			t.Errorf("render %d differs (%d vs %d runes)", i, len([]rune(got)), len([]rune(c.Rendered)))
		}
	}
	if s.CodeMap.TraceChars != agent.TraceChars || s.CodeMap.TraceScript != agent.TraceScript ||
		!reflect.DeepEqual(s.CodeMap.TraceArgv, agent.TraceArgv()) {
		t.Fatalf("trace constants differ from python")
	}
	if len(s.CodeMap.Query) == 0 || len(s.CodeMap.TraceEnv) == 0 || len(s.CodeMap.FoundCode) == 0 {
		t.Fatal("no query/trace/found cases")
	}
	for _, c := range s.CodeMap.Query {
		if got := agent.CodeMapQuery(c.Item); got != c.Query {
			t.Errorf("CodeMapQuery(%s) = %q, want %q", c.Item.ID, got, c.Query)
		}
	}
	for _, c := range s.CodeMap.TraceEnv {
		if got := agent.TraceEnv(c.Item, c.Budget); !reflect.DeepEqual(got, c.Env) {
			t.Errorf("TraceEnv(%s) differs", c.Item.ID)
		}
	}
	for _, c := range s.CodeMap.FoundCode {
		if got := agent.FoundCode(c.Output); got != c.Found {
			t.Errorf("FoundCode(%q) = %v", c.Output, got)
		}
	}
}

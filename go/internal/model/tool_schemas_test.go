package model

import (
	"reflect"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

var readFileSpec = contracts.ToolSpec{
	Name:        "read_file",
	Description: "Read a file.",
	Parameters: map[string]any{
		"type":       "object",
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
		"required":   []any{"path"},
	},
}

func TestToOpenAIToolsShape(t *testing.T) {
	tools := ToOpenAITools([]contracts.ToolSpec{readFileSpec})
	if tools[0]["type"] != "function" {
		t.Errorf("type = %v", tools[0]["type"])
	}
	fn := tools[0]["function"].(map[string]any)
	if fn["name"] != "read_file" || fn["description"] != "Read a file." ||
		!reflect.DeepEqual(fn["parameters"].(map[string]any)["required"], []any{"path"}) {
		t.Errorf("function = %v", fn)
	}
}

func TestToClaudeToolsShape(t *testing.T) {
	tools := ToClaudeTools([]contracts.ToolSpec{readFileSpec})
	schema := tools[0]["input_schema"].(map[string]any)
	if tools[0]["name"] != "read_file" || tools[0]["description"] != "Read a file." ||
		schema["properties"].(map[string]any)["path"].(map[string]any)["type"] != "string" {
		t.Errorf("tool = %v", tools[0])
	}
}

func TestEmptyParamsDefault(t *testing.T) {
	want := map[string]any{"type": "object", "properties": map[string]any{}}
	for _, spec := range []contracts.ToolSpec{{Name: "noop", Description: "d"}, {Name: "noop", Parameters: map[string]any{}}} {
		if got := ToOpenAITools([]contracts.ToolSpec{spec})[0]["function"].(map[string]any)["parameters"]; !reflect.DeepEqual(got, want) {
			t.Errorf("openai parameters = %v", got)
		}
		if got := ToClaudeTools([]contracts.ToolSpec{spec})[0]["input_schema"]; !reflect.DeepEqual(got, want) {
			t.Errorf("claude input_schema = %v", got)
		}
	}
	if got := ToClaudeTools(nil); got == nil || len(got) != 0 {
		t.Errorf("no specs => empty (non-nil) list, got %#v", got)
	}
}

package model

import "github.com/calvinchengx/long-horizon-agent/go/internal/contracts"

// Convert ToolSpecs to provider-native tool-calling schemas (python: tool_schemas.py).

func emptySchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func specParameters(spec contracts.ToolSpec) map[string]any {
	if len(spec.Parameters) == 0 { // python: spec.parameters or _EMPTY_SCHEMA
		return emptySchema()
	}
	return spec.Parameters
}

// ToOpenAITools builds the OpenAI / OpenAI-compatible `tools` array (function-calling).
func ToOpenAITools(specs []contracts.ToolSpec) []map[string]any {
	out := make([]map[string]any, 0, len(specs))
	for _, spec := range specs {
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        spec.Name,
				"description": spec.Description,
				"parameters":  specParameters(spec),
			},
		})
	}
	return out
}

// ToClaudeTools builds the Anthropic Messages API `tools` array.
func ToClaudeTools(specs []contracts.ToolSpec) []map[string]any {
	out := make([]map[string]any, 0, len(specs))
	for _, spec := range specs {
		out = append(out, map[string]any{
			"name":         spec.Name,
			"description":  spec.Description,
			"input_schema": specParameters(spec),
		})
	}
	return out
}

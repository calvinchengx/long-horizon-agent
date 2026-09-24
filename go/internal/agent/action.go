package agent

import (
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// Action is a parsed model action: a tool call, a done signal, or an invalid reply (Error set).
type Action struct {
	Done      bool
	Tool      string // "" when none (python: None)
	Arguments map[string]any
	Summary   string
	// Error is non-empty when the reply could not be interpreted; the loop sends a corrective
	// turn.
	Error string
}

// IsValid reports whether the reply was interpretable.
func (a Action) IsValid() bool { return a.Error == "" }

var truncatedStopReasons = map[string]bool{"max_tokens": true, "length": true, "model_length": true}

// IsTruncated is true when the provider stopped because it ran out of output tokens.
func IsTruncated(stopReason *string) bool {
	if stopReason == nil {
		return false
	}
	return truncatedStopReasons[strings.ToLower(*stopReason)]
}

// extractJSON is the first "{" .. last "}" span parsed as a JSON object (key order kept), or nil.
func extractJSON(text string) *pyfmt.OrderedMap {
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

// ParseAction interprets a model turn as an Action (native tool calls take precedence over JSON
// text). Unparseable replies and replies cut off at max_tokens are NOT done: they come back with
// Error set so the caller can ask for a well-formed action.
func ParseAction(text string, native []contracts.ToolCall, stopReason *string) Action {
	if IsTruncated(stopReason) {
		return Action{Error: "reply was truncated at the output-token limit", Summary: pyfmt.Head(text, 200), Arguments: map[string]any{}}
	}
	if len(native) > 0 {
		args := map[string]any{}
		for k, v := range native[0].Arguments {
			args[k] = v
		}
		return Action{Tool: native[0].Name, Arguments: args}
	}
	obj := extractJSON(text)
	if obj == nil {
		return Action{Error: "no JSON object found in reply", Summary: pyfmt.Head(text, 200), Arguments: map[string]any{}}
	}
	if done, ok := obj.Values["done"].(bool); ok && done {
		summary := ""
		if v, ok := obj.Values["summary"]; ok {
			summary = pyfmt.PyStr(v)
		}
		return Action{Done: true, Summary: summary, Arguments: map[string]any{}}
	}
	if tool, ok := obj.Values["tool"].(string); ok && tool != "" {
		args := map[string]any{}
		if raw, ok := obj.Values["arguments"].(*pyfmt.OrderedMap); ok {
			args = pyfmt.PlainJSON(raw).(map[string]any)
		}
		return Action{Tool: tool, Arguments: args}
	}
	return Action{Error: `JSON has neither "tool" nor "done": true`, Summary: pyfmt.Head(text, 200), Arguments: map[string]any{}}
}

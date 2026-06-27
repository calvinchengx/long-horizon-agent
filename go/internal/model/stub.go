package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// DefaultStubModelName is the stub's default model name (python: StubModel(model_name="stub-1")).
const DefaultStubModelName = "stub-1"

// StubModel is a deterministic, offline ModelProvider for tests and CI — an HONEST test double,
// never presented as a real run: its name is always "stub:*" and its cost is always $0.
//
// Without a script its output is a deterministic function of the conversation. With a script it
// returns the scripted turns in order, repeating the last one once the script is exhausted.
type StubModel struct {
	name   string
	script []contracts.TurnResult

	mu   sync.Mutex
	turn int
}

var _ contracts.ModelProvider = (*StubModel)(nil)

// NewStub is a "stub:stub-1" model driven by script (nil or empty = deterministic echo mode).
func NewStub(script []contracts.TurnResult) *StubModel {
	return NewStubNamed(DefaultStubModelName, script)
}

// NewStubNamed is NewStub with an explicit model name (the provider is named "stub:<name>").
func NewStubNamed(modelName string, script []contracts.TurnResult) *StubModel {
	s := &StubModel{name: "stub:" + modelName}
	if len(script) > 0 {
		s.script = make([]contracts.TurnResult, len(script))
		for i, r := range script {
			s.script[i] = copyTurnResult(r)
		}
	}
	return s
}

// Name is "stub:<model name>".
func (s *StubModel) Name() string { return s.name }

// Turns is how many turns have been completed.
func (s *StubModel) Turns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn
}

// Complete returns the next scripted turn (a deep copy), or a deterministic echo of messages.
func (s *StubModel) Complete(_ context.Context, messages []contracts.ModelMessage, _ []map[string]any, _ int) (contracts.TurnResult, error) {
	s.mu.Lock()
	turn := s.turn
	s.turn++
	s.mu.Unlock()

	if s.script != nil {
		return copyTurnResult(s.script[min(turn, len(s.script)-1)]), nil
	}

	lines := make([]string, len(messages))
	for i, m := range messages {
		lines[i] = m.Role + ": " + m.Content
	}
	prompt := strings.Join(lines, "\n")
	sum := sha256.Sum256([]byte(prompt))
	digest := hex.EncodeToString(sum[:])[:8]
	// Python's len() counts code points, not bytes.
	inTokens := max(1, utf8.RuneCountInString(prompt)/4)
	text := fmt.Sprintf("[stub:%s] acknowledged %d message(s).", digest, len(messages))
	return contracts.TurnResult{
		Text:      text,
		Thinking:  contracts.Str("deterministic stub reasoning for digest " + digest),
		ToolCalls: []contracts.ToolCall{},
		Usage: contracts.Usage{
			InputTokens:  inTokens,
			OutputTokens: utf8.RuneCountInString(text) / 4,
			Model:        s.name,
		},
		StopReason: contracts.Str("end_turn"),
	}, nil
}

// EstimateCostUSD is always 0: a stub is free, by definition.
func (s *StubModel) EstimateCostUSD(contracts.Usage) (float64, error) { return 0, nil }

func copyStr(p *string) *string {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// copyTurnResult is a deep copy (python: result.model_copy(deep=True)).
func copyTurnResult(r contracts.TurnResult) contracts.TurnResult {
	out := r
	out.Thinking = copyStr(r.Thinking)
	out.StopReason = copyStr(r.StopReason)
	out.SessionID = copyStr(r.SessionID)
	out.ToolCalls = make([]contracts.ToolCall, len(r.ToolCalls))
	for i, c := range r.ToolCalls {
		out.ToolCalls[i] = contracts.ToolCall{ID: c.ID, Name: c.Name, Arguments: deepCopyMap(c.Arguments)}
	}
	return out
}

func deepCopyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = deepCopyValue(v)
	}
	return out
}

func deepCopyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return deepCopyMap(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopyValue(e)
		}
		return out
	}
	return v
}

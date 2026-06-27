// Package contracts holds the cross-plane types and interfaces shared by every LHA component.
//
// It is the Go mirror of python/src/lha/contracts. JSON field names, defaults and null-ness match
// the Python (pydantic) models exactly, because these values cross process and language
// boundaries: the .lha/ anchor files, Postgres rows and Temporal payloads are read and written by
// both implementations. Fields are never omitted when empty, mirroring pydantic's model_dump.
package contracts

import (
	"context"
	"errors"
	"fmt"
)

// Usage is real token accounting as reported by the provider.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	// Subset of CacheCreationInputTokens written with the 1-hour TTL (billed higher).
	CacheCreation1hInputTokens int `json:"cache_creation_1h_input_tokens"`
	// The model that ACTUALLY served the turn; cost is keyed on it.
	Model string `json:"model"`
	// The provider (ModelProvider.Name) that served the turn; set by failover.
	Provider string `json:"provider"`
}

// ErrUnknownPrice is wrapped by errors from EstimateCostUSD when the responding model has no
// configured price. Cost must never silently become $0.
var ErrUnknownPrice = errors.New("unknown price")

// UnknownPriceError names the unpriced model.
type UnknownPriceError struct{ Message string }

func (e *UnknownPriceError) Error() string { return e.Message }
func (e *UnknownPriceError) Unwrap() error { return ErrUnknownPrice }

// NewUnknownPriceError builds the error the Python implementation raises for an unpriced model.
func NewUnknownPriceError(model, provider string) error {
	return &UnknownPriceError{Message: fmt.Sprintf(
		"no price configured for model %s on provider %s; "+
			"configure explicit prices (cost is never assumed to be $0)",
		PyRepr(model), PyRepr(provider))}
}

// ToolCall is a tool invocation the model requested (executed by the dispatcher).
type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// ModelMessage is one message in a model conversation ("system" | "user" | "assistant" | "tool").
type ModelMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls"`
	ToolCallID *string    `json:"tool_call_id"`
}

// TurnResult is the real result of one model turn.
type TurnResult struct {
	Text       string     `json:"text"`
	Thinking   *string    `json:"thinking"`
	ToolCalls  []ToolCall `json:"tool_calls"`
	Usage      Usage      `json:"usage"`
	StopReason *string    `json:"stop_reason"`
	SessionID  *string    `json:"session_id"`
}

// ModelProvider is a pluggable LLM backend (stub / Ollama / OpenAI-compatible / Claude).
type ModelProvider interface {
	Name() string
	// Complete runs one real model turn. maxTokens <= 0 means "provider default".
	Complete(ctx context.Context, messages []ModelMessage, tools []map[string]any, maxTokens int) (TurnResult, error)
	// EstimateCostUSD computes real USD cost from real token counts (0 for local/free backends).
	// It returns an error wrapping ErrUnknownPrice when the responding model has no price.
	EstimateCostUSD(usage Usage) (float64, error)
}

// Str returns a pointer to s (for the nullable string fields).
func Str(s string) *string { return &s }

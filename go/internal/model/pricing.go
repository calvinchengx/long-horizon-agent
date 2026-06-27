// Package model is the pluggable model layer (the Go mirror of python/src/lha/model).
//
// One contracts.ModelProvider interface, many backends. BuildProvider selects the concrete backend
// from Settings.ModelBackend so the rest of the system never imports a specific LLM:
//
//   - "stub"          -> StubModel (deterministic; tests/CI only, never shown as a real run)
//   - "ollama"        -> OpenAICompatModel against <ollama_base_url>/v1, priced at $0 (local)
//   - "openai_compat" -> OpenAICompatModel (Groq/Gemini/OpenRouter, ...), explicit prices or UNKNOWN
//   - "claude"        -> ClaudeModel (Anthropic Messages API)
//
// HTTP request bodies, headers, endpoints, retry classification, cost arithmetic and error
// messages match the Python implementation.
package model

import (
	"regexp"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

const mtok = 1_000_000.0

// Default prompt-cache multipliers relative to the input price (Anthropic first-party rates).
const (
	DefaultCacheWriteMultiplier   = 1.25 // 5-minute TTL cache write
	DefaultCacheWrite1hMultiplier = 2.0  // 1-hour TTL cache write
	DefaultCacheReadMultiplier    = 0.1
)

// ModelPrice is USD per 1M tokens, plus prompt-cache multipliers relative to the input price.
// Build it with NewModelPrice to get the default multipliers.
type ModelPrice struct {
	InputPerMTok           float64
	OutputPerMTok          float64
	CacheWriteMultiplier   float64
	CacheWrite1hMultiplier float64
	CacheReadMultiplier    float64
}

// NewModelPrice is a price with the default cache multipliers (python: ModelPrice(in, out)).
func NewModelPrice(inputPerMTok, outputPerMTok float64) ModelPrice {
	return ModelPrice{
		InputPerMTok:           inputPerMTok,
		OutputPerMTok:          outputPerMTok,
		CacheWriteMultiplier:   DefaultCacheWriteMultiplier,
		CacheWrite1hMultiplier: DefaultCacheWrite1hMultiplier,
		CacheReadMultiplier:    DefaultCacheReadMultiplier,
	}
}

// Cost is the USD for usage under this price. input_tokens as reported by the Messages API
// excludes cache reads and writes, so the three are summed separately; 1-hour cache writes are a
// subset of cache_creation_input_tokens billed at the higher multiplier. The arithmetic (and its
// evaluation order) is identical to Python's, so results are bit-for-bit equal.
func (p ModelPrice) Cost(u contracts.Usage) float64 {
	write1h := min(u.CacheCreation1hInputTokens, u.CacheCreationInputTokens)
	write5m := u.CacheCreationInputTokens - write1h
	inputEquiv := float64(u.InputTokens) +
		float64(write5m)*p.CacheWriteMultiplier +
		float64(write1h)*p.CacheWrite1hMultiplier +
		float64(u.CacheReadInputTokens)*p.CacheReadMultiplier
	return inputEquiv/mtok*p.InputPerMTok + float64(u.OutputTokens)/mtok*p.OutputPerMTok
}

// ClaudePrices holds Anthropic first-party rates (last checked 2026-06; verify before relying on
// them — partner platforms price differently). Anything not listed must be configured explicitly.
// Treat it as read-only.
var ClaudePrices = map[string]ModelPrice{
	"claude-opus-4-8":   NewModelPrice(5.0, 25.0),
	"claude-sonnet-4-6": NewModelPrice(3.0, 15.0),
	"claude-haiku-4-5":  NewModelPrice(1.0, 5.0),
}

// dateSuffix mirrors Python's re `-\d{8}$`: \d is any Unicode decimal digit, and `$` also matches
// just before a trailing newline (which is preserved by the substitution).
var dateSuffix = regexp.MustCompile(`-\p{Nd}{8}(\n?)$`)

// LookupClaudePrice returns the price of a Claude model id (tolerating a dated snapshot suffix
// such as "-20251001"), or nil when the model is not in ClaudePrices.
func LookupClaudePrice(model string) *ModelPrice {
	if p, ok := ClaudePrices[model]; ok {
		return &p
	}
	if p, ok := ClaudePrices[dateSuffix.ReplaceAllString(model, "$1")]; ok {
		return &p
	}
	return nil
}

// RequirePrice returns *price, or an error wrapping contracts.ErrUnknownPrice naming the unpriced
// model when price is nil (cost is never silently $0).
func RequirePrice(price *ModelPrice, model, provider string) (ModelPrice, error) {
	if price == nil {
		return ModelPrice{}, contracts.NewUnknownPriceError(model, provider)
	}
	return *price, nil
}

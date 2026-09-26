package model

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// SecretValue unwraps a settings secret ("" when unset or empty; python: secret_value -> None).
func SecretValue(s *config.Secret) string { return s.Value() }

// ChainMemberRetries is how often each member of a failover chain retries a transient error
// before the chain moves on, so an outage fails over in seconds instead of after the full
// per-provider backoff (python: CHAIN_MEMBER_RETRIES).
const ChainMemberRetries = 1

// singleProviderRetries is a lone provider's retry count (the backends' default).
const singleProviderRetries = 3

// BuildProvider constructs the configured model backend — the only place a concrete LLM is
// chosen.
//
// settings nil loads them from the environment (python: get_settings()). name overrides
// settings.ModelName when non-empty (per-role routing) for the PRIMARY model. client lets several
// providers share one HTTP connection pool; the caller then owns it, and the provider's Close
// leaves it open. With a nil client each HTTP provider owns its own, released by its Close (it
// implements io.Closer). With LHA_FALLBACK_MODELS set it returns a *FailoverModel over the primary
// followed by each fallback in order; every turn is priced by the provider that served it.
func BuildProvider(settings *config.Settings, name string, client *http.Client) (contracts.ModelProvider, error) {
	if settings == nil {
		s, err := config.Load()
		if err != nil {
			return nil, err
		}
		settings = s
	}
	if name == "" {
		name = settings.ModelName
	}
	var fallbacks []FallbackSpec
	for _, entry := range settings.FallbackModelEntries() {
		spec, err := ParseFallbackEntry(entry)
		if err != nil {
			return nil, err
		}
		fallbacks = append(fallbacks, spec)
	}
	retries := singleProviderRetries
	if len(fallbacks) > 0 {
		retries = ChainMemberRetries
	}
	price, err := primaryPrice(settings, settings.ModelBackend, name)
	if err != nil {
		return nil, err
	}
	primary, err := buildBackend(settings, settings.ModelBackend, name, client, price, retries)
	if err != nil || len(fallbacks) == 0 {
		return primary, err
	}
	chain := []contracts.ModelProvider{primary}
	for _, spec := range fallbacks {
		p, err := buildBackend(settings, spec.Backend, spec.Model, client, spec.Price, retries)
		if err != nil {
			return nil, err
		}
		chain = append(chain, p)
	}
	return asProvider(NewFailover(chain, FailoverOptions{MaxRounds: Int(settings.FallbackMaxRounds)}))
}

// explicitPrice is an explicit USD-per-1M price (python: the ModelPrice passed to _build_backend).
type explicitPrice = *ModelPrice

// primaryPrice is the explicit price settings give the PRIMARY backend
// (LHA_OPENAI_/CLAUDE_PRICE_*); nil = none.
func primaryPrice(settings *config.Settings, backend, name string) (explicitPrice, error) {
	switch backend {
	case "openai_compat":
		in, out := settings.OpenAIPriceInPerMTok, settings.OpenAIPriceOutPerMTok
		if (in == nil) != (out == nil) {
			return nil, errors.New("configure both price_in_per_mtok and price_out_per_mtok, or neither")
		}
		if in != nil {
			p := NewModelPrice(*in, *out)
			return &p, nil
		}
	case "claude":
		// Explicit prices describe the configured model only, not per-role overrides.
		in, out := settings.ClaudePriceInPerMTok, settings.ClaudePriceOutPerMTok
		if in != nil && out != nil && name == settings.ModelName {
			p := NewModelPrice(*in, *out)
			return &p, nil
		}
	}
	return nil, nil
}

// buildBackend builds one concrete backend. price (explicit) wins over the table / settings
// prices; maxRetries is its per-call retry budget.
func buildBackend(settings *config.Settings, backend, name string, client *http.Client, price explicitPrice, maxRetries int) (contracts.ModelProvider, error) {
	switch backend {
	case "stub":
		return NewStubNamed(name, nil), nil

	case "ollama":
		// Ollama exposes an OpenAI-compatible API at <base>/v1; local, so genuinely $0.
		zero := 0.0
		return asProvider(NewOpenAICompat(OpenAICompatOptions{
			BaseURL:         strings.TrimRight(settings.OllamaBaseURL, "/") + "/v1",
			ModelName:       name,
			APIKey:          "ollama", // Ollama ignores it but the OpenAI shape expects a bearer.
			Label:           "ollama",
			PriceInPerMTok:  &zero,
			PriceOutPerMTok: &zero,
			Client:          client,
			MaxRetries:      Int(maxRetries),
		}))

	case "openai_compat":
		if settings.OpenAIBaseURL == nil || *settings.OpenAIBaseURL == "" {
			return nil, errors.New("LHA_OPENAI_BASE_URL is required for the 'openai_compat' backend.")
		}
		// Prices are optional; unset => cost is UNKNOWN (never silently $0).
		var in, out *float64
		if price != nil {
			in, out = Float(price.InputPerMTok), Float(price.OutputPerMTok)
		}
		return asProvider(NewOpenAICompat(OpenAICompatOptions{
			BaseURL:         *settings.OpenAIBaseURL,
			ModelName:       name,
			APIKey:          SecretValue(settings.OpenAIAPIKey),
			PriceInPerMTok:  in,
			PriceOutPerMTok: out,
			Client:          client,
			MaxRetries:      Int(maxRetries),
		}))

	case "claude":
		apiKey := SecretValue(settings.AnthropicAPIKey)
		if apiKey == "" {
			return nil, errors.New("LHA_ANTHROPIC_API_KEY is required for the 'claude' backend.")
		}
		return asProvider(NewClaude(ClaudeOptions{APIKey: apiKey, ModelName: name, Client: client, Price: price,
			MaxRetries: Int(maxRetries)}))

	case "claude_code":
		// Left at the stub's default name, the model is Claude Code's own choice. No explicit
		// price applies (python: _primary_price is None for claude_code).
		if name == DefaultSettingsModelName() {
			name = ClaudeCodeDefaultModel
		}
		return NewClaudeCode(ClaudeCodeOptions{
			ModelName:    name,
			Binary:       settings.ClaudeCodeBin,
			MaxBudgetUSD: settings.ClaudeCodeMaxBudgetUSD,
			TimeoutS:     settings.ClaudeCodeTimeoutS,
		}), nil

	default:
		return nil, fmt.Errorf("Unknown model backend: %s", contracts.PyRepr(backend))
	}
}

// asProvider avoids returning a typed nil pointer inside a non-nil interface on error.
func asProvider[P contracts.ModelProvider](p P, err error) (contracts.ModelProvider, error) {
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Int returns a pointer to n (for the optional *int option fields).
func Int(n int) *int { return &n }

// Float returns a pointer to x (for the optional *float64 option fields and prices).
func Float(x float64) *float64 { return &x }

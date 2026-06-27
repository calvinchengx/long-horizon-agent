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

// BuildProvider constructs the configured model backend — the only place a concrete LLM is
// chosen.
//
// settings nil loads them from the environment (python: get_settings()). name overrides
// settings.ModelName when non-empty (per-role routing). client lets several providers share one
// HTTP connection pool; the caller then owns it, and the provider's Close leaves it open. With a
// nil client each HTTP provider owns its own, released by its Close (it implements io.Closer).
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

	switch backend := settings.ModelBackend; backend {
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
		}))

	case "openai_compat":
		if settings.OpenAIBaseURL == nil || *settings.OpenAIBaseURL == "" {
			return nil, errors.New("LHA_OPENAI_BASE_URL is required for the 'openai_compat' backend.")
		}
		// Prices are optional settings; unset => cost is UNKNOWN (never silently $0).
		return asProvider(NewOpenAICompat(OpenAICompatOptions{
			BaseURL:         *settings.OpenAIBaseURL,
			ModelName:       name,
			APIKey:          SecretValue(settings.OpenAIAPIKey),
			PriceInPerMTok:  settings.OpenAIPriceInPerMTok,
			PriceOutPerMTok: settings.OpenAIPriceOutPerMTok,
			Client:          client,
		}))

	case "claude":
		apiKey := SecretValue(settings.AnthropicAPIKey)
		if apiKey == "" {
			return nil, errors.New("LHA_ANTHROPIC_API_KEY is required for the 'claude' backend.")
		}
		// Explicit prices describe the configured model only, not per-role overrides.
		var price *ModelPrice
		in, out := settings.ClaudePriceInPerMTok, settings.ClaudePriceOutPerMTok
		if in != nil && out != nil && name == settings.ModelName {
			p := NewModelPrice(*in, *out)
			price = &p
		}
		return asProvider(NewClaude(ClaudeOptions{APIKey: apiKey, ModelName: name, Client: client, Price: price}))

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

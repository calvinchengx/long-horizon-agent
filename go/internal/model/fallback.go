package model

import (
	"errors"
	"strings"
	"unicode"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Backends are the model backends BuildProvider knows, in python's _BACKENDS order.
var Backends = []string{"stub", "ollama", "openai_compat", "claude", "claude_code", "opencode"}

// FallbackSpec is one LHA_FALLBACK_MODELS entry: backend:model[@in/out] (USD per 1M tokens).
type FallbackSpec struct {
	Backend string
	Model   string
	Price   *ModelPrice // nil: no explicit price
	BaseURL string      // an openai_compat entry's own endpoint; "" = the shared LHA_OPENAI_BASE_URL
}

// ParseFallbackEntry parses "backend:model[@in/out]"; the model part may itself contain ':'
// ("ollama:qwen3:8b"). Errors are python's ValueError messages, byte for byte.
func ParseFallbackEntry(entry string) (FallbackSpec, error) {
	body, endpoint := pyStrip(entry), ""
	bar := false
	if i := strings.LastIndex(body, "|"); i >= 0 {
		body, endpoint, bar = body[:i], pyStrip(body[i+1:]), true
	}
	backend, rest, sep := strings.Cut(pyStrip(body), ":")
	backend = strings.ToLower(pyStrip(backend))
	known := false
	for _, b := range Backends {
		known = known || b == backend
	}
	if !sep || !known {
		return FallbackSpec{}, errors.New("invalid LHA_FALLBACK_MODELS entry " + contracts.PyRepr(entry) +
			": expected 'backend:model[@in/out]' with backend one of " + strings.Join(Backends, ", "))
	}
	if bar {
		if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
			return FallbackSpec{}, errors.New("invalid endpoint in LHA_FALLBACK_MODELS entry " + contracts.PyRepr(entry) +
				": expected '|http(s)://host/v1'")
		}
		if backend != "openai_compat" {
			return FallbackSpec{}, errors.New("invalid LHA_FALLBACK_MODELS entry " + contracts.PyRepr(entry) +
				": only openai_compat entries take an endpoint")
		}
	}
	name := rest
	var price *ModelPrice
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		name = rest[:at]
		priceIn, priceOut, slash := strings.Cut(rest[at+1:], "/")
		in, okIn := pyFloat(priceIn)
		out, okOut := pyFloat(priceOut)
		if slash && okIn && okOut {
			p := NewModelPrice(in, out)
			price = &p
		}
		if price == nil || price.InputPerMTok < 0 || price.OutputPerMTok < 0 {
			return FallbackSpec{}, errors.New("invalid price in LHA_FALLBACK_MODELS entry " + contracts.PyRepr(entry) +
				": expected '@<in>/<out>' USD per 1M tokens")
		}
	}
	name = pyStrip(name)
	if name == "" {
		return FallbackSpec{}, errors.New("invalid LHA_FALLBACK_MODELS entry " + contracts.PyRepr(entry) + ": empty model name")
	}
	return FallbackSpec{Backend: backend, Model: name, Price: price, BaseURL: endpoint}, nil
}

func pyIsSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

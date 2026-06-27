package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// OpenAI-compatible defaults (python: OpenAICompatModel keyword defaults).
const (
	DefaultOpenAIMaxTokens = 8192
	DefaultOpenAITimeout   = 120 * time.Second
	DefaultOpenAILabel     = "openai_compat"
)

// OpenAICompatOptions configures NewOpenAICompat. Zero values take the Python defaults; pointer
// fields distinguish "unset" from a meaningful zero.
type OpenAICompatOptions struct {
	BaseURL   string // e.g. "https://api.groq.com/openai/v1" (trailing slashes are stripped)
	ModelName string
	APIKey    string // "" sends no Authorization header
	// USD per 1M tokens. Both nil = UNKNOWN cost (EstimateCostUSD errors, never $0); set both to
	// 0 for a genuinely free/local endpoint. Setting exactly one is an error.
	PriceInPerMTok   *float64
	PriceOutPerMTok  *float64
	DefaultMaxTokens int           // 0 = 8192; always sent as max_tokens
	Timeout          time.Duration // for an owned client only; 0 = 120s
	Client           *http.Client  // nil = an owned client (closed by Close)
	Label            string        // provider name prefix; "" = "openai_compat"
	MaxRetries       *int          // nil = 3
	RetryBaseDelay   *float64      // seconds; nil = 1.0
	Sleep            SleepFunc     // nil = real sleep
}

// OpenAICompatModel is a ModelProvider backed by any OpenAI-compatible /chat/completions endpoint
// (Ollama's /v1, Groq, Gemini's OpenAI endpoint, OpenRouter, Together, ...).
type OpenAICompatModel struct {
	name             string
	baseURL          string
	model            string
	apiKey           string
	defaultMaxTokens int
	price            *ModelPrice
	http             *httpClient
	retry            RetryPolicy
}

var _ contracts.ModelProvider = (*OpenAICompatModel)(nil)

// NewOpenAICompat builds an OpenAI-compatible provider.
func NewOpenAICompat(o OpenAICompatOptions) (*OpenAICompatModel, error) {
	if (o.PriceInPerMTok == nil) != (o.PriceOutPerMTok == nil) {
		return nil, errors.New("configure both price_in_per_mtok and price_out_per_mtok, or neither")
	}
	label := o.Label
	if label == "" {
		label = DefaultOpenAILabel
	}
	m := &OpenAICompatModel{
		name:             label + ":" + o.ModelName,
		baseURL:          strings.TrimRight(o.BaseURL, "/"),
		model:            o.ModelName,
		apiKey:           o.APIKey,
		defaultMaxTokens: o.DefaultMaxTokens,
		retry:            retryPolicy(o.MaxRetries, o.RetryBaseDelay, o.Sleep),
	}
	if m.defaultMaxTokens == 0 {
		m.defaultMaxTokens = DefaultOpenAIMaxTokens
	}
	if o.PriceInPerMTok != nil {
		p := NewModelPrice(*o.PriceInPerMTok, *o.PriceOutPerMTok)
		m.price = &p
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = DefaultOpenAITimeout
	}
	m.http = newHTTPClient(o.Client, timeout)
	return m, nil
}

func retryPolicy(maxRetries *int, baseDelay *float64, sleep SleepFunc) RetryPolicy {
	p := DefaultRetryPolicy()
	if maxRetries != nil {
		p.MaxRetries = *maxRetries
	}
	if baseDelay != nil {
		p.BaseDelaySeconds = *baseDelay
	}
	p.Sleep = sleep
	return p
}

// Name is "<label>:<model>".
func (m *OpenAICompatModel) Name() string { return m.name }

// DefaultMaxTokens is the max_tokens sent when Complete is given maxTokens <= 0.
func (m *OpenAICompatModel) DefaultMaxTokens() int { return m.defaultMaxTokens }

// Close closes the HTTP client if this provider created it.
func (m *OpenAICompatModel) Close() error { return m.http.Close() }

// Wire shapes, field order as in the Python dicts (httpx serialises in insertion order).
type openAIRequest struct {
	Model     string           `json:"model"`
	Messages  []any            `json:"messages"`
	MaxTokens int              `json:"max_tokens"`
	Tools     []map[string]any `json:"tools,omitempty"`
}

type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIToolMessage struct {
	Role       string `json:"role"`
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
}

type openAIAssistantToolCalls struct {
	Role      string           `json:"role"`
	Content   *string          `json:"content"`
	ToolCalls []openAIToolCall `json:"tool_calls"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// toOpenAIMessages maps neutral messages to chat-completions messages (tool calls / results
// preserved).
func toOpenAIMessages(messages []contracts.ModelMessage) ([]any, error) {
	out := make([]any, 0, len(messages))
	for _, m := range messages {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			msg := openAIAssistantToolCalls{Role: "assistant", ToolCalls: []openAIToolCall{}}
			if m.Content != "" {
				msg.Content = contracts.Str(m.Content)
			}
			for _, c := range m.ToolCalls {
				args, err := pyJSONDumps(c.Arguments)
				if err != nil {
					return nil, err
				}
				msg.ToolCalls = append(msg.ToolCalls, openAIToolCall{
					ID: c.ID, Type: "function",
					Function: openAIToolFunction{Name: c.Name, Arguments: args},
				})
			}
			out = append(out, msg)
		case m.Role == "tool":
			if m.ToolCallID != nil && *m.ToolCallID != "" {
				out = append(out, openAIToolMessage{Role: "tool", ToolCallID: *m.ToolCallID, Content: m.Content})
			} else {
				// OpenAI rejects role:"tool" without tool_call_id; degrade to a plain user turn.
				out = append(out, openAIMessage{Role: "user", Content: "TOOL RESULT:\n" + m.Content})
			}
		default:
			out = append(out, openAIMessage{Role: m.Role, Content: m.Content})
		}
	}
	return out, nil
}

// Complete runs one chat-completions turn. maxTokens <= 0 sends DefaultMaxTokens.
func (m *OpenAICompatModel) Complete(ctx context.Context, messages []contracts.ModelMessage, tools []map[string]any, maxTokens int) (contracts.TurnResult, error) {
	msgs, err := toOpenAIMessages(messages)
	if err != nil {
		return contracts.TurnResult{}, err
	}
	if maxTokens <= 0 {
		maxTokens = m.defaultMaxTokens
	}
	body, err := encodeJSON(openAIRequest{Model: m.model, Messages: msgs, MaxTokens: maxTokens, Tools: tools})
	if err != nil {
		return contracts.TurnResult{}, err
	}
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	if m.apiKey != "" {
		header.Set("Authorization", "Bearer "+m.apiKey)
	}
	raw, err := m.http.postJSON(ctx, m.baseURL+"/chat/completions", header, body, m.retry)
	if err != nil {
		return contracts.TurnResult{}, err
	}
	return m.parseResponse(raw)
}

func (m *OpenAICompatModel) parseResponse(raw []byte) (contracts.TurnResult, error) {
	decoded, err := decodeJSON(raw)
	if err != nil {
		return contracts.TurnResult{}, err
	}
	data, ok := decoded.(map[string]any)
	if !ok {
		return contracts.TurnResult{}, fmt.Errorf("%s response is not a JSON object", m.name)
	}
	choices, ok := data["choices"].([]any)
	if !ok || len(choices) == 0 {
		return contracts.TurnResult{}, fmt.Errorf("%s response has no choices[0]", m.name)
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return contracts.TurnResult{}, fmt.Errorf("%s response choices[0] is not an object", m.name)
	}
	message := map[string]any{}
	if v, present := choice["message"]; present {
		if message, ok = v.(map[string]any); !ok {
			return contracts.TurnResult{}, fmt.Errorf("%s response message is not an object", m.name)
		}
	}
	text := ""
	if pyTruthy(message["content"]) {
		s, ok := message["content"].(string)
		if !ok {
			return contracts.TurnResult{}, fmt.Errorf("%s response content is not a string", m.name)
		}
		text = s
	}
	toolCalls, err := parseOpenAIToolCalls(message["tool_calls"])
	if err != nil {
		return contracts.TurnResult{}, err
	}
	usage := map[string]any{}
	if u, ok := data["usage"].(map[string]any); ok {
		usage = u
	}
	in, err := pyIntOr0(usage["prompt_tokens"])
	if err != nil {
		return contracts.TurnResult{}, err
	}
	out, err := pyIntOr0(usage["completion_tokens"])
	if err != nil {
		return contracts.TurnResult{}, err
	}
	model := m.model
	if s, ok := data["model"].(string); ok && s != "" {
		model = s
	}
	var stop *string
	if s, ok := choice["finish_reason"].(string); ok {
		stop = contracts.Str(s)
	}
	return contracts.TurnResult{
		Text:       text,
		ToolCalls:  toolCalls,
		Usage:      contracts.Usage{InputTokens: in, OutputTokens: out, Model: model, Provider: m.name},
		StopReason: stop,
	}, nil
}

// parseOpenAIToolCalls maps OpenAI tool_calls blocks to ToolCalls (arguments arrive as a JSON
// string; unparseable or non-object arguments are kept as {"_raw": <original>}).
func parseOpenAIToolCalls(raw any) ([]contracts.ToolCall, error) {
	calls := []contracts.ToolCall{}
	entries, ok := raw.([]any)
	if !ok {
		return calls, nil
	}
	for _, e := range entries {
		entry, ok := e.(map[string]any)
		if !ok {
			continue
		}
		fn := map[string]any{}
		if v, present := entry["function"]; present {
			if fn, ok = v.(map[string]any); !ok {
				return nil, fmt.Errorf("'%s' object has no attribute 'get'", pyTypeName(v))
			}
		}
		var argsRaw any = "{}"
		if v, present := fn["arguments"]; present {
			argsRaw = v
		}
		var arguments map[string]any
		switch a := argsRaw.(type) {
		case string:
			var parsed any
			if err := json.Unmarshal([]byte(a), &parsed); err == nil {
				arguments, _ = parsed.(map[string]any)
			}
		case map[string]any:
			arguments = plainNumbers(a).(map[string]any)
		}
		if arguments == nil {
			arguments = map[string]any{"_raw": plainNumbers(argsRaw)}
		}
		id, name := "", ""
		if v, present := entry["id"]; present {
			id = pyStr(v)
		}
		if v, present := fn["name"]; present {
			name = pyStr(v)
		}
		calls = append(calls, contracts.ToolCall{ID: id, Name: name, Arguments: arguments})
	}
	return calls, nil
}

// EstimateCostUSD is the cost at the configured prices; an error wrapping
// contracts.ErrUnknownPrice when unpriced.
func (m *OpenAICompatModel) EstimateCostUSD(u contracts.Usage) (float64, error) {
	model := u.Model
	if model == "" {
		model = m.model
	}
	p, err := RequirePrice(m.price, model, m.name)
	if err != nil {
		return 0, err
	}
	return p.Cost(u), nil
}

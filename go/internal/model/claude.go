package model

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Anthropic Messages API constants (python: ClaudeModel.ENDPOINT / API_VERSION / defaults).
const (
	ClaudeEndpoint         = "https://api.anthropic.com/v1/messages"
	ClaudeAPIVersion       = "2023-06-01"
	DefaultClaudeMaxTokens = 4096
	DefaultClaudeTimeout   = 300 * time.Second
)

// ClaudeOptions configures NewClaude. Zero values take the Python defaults.
type ClaudeOptions struct {
	APIKey           string
	ModelName        string
	DefaultMaxTokens int           // 0 = 4096
	Timeout          time.Duration // for an owned client only; 0 = 300s
	Client           *http.Client  // nil = an owned client (closed by Close)
	// Explicit price for ModelName; nil = the ClaudePrices table. NewClaude fails when neither
	// prices the model (cost is never silently $0).
	Price          *ModelPrice
	MaxRetries     *int      // nil = 3
	RetryBaseDelay *float64  // seconds; nil = 1.0
	Sleep          SleepFunc // nil = real sleep
	Endpoint       string    // "" = ClaudeEndpoint (override for tests / proxies)
}

// ClaudeModel is a ModelProvider backed by Anthropic's Messages API: real text + tool use, real
// usage (including prompt-cache read/write tokens and the 1-hour write breakdown), and cost from
// the model the API reports as having served the turn.
type ClaudeModel struct {
	name             string
	apiKey           string
	model            string
	defaultMaxTokens int
	price            *ModelPrice
	endpoint         string
	http             *httpClient
	retry            RetryPolicy
}

var _ contracts.ModelProvider = (*ClaudeModel)(nil)

// NewClaude builds a Claude provider; an unpriced model without an explicit Price is an error
// wrapping contracts.ErrUnknownPrice.
func NewClaude(o ClaudeOptions) (*ClaudeModel, error) {
	price := o.Price
	if price != nil {
		p := *price
		price = &p
	}
	check := price
	if check == nil {
		check = LookupClaudePrice(o.ModelName)
	}
	if _, err := RequirePrice(check, o.ModelName, "claude"); err != nil {
		return nil, err
	}
	m := &ClaudeModel{
		name:             "claude:" + o.ModelName,
		apiKey:           o.APIKey,
		model:            o.ModelName,
		defaultMaxTokens: o.DefaultMaxTokens,
		price:            price,
		endpoint:         o.Endpoint,
		retry:            retryPolicy(o.MaxRetries, o.RetryBaseDelay, o.Sleep),
	}
	if m.defaultMaxTokens == 0 {
		m.defaultMaxTokens = DefaultClaudeMaxTokens
	}
	if m.endpoint == "" {
		m.endpoint = ClaudeEndpoint
	}
	timeout := o.Timeout
	if timeout == 0 {
		timeout = DefaultClaudeTimeout
	}
	m.http = newHTTPClient(o.Client, timeout)
	return m, nil
}

// Name is "claude:<model>".
func (m *ClaudeModel) Name() string { return m.name }

// DefaultMaxTokens is the max_tokens sent when Complete is given maxTokens <= 0.
func (m *ClaudeModel) DefaultMaxTokens() int { return m.defaultMaxTokens }

// Close closes the HTTP client if this provider created it.
func (m *ClaudeModel) Close() error { return m.http.Close() }

// Wire shapes, field order as in the Python dicts.
type claudeRequest struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	Messages  []*claudeMessage `json:"messages"`
	System    string           `json:"system,omitempty"`
	Tools     []map[string]any `json:"tools,omitempty"`
}

type claudeMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string or []any of the block types below
}

type claudeTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type claudeToolUseBlock struct {
	Type  string         `json:"type"`
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

type claudeToolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
}

// toClaudeMessages maps neutral messages to Messages-API turns (tool_use / tool_result blocks
// preserved). System messages are dropped (sent separately). Consecutive tool results are merged
// into ONE user turn, as the API expects all results for an assistant turn's calls together.
func toClaudeMessages(messages []contracts.ModelMessage) []*claudeMessage {
	out := []*claudeMessage{}
	for _, m := range messages {
		switch {
		case m.Role == "system":
			continue
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			blocks := []any{}
			if m.Content != "" {
				blocks = append(blocks, claudeTextBlock{Type: "text", Text: m.Content})
			}
			for _, c := range m.ToolCalls {
				input := c.Arguments
				if input == nil {
					input = map[string]any{}
				}
				blocks = append(blocks, claudeToolUseBlock{Type: "tool_use", ID: c.ID, Name: c.Name, Input: input})
			}
			out = append(out, &claudeMessage{Role: "assistant", Content: blocks})
		case m.Role == "tool" && m.ToolCallID != nil && *m.ToolCallID != "":
			block := claudeToolResultBlock{Type: "tool_result", ToolUseID: *m.ToolCallID, Content: m.Content}
			if n := len(out); n > 0 && out[n-1].Role == "user" && allToolResults(out[n-1].Content) {
				out[n-1].Content = append(out[n-1].Content.([]any), block)
			} else {
				out = append(out, &claudeMessage{Role: "user", Content: []any{block}})
			}
		case m.Role == "tool":
			// A tool result with no call id cannot be a tool_result block; send it as plain text.
			out = append(out, &claudeMessage{Role: "user", Content: "TOOL RESULT:\n" + m.Content})
		default:
			role := "user"
			if m.Role == "assistant" {
				role = "assistant"
			}
			out = append(out, &claudeMessage{Role: role, Content: m.Content})
		}
	}
	return out
}

// allToolResults: content is a block list made only of tool_result blocks (vacuously true for an
// empty list, as Python's all()).
func allToolResults(content any) bool {
	blocks, ok := content.([]any)
	if !ok {
		return false
	}
	for _, b := range blocks {
		if _, ok := b.(claudeToolResultBlock); !ok {
			return false
		}
	}
	return true
}

// Complete runs one Messages API turn. maxTokens <= 0 sends DefaultMaxTokens.
func (m *ClaudeModel) Complete(ctx context.Context, messages []contracts.ModelMessage, tools []map[string]any, maxTokens int) (contracts.TurnResult, error) {
	// Anthropic takes the system prompt separately from the conversation turns.
	var system []string
	for _, msg := range messages {
		if msg.Role == "system" {
			system = append(system, msg.Content)
		}
	}
	if maxTokens <= 0 {
		maxTokens = m.defaultMaxTokens
	}
	body, err := encodeJSON(claudeRequest{
		Model:     m.model,
		MaxTokens: maxTokens,
		Messages:  toClaudeMessages(messages),
		System:    strings.Join(system, "\n\n"),
		Tools:     tools,
	})
	if err != nil {
		return contracts.TurnResult{}, err
	}
	header := http.Header{}
	header.Set("x-api-key", m.apiKey)
	header.Set("anthropic-version", ClaudeAPIVersion)
	header.Set("content-type", "application/json")
	raw, err := m.http.postJSON(ctx, m.endpoint, header, body, m.retry)
	if err != nil {
		return contracts.TurnResult{}, err
	}
	return m.parseResponse(raw)
}

func (m *ClaudeModel) parseResponse(raw []byte) (contracts.TurnResult, error) {
	decoded, err := decodeJSON(raw)
	if err != nil {
		return contracts.TurnResult{}, err
	}
	data, ok := decoded.(map[string]any)
	if !ok {
		return contracts.TurnResult{}, fmt.Errorf("%s response is not a JSON object", m.name)
	}
	var blocks []any
	if v, present := data["content"]; present {
		if blocks, ok = v.([]any); !ok {
			return contracts.TurnResult{}, fmt.Errorf("%s response content is not a list", m.name)
		}
	}
	var text strings.Builder
	toolCalls := []contracts.ToolCall{}
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			return contracts.TurnResult{}, fmt.Errorf("%s response content block is not an object", m.name)
		}
		switch block["type"] {
		case "text":
			if v, present := block["text"]; present {
				s, ok := v.(string)
				if !ok {
					return contracts.TurnResult{}, fmt.Errorf("%s response text block is not a string", m.name)
				}
				text.WriteString(s)
			}
		case "tool_use":
			id, name := "", ""
			if v, present := block["id"]; present {
				id = pyStr(v)
			}
			if v, present := block["name"]; present {
				name = pyStr(v)
			}
			input := map[string]any{}
			if v, present := block["input"]; present {
				obj, ok := v.(map[string]any)
				if !ok {
					return contracts.TurnResult{}, fmt.Errorf("%s response tool_use input is not an object", m.name)
				}
				input = plainNumbers(obj).(map[string]any)
			}
			toolCalls = append(toolCalls, contracts.ToolCall{ID: id, Name: name, Arguments: input})
		}
	}

	usage := map[string]any{}
	if pyTruthy(data["usage"]) {
		if usage, ok = data["usage"].(map[string]any); !ok {
			return contracts.TurnResult{}, fmt.Errorf("%s response usage is not an object", m.name)
		}
	}
	var u contracts.Usage
	for _, f := range []struct {
		key string
		dst *int
	}{
		{"input_tokens", &u.InputTokens},
		{"output_tokens", &u.OutputTokens},
		{"cache_read_input_tokens", &u.CacheReadInputTokens},
		{"cache_creation_input_tokens", &u.CacheCreationInputTokens},
	} {
		if *f.dst, err = pyIntOr0(usage[f.key]); err != nil {
			return contracts.TurnResult{}, err
		}
	}
	if breakdown, ok := usage["cache_creation"].(map[string]any); ok {
		if u.CacheCreation1hInputTokens, err = pyIntOr0(breakdown["ephemeral_1h_input_tokens"]); err != nil {
			return contracts.TurnResult{}, err
		}
	}
	u.Model = m.model
	if s, ok := data["model"].(string); ok && s != "" {
		u.Model = s
	}
	u.Provider = m.name
	var stop *string
	if s, ok := data["stop_reason"].(string); ok {
		stop = contracts.Str(s)
	}
	return contracts.TurnResult{Text: text.String(), ToolCalls: toolCalls, Usage: u, StopReason: stop}, nil
}

// EstimateCostUSD prices usage by the model the API reported (falling back to the configured
// model when unset). An explicit Price applies to the configured model only; any other reported
// model uses the ClaudePrices table (then the explicit price, if any).
func (m *ClaudeModel) EstimateCostUSD(u contracts.Usage) (float64, error) {
	model := u.Model
	if model == "" {
		model = m.model
	}
	var price *ModelPrice
	if model == m.model {
		price = m.price
	}
	if price == nil {
		price = LookupClaudePrice(model)
	}
	if price == nil {
		price = m.price
	}
	p, err := RequirePrice(price, model, m.name)
	if err != nil {
		return 0, err
	}
	return p.Cost(u), nil
}

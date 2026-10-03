package model

import (
	"fmt"
	"math"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// SessionProgress is a running claude -p session's progress, from its stream-json output (python:
// lha.model.claude_code.SessionProgress).
//
// SpentUSD prices the tokens of every model turn so far; it is unknown before the first turn and
// while a turn's model has no price, so a session whose spend cannot be seen is charged its cap,
// never $0. It can be below what Claude Code reports at the end, which also counts calls it makes
// outside the conversation.
type SessionProgress struct {
	Turns     int
	ToolCalls int
	Tool      string // the last tool called
	SessionID string
	order     []string                   // message ids, first seen first
	messages  map[string]progressMessage // message id -> its model and usage
	toolIDs   map[string]bool
}

type progressMessage struct {
	model string
	usage *pyfmt.OrderedMap
}

// Observe takes one stream-json event; it reports whether the event began a turn or called a tool.
func (p *SessionProgress) Observe(event *pyfmt.OrderedMap) bool {
	if p.messages == nil {
		p.messages, p.toolIDs = map[string]progressMessage{}, map[string]bool{}
	}
	if s, ok := omGet(event, "session_id").(string); ok && s != "" {
		p.SessionID = s
	}
	message, ok := omGet(event, "message").(*pyfmt.OrderedMap)
	if omGet(event, "type") != "assistant" || !ok {
		return false
	}
	changed := false
	id, _ := omGet(message, "id").(string)
	if id == "" {
		id = fmt.Sprintf("anonymous-%d", len(p.messages))
	}
	if _, seen := p.messages[id]; !seen {
		p.Turns++
		p.order = append(p.order, id)
		changed = true
	}
	m, _ := omGet(message, "model").(string)
	usage, _ := omGet(message, "usage").(*pyfmt.OrderedMap)
	p.messages[id] = progressMessage{model: m, usage: usage}
	content, _ := omGet(message, "content").([]any)
	for _, raw := range content {
		block, ok := raw.(*pyfmt.OrderedMap)
		if !ok || omGet(block, "type") != "tool_use" {
			continue
		}
		blockID, _ := omGet(block, "id").(string)
		if blockID == "" {
			blockID = fmt.Sprintf("anonymous-%d", len(p.toolIDs))
		}
		if !p.toolIDs[blockID] {
			p.toolIDs[blockID] = true
			p.ToolCalls++
			name, _ := omGet(block, "name").(string)
			p.Tool = name
			changed = true
		}
	}
	return changed
}

// SpentUSD is the priced cost of the turns so far (rounded to 6 places); ok is false before the
// first turn and while a turn's model has no price.
func (p *SessionProgress) SpentUSD() (usd float64, ok bool) {
	if len(p.order) == 0 {
		return 0, false
	}
	cost := 0.0
	for _, id := range p.order {
		m := p.messages[id]
		price := LookupClaudePrice(m.model)
		if price == nil {
			return 0, false
		}
		cost += price.Cost(turnUsage(m.usage, m.model, ""))
	}
	return math.Round(cost*1e6) / 1e6, true
}

// Usage is the tokens so far as one contracts.Usage, with SpentUSD as its reported cost (nil
// when unpriced).
func (p *SessionProgress) Usage(provider, fallbackModel string) contracts.Usage {
	total := contracts.Usage{Provider: provider, Model: fallbackModel}
	for _, id := range p.order {
		m := p.messages[id]
		model := m.model
		if model == "" {
			model = fallbackModel
		}
		t := turnUsage(m.usage, model, provider)
		total.InputTokens += t.InputTokens
		total.OutputTokens += t.OutputTokens
		total.CacheReadInputTokens += t.CacheReadInputTokens
		total.CacheCreationInputTokens += t.CacheCreationInputTokens
		total.CacheCreation1hInputTokens += t.CacheCreation1hInputTokens
		if t.Model != "" {
			total.Model = t.Model
		}
	}
	if usd, ok := p.SpentUSD(); ok {
		total.ReportedCostUSD = &usd
	}
	return total
}

func turnUsage(raw *pyfmt.OrderedMap, model, provider string) contracts.Usage {
	u := contracts.Usage{
		InputTokens:              pyInt(omGet(raw, "input_tokens")),
		OutputTokens:             pyInt(omGet(raw, "output_tokens")),
		CacheReadInputTokens:     pyInt(omGet(raw, "cache_read_input_tokens")),
		CacheCreationInputTokens: pyInt(omGet(raw, "cache_creation_input_tokens")),
		Model:                    model,
		Provider:                 provider,
	}
	if breakdown, ok := omGet(raw, "cache_creation").(*pyfmt.OrderedMap); ok {
		u.CacheCreation1hInputTokens = pyInt(omGet(breakdown, "ephemeral_1h_input_tokens"))
	}
	return u
}

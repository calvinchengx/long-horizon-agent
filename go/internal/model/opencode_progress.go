package model

import (
	"math"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// Progress is what a lead session reports as it runs, shared by the claude_code and opencode
// engines (their per-stream progress types differ, but the loop's session_progress event only
// needs these four answers).
type Progress interface {
	ProgressTurns() int
	ProgressToolCalls() int
	ProgressTool() string
	ProgressSpentUSD() (float64, bool)
}

// ProgressTurns is the turns so far (claude: SessionProgress.Turns).
func (p *SessionProgress) ProgressTurns() int { return p.Turns }

// ProgressToolCalls is the tool calls so far (claude: SessionProgress.ToolCalls).
func (p *SessionProgress) ProgressToolCalls() int { return p.ToolCalls }

// ProgressTool is the last tool called (claude: SessionProgress.Tool).
func (p *SessionProgress) ProgressTool() string { return p.Tool }

// ProgressSpentUSD is the priced cost so far (claude: SessionProgress.SpentUSD).
func (p *SessionProgress) ProgressSpentUSD() (float64, bool) { return p.SpentUSD() }

// OpenCodeSessionProgress is a running opencode run session's progress, from its --format json
// stream (python: lha.model.opencode.SessionProgress).
//
// SpentUSD sums the costs OpenCode reports per step; it is unknown before the first step and while
// a step reports no cost, so a session whose spend cannot be seen is charged its cap, never $0.
type OpenCodeSessionProgress struct {
	Turns     int
	ToolCalls int
	Tool      string // the last tool called
	SessionID string
	// steps is messageID -> the step's usage tokens and cost, filled at step_finish.
	steps   map[string]openCodeStep
	toolIDs map[string]bool
}

type openCodeStep struct {
	tokens *pyfmt.OrderedMap
	cost   *float64 // nil when the step reported no cost
}

// Observe takes one stream event; it reports whether the event began a turn or called a tool.
func (p *OpenCodeSessionProgress) Observe(event *pyfmt.OrderedMap) bool {
	if p.steps == nil {
		p.steps, p.toolIDs = map[string]openCodeStep{}, map[string]bool{}
	}
	session := omGet(event, "sessionID")
	if !ccTruthy(session) {
		session = omGet(event, "session_id")
	}
	if s, ok := session.(string); ok && s != "" {
		p.SessionID = s
	}
	part, _ := omGet(event, "part").(*pyfmt.OrderedMap)
	kind := omGet(event, "type")
	stepID, stepIDIsStr := omGet(part, "messageID").(string)
	switch kind {
	case "step_start":
		if stepIDIsStr {
			if _, seen := p.steps[stepID]; !seen {
				p.steps[stepID] = openCodeStep{}
				p.Turns++
				return true
			}
		}
		return false
	case "step_finish":
		changed := false
		if stepIDIsStr {
			if _, seen := p.steps[stepID]; !seen {
				p.Turns++
				changed = true
			}
		}
		tokens, _ := omGet(part, "tokens").(*pyfmt.OrderedMap)
		var cost *float64
		if c, _, ok := pyNumber(omGet(part, "cost")); ok {
			cost = &c
		}
		if stepIDIsStr {
			p.steps[stepID] = openCodeStep{tokens: tokens, cost: cost}
		}
		return changed
	case "tool_use":
		id := omGet(part, "id")
		if !ccTruthy(id) {
			id = omGet(part, "partID")
		}
		if callID, ok := id.(string); ok && !p.toolIDs[callID] {
			p.toolIDs[callID] = true
			p.ToolCalls++
			tool := omGet(part, "tool")
			if !ccTruthy(tool) { // python: str(part.get("tool") or "")
				p.Tool = ""
			} else {
				p.Tool = pyfmt.PyStr(tool)
			}
			return true
		}
	}
	return false
}

// SpentUSD is the cost OpenCode reported per step so far (rounded to 6 places); ok is false before
// the first step and while any step reported no cost.
func (p *OpenCodeSessionProgress) SpentUSD() (usd float64, ok bool) {
	if len(p.steps) == 0 {
		return 0, false
	}
	cost := 0.0
	for _, step := range p.steps {
		if step.cost == nil {
			return 0, false
		}
		cost += *step.cost
	}
	return math.Round(cost*1e6) / 1e6, true
}

// Usage is the tokens so far as one contracts.Usage, with SpentUSD as its reported cost (nil when
// unpriced).
func (p *OpenCodeSessionProgress) Usage(provider, fallbackModel string) contracts.Usage {
	total := contracts.Usage{Provider: provider, Model: fallbackModel}
	for _, step := range p.steps {
		u := openCodeStepUsage(step)
		total.InputTokens += u.InputTokens
		total.OutputTokens += u.OutputTokens
		total.CacheReadInputTokens += u.CacheReadInputTokens
		total.CacheCreationInputTokens += u.CacheCreationInputTokens
	}
	if usd, ok := p.SpentUSD(); ok {
		total.ReportedCostUSD = &usd
	}
	return total
}

// ProgressTurns is the turns so far.
func (p *OpenCodeSessionProgress) ProgressTurns() int { return p.Turns }

// ProgressToolCalls is the tool calls so far.
func (p *OpenCodeSessionProgress) ProgressToolCalls() int { return p.ToolCalls }

// ProgressTool is the last tool called.
func (p *OpenCodeSessionProgress) ProgressTool() string { return p.Tool }

// ProgressSpentUSD is the cost OpenCode reported so far (unknown while a step reports none).
func (p *OpenCodeSessionProgress) ProgressSpentUSD() (float64, bool) { return p.SpentUSD() }

// openCodeStepUsage is one step's tokens as a Usage (python: _step_usage).
func openCodeStepUsage(step openCodeStep) contracts.Usage {
	tokens := step.tokens
	cache, _ := omGet(tokens, "cache").(*pyfmt.OrderedMap)
	return contracts.Usage{
		InputTokens:              pyInt(omGet(tokens, "input")),
		OutputTokens:             pyInt(omGet(tokens, "output")),
		CacheReadInputTokens:     pyInt(omGet(cache, "read")),
		CacheCreationInputTokens: pyInt(omGet(cache, "write")),
	}
}

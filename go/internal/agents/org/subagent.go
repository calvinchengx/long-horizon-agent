// Package org is the multi-agent organization (python: lha.agents.orchestrator, waves,
// integrator, reviewer, team, subagent, specialists): read-only Researchers fanned out in
// parallel, the Lead's serial cycles, parallel Implementer waves in git worktrees merged by the
// deterministic BranchIntegrator, the fresh-context Reviewer and reflection on failure — driven
// by the Orchestrator behind `lha orchestrate`.
//
// The building blocks are plain exported functions (ImplementInWorktree, IntegrateRun,
// BranchIntegrator, ResearchFanout, Reviewer.Review, coordination.LeaseBroker) so the durable
// organization rounds can run each of them as its own activity.
package org

import (
	"context"
	"fmt"

	"github.com/calvinchengx/long-horizon-agent/go/internal/agent"
	"github.com/calvinchengx/long-horizon-agent/go/internal/agents"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// A generic sub-agent (python: lha.agents.subagent): runs a role's bounded ReAct loop and returns
// a condensed brief. Read-only roles (researcher / reviewer) use it directly; the Implementer
// uses it with a scoped, ownership-guarded dispatcher. It reuses the Lead's action parser.
//
// The role's tool policy is enforced HERE as well as in the dispatcher: a role without
// AllowMutating (or AllowEgress) is never shown those tools, and any call to one is refused
// without being dispatched.

const (
	subAgentObservationCap = 4000
	subAgentBriefCap       = 8000
)

// FinalTurnMessage is the last user turn of a sub-agent whose tool budget ran out while it was
// still working (python: FINAL_TURN_MESSAGE).
const FinalTurnMessage = "Your tool budget is spent: no further tool call will be answered. Reply now with your " +
	`final answer as the JSON object described above ({"done": true, ...}), using what you ` +
	"have seen so far."

// SubAgentResult is what a sub-agent returns.
type SubAgentResult struct {
	Role      string
	Brief     string
	ToolCalls int
	Turns     int
	// FinalText is the raw text of the final model turn (structured verdicts are parsed from it).
	FinalText string
	// Error is set when the sub-agent failed (e.g. a model/API error in a fan-out); Brief is then
	// empty.
	Error string
}

// SubAgent runs one role as a bounded, read-or-scoped loop returning a condensed artifact.
type SubAgent struct {
	role       agents.RoleSpec
	model      contracts.ModelProvider
	dispatcher contracts.ToolDispatcher
	maxTurns   int
}

// NewSubAgent returns a sub-agent for role (maxTurns <= 0: the role's MaxTurns).
func NewSubAgent(role agents.RoleSpec, model contracts.ModelProvider, dispatcher contracts.ToolDispatcher, maxTurns int) *SubAgent {
	if maxTurns <= 0 {
		maxTurns = role.MaxTurns
	}
	return &SubAgent{role: role, model: model, dispatcher: dispatcher, maxTurns: maxTurns}
}

func (s *SubAgent) rolePermits(spec contracts.ToolSpec) bool {
	if spec.Mutating && !s.role.AllowMutating {
		return false
	}
	return !(spec.Egress && !s.role.AllowEgress)
}

// VisibleSpecs are the tools this role may see and call (dispatcher permits ∩ role policy).
func (s *SubAgent) VisibleSpecs() []contracts.ToolSpec {
	out := []contracts.ToolSpec{}
	for _, spec := range s.dispatcher.Specs() {
		if s.rolePermits(spec) {
			out = append(out, spec)
		}
	}
	return out
}

// SubAgentMessages are the first two messages of a sub-agent run: the role's system prompt with
// its tools and the action protocol, and the objective (+ extra context).
func SubAgentMessages(role agents.RoleSpec, specs []contracts.ToolSpec, objective, extraContext string) []contracts.ModelMessage {
	system := role.SystemPrompt + "\n\nAvailable tools:\n" + agent.RenderTools(specs) + "\n\n" + agent.ActionInstructions
	return []contracts.ModelMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: pyfmt.PyStrip(objective + "\n\n" + extraContext)},
	}
}

func (s *SubAgent) dispatch(ctx context.Context, call contracts.ToolCall, tctx contracts.ToolContext, allowed map[string]bool) contracts.ToolResult {
	if !allowed[call.Name] {
		return contracts.Failure(fmt.Sprintf("tool %s is not available to the %s role", contracts.PyRepr(call.Name), s.role.Name))
	}
	if call.Arguments == nil {
		call.Arguments = map[string]any{}
	}
	return s.dispatcher.Dispatch(ctx, call, tctx)
}

func observationOf(r contracts.ToolResult) string {
	text := r.Content
	if text == "" {
		text = r.ErrorText()
	}
	return pyfmt.Head(text, subAgentObservationCap)
}

// Run runs the role on objective until it signals done or its turns run out. A model error is
// returned as an error (ResearchFanout turns it into a failed result).
func (s *SubAgent) Run(ctx context.Context, objective string, tctx contracts.ToolContext, extraContext string) (SubAgentResult, error) {
	specs := s.VisibleSpecs()
	allowed := map[string]bool{}
	for _, spec := range specs {
		allowed[spec.Name] = true
	}
	messages := SubAgentMessages(s.role, specs, objective, extraContext)

	toolCalls, turns := 0, 0
	brief, finalText := "", ""
	for turn := 1; turn <= s.maxTurns; turn++ {
		turns = turn
		result, err := s.model.Complete(ctx, messages, nil, 0)
		if err != nil {
			return SubAgentResult{}, err
		}
		finalText = result.Text

		if len(result.ToolCalls) > 0 {
			// Native tool use: keep the calls on the assistant turn and answer each by id.
			messages = append(messages, contracts.ModelMessage{Role: "assistant", Content: result.Text, ToolCalls: result.ToolCalls})
			for _, native := range result.ToolCalls {
				observation := observationOf(s.dispatch(ctx, native, tctx, allowed))
				toolCalls++
				id := native.ID
				messages = append(messages, contracts.ModelMessage{Role: "tool", Content: observation, ToolCallID: &id})
			}
			continue
		}

		action := agent.ParseAction(result.Text, nil, nil)
		if action.Done || action.Tool == "" {
			brief = action.Summary
			if brief == "" {
				brief = result.Text
			}
			break
		}
		call := contracts.ToolCall{ID: fmt.Sprintf("%s-%d", s.role.Name, turn), Name: action.Tool, Arguments: action.Arguments}
		observation := observationOf(s.dispatch(ctx, call, tctx, allowed))
		toolCalls++
		messages = append(messages,
			contracts.ModelMessage{Role: "assistant", Content: result.Text},
			contracts.ModelMessage{Role: "user", Content: "OBSERVATION (" + action.Tool + "): " + observation})
	}
	if brief == "" {
		// The turn budget ran out while the model was still working. One last turn, told that
		// no tool call will be answered, asks for the answer it has; a reviewer that was still
		// reading files returns its verdict instead of a dangling tool call.
		turns++
		messages = append(messages,
			contracts.ModelMessage{Role: "assistant", Content: finalText},
			contracts.ModelMessage{Role: "user", Content: FinalTurnMessage})
		result, err := s.model.Complete(ctx, messages, nil, 0)
		if err != nil {
			return SubAgentResult{}, err
		}
		finalText = result.Text
		action := agent.ParseAction(result.Text, nil, nil)
		brief = action.Summary
		if brief == "" {
			brief = result.Text
		}
	}
	return SubAgentResult{
		Role:      s.role.Name,
		Brief:     pyfmt.Head(brief, subAgentBriefCap),
		ToolCalls: toolCalls,
		Turns:     turns,
		FinalText: finalText,
	}, nil
}

// --- the role runners --------------------------------------------------------------------------

// Implementer works one file-disjoint checklist item with a scoped dispatcher (python:
// lha.agents.specialists.Implementer).
type Implementer struct{ agent *SubAgent }

// NewImplementer returns the implementer role runner (maxTurns <= 0: the role's default).
func NewImplementer(model contracts.ModelProvider, dispatcher contracts.ToolDispatcher, maxTurns int) *Implementer {
	return &Implementer{agent: NewSubAgent(agents.Roles["implementer"], model, dispatcher, maxTurns)}
}

// Run works objective.
func (i *Implementer) Run(ctx context.Context, objective string, tctx contracts.ToolContext, extraContext string) (SubAgentResult, error) {
	return i.agent.Run(ctx, objective, tctx, extraContext)
}

// Reviewer is the adversarial, fresh-context diff review (python: lha.agents.reviewer.Reviewer):
// it shares no trace with the author and returns a structured verdict.
type Reviewer struct{ agent *SubAgent }

// NewReviewer returns the Reviewer over a read-only dispatcher.
func NewReviewer(model contracts.ModelProvider, dispatcher contracts.ToolDispatcher) *Reviewer {
	return &Reviewer{agent: NewSubAgent(agents.Roles["reviewer"], model, dispatcher, 0)}
}

// Review reviews diff against criteria.
func (r *Reviewer) Review(ctx context.Context, diff, criteria string, tctx contracts.ToolContext) (agents.ReviewResult, error) {
	result, err := r.agent.Run(ctx, agents.ReviewObjective, tctx, agents.ReviewExtraContext(criteria, diff))
	if err != nil {
		return agents.ReviewResult{}, err
	}
	text := result.FinalText
	if text == "" {
		text = result.Brief
	}
	verdict, blocking, issues, advisory := agents.ParseReview(text)
	return agents.ReviewResult{
		Brief: result.Brief, Blocking: blocking, ToolCalls: result.ToolCalls,
		Verdict: verdict, BlockingIssues: issues, Advisory: advisory,
	}, nil
}

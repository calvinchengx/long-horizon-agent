package tools

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution"
	"github.com/calvinchengx/long-horizon-agent/go/internal/execution/internal/pyval"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
	"github.com/calvinchengx/long-horizon-agent/go/internal/safety/pystr"
)

// record_decision lets the agent write a design decision to the never-compacted log
// (python/src/lha/execution/tools/decisions.py).
//
// The tool does not touch the workspace. It hands a DecisionRecord to a DecisionSink:
// *state.GitMissionAnchor (queued in memory, chained onto .lha/decisions.ndjson by the cycle's
// checkpoint commit) or a DecisionBuffer (an implementer's decisions, committed by the integrator
// only if its work is merged). WithDecisionTool wraps any ToolDispatcher so the tool is offered
// and served next to the dispatcher's own tools.

// RecordDecision is the tool's name.
const RecordDecision = "record_decision"

const (
	maxDecision = 500
	maxText     = 2_000
	maxAffected = 50
	maxPath     = 300
)

// DecisionSink is where recorded decisions go until they are committed.
// *state.GitMissionAnchor implements it.
type DecisionSink interface {
	// RecordDecision queues record and returns how many decisions are now queued.
	RecordDecision(record contracts.DecisionRecord) int
}

// DecisionBuffer is an in-memory DecisionSink (e.g. one parallel implementer's decisions).
type DecisionBuffer struct {
	mu      sync.Mutex
	Records []contracts.DecisionRecord
}

// RecordDecision implements DecisionSink.
func (b *DecisionBuffer) RecordDecision(record contracts.DecisionRecord) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Records = append(b.Records, record)
	return len(b.Records)
}

// RecordDecisionTool records one design decision (committed with the current cycle's checkpoint).
type RecordDecisionTool struct {
	Sink DecisionSink
}

// Spec implements contracts.Tool.
func (RecordDecisionTool) Spec() contracts.ToolSpec {
	return contracts.ToolSpec{
		Name: RecordDecision,
		Description: "Record a design decision that later work must stay consistent with (an interface, " +
			"data format, library choice, naming rule...). It is committed with this cycle's " +
			"checkpoint to the mission's append-only, hash-chained decision log and shown to " +
			"every later cycle. Record the decision, why, what you rejected, and affected files.",
		Parameters: map[string]any{
			"type": "object",
			"properties": pyfmt.NewOrderedMap(
				"decision", map[string]any{"type": "string"},
				"rationale", map[string]any{"type": "string"},
				"alternatives_rejected", map[string]any{"type": "string"},
				"affected", pyfmt.NewOrderedMap("type", "array", "items", map[string]any{"type": "string"}),
			),
			"required":             []any{"decision", "rationale"},
			"additionalProperties": false,
		},
		// It writes durable mission state (not the workspace): read-only roles never get it.
		Mutating: true,
	}
}

// Run implements contracts.Tool.
func (t RecordDecisionTool) Run(_ context.Context, arguments map[string]any, _ contracts.ToolContext) contracts.ToolResult {
	decision := pystr.Strip(strArg(arguments, "decision"))
	rationale := pystr.Strip(strArg(arguments, "rationale"))
	if decision == "" || rationale == "" {
		return contracts.Failure("record_decision needs a non-empty decision and rationale")
	}
	if n := pyval.Len(decision); n > maxDecision {
		return contracts.Failure(fmt.Sprintf("decision is too long (%d > %d chars); state it "+
			"briefly and put the detail in the rationale", n, maxDecision))
	}
	affected := []string{}
	if raw, ok := arguments["affected"].([]any); ok {
		for _, a := range raw {
			if s := pystr.Strip(pyval.Str(a)); s != "" {
				affected = append(affected, pyval.Head(s, maxPath))
			}
		}
	} else if raw, ok := arguments["affected"].([]string); ok {
		for _, a := range raw {
			if s := pystr.Strip(a); s != "" {
				affected = append(affected, pyval.Head(s, maxPath))
			}
		}
	}
	if len(affected) > maxAffected {
		affected = affected[:maxAffected]
	}
	record := contracts.DecisionRecord{
		Decision:             decision,
		Rationale:            pyval.Head(rationale, maxText),
		AlternativesRejected: pyval.Head(pystr.Strip(strArg(arguments, "alternatives_rejected")), maxText),
		Affected:             affected,
	}
	queued := t.Sink.RecordDecision(record)
	return contracts.Success("recorded decision (" + strconv.Itoa(queued) +
		" this cycle); it is committed with this cycle's checkpoint")
}

// DecisionToolDispatcher serves record_decision itself and delegates everything else.
type DecisionToolDispatcher struct {
	inner contracts.ToolDispatcher
	tool  RecordDecisionTool
}

var (
	_ contracts.ToolDispatcher = (*DecisionToolDispatcher)(nil)
	_ contracts.EventDrainer   = (*DecisionToolDispatcher)(nil)
)

// NewDecisionToolDispatcher wraps inner with record_decision bound to sink.
func NewDecisionToolDispatcher(inner contracts.ToolDispatcher, sink DecisionSink) *DecisionToolDispatcher {
	return &DecisionToolDispatcher{inner: inner, tool: RecordDecisionTool{Sink: sink}}
}

// Inner is the wrapped dispatcher.
func (d *DecisionToolDispatcher) Inner() contracts.ToolDispatcher { return d.inner }

// Specs is the inner specs plus record_decision.
func (d *DecisionToolDispatcher) Specs() []contracts.ToolSpec {
	return append(append([]contracts.ToolSpec{}, d.inner.Specs()...), d.tool.Spec())
}

// DrainEvents forwards the wrapped dispatcher's gate events (tool_approval etc.), so wrapping
// never hides the approval audit trail from the agent loop.
func (d *DecisionToolDispatcher) DrainEvents() []contracts.EventRecord {
	if drainer, ok := d.inner.(contracts.EventDrainer); ok {
		return append([]contracts.EventRecord{}, drainer.DrainEvents()...)
	}
	return []contracts.EventRecord{}
}

// Dispatch serves record_decision (validated like the AllowListDispatcher does) or delegates.
func (d *DecisionToolDispatcher) Dispatch(ctx context.Context, call contracts.ToolCall, tctx contracts.ToolContext) (result contracts.ToolResult) {
	if call.Name != RecordDecision {
		return d.inner.Dispatch(ctx, call, tctx)
	}
	arguments := call.Arguments
	if arguments == nil {
		arguments = map[string]any{}
	}
	if errText := execution.ValidateArguments(d.tool.Spec().Parameters, arguments, ""); errText != "" {
		return contracts.Failure("invalid args for " + contracts.PyRepr(RecordDecision) + ": " + errText)
	}
	defer func() {
		if r := recover(); r != nil { // the loop never crashes on a tool error
			result = contracts.Failure(fmt.Sprintf("RuntimeError: %v", r))
		}
	}()
	return d.tool.Run(ctx, arguments, tctx)
}

// WithDecisionTool returns dispatcher plus record_decision bound to sink (unchanged if it
// already offers the tool).
func WithDecisionTool(dispatcher contracts.ToolDispatcher, sink DecisionSink) contracts.ToolDispatcher {
	for _, spec := range dispatcher.Specs() {
		if spec.Name == RecordDecision {
			return dispatcher
		}
	}
	return NewDecisionToolDispatcher(dispatcher, sink)
}

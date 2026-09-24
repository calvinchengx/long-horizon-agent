package agents

import (
	"context"
	"fmt"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// The Replanner splits a blocked checklist item into smaller ones instead of deadlocking
// (python: lha.agents.replanner). It asks the model to decompose the blocked item into 2-6
// smaller, ordered steps, using the harness's failure report as evidence; Checklist.Split then
// replaces the item with children <id>.1 .. <id>.n, and the parent's witnesses move to the last
// child, so the original acceptance still gates the result. An unusable reply means "no split".

// MaxChildren bounds how many children a split may produce.
const MaxChildren = 6

const replanFailureCap = 3000

// ReplannerSystemPrompt is the Replanner's system prompt.
const ReplannerSystemPrompt = "You are the Planner. A checklist item failed deterministic verification repeatedly and was " +
	"blocked. Break it into smaller steps that can each be completed and verified in one short " +
	"work session, in dependency order. Each step must be self-contained (do not refer to " +
	`"the item above"), concrete, and leave the code building and the tests passing. The last ` +
	"step must complete the original item."

// ReplannerInstructions is the reply format request.
const ReplannerInstructions = "Reply with ONLY a JSON array of 2-6 steps; each element: " +
	`{"description": "<imperative step>"}. No prose, no code fences.`

// Replanner turns a blocked item into ordered child drafts.
type Replanner struct {
	model contracts.ModelProvider
}

// NewReplanner returns a Replanner using model.
func NewReplanner(model contracts.ModelProvider) *Replanner { return &Replanner{model: model} }

// ReplannerMessages are the messages Split sends (exported for parity tests).
func ReplannerMessages(missionText string, item contracts.ChecklistItem) []contracts.ModelMessage {
	failure := pyfmt.Tail(item.LastFailure, replanFailureCap)
	if failure == "" {
		failure = "(no failure report)"
	}
	acceptance := "It has no item-specific acceptance checks."
	if len(item.Witnesses) > 0 {
		acceptance = "Its acceptance checks (they will gate the LAST step): " + strings.Join(item.Witnesses, ", ")
	}
	return []contracts.ModelMessage{
		{Role: "system", Content: ReplannerSystemPrompt},
		{Role: "user", Content: fmt.Sprintf("%s\n\nBlocked item [%s]: %s\n%s\n\nIt failed %d times in a row. "+
			"Latest verification report:\n%s\n\n%s",
			missionText, item.ID, item.Description, acceptance, item.ConsecutiveFailures, failure, ReplannerInstructions)},
	}
}

// Split returns the child drafts (nil when no usable split is offered: fewer than two steps).
func (r *Replanner) Split(ctx context.Context, missionText string, item contracts.ChecklistItem) ([]contracts.ChecklistItem, error) {
	result, err := r.model.Complete(ctx, ReplannerMessages(missionText, item), nil, 0)
	if err != nil {
		return nil, err
	}
	drafts := []contracts.ChecklistItem{}
	for _, d := range ParseChecklist(result.Text) {
		if pyfmt.PyStrip(d.Description) != "" {
			drafts = append(drafts, contracts.NewChecklistItem(fmt.Sprintf("draft-%d", len(drafts)+1), d.Description))
		}
	}
	if len(drafts) < 2 {
		return nil, nil
	}
	if len(drafts) > MaxChildren {
		drafts = drafts[:MaxChildren]
	}
	return drafts, nil
}

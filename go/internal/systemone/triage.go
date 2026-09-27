package systemone

import (
	"context"
	"unicode/utf8"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
)

// Stall triage (python: lha.systemone.triage): after repeated failures, ask why an item keeps
// failing, and act only on a confident answer. "scope" splits the item now (when the replanner
// may), "environment" blocks it now; anything else, or a failed call, carries on as without
// triage. Both actions only stop work on an item sooner; neither marks anything done.

const (
	// TriageQuestionID is the id of the triage question.
	TriageQuestionID = "cause"
	// MaxFailureChars is the characters of each failure report sent (the end carries the errors).
	MaxFailureChars = 3000
)

// Triage actions.
const (
	ActionContinue = "continue"
	ActionSplit    = "split"
	ActionBlock    = "block"
)

// Causes are the triage options and their descriptions, in order.
func Causes() *contracts.OrderedMap {
	return contracts.NewOrderedMap(
		"defect", "The code written for the task has a specific bug or omission that one more focused "+
			"attempt can fix.",
		"scope", "The task needs several distinct pieces of work, for example new files, packages or "+
			"components that do not exist yet, and the attempts only got part of the way.",
		"environment", "The failure comes from outside the code: a missing tool or dependency, no network "+
			"access, permissions, disk or memory limits, or a broken check command.",
	)
}

// TriageQuestion is the one Choice triage asks.
func TriageQuestion() Question {
	return Choice("The attempts at `task` failed their checks, most recently with `latest_failure` and "+
		"before that with `previous_failure`. Which option best describes why they fail?", Causes())
}

func tail(text string, limit int) string {
	text = obs.RedactText(text)
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return "..." + string(runes[len(runes)-limit:])
}

// TriageState is what the model sees: the task, its witnesses and the two failures (redacted,
// their tails).
func TriageState(item contracts.ChecklistItem, latest, previous string, maxChars int) *contracts.OrderedMap {
	witnesses := make([]any, 0, len(item.Witnesses))
	for _, w := range item.Witnesses {
		witnesses = append(witnesses, w)
	}
	return contracts.NewOrderedMap(
		"task", obs.RedactText(item.Description),
		"witnesses", witnesses,
		"latest_failure", tail(latest, maxChars),
		"previous_failure", tail(previous, maxChars),
	)
}

// TriageAction is the action for an answer: only a confident scope or environment acts.
func TriageAction(a Answer, threshold float64, canSplit bool) string {
	if a.Confidence < threshold {
		return ActionContinue
	}
	if a.Choice == "scope" && canSplit {
		return ActionSplit
	}
	if a.Choice == "environment" {
		return ActionBlock
	}
	return ActionContinue
}

// Verdict is triage's answer and action.
type Verdict struct {
	Action        string
	Cause         string
	Confidence    float64
	Probabilities *contracts.OrderedMap
	Model         string
	Error         string
}

// Payload is the system_one event recorded with the cycle.
func (v Verdict) Payload(itemID string, threshold float64) *contracts.OrderedMap {
	probs := v.Probabilities
	if probs == nil {
		probs = contracts.NewOrderedMap()
	}
	return contracts.NewOrderedMap(
		"use", "stall_triage",
		"item_id", itemID,
		"model", v.Model,
		"answer", v.Cause,
		"confidence", v.Confidence,
		"probabilities", probs,
		"threshold", threshold,
		"action", v.Action,
		"error", v.Error,
	)
}

// StallTriage asks Model why an item keeps failing once it has failed MinFailures in a row.
type StallTriage struct {
	Model       Model
	Threshold   float64
	MinFailures int
}

// NewStallTriage is a triage with the defaults (threshold 0.9, two failures in a row).
func NewStallTriage(m Model) *StallTriage {
	return &StallTriage{Model: m, Threshold: 0.9, MinFailures: 2}
}

// Applies reports whether item is due for triage.
func (t *StallTriage) Applies(item contracts.ChecklistItem) bool {
	return item.Status == "in_progress" && item.ConsecutiveFailures >= t.MinFailures
}

// Assess asks why item keeps failing.
func (t *StallTriage) Assess(ctx context.Context, item contracts.ChecklistItem, latest, previous string, canSplit bool) Verdict {
	state := TriageState(item, latest, previous, MaxFailureChars)
	result, err := t.Model.Evaluate(ctx, state, []Named{{ID: TriageQuestionID, Question: TriageQuestion()}})
	if err != nil {
		return Verdict{Action: ActionContinue, Error: err.Error()}
	}
	a := result.Answers[TriageQuestionID]
	if a.Type != "choice" {
		return Verdict{Action: ActionContinue, Error: "the answer is not a choice"}
	}
	return Verdict{
		Action:        TriageAction(a, t.Threshold, canSplit),
		Cause:         a.Choice,
		Confidence:    a.Confidence,
		Probabilities: a.Probabilities,
		Model:         result.Model,
	}
}

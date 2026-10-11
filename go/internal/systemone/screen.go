package systemone

import (
	"context"
	"fmt"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// The model-backed pre-review screen (python: lha.systemone.screen): a System One model asked the
// pre-review question (review.go) on a verified diff. On a confident "block" it adds one finding
// so the diff gets the full review exactly as the deterministic findings do. It can only make LHA
// more cautious: a block is one more review, never an approval, and it never skips the reviewer.
//
// Off by default (LHA_SYSTEM_ONE_REVIEW): it needs a System One backend (like triage), and its
// threshold is unmeasured until the --judge system_one scorecard says what it should be.

const (
	// SystemOneEvent is the anchor event each model-backed screen is recorded as.
	SystemOneEvent = "system_one"
	// UsePreReview is the use this screen writes into its system_one event.
	UsePreReview = "pre_review"
)

// ScreenVerdict is the model's answer and what it changed (Asked is false when the diff was empty).
type ScreenVerdict struct {
	Action        string
	Answer        string
	Confidence    float64
	Probabilities *contracts.OrderedMap
	Model         string
	Error         string
	Asked         bool
}

// Findings is the finding a confident block adds (nothing otherwise).
func (v ScreenVerdict) Findings() []string {
	if v.Action == ActionBlock {
		return []string{fmt.Sprintf("system_one: block (confidence %.2f)", v.Confidence)}
	}
	return []string{}
}

// Payload is the system_one event recorded with the review (the shape triage's event has).
func (v ScreenVerdict) Payload(itemID string, threshold float64) *contracts.OrderedMap {
	probs := v.Probabilities
	if probs == nil {
		probs = contracts.NewOrderedMap()
	}
	return contracts.NewOrderedMap(
		"use", UsePreReview,
		"item_id", itemID,
		"model", v.Model,
		"answer", v.Answer,
		"confidence", v.Confidence,
		"probabilities", probs,
		"threshold", threshold,
		"action", v.Action,
		"error", v.Error,
	)
}

// ReviewScreen asks Model the pre-review question on a verified diff, and blocks only when
// confident.
type ReviewScreen struct {
	Model     Model
	Threshold float64
}

// NewReviewScreen is a screen with the default threshold (0.9).
func NewReviewScreen(m Model) *ReviewScreen {
	return &ReviewScreen{Model: m, Threshold: 0.9}
}

// Assess is the model's answer for diff (no call on an empty diff; a failure changes nothing).
func (s *ReviewScreen) Assess(ctx context.Context, diff, task string) ScreenVerdict {
	if diff == "" {
		return ScreenVerdict{Action: ActionContinue}
	}
	state := ReviewState(task, diff, MaxDiffChars)
	result, err := s.Model.Evaluate(ctx, state, []Named{{ID: ReviewQuestionID, Question: ReviewQuestion()}})
	if err != nil {
		return ScreenVerdict{Action: ActionContinue, Error: err.Error(), Asked: true}
	}
	a := result.Answers[ReviewQuestionID]
	if a.Type != "choice" { // parse_response guarantees it; be explicit
		return ScreenVerdict{Action: ActionContinue, Error: "the answer is not a choice", Asked: true}
	}
	action := ActionContinue
	if a.Choice == "block" && a.Confidence >= s.Threshold {
		action = ActionBlock
	}
	return ScreenVerdict{
		Action:        action,
		Answer:        a.Choice,
		Confidence:    a.Confidence,
		Probabilities: a.Probabilities,
		Model:         result.Model,
		Asked:         true,
	}
}

// Findings is the finding a confident block adds, or an empty slice.
func (s *ReviewScreen) Findings(ctx context.Context, diff, task string) []string {
	return s.Assess(ctx, diff, task).Findings()
}

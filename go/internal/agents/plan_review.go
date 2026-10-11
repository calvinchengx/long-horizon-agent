package agents

// The optional plan (spec) review (python: lha.agents.plan_review): an advisory design review of
// the Planner's checklist. When LHA_PLAN_REVIEW is on (off by default), a small PlanReviewer asks a
// design reviewer model to list the ways the plan is inadequate, and ReviewPlan re-plans (bounded
// by LHA_PLAN_REVIEW_MAX) so the Planner can fix them, then reviews again. It is ADVISORY: an
// unparseable reply means "no findings" (never blocks the mission), and the per-item deterministic
// verifier still gates every item. Nothing else about the plan path changes.

import (
	"context"
	"strings"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
	"github.com/calvinchengx/long-horizon-agent/go/internal/pyfmt"
)

// PlanReviewerSystemPrompt is the plan reviewer's system prompt (mirrors the Replanner's own
// constant, not a ROLES entry).
const PlanReviewerSystemPrompt = "You are a design reviewer. You are given a mission and the checklist a planner produced for " +
	"it. List only the ways the plan is inadequate: items that are not independently verifiable, " +
	"items too coarse to finish in one focused attempt, work the task needs that is missing, or " +
	"dependencies that are wrong. Be concrete and brief. If the plan is sound, return an empty " +
	"array."

// PlanReviewerInstructions is the reply format request.
const PlanReviewerInstructions = "Reply with ONLY a JSON array of short findings (strings). An empty array [] means the plan is " +
	"sound. No prose, no code fences."

// MaxFindings bounds how many findings a review keeps.
const MaxFindings = 20

// ParseFindings extracts short findings from model output; unparseable output means no findings.
// Mirrors planner.ParsePlan's bracket extraction (python: parse_findings).
func ParseFindings(text string) []string {
	start := strings.Index(text, "[")
	end := strings.LastIndex(text, "]")
	if start == -1 || end == -1 || end <= start {
		return []string{}
	}
	parsed, err := pyfmt.DecodeOrdered([]byte(text[start : end+1]))
	if err != nil {
		return []string{}
	}
	list, ok := parsed.([]any)
	if !ok {
		return []string{}
	}
	out := []string{}
	for _, v := range list {
		finding := pyfmt.PyStrip(pyfmt.PyStr(v))
		if finding != "" && !contains(out, finding) {
			out = append(out, finding)
		}
	}
	if len(out) > MaxFindings {
		out = out[:MaxFindings]
	}
	return out
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// PlanReview is one review of a plan: what it saw and whether it forced a re-plan.
type PlanReview struct {
	ItemCount int
	Findings  []string
	Replanned bool
}

// PlanReviewFields is the plan_review trace event's payload, in the Python key order.
func (r PlanReview) PlanReviewFields() []obs.Field {
	replanned := 0
	if r.Replanned {
		replanned = 1
	}
	return []obs.Field{
		obs.F("item_count", r.ItemCount),
		obs.F("findings", len(r.Findings)),
		obs.F("replanned", replanned),
	}
}

// PlanText renders the plan as the reviewer sees it (python: plan_text).
func PlanText(title, description, acceptance string, plan MissionPlan) string {
	done := pyfmt.PyStrip(acceptance)
	if done == "" {
		done = "(use your judgment)"
	}
	lines := []string{
		"Mission: " + title,
		"",
		description,
		"",
		"Definition of done: " + done,
		"",
		"Plan:",
	}
	for _, item := range plan.Checklist.Items {
		files := plan.Files[item.ID]
		suffix := ""
		if len(files) > 0 {
			suffix = " (files: " + strings.Join(files, ", ") + ")"
		}
		lines = append(lines, "["+item.ID+"] "+item.Description+suffix)
	}
	return strings.Join(lines, "\n")
}

// PlanReviewerMessages are the messages Review sends (exported for parity tests).
func PlanReviewerMessages(title, description, acceptance string, plan MissionPlan) []contracts.ModelMessage {
	return []contracts.ModelMessage{
		{Role: "system", Content: PlanReviewerSystemPrompt},
		{Role: "user", Content: PlanText(title, description, acceptance, plan) + "\n\n" + PlanReviewerInstructions},
	}
}

// AcceptanceWithFindings is the Planner's acceptance with the review's findings appended, so a
// re-plan can fix them (python: acceptance_with_findings).
func AcceptanceWithFindings(acceptance string, findings []string) string {
	base := pyfmt.PyStrip(acceptance)
	if base == "" {
		base = "(use your judgment)"
	}
	listed := make([]string, 0, len(findings))
	for _, finding := range findings {
		listed = append(listed, "- "+finding)
	}
	return base + "\n\nA design review of the previous plan found:\n" + strings.Join(listed, "\n") +
		"\nFix these in the new plan."
}

// PlanReviewer is one model call that lists the ways a plan is inadequate.
type PlanReviewer struct {
	model contracts.ModelProvider
}

// NewPlanReviewer returns a PlanReviewer using model (the planner's model).
func NewPlanReviewer(model contracts.ModelProvider) *PlanReviewer { return &PlanReviewer{model: model} }

// Review returns the plan's findings ([] when it is sound or the reply is unparseable).
func (r *PlanReviewer) Review(ctx context.Context, title, description, acceptance string, plan MissionPlan) ([]string, error) {
	result, err := r.model.Complete(ctx, PlanReviewerMessages(title, description, acceptance, plan), nil, 0)
	if err != nil {
		return nil, err
	}
	return ParseFindings(result.Text), nil
}

// ReviewPlan plans, reviews, and re-plans up to maxReplans times; returns the plan and every
// review. A review with findings triggers a re-plan only while re-plans remain; after the bound
// the last plan starts anyway (python: review_plan).
func ReviewPlan(ctx context.Context, planner *Planner, reviewer *PlanReviewer, title, description, acceptance string, maxReplans int) (MissionPlan, []PlanReview, error) {
	plan, err := planner.PlanMission(ctx, title, description, acceptance)
	if err != nil {
		return MissionPlan{}, nil, err
	}
	reviews := []PlanReview{}
	replans := 0
	for {
		findings, err := reviewer.Review(ctx, title, description, acceptance, plan)
		if err != nil {
			return MissionPlan{}, nil, err
		}
		replanned := len(findings) > 0 && replans < maxReplans
		reviews = append(reviews, PlanReview{
			ItemCount: len(plan.Checklist.Items), Findings: findings, Replanned: replanned,
		})
		if !replanned {
			return plan, reviews, nil
		}
		replans++
		plan, err = planner.PlanMission(ctx, title, description, AcceptanceWithFindings(acceptance, findings))
		if err != nil {
			return MissionPlan{}, nil, err
		}
	}
}

// PlanWithOptionalReview is the Planner, reviewed and re-planned when settings.PlanReview is on.
// Off (the default): the Planner is called exactly once and no reviewer is built, so planning is
// byte-identical to before (python: plan_with_optional_review).
func PlanWithOptionalReview(ctx context.Context, planner *Planner, model contracts.ModelProvider, settings *config.Settings, title, description, acceptance string) (MissionPlan, []PlanReview, error) {
	if !settings.PlanReview {
		plan, err := planner.PlanMission(ctx, title, description, acceptance)
		return plan, nil, err
	}
	return ReviewPlan(ctx, planner, NewPlanReviewer(model), title, description, acceptance, settings.PlanReviewMax)
}

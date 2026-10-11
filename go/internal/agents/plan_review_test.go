package agents

// Ported from python/tests/unit/test_plan_review.py: the optional plan (spec) review's defensive
// parsing, the bounded re-plan gate, and the settings that turn it on.

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

const (
	reviewPlan      = `[{"description": "step one"}, {"description": "step two"}]`
	reviewPlanFixed = `[{"description": "step one"}, {"description": "step two"}, {"description": "step three"}]`
	reviewFindings  = `["item 02 is not independently verifiable", "missing the migration step"]`
)

// countingModel is a scripted stub that counts its calls (so a test can prove one was never made).
type countingModel struct {
	inner *model.StubModel
	calls int
}

func newCounting(script ...contracts.TurnResult) *countingModel {
	return &countingModel{inner: model.NewStub(script)}
}

func (c *countingModel) Name() string { return "stub:counting" }

func (c *countingModel) Complete(ctx context.Context, msgs []contracts.ModelMessage, tools []map[string]any, n int) (contracts.TurnResult, error) {
	c.calls++
	return c.inner.Complete(ctx, msgs, tools, n)
}

func (c *countingModel) EstimateCostUSD(contracts.Usage) (float64, error) { return 0, nil }

func reviewSettings(t *testing.T, env ...string) *config.Settings {
	t.Helper()
	s, err := config.LoadFrom(env, "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestParseFindingsIsDefensive(t *testing.T) {
	cases := []struct {
		text string
		want []string
	}{
		{reviewFindings, []string{"item 02 is not independently verifiable", "missing the migration step"}},
		{"[]", []string{}},
		{"Sure:\n[\"too coarse\"]\nHope that helps.", []string{"too coarse"}},
		{"no json here", []string{}},
		{`{"findings": ["x"]}`, []string{"x"}}, // the outermost [...] wins
		{`["a", " a ", "b", "a", ""]`, []string{"a", "b"}},
	}
	for _, c := range cases {
		if got := ParseFindings(c.text); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ParseFindings(%q) = %v, want %v", c.text, got, c.want)
		}
	}
}

func TestAcceptanceWithFindingsAppendsThem(t *testing.T) {
	text := AcceptanceWithFindings("all tests pass", []string{"fix deps"})
	if !strings.HasPrefix(text, "all tests pass") || !strings.Contains(text, "- fix deps") {
		t.Fatalf("%q", text)
	}
	if !strings.HasPrefix(AcceptanceWithFindings("", nil), "(use your judgment)") {
		t.Fatal("blank acceptance not defaulted")
	}
}

func TestPlanReviewFindingsThenNoneReplansOnce(t *testing.T) {
	planner := newCounting(contracts.TurnResult{Text: reviewPlan}, contracts.TurnResult{Text: reviewPlanFixed})
	reviewer := newCounting(contracts.TurnResult{Text: reviewFindings}, contracts.TurnResult{Text: "[]"})
	plan, reviews, err := ReviewPlan(context.Background(), NewPlanner(planner), NewPlanReviewer(reviewer), "T", "D", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if planner.calls != 2 {
		t.Fatalf("planner calls = %d, want 2", planner.calls)
	}
	wantFindings := [][]string{
		{"item 02 is not independently verifiable", "missing the migration step"},
		{},
	}
	gotFindings := [][]string{}
	gotReplanned := []bool{}
	gotCounts := []int{}
	for _, r := range reviews {
		gotFindings = append(gotFindings, r.Findings)
		gotReplanned = append(gotReplanned, r.Replanned)
		gotCounts = append(gotCounts, r.ItemCount)
	}
	if !reflect.DeepEqual(gotFindings, wantFindings) || !reflect.DeepEqual(gotReplanned, []bool{true, false}) {
		t.Fatalf("reviews %+v", reviews)
	}
	if !reflect.DeepEqual(gotCounts, []int{2, 3}) || len(plan.Checklist.Items) != 3 {
		t.Fatalf("counts %v items %d", gotCounts, len(plan.Checklist.Items))
	}
}

func TestPlanReviewNoFindingsDoesNotReplan(t *testing.T) {
	planner := newCounting(contracts.TurnResult{Text: reviewPlan})
	reviewer := newCounting(contracts.TurnResult{Text: "[]"})
	plan, reviews, err := ReviewPlan(context.Background(), NewPlanner(planner), NewPlanReviewer(reviewer), "T", "D", "", 3)
	if err != nil {
		t.Fatal(err)
	}
	if planner.calls != 1 || reviewer.calls != 1 || len(reviews) != 1 {
		t.Fatalf("calls %d/%d reviews %d", planner.calls, reviewer.calls, len(reviews))
	}
	if len(reviews[0].Findings) != 0 || reviews[0].Replanned || len(plan.Checklist.Items) != 2 {
		t.Fatalf("%+v", reviews[0])
	}
}

func TestPlanReviewPersistingToBoundProceeds(t *testing.T) {
	planner := newCounting(contracts.TurnResult{Text: reviewPlan}, contracts.TurnResult{Text: reviewPlanFixed})
	reviewer := newCounting(contracts.TurnResult{Text: reviewFindings}) // always finds something
	_, reviews, err := ReviewPlan(context.Background(), NewPlanner(planner), NewPlanReviewer(reviewer), "T", "D", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if planner.calls != 2 { // exactly MAX re-plans, then it starts anyway
		t.Fatalf("planner calls = %d, want 2", planner.calls)
	}
	if len(reviews) != 2 || !reviews[0].Replanned || reviews[1].Replanned {
		t.Fatalf("%+v", reviews)
	}
	for _, r := range reviews {
		if len(r.Findings) == 0 {
			t.Fatalf("expected findings: %+v", r)
		}
	}
}

func TestPlanReviewUnparseableMeansNoFindings(t *testing.T) {
	planner := newCounting(contracts.TurnResult{Text: reviewPlan})
	reviewer := newCounting(contracts.TurnResult{Text: "I could not review this plan."})
	_, reviews, err := ReviewPlan(context.Background(), NewPlanner(planner), NewPlanReviewer(reviewer), "T", "D", "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if planner.calls != 1 || len(reviews) != 1 || reviews[0].Replanned {
		t.Fatalf("calls %d reviews %+v", planner.calls, reviews)
	}
}

func TestPlanReviewOffMakesNoReviewerCall(t *testing.T) {
	planner := newCounting(contracts.TurnResult{Text: reviewPlan})
	reviewerModel := newCounting(contracts.TurnResult{Text: reviewFindings})
	plan, reviews, err := PlanWithOptionalReview(
		context.Background(), NewPlanner(planner), reviewerModel,
		reviewSettings(t, "LHA_PLAN_REVIEW=false"), "T", "D", "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if reviewerModel.calls != 0 || len(reviews) != 0 || planner.calls != 1 || len(plan.Checklist.Items) != 2 {
		t.Fatalf("reviewer %d reviews %d planner %d", reviewerModel.calls, len(reviews), planner.calls)
	}
}

func TestPlanReviewUsesTheBoundFromSettings(t *testing.T) {
	planner := newCounting(contracts.TurnResult{Text: reviewPlan}, contracts.TurnResult{Text: reviewPlanFixed})
	reviewerModel := newCounting(contracts.TurnResult{Text: reviewFindings})
	_, reviews, err := PlanWithOptionalReview(
		context.Background(), NewPlanner(planner), reviewerModel,
		reviewSettings(t, "LHA_PLAN_REVIEW=true", "LHA_PLAN_REVIEW_MAX=1"), "T", "D", "",
	)
	if err != nil {
		t.Fatal(err)
	}
	if planner.calls != 2 || len(reviews) != 2 {
		t.Fatalf("calls %d reviews %d", planner.calls, len(reviews))
	}
	fields := reviews[0].PlanReviewFields()
	if fields[0].Value != 2 || fields[1].Value != 2 || fields[2].Value != 1 {
		t.Fatalf("review 0 fields %+v", fields)
	}
	fields = reviews[1].PlanReviewFields()
	if fields[0].Value != 3 || fields[1].Value != 2 || fields[2].Value != 0 {
		t.Fatalf("review 1 fields %+v", fields)
	}
}

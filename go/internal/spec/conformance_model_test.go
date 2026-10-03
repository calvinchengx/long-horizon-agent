package spec

import (
	"math"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/model"
)

// TestPricing runs spec/model/pricing.json: which Claude model ids are priced (dated snapshot
// suffixes tolerated) and the USD cost of each usage, to a relative tolerance of 1e-12.
func TestPricing(t *testing.T) {
	var s struct {
		Claude []struct {
			Model  string `json:"model"`
			Priced bool   `json:"priced"`
			Costs  []struct {
				Usage contracts.Usage `json:"usage"`
				USD   float64         `json:"usd"`
			} `json:"costs"`
		} `json:"claude"`
	}
	Load(t, "model/pricing.json", &s)
	if len(s.Claude) == 0 {
		t.Fatal("no pricing cases")
	}
	for _, c := range s.Claude {
		price := model.LookupClaudePrice(c.Model)
		if (price != nil) != c.Priced {
			t.Errorf("LookupClaudePrice(%q) priced = %v, want %v", c.Model, price != nil, c.Priced)
			continue
		}
		for _, cost := range c.Costs {
			got := price.Cost(cost.Usage)
			if math.Abs(got-cost.USD) > 1e-12*math.Abs(cost.USD) {
				t.Errorf("%s: Cost(%+v) = %.17g, want %.17g", c.Model, cost.Usage, got, cost.USD)
			}
		}
	}
}

// TestClaudeCodeCallBudget runs spec/model/claude_code_budget.json: a claude -p call's spend cap
// is the configured cap, or what is left of the budget when that is less (and at least a cent).
func TestClaudeCodeCallBudget(t *testing.T) {
	var s struct {
		Min   float64 `json:"min_call_budget_usd"`
		Cases []struct {
			Configured float64 `json:"configured"`
			Remaining  float64 `json:"remaining"`
			Cap        float64 `json:"cap"`
		} `json:"cases"`
	}
	Load(t, "model/claude_code_budget.json", &s)
	if s.Min != model.MinCallBudgetUSD || len(s.Cases) == 0 {
		t.Fatalf("min %v, %d cases", s.Min, len(s.Cases))
	}
	for _, c := range s.Cases {
		if got := model.CallBudgetUSD(c.Configured, c.Remaining); got != c.Cap {
			t.Errorf("CallBudgetUSD(%v, %v) = %v, want %v", c.Configured, c.Remaining, got, c.Cap)
		}
	}
}

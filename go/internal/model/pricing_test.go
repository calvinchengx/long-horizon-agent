package model

import (
	"errors"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

func TestClaudeCacheTokensBilledWithAnthropicMultipliers(t *testing.T) {
	m := mustClaude(t, ClaudeOptions{})
	u := contracts.Usage{
		InputTokens: 1_000_000, OutputTokens: 1_000_000,
		CacheReadInputTokens: 1_000_000, CacheCreationInputTokens: 1_000_000,
		Model: "claude-sonnet-4-6",
	}
	got, err := m.EstimateCostUSD(u)
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "cost", got, 3+3.75+0.3+15) // input + write*1.25 + read*0.1 + output
}

func TestClaudeOneHourCacheWritesBilledAt2x(t *testing.T) {
	m := mustClaude(t, ClaudeOptions{})
	got, err := m.EstimateCostUSD(contracts.Usage{
		CacheCreationInputTokens: 1_000_000, CacheCreation1hInputTokens: 1_000_000, Model: "claude-sonnet-4-6",
	})
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "cost", got, 3*2.0)
}

func TestOneHourWritesCappedAtTotalWrites(t *testing.T) {
	p := NewModelPrice(5, 25)
	got := p.Cost(contracts.Usage{CacheCreationInputTokens: 10, CacheCreation1hInputTokens: 50})
	approx(t, "cost", got, 10*2.0/1e6*5)
}

func TestClaudePricesByReportedModelNotConfiguredModel(t *testing.T) {
	m := mustClaude(t, ClaudeOptions{})
	got, err := m.EstimateCostUSD(contracts.Usage{InputTokens: 1_000_000, Model: "claude-haiku-4-5-20251001"})
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "cost", got, 1.0)
	// An empty reported model falls back to the configured one.
	got, err = m.EstimateCostUSD(contracts.Usage{InputTokens: 1_000_000})
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "cost", got, 3.0)
}

func TestClaudeUnknownModelFailsFastWithoutExplicitPrices(t *testing.T) {
	_, err := NewClaude(ClaudeOptions{APIKey: "k", ModelName: "claude-imaginary-9"})
	if !errors.Is(err, contracts.ErrUnknownPrice) {
		t.Fatalf("err = %v, want ErrUnknownPrice", err)
	}
	want := "no price configured for model 'claude-imaginary-9' on provider 'claude'; " +
		"configure explicit prices (cost is never assumed to be $0)"
	if err.Error() != want {
		t.Errorf("message = %q", err.Error())
	}
	p := NewModelPrice(2.0, 4.0)
	m := mustClaude(t, ClaudeOptions{ModelName: "claude-imaginary-9", Price: &p})
	got, err := m.EstimateCostUSD(contracts.Usage{InputTokens: 1_000_000, Model: "claude-imaginary-9"})
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "cost", got, 2.0)
	// A different reported model uses the table, then the explicit price as the last resort.
	got, err = m.EstimateCostUSD(contracts.Usage{InputTokens: 1_000_000, Model: "claude-opus-4-8"})
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "table cost", got, 5.0)
	got, err = m.EstimateCostUSD(contracts.Usage{InputTokens: 1_000_000, Model: "other"})
	if err != nil {
		t.Fatal(err)
	}
	approx(t, "fallback cost", got, 2.0)
}

func TestExplicitPriceOverridesTableForConfiguredModel(t *testing.T) {
	p := NewModelPrice(10, 10)
	m := mustClaude(t, ClaudeOptions{Price: &p})
	got, _ := m.EstimateCostUSD(contracts.Usage{InputTokens: 1_000_000, Model: "claude-sonnet-4-6"})
	approx(t, "cost", got, 10)
}

func TestClaudeUnknownReportedModelRaises(t *testing.T) {
	m := mustClaude(t, ClaudeOptions{})
	_, err := m.EstimateCostUSD(contracts.Usage{InputTokens: 1, Model: "some-other-model"})
	if !errors.Is(err, contracts.ErrUnknownPrice) {
		t.Fatalf("err = %v", err)
	}
	want := "no price configured for model 'some-other-model' on provider 'claude:claude-sonnet-4-6'; " +
		"configure explicit prices (cost is never assumed to be $0)"
	if err.Error() != want {
		t.Errorf("message = %q", err.Error())
	}
}

func TestLookupClaudePrice(t *testing.T) {
	cases := map[string]float64{
		"claude-opus-4-8":            5,
		"claude-haiku-4-5-20251001":  1,
		"claude-sonnet-4-6-20260101": 3,
		"claude-haiku-4-5-\u0662\u0660\u0662\u0665\u0661\u0660\u0660\u0661": 1, // Python \d is Unicode
	}
	for model, in := range cases {
		p := LookupClaudePrice(model)
		if p == nil || p.InputPerMTok != in {
			t.Errorf("LookupClaudePrice(%q) = %v, want input %v", model, p, in)
		}
	}
	for _, model := range []string{"gpt-4o", "claude-haiku-4-5-2025100", "claude-haiku-4-5-20251001\n", "claude-haiku-4-5-20251001-20251001"} {
		if p := LookupClaudePrice(model); p != nil {
			t.Errorf("LookupClaudePrice(%q) = %v, want nil", model, p)
		}
	}
	// The table itself cannot be mutated through a lookup.
	LookupClaudePrice("claude-opus-4-8").InputPerMTok = 99
	if ClaudePrices["claude-opus-4-8"].InputPerMTok != 5 {
		t.Error("lookup leaked a pointer into the table")
	}
}

func TestRequirePrice(t *testing.T) {
	p := NewModelPrice(1, 2)
	got, err := RequirePrice(&p, "m", "prov")
	if err != nil || got != p {
		t.Fatalf("RequirePrice = %v, %v", got, err)
	}
	if _, err := RequirePrice(nil, "it's", "p"); err == nil ||
		err.Error() != `no price configured for model "it's" on provider 'p'; configure explicit prices (cost is never assumed to be $0)` {
		t.Errorf("err = %v", err)
	}
}

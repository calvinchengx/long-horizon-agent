package governor

import (
	"fmt"
	"sync"
)

// GovernorDecision says whether the next step may proceed, and why.
type GovernorDecision struct {
	Allow        bool    `json:"allow"`
	Reason       string  `json:"reason"`
	ProjectedUSD float64 `json:"projected_usd"`
	SpentUSD     float64 `json:"spent_usd"`
	CeilingUSD   float64 `json:"ceiling_usd"`
}

// BudgetGovernor refuses the next step BEFORE it runs if it would breach the spend/iteration
// ceiling.
//
// Two checks: AuthorizeNext (per cycle, projected from the most expensive cycle so far) and
// AuthorizeCall (per model call, the hard stop: spent + in-flight reservations + this call's
// worst-case cost must stay within the ceiling). Unknown spend (an unpriced model) is treated
// conservatively: denied unless allowUnknownCost was explicitly set.
type BudgetGovernor struct {
	ceiling          float64
	maxCycles        int
	allowUnknownCost bool
}

// NewBudgetGovernor returns a governor.
func NewBudgetGovernor(ceilingUSD float64, maxCycles int, allowUnknownCost bool) *BudgetGovernor {
	return &BudgetGovernor{ceiling: ceilingUSD, maxCycles: maxCycles, allowUnknownCost: allowUnknownCost}
}

// CeilingUSD is the spend ceiling.
func (g *BudgetGovernor) CeilingUSD() float64 { return g.ceiling }

// AuthorizeNext decides whether another cycle may start. projectedUSD == nil estimates the next
// cycle from history: the most expensive cycle so far (a mean under-projects a spiky run).
func (g *BudgetGovernor) AuthorizeNext(ledger *CostLedger, cyclesDone int, projectedUSD *float64) GovernorDecision {
	spent := ledger.TotalUSD()
	var estimate float64
	if projectedUSD != nil {
		estimate = *projectedUSD
	} else {
		estimate = max(ledger.MeanUSDPerCycle(), ledger.MaxUSDPerCycle())
	}
	projectedTotal := spent + estimate

	if cyclesDone >= g.maxCycles {
		return g.deny("max cycles reached", projectedTotal, spent)
	}
	if n := ledger.UnknownCostEntries(); n > 0 && !g.allowUnknownCost {
		return g.deny(unknownReason(n), projectedTotal, spent)
	}
	if projectedTotal > g.ceiling {
		return g.deny("projected spend exceeds ceiling", projectedTotal, spent)
	}
	return g.allow(projectedTotal, spent)
}

// AuthorizeCall is the hard per-call stop. worstCaseUSD == nil means the call cannot be priced.
func (g *BudgetGovernor) AuthorizeCall(ledger *CostLedger, worstCaseUSD *float64, reservedUSD float64) GovernorDecision {
	spent := ledger.TotalUSD()
	committed := spent + reservedUSD
	if n := ledger.UnknownCostEntries(); n > 0 && !g.allowUnknownCost {
		return g.deny(unknownReason(n), committed, spent)
	}
	if worstCaseUSD == nil {
		if g.allowUnknownCost {
			return g.allow(committed, spent)
		}
		return g.deny("cannot price this model call (no configured price); refusing to spend an "+
			"unknown amount", committed, spent)
	}
	projected := committed + *worstCaseUSD
	if projected > g.ceiling {
		return g.deny("worst-case cost of this call exceeds the budget", projected, spent)
	}
	return g.allow(projected, spent)
}

func unknownReason(n int) string {
	return fmt.Sprintf("spend is unverifiable: %d call(s) with unknown price "+
		"(configure prices or explicitly allow unknown cost)", n)
}

func (g *BudgetGovernor) allow(projected, spent float64) GovernorDecision {
	return GovernorDecision{Allow: true, Reason: "within budget", ProjectedUSD: projected, SpentUSD: spent, CeilingUSD: g.ceiling}
}

func (g *BudgetGovernor) deny(reason string, projected, spent float64) GovernorDecision {
	return GovernorDecision{Allow: false, Reason: reason, ProjectedUSD: projected, SpentUSD: spent, CeilingUSD: g.ceiling}
}

// DefaultLoopThreshold is LoopDetector's default threshold.
const DefaultLoopThreshold = 3

// LoopDetector flags a signature that FAILS threshold times IN A ROW (e.g. the same item not
// advancing). Counts are consecutive per signature: observing a signature with failed=false
// (progress) resets its count, matching Settings.stall_limit ("consecutive failed attempts").
type LoopDetector struct {
	threshold int
	mu        sync.Mutex
	counts    map[string]int
}

// NewLoopDetector returns a detector that trips after threshold consecutive failures.
func NewLoopDetector(threshold int) *LoopDetector {
	return &LoopDetector{threshold: threshold, counts: map[string]int{}}
}

// Observe records an attempt for signature and reports whether it has now failed threshold
// times consecutively. A successful attempt (failed=false) resets the streak and returns false.
func (d *LoopDetector) Observe(signature string, failed bool) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !failed {
		delete(d.counts, signature)
		return false
	}
	d.counts[signature]++
	return d.counts[signature] >= d.threshold
}

// Reset clears the streak of one signature.
func (d *LoopDetector) Reset(signature string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.counts, signature)
}

// ResetAll clears every streak (Python: reset(None)).
func (d *LoopDetector) ResetAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	clear(d.counts)
}

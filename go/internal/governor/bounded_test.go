package governor

import (
	"fmt"
	"math"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Ported from python/tests/unit/test_bounded_memory.py: the ledger keeps the newest entries and
// totals over every entry.
func TestTheCostLedgerKeepsTotalsOverEveryEntry(t *testing.T) {
	defer func(n int) { MaxLedgerEntries = n }(MaxLedgerEntries)
	MaxLedgerEntries = 10
	l := NewCostLedger()
	half := 0.5
	for n := 0; n < 100; n++ {
		usd := &half
		if n%10 == 0 {
			usd = nil
		}
		l.Record(fmt.Sprintf("c%d", n/4), contracts.Usage{Model: "m", InputTokens: 10, OutputTokens: 1}, usd, "")
	}
	entries := l.Entries()
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
	if len(entries) > 11 || entries[len(entries)-1].CycleID != "c24" || !near(l.TotalUSD(), 45) ||
		l.UnknownCostEntries() != 10 || l.TotalInputTokens() != 1000 || l.TotalOutputTokens() != 100 ||
		!near(l.MeanUSDPerCycle(), 45.0/25) || !near(l.MaxUSDPerCycle(), 2) {
		t.Fatalf("%d entries, total %v, unknown %d, mean %v, max %v", len(entries), l.TotalUSD(),
			l.UnknownCostEntries(), l.MeanUSDPerCycle(), l.MaxUSDPerCycle())
	}
	var seeded CostLedger
	if err := seeded.UnmarshalJSON([]byte(`{"entries":[{"cycle_id":"p","model":"m","input_tokens":1,"output_tokens":2,"usd":3,"cost_known":true}]}`)); err != nil ||
		seeded.TotalUSD() != 3 || seeded.TotalInputTokens() != 1 || len(seeded.Entries()) != 1 {
		t.Fatal(err, seeded.TotalUSD())
	}
}

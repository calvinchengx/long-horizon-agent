// Package governor is pre-emptive cost control + loop/oscillation detection. It mirrors
// python/src/lha/governor.
//
// The governor refuses the NEXT step before it runs if projected spend/iterations would breach
// the ceiling, and detects repeated failing signatures so an oscillating agent is stopped and
// re-planned rather than thrashing. CostMeter/MeteredModel wrap every model provider so each call
// is budget-checked before it runs and recorded after.
package governor

import (
	"encoding/json"
	"sync"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// CostEntry is one recorded model turn. USD is 0 when CostKnown is false (the real cost is
// unknown, NOT zero).
type CostEntry struct {
	CycleID                  string  `json:"cycle_id"`
	Model                    string  `json:"model"`
	InputTokens              int     `json:"input_tokens"`
	OutputTokens             int     `json:"output_tokens"`
	USD                      float64 `json:"usd"`
	CacheReadInputTokens     int     `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int     `json:"cache_creation_input_tokens"`
	CostKnown                bool    `json:"cost_known"`
	Role                     string  `json:"role"`
}

// UnmarshalJSON applies the pydantic defaults (cost_known=true).
func (e *CostEntry) UnmarshalJSON(data []byte) error {
	type alias CostEntry
	a := alias{CostKnown: true}
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*e = CostEntry(a)
	return nil
}

// MaxLedgerEntries is how many entries a CostLedger keeps (the newest; python:
// MAX_LEDGER_ENTRIES). The totals cover every entry ever added; the rows themselves are in the
// mission store's cost_ledger (lha costs).
var MaxLedgerEntries = 5_000

// CostLedger is the ledger of spend for one mission: running totals over every entry, and the
// newest MaxLedgerEntries entries. It is safe for concurrent use.
type CostLedger struct {
	mu           sync.Mutex
	entries      []CostEntry
	usd          float64
	unknown      int
	inputTokens  int
	outputTokens int
	perCycle     map[string]float64
}

// NewCostLedger returns an empty ledger.
func NewCostLedger() *CostLedger { return &CostLedger{} }

// Record records one turn; usd == nil marks the cost as unknown (tokens are still kept).
func (l *CostLedger) Record(cycleID string, usage contracts.Usage, usd *float64, role string) CostEntry {
	entry := CostEntry{
		CycleID:                  cycleID,
		Model:                    usage.Model,
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CostKnown:                usd != nil,
		Role:                     role,
	}
	if usd != nil {
		entry.USD = *usd
	}
	l.mu.Lock()
	l.addLocked(entry)
	l.mu.Unlock()
	return entry
}

// Add adds one entry to the totals and the window of newest entries (python: CostLedger.add).
func (l *CostLedger) Add(entry CostEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.addLocked(entry)
}

func (l *CostLedger) addLocked(entry CostEntry) {
	l.usd += entry.USD
	if !entry.CostKnown {
		l.unknown++
	}
	l.inputTokens += entry.InputTokens
	l.outputTokens += entry.OutputTokens
	if l.perCycle == nil {
		l.perCycle = map[string]float64{}
	}
	l.perCycle[entry.CycleID] += entry.USD
	l.entries = append(l.entries, entry)
	if limit := MaxLedgerEntries; len(l.entries) > limit+limit/10 {
		l.entries = append([]CostEntry(nil), l.entries[len(l.entries)-limit:]...)
	}
}

// Entries returns a copy of the kept (newest) entries, in order.
func (l *CostLedger) Entries() []CostEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]CostEntry{}, l.entries...)
}

// MarshalJSON writes {"entries": [...]} like the pydantic model.
func (l *CostLedger) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Entries []CostEntry `json:"entries"`
	}{l.Entries()})
}

// UnmarshalJSON reads {"entries": [...]}.
func (l *CostLedger) UnmarshalJSON(data []byte) error {
	var v struct {
		Entries []CostEntry `json:"entries"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries, l.usd, l.unknown, l.inputTokens, l.outputTokens, l.perCycle = nil, 0, 0, 0, 0, nil
	for _, e := range v.Entries {
		l.addLocked(e)
	}
	return nil
}

// TotalUSD is known spend only; check UnknownCostEntries before trusting it as complete.
func (l *CostLedger) TotalUSD() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.usd
}

// UnknownCostEntries counts entries whose cost could not be priced.
func (l *CostLedger) UnknownCostEntries() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.unknown
}

// TotalInputTokens sums input tokens.
func (l *CostLedger) TotalInputTokens() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inputTokens
}

// TotalOutputTokens sums output tokens.
func (l *CostLedger) TotalOutputTokens() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.outputTokens
}

// MeanUSDPerCycle is known spend divided by the number of distinct cycles (0 when empty).
func (l *CostLedger) MeanUSDPerCycle() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.perCycle) == 0 {
		return 0
	}
	return l.usd / float64(len(l.perCycle))
}

// MaxUSDPerCycle is the most expensive cycle so far (a conservative next-cycle projection).
func (l *CostLedger) MaxUSDPerCycle() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	best := 0.0
	for _, usd := range l.perCycle {
		best = max(best, usd)
	}
	return best
}

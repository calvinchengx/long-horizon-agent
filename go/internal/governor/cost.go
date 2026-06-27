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

// CostLedger is the append-only ledger of spend for one mission. It is safe for concurrent use.
type CostLedger struct {
	mu      sync.Mutex
	entries []CostEntry
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
	l.entries = append(l.entries, entry)
	l.mu.Unlock()
	return entry
}

// Entries returns a copy of the recorded entries, in order.
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
	l.entries = v.Entries
	l.mu.Unlock()
	return nil
}

// TotalUSD is known spend only; check UnknownCostEntries before trusting it as complete.
func (l *CostLedger) TotalUSD() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.totalUSDLocked()
}

func (l *CostLedger) totalUSDLocked() float64 {
	total := 0.0
	for _, e := range l.entries {
		total += e.USD
	}
	return total
}

// UnknownCostEntries counts entries whose cost could not be priced.
func (l *CostLedger) UnknownCostEntries() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.entries {
		if !e.CostKnown {
			n++
		}
	}
	return n
}

// TotalInputTokens sums input tokens.
func (l *CostLedger) TotalInputTokens() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.entries {
		n += e.InputTokens
	}
	return n
}

// TotalOutputTokens sums output tokens.
func (l *CostLedger) TotalOutputTokens() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, e := range l.entries {
		n += e.OutputTokens
	}
	return n
}

// MeanUSDPerCycle is known spend divided by the number of distinct cycles (0 when empty).
func (l *CostLedger) MeanUSDPerCycle() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	cycles := map[string]bool{}
	for _, e := range l.entries {
		cycles[e.CycleID] = true
	}
	if len(cycles) == 0 {
		return 0
	}
	return l.totalUSDLocked() / float64(len(cycles))
}

// MaxUSDPerCycle is the most expensive cycle so far (a conservative next-cycle projection).
func (l *CostLedger) MaxUSDPerCycle() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	perCycle := map[string]float64{}
	order := []string{}
	for _, e := range l.entries {
		if _, ok := perCycle[e.CycleID]; !ok {
			order = append(order, e.CycleID)
		}
		perCycle[e.CycleID] += e.USD
	}
	best := 0.0
	for i, id := range order {
		if i == 0 || perCycle[id] > best {
			best = perCycle[id]
		}
	}
	return best
}

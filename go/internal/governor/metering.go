package governor

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"unicode/utf8"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// Metered model provider: every Complete is budget-checked BEFORE it runs and recorded AFTER.
//
// Wrap every provider a mission uses with the SAME CostMeter so one ledger sees all spend and
// one governor enforces one ceiling. Before each call the governor's hard stop (AuthorizeCall)
// is consulted with this call's WORST-CASE cost (conservative input-token estimate + the full
// max_tokens of output) plus the worst case of calls already in flight (so a parallel fan-out
// cannot collectively overshoot). A refusal returns a *BudgetExceeded. After the call the ACTUAL
// cost is computed by the provider from the reported usage and recorded; an unpriced model is
// recorded with cost_known=false.

// Conservative chars-per-token for the pre-call input estimate (real text averages ~4).
const (
	charsPerToken             = 2.0
	perMessageOverheadTokens  = 8
	DefaultAssumedMaxOutput   = 8192 // output ceiling when neither call nor provider states one
	defaultCycleID            = "c0"
	budgetExceededMessageBase = "budget governor refused model call: "
)

// BudgetExceeded is a model call refused before it ran because it could breach the budget.
type BudgetExceeded struct{ Decision GovernorDecision }

func (e *BudgetExceeded) Error() string {
	d := e.Decision
	return fmt.Sprintf(budgetExceededMessageBase+"%s (spent $%.4f, projected $%.4f, ceiling $%.4f)",
		d.Reason, d.SpentUSD, d.ProjectedUSD, d.CeilingUSD)
}

// DefaultMaxTokenser is implemented by providers that state their default output ceiling (the
// Python providers' default_max_tokens attribute). 0 means "not stated".
type DefaultMaxTokenser interface {
	DefaultMaxTokens() int
}

// EstimateInputTokens is a deliberately conservative (high) input-token estimate for a request.
// Character counts follow Python: len() in code points of json.dumps output (ASCII-escaped,
// ", "/": " separators).
func EstimateInputTokens(messages []contracts.ModelMessage, tools []map[string]any) int {
	chars := 0
	for _, m := range messages {
		chars += utf8.RuneCountInString(m.Content)
	}
	for _, m := range messages {
		for _, c := range m.ToolCalls {
			chars += len(pyJSONDumps(mapOrEmpty(c.Arguments))) + utf8.RuneCountInString(c.Name)
		}
	}
	if len(tools) > 0 {
		list := make([]any, len(tools))
		for i, t := range tools {
			list[i] = t
		}
		chars += len(pyJSONDumps(list))
	}
	return int(math.Ceil(float64(chars)/charsPerToken)) + perMessageOverheadTokens*len(messages)
}

func mapOrEmpty(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

// CostMeter is the shared ledger + governor + in-flight reservations for every metered provider
// of a mission. It is safe for concurrent use.
type CostMeter struct {
	Ledger                 *CostLedger
	Governor               *BudgetGovernor
	AssumedMaxOutputTokens int

	mu       sync.Mutex
	cycleID  string
	reserved float64
}

// NewCostMeter returns a meter attributing calls to cycle "c0" until SetCycleID is called.
func NewCostMeter(ledger *CostLedger, governor *BudgetGovernor) *CostMeter {
	return &CostMeter{Ledger: ledger, Governor: governor, AssumedMaxOutputTokens: DefaultAssumedMaxOutput, cycleID: defaultCycleID}
}

// CycleID is the cycle subsequent calls are attributed to.
func (m *CostMeter) CycleID() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cycleID
}

// SetCycleID attributes subsequent calls to cycleID.
func (m *CostMeter) SetCycleID(cycleID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cycleID = cycleID
}

// ReservedUSD is the worst-case cost of calls currently in flight.
func (m *CostMeter) ReservedUSD() float64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reserved
}

// Wrap returns provider metered by this meter, recording its calls under role.
func (m *CostMeter) Wrap(provider contracts.ModelProvider, role string) *MeteredModel {
	return &MeteredModel{provider: provider, meter: m, Role: role}
}

// MeteredModel is a contracts.ModelProvider that enforces the budget per call and records every
// call's spend.
type MeteredModel struct {
	provider contracts.ModelProvider
	meter    *CostMeter
	Role     string
}

var _ contracts.ModelProvider = (*MeteredModel)(nil)

// Name is the wrapped provider's name.
func (mm *MeteredModel) Name() string { return mm.provider.Name() }

// Inner is the wrapped provider.
func (mm *MeteredModel) Inner() contracts.ModelProvider { return mm.provider }

// DefaultMaxTokens forwards the wrapped provider's default output ceiling (0 if not stated).
func (mm *MeteredModel) DefaultMaxTokens() int {
	if d, ok := mm.provider.(DefaultMaxTokenser); ok {
		return d.DefaultMaxTokens()
	}
	return 0
}

// WorstCaseUSD is an upper bound on this call's cost; nil (with a nil error) if the provider
// cannot price it. maxTokens <= 0 means "provider default".
func (mm *MeteredModel) WorstCaseUSD(messages []contracts.ModelMessage, tools []map[string]any, maxTokens int) (*float64, error) {
	outputCap := maxTokens
	if outputCap <= 0 {
		outputCap = mm.DefaultMaxTokens()
	}
	if outputCap <= 0 {
		outputCap = mm.meter.AssumedMaxOutputTokens
	}
	usage := contracts.Usage{InputTokens: EstimateInputTokens(messages, tools), OutputTokens: outputCap}
	return priceOrNil(mm.provider, usage)
}

func priceOrNil(p contracts.ModelProvider, usage contracts.Usage) (*float64, error) {
	usd, err := p.EstimateCostUSD(usage)
	if errors.Is(err, contracts.ErrUnknownPrice) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &usd, nil
}

// Complete authorizes the call's worst case (plus in-flight reservations), runs it, and records
// its actual cost. A refusal returns a *BudgetExceeded without calling the provider.
func (mm *MeteredModel) Complete(ctx context.Context, messages []contracts.ModelMessage, tools []map[string]any, maxTokens int) (contracts.TurnResult, error) {
	meter := mm.meter
	worst, err := mm.WorstCaseUSD(messages, tools, maxTokens)
	if err != nil {
		return contracts.TurnResult{}, err
	}
	reservation := 0.0
	if worst != nil {
		reservation = *worst
	}
	// Authorize and reserve atomically so concurrent calls cannot collectively overshoot.
	meter.mu.Lock()
	decision := meter.Governor.AuthorizeCall(meter.Ledger, worst, meter.reserved)
	if !decision.Allow {
		meter.mu.Unlock()
		return contracts.TurnResult{}, &BudgetExceeded{Decision: decision}
	}
	meter.reserved += reservation
	meter.mu.Unlock()

	result, err := mm.provider.Complete(ctx, messages, tools, maxTokens)
	meter.mu.Lock()
	meter.reserved = max(0.0, meter.reserved-reservation)
	meter.mu.Unlock()
	if err != nil {
		return contracts.TurnResult{}, err
	}

	usd, err := priceOrNil(mm.provider, result.Usage)
	if err != nil {
		return contracts.TurnResult{}, err
	}
	meter.Ledger.Record(meter.CycleID(), result.Usage, usd, mm.Role)
	return result, nil
}

// EstimateCostUSD forwards to the wrapped provider.
func (mm *MeteredModel) EstimateCostUSD(usage contracts.Usage) (float64, error) {
	return mm.provider.EstimateCostUSD(usage)
}

// Close closes the wrapped provider if it has a Close(ctx) method (Python: aclose).
func (mm *MeteredModel) Close(ctx context.Context) error {
	if c, ok := mm.provider.(interface{ Close(context.Context) error }); ok {
		return c.Close(ctx)
	}
	return nil
}

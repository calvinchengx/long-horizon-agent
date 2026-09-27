package governor

import (
	"context"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

// ExternalWork is work that spends outside Complete (a whole claude -p session). It returns the
// usage to record; Usage.ReportedCostUSD is the cost the work itself reported (nil = none).
type ExternalWork func(ctx context.Context) (contracts.Usage, error)

// RunExternal meters work that spends outside Complete (python: MeteredModel.run_external).
//
// It is authorized with worstCaseUSD like any call (plus in-flight reservations; a refusal
// returns a *BudgetExceeded without running it); afterwards the cost the work reported
// (Usage.ReportedCostUSD) is recorded. Work that reported nothing (killed on a timeout) is
// charged its full worstCaseUSD: conservative, never $0. Work that fails records nothing.
func (mm *MeteredModel) RunExternal(ctx context.Context, worstCaseUSD float64, run ExternalWork) error {
	worst := worstCaseUSD
	return mm.meter.RunExternal(ctx, &worst, mm.Role, run)
}

// RunExternal meters work that is not a ModelProvider call (python: CostMeter.run_external, a
// System One evaluation), recorded under role. A nil worstCaseUSD is work that cannot be priced,
// which the governor refuses unless unknown cost is allowed; its cost is then recorded as unknown
// unless the work reported one.
func (m *CostMeter) RunExternal(ctx context.Context, worstCaseUSD *float64, role string, run ExternalWork) error {
	reservation := 0.0
	if worstCaseUSD != nil {
		reservation = *worstCaseUSD
	}
	m.mu.Lock()
	decision := m.Governor.AuthorizeCall(m.Ledger, worstCaseUSD, m.reserved)
	if !decision.Allow {
		m.mu.Unlock()
		return &BudgetExceeded{Decision: decision}
	}
	m.reserved += reservation
	m.mu.Unlock()

	usage, err := run(ctx)
	m.mu.Lock()
	m.reserved = max(0.0, m.reserved-reservation)
	m.mu.Unlock()
	if err != nil {
		return err
	}
	usd := worstCaseUSD
	if usage.ReportedCostUSD != nil {
		reported := *usage.ReportedCostUSD
		usd = &reported
	}
	m.record(ctx, usage, usd, role) // reaches the CostHook too (python: on_record)
	return nil
}

// RunExternal is MeteredModel.RunExternal when model is metered; otherwise it just runs the work
// (python: governor.metering.run_external).
func RunExternal(ctx context.Context, model contracts.ModelProvider, worstCaseUSD float64, run ExternalWork) error {
	if mm, ok := model.(*MeteredModel); ok {
		return mm.RunExternal(ctx, worstCaseUSD, run)
	}
	_, err := run(ctx)
	return err
}

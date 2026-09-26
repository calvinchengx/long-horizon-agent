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
	meter := mm.meter
	worst := worstCaseUSD
	meter.mu.Lock()
	decision := meter.Governor.AuthorizeCall(meter.Ledger, &worst, meter.reserved)
	if !decision.Allow {
		meter.mu.Unlock()
		return &BudgetExceeded{Decision: decision}
	}
	meter.reserved += worstCaseUSD
	meter.mu.Unlock()

	usage, err := run(ctx)
	meter.mu.Lock()
	meter.reserved = max(0.0, meter.reserved-worstCaseUSD)
	meter.mu.Unlock()
	if err != nil {
		return err
	}
	usd := worstCaseUSD
	if usage.ReportedCostUSD != nil {
		usd = *usage.ReportedCostUSD
	}
	meter.record(ctx, usage, &usd, mm.Role) // reaches the CostHook too (python: on_record)
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

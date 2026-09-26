package governor

import (
	"context"
	"errors"
	"testing"

	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
)

type freeModel struct{}

func (freeModel) Name() string { return "free" }
func (freeModel) Complete(context.Context, []contracts.ModelMessage, []map[string]any, int) (contracts.TurnResult, error) {
	return contracts.TurnResult{}, nil
}
func (freeModel) EstimateCostUSD(contracts.Usage) (float64, error) { return 0, nil }

// python: MeteredModel.run_external / governor.metering.run_external.
func TestRunExternalAuthorizesReservesAndRecordsTheReportedCost(t *testing.T) {
	meter := NewCostMeter(NewCostLedger(), NewBudgetGovernor(1.0, 10, false))
	meter.SetCycleID("c3")
	lead := meter.Wrap(freeModel{}, "lead")

	reported := 0.25
	err := RunExternal(context.Background(), lead, 0.5, func(context.Context) (contracts.Usage, error) {
		if meter.ReservedUSD() != 0.5 { // the worst case is held while the work runs
			t.Errorf("reserved %v", meter.ReservedUSD())
		}
		return contracts.Usage{Provider: "p", ReportedCostUSD: &reported}, nil
	})
	if err != nil || meter.ReservedUSD() != 0 {
		t.Fatal(err, meter.ReservedUSD())
	}
	// Nothing reported (a killed session): charged the full worst case, never $0.
	if err := RunExternal(context.Background(), lead, 0.5, func(context.Context) (contracts.Usage, error) {
		return contracts.Usage{Provider: "p"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	entries := meter.Ledger.Entries()
	if len(entries) != 2 || entries[0].CycleID != "c3" || entries[0].Role != "lead" || meter.Ledger.TotalUSD() != 0.75 {
		t.Fatalf("%+v", entries)
	}

	// $0.75 spent + $0.5 worst case > $1: refused before the work runs.
	ran := false
	err = RunExternal(context.Background(), lead, 0.5, func(context.Context) (contracts.Usage, error) {
		ran = true
		return contracts.Usage{}, nil
	})
	var refused *BudgetExceeded
	if !errors.As(err, &refused) || ran {
		t.Fatalf("err = %v, ran = %v", err, ran)
	}

	// Failed work records nothing and releases its reservation.
	boom := errors.New("boom")
	if err := RunExternal(context.Background(), meter.Wrap(freeModel{}, "x"), 0.1, func(context.Context) (contracts.Usage, error) {
		return contracts.Usage{}, boom
	}); !errors.Is(err, boom) || len(meter.Ledger.Entries()) != 2 || meter.ReservedUSD() != 0 {
		t.Fatalf("err = %v", err)
	}

	// Unmetered: the work just runs.
	if err := RunExternal(context.Background(), freeModel{}, 99, func(context.Context) (contracts.Usage, error) {
		ran = true
		return contracts.Usage{}, nil
	}); err != nil || !ran {
		t.Fatal(err)
	}
}

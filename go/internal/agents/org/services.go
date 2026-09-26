package org

import (
	"context"

	"github.com/calvinchengx/long-horizon-agent/go/internal/config"
	"github.com/calvinchengx/long-horizon-agent/go/internal/contracts"
	"github.com/calvinchengx/long-horizon-agent/go/internal/governor"
	"github.com/calvinchengx/long-horizon-agent/go/internal/obs"
)

// What a run persists besides the anchor (python: lha.persistence.services.open_run_services):
// the mission row, the persistent cost ledger (a sink on the meter) and tiered memory. The
// orchestrator talks to it through this small interface; the default is a no-op until the
// persistence plane is linked (set DefaultServices, or OrchestratorOptions.Services).

// ServicesRequest is what a ServicesOpener gets (python: open_run_services' arguments).
type ServicesRequest struct {
	Settings    *config.Settings
	MissionID   string
	Workdir     string
	Meter       *governor.CostMeter
	Title       string
	Description string
	// Model is the librarian's provider (the lead's model, metered as "librarian").
	Model    contracts.ModelProvider
	Recorder *obs.TraceRecorder
	// KeyPrefix keeps a resumed run's ledger keys apart ("" on the first run, "run<N>" after).
	KeyPrefix string
	// Gate is the run's human gate, for services that record its events (python:
	// bind_gate_store); nil when there is none.
	Gate contracts.HITLGate
}

// RunServices is one run's persistence (python: RunServices).
type RunServices interface {
	// Running marks the mission row running (python: tracker.running()).
	Running(ctx context.Context) error
	// Finish records how the run ended (python: services.finish(stopped_reason, head_sha=)).
	Finish(ctx context.Context, stoppedReason, headSHA string) error
	// Close releases the store and memory and detaches the ledger sink.
	Close(ctx context.Context) error
}

// ServicesOpener opens a run's services.
type ServicesOpener func(ctx context.Context, req ServicesRequest) (RunServices, error)

// NoopServices persists nothing.
type NoopServices struct{}

// Running implements RunServices.
func (NoopServices) Running(context.Context) error { return nil }

// Finish implements RunServices.
func (NoopServices) Finish(context.Context, string, string) error { return nil }

// Close implements RunServices.
func (NoopServices) Close(context.Context) error { return nil }

// DefaultServices is the opener an Orchestrator uses when its options name none.
var DefaultServices ServicesOpener = func(context.Context, ServicesRequest) (RunServices, error) {
	return NoopServices{}, nil
}
